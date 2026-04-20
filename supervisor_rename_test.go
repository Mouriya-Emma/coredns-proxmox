package proxmox

import (
	"context"
	"net"
	"testing"

	"github.com/Mouriya-Emma/coredns-proxmox/internal/pveapi"
)

// Tests for rename detection (issue #12). Style matches supervisor_test.go:
// drive reconciles synchronously, inspect supervisor state under its mutex.
// pollGuest goroutines spawned by reconcile loop uneventfully against the
// fake's zero-value interface responses and are torn down by stopAll in the
// deferred cleanup.
//
// Rename proof is store-based: we pre-seed a store record that stands in
// for what the old goroutine would have Upserted under the old FQDN, then
// assert the rename path deleted it. Checking that a *new* CancelFunc was
// installed by comparing pointers doesn't work — context.WithCancel's
// returned closure binds to the same method and can collide at identical
// addresses.

func TestReconcile_RenameDetectedAndRespawned(t *testing.T) {
	fake := &supervisorFake{
		nodes: []pveapi.Node{{Node: "pve"}},
		qemu: map[string][]pveapi.QEMU{
			"pve": {{VMID: 100, Name: "app01", Status: "running"}},
		},
	}
	s := newTestSupervisor(fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.stopAll()

	gid := guestID{Node: "pve", Type: "qemu", VMID: 100}

	s.reconcile(ctx)
	s.mu.Lock()
	if _, ok := s.cancels[gid]; !ok {
		s.mu.Unlock()
		t.Fatalf("guest not tracked after first reconcile")
	}
	if got := s.lastMeta[gid].Name; got != "app01" {
		s.mu.Unlock()
		t.Fatalf("lastMeta name after first reconcile: want %q, got %q", "app01", got)
	}
	s.mu.Unlock()

	// Simulate the old goroutine having Upserted under the old FQDN.
	s.store.Upsert(gid, "app01.hb.lan.", []net.IP{net.ParseIP("192.168.1.50")})

	fake.qemu["pve"][0].Name = "app01-v2"
	s.reconcile(ctx)

	// Store record under the OLD fqdn must be gone — this is what
	// consumers of DNS see through Lookup.
	if ips := s.store.Lookup("app01.hb.lan."); len(ips) != 0 {
		t.Errorf("old FQDN still resolvable after rename: %v", ips)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cancels[gid]; !ok {
		t.Fatalf("guest not tracked after rename reconcile")
	}
	if got := s.lastMeta[gid].Name; got != "app01-v2" {
		t.Errorf("lastMeta name after rename: want %q, got %q", "app01-v2", got)
	}
}

func TestReconcile_RenameToEmptyTreatedAsEvict(t *testing.T) {
	fake := &supervisorFake{
		nodes: []pveapi.Node{{Node: "pve"}},
		qemu: map[string][]pveapi.QEMU{
			"pve": {{VMID: 100, Name: "app01", Status: "running"}},
		},
	}
	s := newTestSupervisor(fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.stopAll()

	gid := guestID{Node: "pve", Type: "qemu", VMID: 100}

	s.reconcile(ctx)
	s.mu.Lock()
	if _, ok := s.cancels[gid]; !ok {
		s.mu.Unlock()
		t.Fatalf("guest not tracked after first reconcile")
	}
	s.mu.Unlock()

	// Rename to empty: no new name to spawn under, so we expect the old
	// goroutine cancelled, lastMeta cleared, and seenEmpty populated by
	// the spawn-loop's empty-name branch.
	fake.qemu["pve"][0].Name = ""
	s.reconcile(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cancels[gid]; ok {
		t.Errorf("guest should not be tracked after rename-to-empty")
	}
	if _, ok := s.lastMeta[gid]; ok {
		t.Errorf("lastMeta should be cleared after rename-to-empty")
	}
	if _, ok := s.seenEmpty[gid]; !ok {
		t.Errorf("seenEmpty should be populated after rename-to-empty (spawn loop warning)")
	}
}

func TestReconcile_DoubleRenameInTwoTicks(t *testing.T) {
	fake := &supervisorFake{
		nodes: []pveapi.Node{{Node: "pve"}},
		qemu: map[string][]pveapi.QEMU{
			"pve": {{VMID: 100, Name: "app01", Status: "running"}},
		},
	}
	s := newTestSupervisor(fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.stopAll()

	gid := guestID{Node: "pve", Type: "qemu", VMID: 100}

	s.reconcile(ctx)
	fake.qemu["pve"][0].Name = "app01-v2"
	s.reconcile(ctx)
	fake.qemu["pve"][0].Name = "app01-v3"
	s.reconcile(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cancels[gid]; !ok {
		t.Errorf("guest should still be tracked after two renames")
	}
	if got := s.lastMeta[gid].Name; got != "app01-v3" {
		t.Errorf("lastMeta name after double rename: want %q, got %q", "app01-v3", got)
	}
}

func TestReconcile_RenameAndBack(t *testing.T) {
	fake := &supervisorFake{
		nodes: []pveapi.Node{{Node: "pve"}},
		qemu: map[string][]pveapi.QEMU{
			"pve": {{VMID: 100, Name: "app01", Status: "running"}},
		},
	}
	s := newTestSupervisor(fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.stopAll()

	gid := guestID{Node: "pve", Type: "qemu", VMID: 100}

	s.reconcile(ctx)
	fake.qemu["pve"][0].Name = "app01-v2"
	s.reconcile(ctx)
	fake.qemu["pve"][0].Name = "app01"
	s.reconcile(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	if got := s.lastMeta[gid].Name; got != "app01" {
		t.Errorf("lastMeta name after rename-and-back: want %q, got %q", "app01", got)
	}
}
