package proxmox

import (
	"context"
	"errors"
	"testing"

	"github.com/Mouriya-Emma/coredns-proxmox/internal/pveapi"
)

func TestParseNet0_VMVirtio(t *testing.T) {
	mac, ok := parseNet0("virtio=BC:24:11:AA:BB:CC,bridge=vmbr0,firewall=1")
	if !ok {
		t.Fatal("expected parseable MAC")
	}
	if mac != "bc:24:11:aa:bb:cc" {
		t.Errorf("want bc:24:11:aa:bb:cc (lowercased), got %q", mac)
	}
}

func TestParseNet0_VMOtherModels(t *testing.T) {
	// Works for every NIC model because we regex on the value, not on
	// the key — this guards against a future PVE release adding a new
	// model key.
	cases := []string{
		"e1000=52:54:00:12:34:56,bridge=vmbr0",
		"rtl8139=02:00:00:00:00:01,bridge=vmbr1,tag=10",
		"vmxnet3=aa:bb:cc:dd:ee:ff,bridge=vmbr0",
		"virtio-net=11:22:33:44:55:66,bridge=vmbr0",
		"hypotheticalnic=de:ad:be:ef:00:01,bridge=vmbr0",
	}
	for _, in := range cases {
		mac, ok := parseNet0(in)
		if !ok || mac == "" {
			t.Errorf("parseNet0(%q) = (%q, %v); want some MAC", in, mac, ok)
		}
	}
}

func TestParseNet0_CTHwaddr(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		// hwaddr in middle position
		{"name=eth0,bridge=vmbr0,hwaddr=BC:24:11:37:D8:99,ip=dhcp,type=veth", "bc:24:11:37:d8:99"},
		// hwaddr at end, with a firewall=1 flag
		{"name=eth0,bridge=vmbr0,ip=192.168.1.10/24,gw=192.168.1.1,type=veth,hwaddr=02:00:00:00:00:77,firewall=1", "02:00:00:00:00:77"},
		// hwaddr first
		{"hwaddr=aa:bb:cc:dd:ee:ff,name=eth0,bridge=vmbr0,type=veth", "aa:bb:cc:dd:ee:ff"},
	}
	for _, c := range cases {
		mac, ok := parseNet0(c.in)
		if !ok || mac != c.want {
			t.Errorf("parseNet0(%q) = (%q, %v); want (%q, true)", c.in, mac, ok, c.want)
		}
	}
}

func TestParseNet0_Empty(t *testing.T) {
	if _, ok := parseNet0(""); ok {
		t.Error("empty input must not yield a MAC")
	}
	if _, ok := parseNet0("   "); ok {
		t.Error("whitespace-only input must not yield a MAC")
	}
}

func TestParseNet0_Malformed(t *testing.T) {
	cases := []string{
		"bridge=vmbr0,firewall=1",              // no MAC-shaped value anywhere
		"bridge=vmbr0,tag=10",                  // pure properties
		"virtio=not-a-mac,bridge=vmbr0",        // model= but value isn't MAC-shaped
		"name=eth0,bridge=vmbr0,hwaddr=BC:24",  // truncated MAC
		"name=eth0,bridge=vmbr0,hwaddr=zz:zz:zz:zz:zz:zz", // non-hex
	}
	for _, in := range cases {
		if mac, ok := parseNet0(in); ok {
			t.Errorf("parseNet0(%q) = (%q, true); want !ok", in, mac)
		}
	}
}

func TestParseNet0_MacAtAnyPosition(t *testing.T) {
	// First MAC-shaped value wins. Covers both first-position (VM form)
	// and middle/last position (CT form with hwaddr).
	got, ok := parseNet0("name=eth0,hwaddr=aa:bb:cc:11:22:33,bridge=vmbr0,hwaddr=ff:ff:ff:ff:ff:ff")
	if !ok || got != "aa:bb:cc:11:22:33" {
		t.Errorf("want first MAC, got (%q, %v)", got, ok)
	}
}

