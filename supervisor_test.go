package proxmox

import (
	"context"
	"testing"
	"time"

	"github.com/Mouriya-Emma/coredns-proxmox/internal/pveapi"
)

// supervisorFake is a minimal pveapi.Client for reconcile-flow tests.
// Enumerate methods return the configured state; per-guest methods return
// empty/zero so spawned pollGuest goroutines don't panic — they just loop
// uneventfully until the test's stopAll cancels them.
type supervisorFake struct {
	nodes []pveapi.Node
	qemu  map[string][]pveapi.QEMU
	lxc   map[string][]pveapi.LXC
}

func (f *supervisorFake) GetNodes(context.Context) ([]pveapi.Node, error) { return f.nodes, nil }
func (f *supervisorFake) GetQEMUVMs(_ context.Context, node string) ([]pveapi.QEMU, error) {
	return f.qemu[node], nil
}
func (f *supervisorFake) GetLXCs(_ context.Context, node string) ([]pveapi.LXC, error) {
	return f.lxc[node], nil
}
func (f *supervisorFake) GetQEMUConfig(context.Context, string, int) (pveapi.QEMUConfig, error) {
	return pveapi.QEMUConfig{}, nil
}
func (f *supervisorFake) GetQEMUInterfaces(context.Context, string, int) (pveapi.AgentInterfacesResponse, error) {
	return pveapi.AgentInterfacesResponse{}, nil
}
func (f *supervisorFake) GetLXCConfig(context.Context, string, int) (pveapi.LXCConfig, error) {
	return pveapi.LXCConfig{}, nil
}
func (f *supervisorFake) GetLXCInterfaces(context.Context, string, int) ([]pveapi.LXCInterface, error) {
	return nil, nil
}

func newTestSupervisor(client pveapi.Client) *supervisor {
	return newSupervisor(client, newStore(), []string{"hb.lan."},
		nil, nil, []Channel{newPermissiveChannel(nil, nil)},
		60*time.Second, 60*time.Second, 5*time.Minute)
}

func TestReconcile_SkipsEmptyNameGuest(t *testing.T) {
	fake := &supervisorFake{
		nodes: []pveapi.Node{{Node: "pve"}},
		qemu: map[string][]pveapi.QEMU{
			"pve": {
				{VMID: 100, Name: "", Status: "running"},
				{VMID: 101, Name: "foo", Status: "running"},
			},
		},
	}
	s := newTestSupervisor(fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.stopAll()

	s.reconcile(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cancels[guestID{Node: "pve", Type: "qemu", VMID: 100}]; ok {
		t.Errorf("empty-name guest 100 should not be tracked")
	}
	if _, ok := s.cancels[guestID{Node: "pve", Type: "qemu", VMID: 101}]; !ok {
		t.Errorf("named guest 101 should be tracked")
	}
}

func TestReconcile_SpawnsAfterEmptyNameGetsName(t *testing.T) {
	fake := &supervisorFake{
		nodes: []pveapi.Node{{Node: "pve"}},
		qemu: map[string][]pveapi.QEMU{
			"pve": {{VMID: 100, Name: "", Status: "running"}},
		},
	}
	s := newTestSupervisor(fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.stopAll()

	s.reconcile(ctx)
	s.mu.Lock()
	if _, ok := s.cancels[guestID{Node: "pve", Type: "qemu", VMID: 100}]; ok {
		s.mu.Unlock()
		t.Fatalf("empty-name guest should not be tracked on first pass")
	}
	s.mu.Unlock()

	// Operator sets a name in PVE. VMID is unchanged.
	fake.qemu["pve"][0].Name = "renamed"
	s.reconcile(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.cancels[guestID{Node: "pve", Type: "qemu", VMID: 100}]; !ok {
		t.Errorf("guest should be tracked after acquiring a name")
	}
	if _, ok := s.seenEmpty[guestID{Node: "pve", Type: "qemu", VMID: 100}]; ok {
		t.Errorf("seenEmpty entry should be cleared once the guest gets a name")
	}
}

func TestReconcile_EmptyNameLogsOnce(t *testing.T) {
	fake := &supervisorFake{
		nodes: []pveapi.Node{{Node: "pve"}},
		qemu: map[string][]pveapi.QEMU{
			"pve": {{VMID: 100, Name: "", Status: "running"}},
		},
	}
	s := newTestSupervisor(fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.stopAll()

	s.reconcile(ctx)
	s.reconcile(ctx)
	s.reconcile(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.seenEmpty); n != 1 {
		t.Errorf("seenEmpty size after repeated reconciles: want 1, got %d", n)
	}
}

func TestReconcile_EmptyNameClearsSeenOnEvict(t *testing.T) {
	fake := &supervisorFake{
		nodes: []pveapi.Node{{Node: "pve"}},
		qemu: map[string][]pveapi.QEMU{
			"pve": {{VMID: 100, Name: "", Status: "running"}},
		},
	}
	s := newTestSupervisor(fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer s.stopAll()

	s.reconcile(ctx)
	s.mu.Lock()
	if _, ok := s.seenEmpty[guestID{Node: "pve", Type: "qemu", VMID: 100}]; !ok {
		s.mu.Unlock()
		t.Fatal("expected seenEmpty entry after first reconcile")
	}
	s.mu.Unlock()

	// Guest leaves the cluster.
	fake.qemu["pve"] = nil
	s.reconcile(ctx)

	s.mu.Lock()
	defer s.mu.Unlock()
	if n := len(s.seenEmpty); n != 0 {
		t.Errorf("seenEmpty should clear when guest leaves cluster, got %d entries", n)
	}
}
