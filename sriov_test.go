package proxmox

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"
)

const sriovDumpSample = `{
  "timestamp": "2026-04-20T00:00:00Z",
  "correlatedVfs": [
    {
      "vf": {"kind": "network-vf", "adminMac": "44:cc:77:00:00:04", "vfIndex": 4, "address": "0000:01:11.0"},
      "consumers": [{"kind": "vm-via-mapping", "mappingName": "VF04", "vmids": [101]}]
    },
    {
      "vf": {"kind": "network-vf", "adminMac": "44:CC:77:00:00:05", "vfIndex": 5},
      "consumers": [{"kind": "vm-via-mapping", "mappingName": "VF05", "vmids": [102]}]
    },
    {
      "vf": {"kind": "network-vf", "adminMac": "44:cc:77:00:00:06", "vfIndex": 6},
      "consumers": [{"kind": "vm-direct", "vmid": 103, "hostpciSlot": 0}]
    },
    {
      "vf": {"kind": "network-vf", "adminMac": "44:cc:77:00:00:09", "vfIndex": 9},
      "consumers": [{"kind": "vm-direct", "vmid": 104, "hostpciSlot": 0}]
    },
    {
      "vf": {"kind": "network-vf", "adminMac": "44:cc:77:00:00:0f", "vfIndex": 15},
      "consumers": [{"kind": "container-phys", "vmid": 380, "hostInterface": "eth0vf15"}]
    },
    {
      "vf": {"kind": "network-vf", "adminMac": "00:fe:ef:aa:00:10", "vfIndex": 0},
      "consumers": [{"kind": "host-network", "interfaceName": "eth0pve0"}]
    },
    {
      "vf": {"kind": "network-vf", "adminMac": "", "vfIndex": 99},
      "consumers": [{"kind": "unused"}]
    },
    {
      "vf": {"kind": "gpu-vf", "vfIndex": 0},
      "consumers": [{"kind": "vm-direct", "vmid": 100, "hostpciSlot": 1}]
    }
  ]
}`

