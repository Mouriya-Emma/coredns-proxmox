package proxmox

import (
	"context"
	"strings"
)

// permissiveChannel claims every interface whose name isn't in a small
// deny-list of known-noise prefixes. It's the v0.1.4-and-earlier default
// behaviour kept around as an explicit opt-in — useful when no stronger
// channel (sriov, net0, ssh, …) can positively identify the right
// interface but the operator still wants records.
//
// The plugin defaults to NOT enabling this channel. Operators turn it on
// with `permissive` in the Corefile and accept the risk of an unexpected
// interface name accidentally contributing IPs.
type permissiveChannel struct {
	// dropPrefixes: an interface whose name starts with any of these is
	// dropped. Defaults cover docker, LXC/KVM internal bridges, CNI, veth.
	dropPrefixes []string

	// dropNames: exact-match drops. Defaults: lo, wt0 (netbird).
	dropNames []string
}

// defaultPermissiveDropPrefixes / defaultPermissiveDropNames are the
// historical hard-coded deny-list from v0.1.4's keepInterface. An operator
// who flips the permissive channel on gets this behaviour verbatim unless
// they override the lists.
var (
	defaultPermissiveDropPrefixes = []string{
		"docker",
		"br-",
		"veth",
		"cni-",
	}
	defaultPermissiveDropNames = []string{
		"lo",
		"wt0",
	}
)

func newPermissiveChannel(dropPrefixes, dropNames []string) *permissiveChannel {
	if len(dropPrefixes) == 0 {
		dropPrefixes = defaultPermissiveDropPrefixes
	}
	if len(dropNames) == 0 {
		dropNames = defaultPermissiveDropNames
	}
	return &permissiveChannel{
		dropPrefixes: dropPrefixes,
		dropNames:    dropNames,
	}
}

func (c *permissiveChannel) Name() string                         { return "permissive" }
func (c *permissiveChannel) OnReconcile(_ context.Context) error  { return nil }

func (c *permissiveChannel) Claims(_ guestID, iface InterfaceInfo) bool {
	name := iface.Name
	for _, n := range c.dropNames {
		if name == n {
			return false
		}
	}
	for _, p := range c.dropPrefixes {
		if strings.HasPrefix(name, p) {
			return false
		}
	}
	return true
}