// fakePveClient is a minimal pveapi.Client stub. Tests set the fields
// they need; other methods panic to surface accidental use.
type fakePveClient struct {
	qemuConfigs map[int]pveapi.QEMUConfig
	lxcConfigs  map[int]pveapi.LXCConfig

	// errOnVMID, if non-nil for a vmid, causes the corresponding
	// GetXxxConfig call to return that error.
	errOnVMID map[int]error
}

func (f *fakePveClient) GetNodes(_ context.Context) ([]pveapi.Node, error) {
	panic("GetNodes not expected")
}
func (f *fakePveClient) GetQEMUVMs(_ context.Context, _ string) ([]pveapi.QEMU, error) {
	panic("GetQEMUVMs not expected")
}
func (f *fakePveClient) GetLXCs(_ context.Context, _ string) ([]pveapi.LXC, error) {
	panic("GetLXCs not expected")
}
func (f *fakePveClient) GetQEMUConfig(_ context.Context, _ string, vmID int) (pveapi.QEMUConfig, error) {
	if err, ok := f.errOnVMID[vmID]; ok {
		return pveapi.QEMUConfig{}, err
	}
	return f.qemuConfigs[vmID], nil
}
func (f *fakePveClient) GetQEMUInterfaces(_ context.Context, _ string, _ int) (pveapi.AgentInterfacesResponse, error) {
	panic("GetQEMUInterfaces not expected")
}
func (f *fakePveClient) GetLXCConfig(_ context.Context, _ string, vmID int) (pveapi.LXCConfig, error) {
	if err, ok := f.errOnVMID[vmID]; ok {
		return pveapi.LXCConfig{}, err
	}
	return f.lxcConfigs[vmID], nil
}
func (f *fakePveClient) GetLXCInterfaces(_ context.Context, _ string, _ int) ([]pveapi.LXCInterface, error) {
	panic("GetLXCInterfaces not expected")
}

func TestNet0Channel_ClaimsAfterRefresh(t *testing.T) {
	fake := &fakePveClient{
		qemuConfigs: map[int]pveapi.QEMUConfig{
			102: {Net0: "virtio=BC:24:11:AA:BB:CC,bridge=vmbr0,firewall=1"},
		},
		lxcConfigs: map[int]pveapi.LXCConfig{
			105: {Net0: "name=eth0,bridge=vmbr0,hwaddr=02:00:00:11:22:33,ip=dhcp,type=veth"},
		},
	}
	ch := newNet0Channel(fake)
	vm := guestID{Node: "pve", Type: "qemu", VMID: 102}
	ct := guestID{Node: "pve", Type: "lxc", VMID: 105}
	if err := ch.OnReconcile(context.Background(), []guestID{vm, ct}); err != nil {
		t.Fatalf("OnReconcile: %v", err)
	}

	if !ch.Claims(vm, InterfaceInfo{Name: "enp1s0", Mac: "bc:24:11:aa:bb:cc"}) {
		t.Error("VM: exact MAC match must be claimed")
	}
	if !ch.Claims(vm, InterfaceInfo{Name: "enp1s0", Mac: "BC:24:11:AA:BB:CC"}) {
		t.Error("VM: case-insensitive MAC match must be claimed")
	}
	if ch.Claims(vm, InterfaceInfo{Name: "enp1s0", Mac: "02:00:00:00:00:01"}) {
		t.Error("VM: unrelated MAC must not be claimed")
	}
	if !ch.Claims(ct, InterfaceInfo{Name: "eth0", Mac: "02:00:00:11:22:33"}) {
		t.Error("CT: hwaddr match must be claimed")
	}
	if ch.Claims(ct, InterfaceInfo{Name: "docker0", Mac: "02:42:ac:11:00:02"}) {
		t.Error("CT: docker bridge MAC must not be claimed")
	}
}

func TestNet0Channel_UnknownGuestNotClaimed(t *testing.T) {
	ch := newNet0Channel(&fakePveClient{})
	unknown := guestID{Node: "pve", Type: "qemu", VMID: 999}
	if ch.Claims(unknown, InterfaceInfo{Mac: "bc:24:11:aa:bb:cc"}) {
		t.Error("claim against unseen guest must return false")
	}
}

