package proxmox

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/Mouriya-Emma/coredns-proxmox/internal/pveapi"
)

// net0Channel claims the interface whose hardware MAC matches a guest's
// net0 MAC, as declared in the PVE guest config.
//
// Purpose: cover guests on a regular vmbr (no SR-IOV). Their qemu-agent /
// LXC interface response includes the net0 NIC plus guest-side junk
// (docker0, wt0 from netbird, podman bridges, …); under the strict
// allow-list the plugin can't know which one is the LAN NIC unless a
// channel positively points at it. The PVE config is the authoritative
// pointer: net0 is the guest's primary virtualised NIC and its MAC is
// pinned at config time.
//
// Source formats (both handled by parseNet0):
//   - VM (qm config): `<model>=<mac>,bridge=<br>,...` where model is
//     virtio / e1000 / rtl8139 / vmxnet3 / etc. Value of the model key
//     is the MAC.
//   - CT (pct config): `name=<if>,bridge=<br>,hwaddr=<mac>,...` with an
//     explicit hwaddr key.
//
// We pick the MAC out by regex-matching MAC-shaped values rather than
// hardcoding model names — forward-compatible with any future NIC model
// PVE adds.
//
// Why MAC, not interface name / bridge / IP: MAC is the only identifier
// both the PVE config side and the guest-report side (qemu-agent /
// lxc /interfaces) name consistently. In-guest interface naming is
// decided by the guest kernel + systemd (eth0 / ens18 / enp6s18 /
// whatever) and doesn't match the CT config's `name=eth0` which is the
// veth's host-side name; VM configs have no in-guest name field at all.
// `bridge=` is a host concept the guest never sees. IPs are what we're
// trying to learn. MAC is written into the VM's emulated NIC / the CT's
// veth guest-end at launch and round-trips back through the agent
// verbatim.
//
// MAC presence: the PVE net0 schema makes MAC syntactically optional
// (`<model>[=<mac>]` for VMs, `hwaddr=<mac>` as one of many keys for
// CTs), but every normal creation path — web UI, `qm/pct create`, API —
// auto-generates a MAC and writes it back on create. A net0 config
// without a MAC can only come from hand-editing the config file and
// never triggering a lifecycle operation after; in practice it doesn't
// happen. If it does, parseNet0 returns ("", false), refreshOne clears
// the cache entry, and Claims returns false for that guest — the
// channel simply doesn't cover it. Other channels (sriov, permissive)
// still apply; a guest no channel covers drops from DNS, consistent
// with the v0.1.5 strict-allow-list default.
type net0Channel struct {
	client pveapi.Client

	mu   sync.RWMutex
	macs map[guestID]string // vmid → lowercase MAC
}

func newNet0Channel(client pveapi.Client) *net0Channel {
	return &net0Channel{
		client: client,
		macs:   make(map[guestID]string),
	}
}

func (c *net0Channel) Name() string { return "net0" }

// OnReconcile refreshes the per-guest MAC cache. For each currently-
// running guest, fetch its PVE config and re-parse net0. Guests that
// disappeared from the list are evicted. Transient per-guest fetch
// failures log at Debug and leave the previous cached MAC intact — a
// briefly unreachable PVE API shouldn't blank a good entry and cause
// Claims to start returning false.
func (c *net0Channel) OnReconcile(ctx context.Context, guests []guestID) error {
	current := make(map[guestID]bool, len(guests))
	for _, gid := range guests {
		current[gid] = true
		if err := c.refreshOne(ctx, gid); err != nil {
			log.Debugf("net0: refresh %s/%d on %s failed: %v — keeping cached MAC",
				gid.Type, gid.VMID, gid.Node, err)
		}
	}

	c.mu.Lock()
	for gid := range c.macs {
		if !current[gid] {
			delete(c.macs, gid)
		}
	}
	c.mu.Unlock()
	return nil
}

// refreshOne fetches one guest's config and updates the MAC cache. If the
// fetch or parse says "no net0 here" (empty string, unparseable), the
// cache entry is cleared — a guest whose net0 is removed shouldn't keep
// a stale claim. If the fetch errors out we return the error and leave
// the cache untouched (caller logs at Debug).
func (c *net0Channel) refreshOne(ctx context.Context, gid guestID) error {
	raw, err := c.fetchNet0(ctx, gid)
	if err != nil {
		return err
	}
	mac, ok := parseNet0(raw)
	c.mu.Lock()
	defer c.mu.Unlock()
	if !ok {
		delete(c.macs, gid)
		return nil
	}
	c.macs[gid] = mac
	return nil
}

func (c *net0Channel) fetchNet0(ctx context.Context, gid guestID) (string, error) {
	switch gid.Type {
	case "qemu":
		cfg, err := c.client.GetQEMUConfig(ctx, gid.Node, gid.VMID)
		if err != nil {
			return "", err
		}
		return cfg.Net0, nil
	case "lxc":
		cfg, err := c.client.GetLXCConfig(ctx, gid.Node, gid.VMID)
		if err != nil {
			return "", err
		}
		return cfg.Net0, nil
	}
	return "", fmt.Errorf("unknown guest type: %s", gid.Type)
}

func (c *net0Channel) Claims(gid guestID, iface InterfaceInfo) bool {
	c.mu.RLock()
	mac, ok := c.macs[gid]
	c.mu.RUnlock()
	if !ok || mac == "" {
		return false
	}
	return macEquals(mac, iface.Mac)
}

// macLike matches a 6-pair hex MAC in standard colon-separated form.
// Used to spot MAC values in net0 `k=v` pairs without knowing the full
// set of NIC model keys PVE may use now or in the future.
var macLike = regexp.MustCompile(`^[0-9A-Fa-f]{2}(?::[0-9A-Fa-f]{2}){5}$`)

// parseNet0 extracts a MAC from a PVE net0 string. Accepts both VM
// format (`virtio=MAC,bridge=vmbr0,...`) and CT format (`name=eth0,
// bridge=vmbr0,hwaddr=MAC,...`) — we just scan all `k=v` pairs and
// return the first MAC-shaped value. Lowercased on return.
//
// Returns ("", false) on empty input or when no MAC-shaped value is
// found.
func parseNet0(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	for _, field := range strings.Split(raw, ",") {
		eq := strings.IndexByte(field, '=')
		if eq < 0 {
			continue
		}
		value := strings.TrimSpace(field[eq+1:])
		if macLike.MatchString(value) {
			return strings.ToLower(value), true
		}
	}
	return "", false
}
