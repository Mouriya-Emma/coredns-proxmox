// Package proxmox is a CoreDNS plugin that answers A/AAAA queries for
// Proxmox VM and LXC guest hostnames, resolving them via the Proxmox API.
//
// Architecture: a supervisor goroutine reconciles the cluster state every
// reconcile_every (default 60s). For each running guest it spawns a
// dedicated goroutine that polls that guest's IPs on its own schedule —
// aggressively (poll_never_ips, default 60s) until it first succeeds, then
// relaxed (poll_known_ips, default 5min) to pick up IP changes. This keeps
// slow-booting VMs from blocking anything else, and prevents transient
// agent-not-responding errors from evicting good records.
//
// Key features:
//   - LXC discovery via /lxc/<id>/interfaces (no qemu-agent required)
//   - QEMU discovery via /qemu/<id>/agent/network-get-interfaces
//   - Per-IP CIDR allowlist: essential for SR-IOV passthrough VMs where
//     qemu-agent reports many non-LAN IPs (docker bridges, overlay meshes).
package proxmox

import (
	"context"
	"net/netip"
	"strings"
	"time"

	"github.com/coredns/coredns/plugin"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/request"

	"github.com/miekg/dns"

	"github.com/Mouriya-Emma/coredns-proxmox/internal/pveapi"
)

var log = clog.NewWithPlugin("proxmox")

// Proxmox is the CoreDNS plugin. One instance per server block.
type Proxmox struct {
	Next plugin.Handler

	// Zones this plugin is authoritative-ish for. Lowercase, trailing dot.
	Zones []string

	// TTL for emitted A/AAAA records.
	TTL uint32

	// ReconcileEvery controls the cluster-list sync cadence.
	ReconcileEvery time.Duration

	// PollNever is the per-guest poll interval until the first successful
	// IP acquisition. Short — we want boot-time records to appear fast.
	PollNever time.Duration

	// PollKnown is the per-guest poll interval once we've seen IPs at least
	// once. Long — most of the time IPs don't change; this just catches
	// the cases where they do (DHCP renew with different lease, manual
	// reconfigure, etc.).
	PollKnown time.Duration

	// AllowCIDRs, if non-empty, keeps only IPs in one of these prefixes.
	AllowCIDRs []netip.Prefix

	// ExcludeIPs drops these specific IPs from every emitted record. Matches
	// the scanner's IP_SKIP semantics — used to hide IPs already claimed by
	// an authoritative source (static hosts file, other PVE NICs).
	ExcludeIPs []netip.Addr

	// SriovStatePath, if set, is the on-disk path (usually a bind-mounted
	// file from PVE host) containing `sriov dump` JSON. When present the
	// SR-IOV channel activates — see channel.go for the allow-list model.
	SriovStatePath string

	// PermissiveChannel opts in to the legacy v0.1.4-style deny-list
	// behaviour: every interface whose name isn't known-noise contributes
	// IPs. Default off. Useful for guests with neither SR-IOV nor an
	// explicitly-tracked net0, where "anything sensible-looking" is
	// better than nothing.
	PermissiveChannel bool

	// Net0Channel opts in to the net0 channel — per-guest PVE config
	// fetch that claims the interface whose MAC matches the guest's
	// declared net0 MAC. Covers guests on a regular vmbr (no SR-IOV).
	// Off by default.
	Net0Channel bool

	// Fallthrough hands off to the next plugin on no-match.
	Fallthrough bool

	client pveapi.Client
	store  *store
	sup    *supervisor
	cancel context.CancelFunc
}

func (p *Proxmox) Name() string { return "proxmox" }

func (p *Proxmox) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}
	qname := strings.ToLower(state.Name())

	if plugin.Zones(p.Zones).Matches(qname) == "" {
		return plugin.NextOrFailure(p.Name(), p.Next, ctx, w, r)
	}

	ips := p.store.Lookup(qname)
	if len(ips) == 0 {
		return p.miss(ctx, w, r)
	}

	qtype := state.QType()
	msg := new(dns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true

	for _, ip := range ips {
		if v4 := ip.To4(); v4 != nil && qtype == dns.TypeA {
			msg.Answer = append(msg.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: state.Name(), Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: p.TTL},
				A:   v4,
			})
		} else if v4 == nil && qtype == dns.TypeAAAA {
			msg.Answer = append(msg.Answer, &dns.AAAA{
				Hdr:  dns.RR_Header{Name: state.Name(), Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: p.TTL},
				AAAA: ip,
			})
		}
	}

	if len(msg.Answer) == 0 {
		return p.miss(ctx, w, r)
	}

	if err := w.WriteMsg(msg); err != nil {
		return dns.RcodeServerFailure, err
	}
	return dns.RcodeSuccess, nil
}

func (p *Proxmox) miss(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	if p.Fallthrough {
		return plugin.NextOrFailure(p.Name(), p.Next, ctx, w, r)
	}
	// Authoritative no-data: empty answer section, NOERROR (matches hosts-plugin
	// behaviour when name is known but qtype doesn't match; NXDOMAIN would be
	// wrong since we only know a subset of the zone).
	msg := new(dns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true
	_ = w.WriteMsg(msg)
	return dns.RcodeSuccess, nil
}

// Start wires up the store + supervisor and spawns the reconcile loop.
// Returns immediately; the initial reconcile runs inside the supervisor
// goroutine so the plugin can start even when PVE is unreachable.
func (p *Proxmox) Start() error {
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel
	p.store = newStore()

	// Build the channel list. Order matters only for logging ("first
	// channel that claims wins"); the claim set is the same regardless
	// since claimsAny is a boolean OR.
	var channels []Channel
	if p.SriovStatePath != "" {
		channels = append(channels, newSriovChannel(newSriovState(p.SriovStatePath)))
	}
	if p.Net0Channel {
		channels = append(channels, newNet0Channel(p.client))
	}
	if p.PermissiveChannel {
		channels = append(channels, newPermissiveChannel(nil, nil))
	}
	if len(channels) == 0 {
		log.Warningf("no channels enabled (sriov_state unset, net0 off, permissive off) — plugin will never resolve anything; set at least one channel in the Corefile")
	} else {
		names := make([]string, 0, len(channels))
		for _, ch := range channels {
			names = append(names, ch.Name())
		}
		log.Infof("channels enabled: %s", strings.Join(names, ", "))
	}

	p.sup = newSupervisor(p.client, p.store, p.Zones, p.AllowCIDRs, p.ExcludeIPs, channels,
		p.ReconcileEvery, p.PollNever, p.PollKnown)
	go p.sup.Run(ctx)
	return nil
}

// Stop cancels the supervisor and all per-guest goroutines, then waits for
// the supervisor Run loop to exit. Blocking here preserves the OnShutdown
// invariant that "return = resources released" — without the wait, a
// CoreDNS reload can briefly run the old plugin's supervisor alongside the
// new one's, doubling PVE API load until the old goroutine drains.
func (p *Proxmox) Stop() error {
	if p.cancel != nil {
		p.cancel()
	}
	if p.sup != nil {
		<-p.sup.done
	}
	return nil
}

// Compile-time assertion: we satisfy the plugin.Handler interface.
var _ plugin.Handler = (*Proxmox)(nil)