func TestParseSriovDump_BuildsVmidMacMap(t *testing.T) {
	got, err := parseSriovDump([]byte(sriovDumpSample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := map[int][]string{
		101: {"44:cc:77:00:00:04"},
		102: {"44:cc:77:00:00:05"}, // adminMac was upper-case; lowercased
		103: {"44:cc:77:00:00:06"},
		104: {"44:cc:77:00:00:09"},
		380: {"44:cc:77:00:00:0f"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("want %v, got %v", want, got)
	}
}

func TestParseSriovDump_SkipsIrrelevant(t *testing.T) {
	got, _ := parseSriovDump([]byte(sriovDumpSample))
	// host-network consumer (eth0pve0) should not appear
	for vmid, macs := range got {
		for _, m := range macs {
			if m == "00:fe:ef:aa:00:10" {
				t.Errorf("host-network MAC leaked into vmid %d: %v", vmid, macs)
			}
		}
	}
	// gpu-vf (no adminMac) for VM 100 should not appear
	if _, ok := got[100]; ok {
		t.Errorf("GPU VF should not produce a MAC entry for VM 100, got %v", got[100])
	}
}

func TestParseSriovDump_Dedup(t *testing.T) {
	// Same MAC referenced by two consumer entries for same vmid (hypothetical)
	dupe := `{"correlatedVfs":[
	  {"vf":{"kind":"network-vf","adminMac":"aa:bb:cc:dd:ee:ff"},"consumers":[{"kind":"vm-direct","vmid":7}]},
	  {"vf":{"kind":"network-vf","adminMac":"AA:BB:CC:DD:EE:FF"},"consumers":[{"kind":"vm-direct","vmid":7}]}
	]}`
	got, _ := parseSriovDump([]byte(dupe))
	if len(got[7]) != 1 {
		t.Errorf("want dedup to 1 MAC, got %v", got[7])
	}
}

func TestSriovState_RefreshOnMtimeChange(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "dump.json")
	if err := os.WriteFile(p, []byte(sriovDumpSample), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newSriovState(p)
	s.refresh()
	macs, ok := s.lookup(102)
	if !ok || len(macs) != 1 || macs[0] != "44:cc:77:00:00:05" {
		t.Fatalf("first refresh: want [44:cc:77:00:00:05], got %v ok=%v", macs, ok)
	}
	// Advance mtime + change content
	time.Sleep(10 * time.Millisecond) // ensure mtime resolution advances
	newDump := `{"correlatedVfs":[{"vf":{"kind":"network-vf","adminMac":"de:ad:be:ef:00:05"},"consumers":[{"kind":"vm-via-mapping","mappingName":"VF05","vmids":[102]}]}]}`
	os.WriteFile(p, []byte(newDump), 0o644)
	if err := os.Chtimes(p, time.Now(), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	s.refresh()
	macs, _ = s.lookup(102)
	if len(macs) != 1 || macs[0] != "de:ad:be:ef:00:05" {
		t.Errorf("after rewrite: want [de:ad:be:ef:00:05], got %v", macs)
	}
}

func TestSriovState_NilReceiverLookup(t *testing.T) {
	var s *sriovState
	if _, ok := s.lookup(1); ok {
		t.Error("nil receiver must return !ok")
	}
}

func TestSriovState_MissingFile_NoPanic(t *testing.T) {
	s := newSriovState("/does/not/exist/dump.json")
	s.refresh() // logs warning, does not panic
	if _, ok := s.lookup(102); ok {
		t.Error("missing file must yield no lookups")
	}
}

// Channel tests. These exercise each channel's Claims directly — the
// supervisor just ORs the results across channels, so per-channel
// correctness + the integration smoke in the real deployment together
// cover the interesting behaviour.

func TestSriovChannel_ClaimsByMAC(t *testing.T) {
	s := newStore()
	_ = s
	st := newSriovState("")
	st.vmMacs[102] = []string{"44:cc:77:00:00:05"}
	st.vmMacs[380] = []string{"44:cc:77:00:00:0f"}
	ch := newSriovChannel(st)

	vm102 := guestID{Node: "pve", Type: "qemu", VMID: 102}
	ct380 := guestID{Node: "pve", Type: "lxc", VMID: 380}
	unknown := guestID{Node: "pve", Type: "qemu", VMID: 999}

	if !ch.Claims(vm102, InterfaceInfo{Name: "enp1s0", Mac: "44:cc:77:00:00:05"}) {
		t.Error("vm102: MAC match must be claimed")
	}
	if !ch.Claims(vm102, InterfaceInfo{Name: "docker0", Mac: "44:CC:77:00:00:05"}) {
		t.Error("vm102: MAC match (case-insensitive) overrides even docker0 name")
	}
	if ch.Claims(vm102, InterfaceInfo{Name: "enp1s0", Mac: "02:00:00:00:00:01"}) {
		t.Error("vm102: non-matching MAC must NOT be claimed by sriov")
	}
	if !ch.Claims(ct380, InterfaceInfo{Name: "eth0vf15", Mac: "44:cc:77:00:00:0f"}) {
		t.Error("ct380: container-phys VF MAC must be claimed")
	}
	if ch.Claims(unknown, InterfaceInfo{Name: "any", Mac: "44:cc:77:00:00:05"}) {
		t.Error("unknown vmid: sriov channel has nothing to claim on")
	}
}

func TestSriovChannel_NilStateNeverClaims(t *testing.T) {
	ch := newSriovChannel(nil)
	if ch.Claims(guestID{VMID: 1}, InterfaceInfo{Mac: "any"}) {
		t.Error("nil state must claim nothing")
	}
}

func TestPermissiveChannel_DropsKnownNoise(t *testing.T) {
	ch := newPermissiveChannel(nil, nil) // defaults
	cases := []struct {
		name string
		want bool
	}{
		{"enp1s0", true},
		{"eth0", true},
		{"net0", true},
		{"enp6s18", true},
		{"lo", false},
		{"docker0", false},
		{"br-abc123", false},
		{"veth12", false},
		{"cni-podman0", false},
		{"wt0", false},
	}
	for _, c := range cases {
		got := ch.Claims(guestID{}, InterfaceInfo{Name: c.name, Mac: "any"})
		if got != c.want {
			t.Errorf("permissive Claims(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestChannels_AllowListCombinesAdditively(t *testing.T) {
	// The original motivation: a guest with an SR-IOV VF and a vmbr net0
	// should have both surfaced. Neither channel alone covers both; the
	// supervisor's claimsAny ORs them together.
	st := newSriovState("")
	st.vmMacs[102] = []string{"44:cc:77:00:00:05"}
	chans := []Channel{
		newSriovChannel(st),
		newPermissiveChannel(nil, nil),
	}
	vm := guestID{VMID: 102}

	// SR-IOV VF — sriov channel claims
	if !claimsAny(chans, vm, InterfaceInfo{Name: "enp1s0", Mac: "44:cc:77:00:00:05"}) {
		t.Error("SR-IOV VF must be claimed")
	}
	// vmbr net0 with a totally different MAC — permissive channel claims by name
	if !claimsAny(chans, vm, InterfaceInfo{Name: "enp6s18", Mac: "bc:24:11:aa:bb:cc"}) {
		t.Error("non-SR-IOV vmbr interface must be claimed additively by permissive")
	}
	// docker0 — neither channel claims; stays dropped
	if claimsAny(chans, vm, InterfaceInfo{Name: "docker0", Mac: "02:42:ac:11:00:02"}) {
		t.Error("docker0 must not be claimed by any channel")
	}
	// docker0 with an SR-IOV-matching MAC — sriov claims (authoritative
	// override of name-based drop; defensive against weird ifnames)
	if !claimsAny(chans, vm, InterfaceInfo{Name: "docker0", Mac: "44:cc:77:00:00:05"}) {
		t.Error("MAC-matched VF named docker0 must still be claimed (authoritative)")
	}
}

func TestChannels_StrictAllowListNoPermissive(t *testing.T) {
	// With only sriov channel (permissive off — the new default), any
	// interface not in the MAC set is dropped — even a sensible-looking
	// enp1s0 with a non-SR-IOV MAC.
	st := newSriovState("")
	st.vmMacs[102] = []string{"44:cc:77:00:00:05"}
	chans := []Channel{newSriovChannel(st)}
	vm := guestID{VMID: 102}

	if !claimsAny(chans, vm, InterfaceInfo{Name: "enp1s0", Mac: "44:cc:77:00:00:05"}) {
		t.Error("SR-IOV VF still claimed")
	}
	if claimsAny(chans, vm, InterfaceInfo{Name: "enp6s18", Mac: "bc:24:11:aa:bb:cc"}) {
		t.Error("non-SR-IOV interface must be dropped when only sriov channel is enabled")
	}
}

func TestMacInSet(t *testing.T) {
	set := []string{"44:cc:77:00:00:04", "44:CC:77:00:00:05"}
	if !macInSet("44:cc:77:00:00:04", set) {
		t.Error("exact case match")
	}
	if !macInSet("44:CC:77:00:00:05", set) {
		t.Error("same-case match")
	}
	if !macInSet("44:cc:77:00:00:05", set) {
		t.Error("case-insensitive match")
	}
	if macInSet("00:00:00:00:00:00", set) {
		t.Error("unrelated MAC must not match")
	}
}

// ensure test order stable for test ids
func TestParseSriovDump_SortedOutputForDebug(t *testing.T) {
	got, _ := parseSriovDump([]byte(sriovDumpSample))
	var ids []int
	for id := range got {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	want := []int{101, 102, 103, 104, 380}
	if !reflect.DeepEqual(ids, want) {
		t.Errorf("want vmids %v, got %v", want, ids)
	}
}
