package proxmox

import (
	"context"
	"fmt"
	"math/rand"
	"net"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/Mouriya-Emma/coredns-proxmox/internal/pveapi"
)

// supervisor reconciles PVE cluster state into a population of per-guest
// goroutines.
//
// Why per-guest goroutines: each guest acquires its IP asynchronously and on
// its own timeline. VMs can take minutes (bootloader → kernel → userspace →
// networking → qemu-guest-agent becoming responsive), while LXCs are near-
// instant. A single shared refresh loop that rebuilds the whole records map
// every N seconds has two bad properties:
//
//   1. Per-resource failures cause temporary eviction. A VM whose agent is
//      briefly unreachable vanishes from DNS until the next full sweep
//      succeeds — even if we had a good answer a second ago.
//   2. The "still warming up" case (cold start, VM still booting) is
//      coupled to everything else: we can't poll it fast without
//      hammering the whole cluster.
//
// Splitting into one goroutine per guest fixes both. Each goroutine:
//   - polls its own guest on its own schedule,
//   - writes only its own entry in the store (no contention with peers),
//   - has independent lifetime tied to context cancel.
//
// Schedule: a goroutine that has never successfully acquired IPs polls
// aggressively (pollNever, default 60s) so the "still booting" window
// resolves quickly. Once it's gotten IPs at least once, it backs off to
// pollKnown (default 5min) — enough to catch an IP change within minutes
// without turning the PVE API into a busy-loop target.
type supervisor struct {
	client pveapi.Client
	store  *store
	zones  []string // trailing-dot, lowercase

	allowCIDRs []netip.Prefix
	excludeIPs []netip.Addr

	// channels decide which interfaces of a guest contribute IPs. Strict
	// allow-list: an interface contributes only if at least one channel
	// claims it. See channel.go.
	channels []Channel

	enumerateEvery time.Duration // cluster-list reconcile cadence
	pollNever      time.Duration // per-guest cadence while no IPs yet
	pollKnown      time.Duration // per-guest cadence once first success

	mu      sync.Mutex
	cancels map[guestID]context.CancelFunc
	wg      sync.WaitGroup
}

func newSupervisor(client pveapi.Client, st *store, zones []string,
	allow []netip.Prefix, exclude []netip.Addr, channels []Channel,
	enumerateEvery, pollNever, pollKnown time.Duration) *supervisor {
	return &supervisor{
		client:         client,
		store:          st,
		zones:          zones,
		allowCIDRs:     allow,
		excludeIPs:     exclude,
		channels:       channels,
		enumerateEvery: enumerateEvery,
		pollNever:      pollNever,
		pollKnown:      pollKnown,
		cancels:        make(map[guestID]context.CancelFunc),
	}
}

// Run drives the cluster reconcile loop until ctx is cancelled. Per-guest
// goroutines inherit a child of ctx so they all terminate on plugin shutdown.
func (s *supervisor) Run(ctx context.Context) {
	t := time.NewTicker(s.enumerateEvery)
	defer t.Stop()
	s.reconcile(ctx) // initial, don't wait for first tick
	for {
		select {
		case <-ctx.Done():
			s.stopAll()
			return
		case <-t.C:
			s.reconcile(ctx)
		}
	}
}

