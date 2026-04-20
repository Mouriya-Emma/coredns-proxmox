package proxmox

import (
	"net"
	"sync"
)

// guestID uniquely identifies a Proxmox guest. Stable across name changes
// (renames = same ID, new FQDN) and across plugin restarts (derived from
// PVE-stable fields only — not from PVE's internal upid/session).
type guestID struct {
	Node string
	Type string // "qemu" or "lxc"
	VMID int
}

// guestRec is one goroutine's owned entry in the store.
type guestRec struct {
	FQDN string   // lowercase, trailing dot, e.g. "app01.hb.lan."
	IPs  []net.IP // filtered through allow_cidr + exclude_ip
}

// store is a concurrent-safe map keyed by guestID. Each per-guest goroutine
// owns exactly one entry — it's the only writer for its guestID. Other
// goroutines read via Lookup for DNS queries.
//
// Lookup does a linear scan over all entries. Homelab-scale — a few dozen
// guests — is fine. If this ever ships to a larger deployment, maintain a
// secondary FQDN→[]guestID index here.
type store struct {
	mu    sync.RWMutex
	byGID map[guestID]guestRec
}

func newStore() *store {
	return &store{byGID: make(map[guestID]guestRec)}
}

func (s *store) Lookup(fqdn string) []net.IP {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []net.IP
	for _, r := range s.byGID {
		if r.FQDN == fqdn {
			out = append(out, r.IPs...)
		}
	}
	return out
}

func (s *store) Upsert(gid guestID, fqdn string, ips []net.IP) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byGID[gid] = guestRec{FQDN: fqdn, IPs: ips}
}

func (s *store) Delete(gid guestID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byGID, gid)
}

func (s *store) Size() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byGID)
}