func TestNet0Channel_EvictsGoneGuests(t *testing.T) {
	fake := &fakePveClient{
		qemuConfigs: map[int]pveapi.QEMUConfig{
			102: {Net0: "virtio=bc:24:11:aa:bb:cc,bridge=vmbr0"},
			103: {Net0: "virtio=02:00:00:00:00:03,bridge=vmbr0"},
		},
	}
	ch := newNet0Channel(fake)
	g102 := guestID{Node: "pve", Type: "qemu", VMID: 102}
	g103 := guestID{Node: "pve", Type: "qemu", VMID: 103}

	// First reconcile with both guests — both cached.
	_ = ch.OnReconcile(context.Background(), []guestID{g102, g103})
	if !ch.Claims(g102, InterfaceInfo{Mac: "bc:24:11:aa:bb:cc"}) {
		t.Fatal("g102 should be claimable after first reconcile")
	}
	if !ch.Claims(g103, InterfaceInfo{Mac: "02:00:00:00:00:03"}) {
		t.Fatal("g103 should be claimable after first reconcile")
	}

	// Second reconcile without g103 — g103's entry must be evicted,
	// g102's entry stays.
	_ = ch.OnReconcile(context.Background(), []guestID{g102})
	if !ch.Claims(g102, InterfaceInfo{Mac: "bc:24:11:aa:bb:cc"}) {
		t.Error("g102 must still be claimable after g103 eviction")
	}
	if ch.Claims(g103, InterfaceInfo{Mac: "02:00:00:00:00:03"}) {
		t.Error("g103 must be evicted after being dropped from the guest list")
	}
}

func TestNet0Channel_TransientFetchErrorKeepsCache(t *testing.T) {
	// A briefly-failing PVE API shouldn't cause flicker: existing cached
	// MAC stays until the next successful refresh replaces it or eviction
	// removes the guest entirely.
	fake := &fakePveClient{
		qemuConfigs: map[int]pveapi.QEMUConfig{
			102: {Net0: "virtio=bc:24:11:aa:bb:cc,bridge=vmbr0"},
		},
	}
	ch := newNet0Channel(fake)
	g := guestID{Node: "pve", Type: "qemu", VMID: 102}
	_ = ch.OnReconcile(context.Background(), []guestID{g})
	if !ch.Claims(g, InterfaceInfo{Mac: "bc:24:11:aa:bb:cc"}) {
		t.Fatal("baseline: claimable after first reconcile")
	}

	// Now make the next fetch fail; guest still in list (PVE hiccup, not
	// a delete).
	fake.errOnVMID = map[int]error{102: errors.New("PVE unreachable")}
	_ = ch.OnReconcile(context.Background(), []guestID{g})
	if !ch.Claims(g, InterfaceInfo{Mac: "bc:24:11:aa:bb:cc"}) {
		t.Error("transient fetch error must NOT blank the cached MAC")
	}
}

func TestNet0Channel_EmptyNet0ClearsEntry(t *testing.T) {
	// A successful fetch that returns empty net0 (operator removed the
	// NIC config) should clear the cache — otherwise we'd keep claiming
	// an interface that no longer exists in the guest's declared config.
	fake := &fakePveClient{
		qemuConfigs: map[int]pveapi.QEMUConfig{
			102: {Net0: "virtio=bc:24:11:aa:bb:cc,bridge=vmbr0"},
		},
	}
	ch := newNet0Channel(fake)
	g := guestID{Node: "pve", Type: "qemu", VMID: 102}
	_ = ch.OnReconcile(context.Background(), []guestID{g})
	if !ch.Claims(g, InterfaceInfo{Mac: "bc:24:11:aa:bb:cc"}) {
		t.Fatal("baseline: claimable after first reconcile")
	}
	fake.qemuConfigs[102] = pveapi.QEMUConfig{Net0: ""}
	_ = ch.OnReconcile(context.Background(), []guestID{g})
	if ch.Claims(g, InterfaceInfo{Mac: "bc:24:11:aa:bb:cc"}) {
		t.Error("net0 cleared on PVE side must clear the channel's cache")
	}
}
