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

func TestKeepInterface_MACMatchAuthoritativelyKeeps(t *testing.T) {
	// SR-IOV VF MAC match always keeps, even if the name would otherwise be
	// filtered (defensive: unusual ifname shouldn't drop a confirmed VF).
	expected := []string{"44:cc:77:00:00:05"}
	if !keepInterface("enp1s0", "44:cc:77:00:00:05", expected) {
		t.Error("MAC-matched real NIC must be kept")
	}
	if !keepInterface("docker0", "44:CC:77:00:00:05", expected) {
		t.Error("MAC match (case-insensitive) overrides docker0 name drop")
	}
	if !keepInterface("br-weird-vf", "44:cc:77:00:00:05", expected) {
		t.Error("MAC match overrides br- name drop")
	}
}

func TestKeepInterface_AdditiveWithOtherLegitimateInterfaces(t *testing.T) {
	// The key fix (homelab-tf feedback): a guest with SR-IOV + net0 on vmbr
	// should have *both* interfaces contribute IPs. Earlier exclusive-MAC
	// gating silently dropped net0.
	expected := []string{"44:cc:77:00:00:05"}
	// SR-IOV VF — kept by MAC match
	if !keepInterface("enp1s0", "44:cc:77:00:00:05", expected) {
		t.Error("SR-IOV MAC must be kept")
	}
	// net0 on vmbr with a different MAC — kept by name heuristic, not dropped
	if !keepInterface("enp6s18", "bc:24:11:aa:bb:cc", expected) {
		t.Error("non-SR-IOV real NIC must be kept additively")
	}
	// A second regular eth — kept
	if !keepInterface("eth1", "02:00:00:00:00:01", expected) {
		t.Error("generic eth must be kept additively")
	}
	// Known-noise still dropped even when expectedMacs is set
	if keepInterface("docker0", "02:42:ac:11:00:02", expected) {
		t.Error("docker with unrelated MAC must still be dropped by name")
	}
	if keepInterface("wt0", "00:00:00:00:00:00", expected) {
		t.Error("wt0 must always be dropped")
	}
}

func TestKeepInterface_NameHeuristicWhenNoMACs(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"enp1s0", true},
		{"eth0", true},
		{"net0", true},
		{"lo", false},
		{"docker0", false},
		{"br-abc123", false},
		{"veth123", false},
		{"cni-podman0", false},
		{"wt0", false},
	}
	for _, c := range cases {
		got := keepInterface(c.name, "any:mac:here:00:00:00", nil)
		if got != c.want {
			t.Errorf("%s: want %v, got %v", c.name, c.want, got)
		}
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
