package proxmox

import (
	"context"
	"net"
)

// InterfaceInfo is a channel-agnostic view of one guest interface, collected
// from either qemu-agent's `network-get-interfaces` or LXC's `/interfaces`
// endpoint and normalised to the same shape.
type InterfaceInfo struct {
	// Name is the in-guest interface name (eth0, enp1s0, wt0, docker0, etc.).
	Name string

	// Mac is the interface's hardware address, lowercase. May be empty if
	// the API returned no hwaddr (rare; some containers' lo-like virtual
	// interfaces).
	Mac string

	// IPs are the candidate addresses on this interface (IPv4 + IPv6, post
	// loopback/link-local filtering but pre allow_cidr / exclude_ip). The
	// default Channel.Claims signature doesn't consume these, but a future
	// channel that wants to claim "any interface with a LAN address" could.
	IPs []net.IP
}

// Channel is the plugin's interface-discovery unit. Each Channel owns one
// way of deciding "this interface on this guest is a service address that
// should appear in DNS." The plugin is strictly allow-list: an interface
// contributes IPs only if at least one Channel claims it; otherwise it
// drops out. This is deliberate — earlier deny-list heuristics (drop
// docker*/br-*/veth/...) kept anything not in a small known-noise set,
// which leaks IPs from unexpected interface names. Allow-list inverts the
// default: if nothing consciously claims it, it doesn't reach DNS.
//
// Guest type (VM vs CT) is a dimension orthogonal to channel. Each Channel
// handles both internally if its source needs to (SR-IOV dump already
// unifies them; the net0 channel dispatches on gid.Type to pick the right
// PVE config endpoint). Splitting a channel into vm-X / ct-X siblings
// would duplicate 80% of the logic for the ~20% that differs in parsing,
// so we keep one channel per *semantic source* instead.
type Channel interface {
	// Name is a short stable identifier used in logs ("sriov", "permissive",
	// "net0", "ssh", ...).
	Name() string

	// OnReconcile is called once per supervisor reconcile tick, right after
	// the cluster enumerate succeeds and before any per-guest polling
	// starts. guests is the current running-guest set; channels that cache
	// per-guest state use it to decide what to refresh and what to evict.
	// Channels that don't need the list (sriov's dump is keyed by vmid
	// regardless; permissive holds no state) simply ignore it. Failure
	// here is non-fatal — the channel keeps its previous state and the
	// supervisor continues with other channels.
	OnReconcile(ctx context.Context, guests []guestID) error

	// Claims returns true if this channel owns iface on the guest identified
	// by gid. Claims must be cheap (called per-interface per-guest per-poll)
	// and must assume OnReconcile ran recently; it shouldn't do I/O.
	Claims(gid guestID, iface InterfaceInfo) bool
}

// claimsAny returns true if any channel in the slice claims this interface
// for this guest. The supervisor uses this as the keep/drop gate.
func claimsAny(chans []Channel, gid guestID, iface InterfaceInfo) bool {
	for _, ch := range chans {
		if ch.Claims(gid, iface) {
			return true
		}
	}
	return false
}