func (s *supervisor) stopAll() {
	s.mu.Lock()
	for gid, cancel := range s.cancels {
		cancel()
		delete(s.cancels, gid)
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// guestMeta is the subset of PVE cluster data needed to spawn a per-guest
// goroutine.
type guestMeta struct {
	ID   guestID
	Name string // PVE-side guest name; FQDN is derived at poll time
}

// fqdn builds this guest's primary-zone FQDN. The result has a trailing dot
// to match DNS canonical form (and what Zones.Matches expects).
func (g guestMeta) fqdn(zones []string) string {
	name := strings.ToLower(strings.TrimSuffix(g.Name, "."))
	if i := strings.Index(name, "."); i >= 0 {
		name = name[:i]
	}
	if name == "" || len(zones) == 0 {
		return ""
	}
	return name + "." + zones[0]
}

// reconcile performs one pass: enumerate the cluster, diff against the
// currently-tracked set, spawn goroutines for new guests and cancel +
// evict those that disappeared.
//
// Enumerate is all-or-nothing — a partial failure (e.g. one node's /qemu
// errored) discards the whole reconcile to avoid false evictions from a
// transiently-unreachable node. Existing goroutines keep running with
// their last-known state.
func (s *supervisor) reconcile(ctx context.Context) {
	meta, err := s.enumerate(ctx)
	if err != nil {
		log.Warningf("cluster enumerate failed, skipping reconcile: %v", err)
		return
	}

	currentSet := make(map[guestID]guestMeta, len(meta))
	guestIDs := make([]guestID, 0, len(meta))
	for _, g := range meta {
		currentSet[g.ID] = g
		guestIDs = append(guestIDs, g.ID)
	}

	// Give every channel a chance to refresh its cached state before the
	// per-guest goroutines poll. Channels that cache per-guest config
	// (net0, future ssh) use guestIDs to drive fetch + evict. Failures
	// are non-fatal — a channel with stale data just keeps its last good
	// view.
	for _, ch := range s.channels {
		if err := ch.OnReconcile(ctx, guestIDs); err != nil {
			log.Warningf("channel %s reconcile refresh failed: %v", ch.Name(), err)
		}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Evict: guests no longer running / no longer present.
	for gid, cancel := range s.cancels {
		if _, ok := currentSet[gid]; !ok {
			cancel()
			delete(s.cancels, gid)
			s.store.Delete(gid)
			log.Infof("guest %s/%d on %s gone — goroutine cancelled, record evicted",
				gid.Type, gid.VMID, gid.Node)
		}
	}

	// Spawn: new or restarted guests.
	for gid, g := range currentSet {
		if _, ok := s.cancels[gid]; ok {
			continue
		}
		gctx, gcancel := context.WithCancel(ctx)
		s.cancels[gid] = gcancel
		s.wg.Add(1)
		go s.pollGuest(gctx, g)
		log.Infof("tracking guest %s/%d on %s (%q)", gid.Type, gid.VMID, gid.Node, g.Name)
	}
}

// enumerate lists running guests cluster-wide. Returns an error on any
// per-node call failure so the caller can decide whether to act on a
// partial result. We choose not to (skip reconcile).
func (s *supervisor) enumerate(ctx context.Context) ([]guestMeta, error) {
	rc, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	nodes, err := s.client.GetNodes(rc)
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	var out []guestMeta
	for _, node := range nodes {
		vms, err := s.client.GetQEMUVMs(rc, node.Node)
		if err != nil {
			return nil, fmt.Errorf("list qemu on %s: %w", node.Node, err)
		}
		for _, vm := range vms {
			if vm.Status != "running" {
				continue
			}
			out = append(out, guestMeta{
				ID:   guestID{Node: node.Node, Type: "qemu", VMID: vm.VMID},
				Name: vm.Name,
			})
		}
		lxcs, err := s.client.GetLXCs(rc, node.Node)
		if err != nil {
			return nil, fmt.Errorf("list lxc on %s: %w", node.Node, err)
		}
		for _, lx := range lxcs {
			if lx.Status != "running" {
				continue
			}
			out = append(out, guestMeta{
				ID:   guestID{Node: node.Node, Type: "lxc", VMID: lx.VMID},
				Name: lx.Name,
			})
		}
	}
	return out, nil
}

// pollGuest is the per-guest state machine. Loops until context cancel.
// The first successful fetch flips the cadence from pollNever to pollKnown
// and logs at INFO so the operator can see a cold start progressing.
// Subsequent successes re-upsert at pollKnown pace. Failures keep the last
// known record intact (no eviction from this path — only the reconcile
// can evict).
func (s *supervisor) pollGuest(ctx context.Context, g guestMeta) {
	defer s.wg.Done()

	fqdn := g.fqdn(s.zones)
	if fqdn == "" {
		log.Warningf("guest %s/%d has empty name — not polling", g.ID.Type, g.ID.VMID)
		return
	}

	hasSucceeded := false
	for {
		ips, err := s.fetchGuestIPs(ctx, g.ID)
		switch {
		case err != nil:
			log.Debugf("fetch IPs %s/%d on %s: %v — keeping last record", g.ID.Type, g.ID.VMID, g.ID.Node, err)
		case len(ips) == 0:
			// API succeeded but filter removed everything, or agent returned
			// nothing. Don't evict; the guest might be still bringing up its
			// LAN interface after a good inner-network interface. Try again.
			log.Debugf("fetch IPs %s/%d on %s: no IPs after filter — keeping last record", g.ID.Type, g.ID.VMID, g.ID.Node)
		default:
			s.store.Upsert(g.ID, fqdn, ips)
			if !hasSucceeded {
				log.Infof("first IPs for %s %q (guest %s/%d on %s): %v",
					g.ID.Type, g.Name, g.ID.Type, g.ID.VMID, g.ID.Node, ips)
				hasSucceeded = true
			}
		}

		wait := s.pollNever
		if hasSucceeded {
			wait = s.pollKnown
		}
		wait = withJitter(wait, 0.1)

		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// fetchGuestIPs returns the LAN IPs attributed to a guest. Interfaces are
// gathered from the appropriate PVE API endpoint, normalised into a shared
// InterfaceInfo shape, then filtered through the channel list (strict
// allow-list: keep only if some channel claims the interface). Remaining
// IPs pass through allow_cidr / exclude_ip as a final address-level trim.
func (s *supervisor) fetchGuestIPs(ctx context.Context, gid guestID) ([]net.IP, error) {
	rc, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ifaces, err := s.collectInterfaces(rc, gid)
	if err != nil {
		return nil, err
	}
	var ips []net.IP
	for _, iface := range ifaces {
		if !claimsAny(s.channels, gid, iface) {
			continue
		}
		ips = append(ips, iface.IPs...)
	}
	return filterIPs(ips, s.allowCIDRs, s.excludeIPs), nil
}

// collectInterfaces normalises qemu-agent / lxc-interfaces responses into a
// shared InterfaceInfo list. Keeps the dispatch localised here so channels
// never touch pveapi types.
func (s *supervisor) collectInterfaces(ctx context.Context, gid guestID) ([]InterfaceInfo, error) {
	switch gid.Type {
	case "lxc":
		ifs, err := s.client.GetLXCInterfaces(ctx, gid.Node, gid.VMID)
		if err != nil {
			return nil, err
		}
		out := make([]InterfaceInfo, 0, len(ifs))
		for _, ifc := range ifs {
			var ips []net.IP
			for _, raw := range []string{ifc.Inet, ifc.Inet6} {
				ips = appendParsed(ips, raw)
			}
			out = append(out, InterfaceInfo{
				Name: ifc.Name,
				Mac:  strings.ToLower(strings.TrimSpace(ifc.HardwareAddress)),
				IPs:  ips,
			})
		}
		return out, nil
	case "qemu":
		resp, err := s.client.GetQEMUInterfaces(ctx, gid.Node, gid.VMID)
		if err != nil {
			return nil, err
		}
		out := make([]InterfaceInfo, 0, len(resp.Result))
		for _, ifc := range resp.Result {
			var ips []net.IP
			for _, a := range ifc.IPAddresses {
				ips = appendParsed(ips, a.Address)
			}
			out = append(out, InterfaceInfo{
				Name: ifc.Name,
				Mac:  strings.ToLower(strings.TrimSpace(ifc.HardwareAddress)),
				IPs:  ips,
			})
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unknown guest type: %s", gid.Type)
	}
}

// withJitter spreads a base interval by ±frac (0.1 = ±10%). Prevents every
// goroutine from hitting the PVE API at the same moment after a synchronised
// start.
func withJitter(d time.Duration, frac float64) time.Duration {
	delta := int64(float64(d) * frac)
	if delta <= 0 {
		return d
	}
	// #nosec G404 — non-crypto jitter
	j := rand.Int63n(2*delta) - delta
	out := d + time.Duration(j)
	if out < 0 {
		return d
	}
	return out
}
