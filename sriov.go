package proxmox

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// sriovState reads an on-disk JSON file produced by `sriov dump` (run on
// the PVE host, shipped into the container via a bind mount) and indexes
// vmid → list of hardware MACs that belong to that guest. The plugin uses
// this to filter qemu-agent / lxc interface responses: if the guest has
// known SR-IOV VFs, we keep only IPs from interfaces whose MAC matches.
//
// This is more precise than allow_cidr: CIDRs can match unrelated bridge
// interfaces (podman, docker, netbird) that happen to sit in the same /24;
// MAC matching uses the manufacturer-fixed hardware identity assigned by
// the PF driver at boot, so we're looking at the exact right NIC.
//
// Missing / malformed file is logged once and then soft-fails — downstream
// code treats that as "no SR-IOV knowledge for any vmid" and falls back to
// the allow_cidr + exclude_ip path. This keeps the plugin usable on CTs /
// VMs that don't use SR-IOV at all.
type sriovState struct {
	path string

	mu      sync.RWMutex
	mtime   time.Time
	vmMacs  map[int][]string // vmid → lowercase MACs
	lastErr error            // last non-nil load error, for dedupe logging
}

func newSriovState(path string) *sriovState {
	return &sriovState{path: path, vmMacs: map[int][]string{}}
}

// refresh re-reads the dump file if its mtime has advanced. Safe to call
// from multiple goroutines; holds a write-lock only across the parse.
func (s *sriovState) refresh() {
	if s.path == "" {
		return
	}
	fi, err := os.Stat(s.path)
	if err != nil {
		s.recordErr(fmt.Errorf("stat %s: %w", s.path, err))
		return
	}
	s.mu.RLock()
	unchanged := fi.ModTime().Equal(s.mtime) && len(s.vmMacs) > 0
	s.mu.RUnlock()
	if unchanged {
		return
	}

	raw, err := os.ReadFile(s.path)
	if err != nil {
		s.recordErr(fmt.Errorf("read %s: %w", s.path, err))
		return
	}
	m, err := parseSriovDump(raw)
	if err != nil {
		s.recordErr(fmt.Errorf("parse %s: %w", s.path, err))
		return
	}

	s.mu.Lock()
	s.vmMacs = m
	s.mtime = fi.ModTime()
	s.lastErr = nil
	s.mu.Unlock()
	log.Infof("sriov state refreshed: %d guests with SR-IOV VFs (from %s)", len(m), s.path)
}

func (s *sriovState) recordErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Log at WARN only when the error changes — on a missing-file steady
	// state we don't want to spam every refresh.
	if s.lastErr == nil || s.lastErr.Error() != err.Error() {
		log.Warningf("sriov state: %v", err)
	}
	s.lastErr = err
}

// lookup returns the set of expected MACs for a vmid, or (nil, false) if
// this vmid doesn't appear in the state (guest not using SR-IOV, or state
// file not loaded).
func (s *sriovState) lookup(vmid int) ([]string, bool) {
	if s == nil || s.path == "" {
		return nil, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	macs, ok := s.vmMacs[vmid]
	if !ok || len(macs) == 0 {
		return nil, false
	}
	return macs, true
}

// sriovDump mirrors the subset of `sriov dump` JSON we consume. Extra fields
// in the input are ignored — keeps us forward-compatible with CLI additions.
type sriovDump struct {
	CorrelatedVfs []struct {
		VF struct {
			Kind     string `json:"kind"`
			AdminMac string `json:"adminMac"`
		} `json:"vf"`
		Consumers []struct {
			Kind        string `json:"kind"`
			VMID        int    `json:"vmid"`
			VMIDs       []int  `json:"vmids"`
			MappingName string `json:"mappingName"`
		} `json:"consumers"`
	} `json:"correlatedVfs"`
}

// parseSriovDump flattens the JSON into {vmid: [mac…]}. Handles the three
// consumer kinds that reference a guest: vm-direct, vm-via-mapping (can
// have multiple vmids per VF), container-phys. Silently skips unused / host
// consumer kinds and non-network VF entries (GPUs have no adminMac).
func parseSriovDump(raw []byte) (map[int][]string, error) {
	var d sriovDump
	if err := json.Unmarshal(raw, &d); err != nil {
		return nil, err
	}
	out := make(map[int][]string)
	add := func(vmid int, mac string) {
		mac = strings.ToLower(strings.TrimSpace(mac))
		if mac == "" {
			return
		}
		for _, existing := range out[vmid] {
			if existing == mac {
				return // dedupe
			}
		}
		out[vmid] = append(out[vmid], mac)
	}
	for _, cv := range d.CorrelatedVfs {
		if cv.VF.Kind != "network-vf" || cv.VF.AdminMac == "" {
			continue
		}
		mac := cv.VF.AdminMac
		for _, c := range cv.Consumers {
			switch c.Kind {
			case "vm-direct", "container-phys":
				if c.VMID > 0 {
					add(c.VMID, mac)
				}
			case "vm-via-mapping":
				for _, vmid := range c.VMIDs {
					add(vmid, mac)
				}
			}
		}
	}
	return out, nil
}

// macEquals compares two MAC strings case-insensitively and tolerates
// differences in separator (qemu-agent sometimes returns with different
// casing).
func macEquals(a, b string) bool {
	return strings.EqualFold(strings.TrimSpace(a), strings.TrimSpace(b))
}

// macInSet returns true if any mac in set matches the given mac.
func macInSet(mac string, set []string) bool {
	for _, s := range set {
		if macEquals(mac, s) {
			return true
		}
	}
	return false
}
