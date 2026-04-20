package proxmox

import (
	"context"
)

// sriovChannel claims interfaces whose MAC matches one of the guest's
// SR-IOV VF adminMacs, as reported by the on-disk `sriov dump` file the
// PVE host writes. Identity is exact: an adminMac pins a specific VF that
// was assigned to the guest by the PF driver at boot, and no two guests
// share a VF.
//
// nil state (sriov_state unset in the Corefile) means this channel never
// claims — safe to add to the channel list unconditionally.
type sriovChannel struct {
	state *sriovState
}

func newSriovChannel(state *sriovState) *sriovChannel {
	return &sriovChannel{state: state}
}

func (c *sriovChannel) Name() string { return "sriov" }

func (c *sriovChannel) OnReconcile(_ context.Context, _ []guestID) error {
	if c.state == nil {
		return nil
	}
	c.state.refresh()
	return nil
}

func (c *sriovChannel) Claims(gid guestID, iface InterfaceInfo) bool {
	if c.state == nil {
		return false
	}
	macs, ok := c.state.lookup(gid.VMID)
	if !ok {
		return false
	}
	return macInSet(iface.Mac, macs)
}
