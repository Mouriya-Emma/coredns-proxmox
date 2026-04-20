package proxmox

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Mouriya-Emma/coredns-proxmox/internal/pveapi"
)

// blockingPveClient holds GetNodes inside `<-ctx.Done()` to simulate a
// slow/unresponsive PVE. Tracks active inflight calls via getNodesActive so
// tests can assert supervisor state around Stop().
type blockingPveClient struct {
	getNodesActive atomic.Int32
}

func (c *blockingPveClient) GetNodes(ctx context.Context) ([]pveapi.Node, error) {
	c.getNodesActive.Add(1)
	defer c.getNodesActive.Add(-1)
	<-ctx.Done()
	return nil, ctx.Err()
}
func (c *blockingPveClient) GetQEMUVMs(context.Context, string) ([]pveapi.QEMU, error) {
	return nil, nil
}
func (c *blockingPveClient) GetLXCs(context.Context, string) ([]pveapi.LXC, error) {
	return nil, nil
}
func (c *blockingPveClient) GetQEMUConfig(context.Context, string, int) (pveapi.QEMUConfig, error) {
	return pveapi.QEMUConfig{}, nil
}
func (c *blockingPveClient) GetQEMUInterfaces(context.Context, string, int) (pveapi.AgentInterfacesResponse, error) {
	return pveapi.AgentInterfacesResponse{}, nil
}
func (c *blockingPveClient) GetLXCConfig(context.Context, string, int) (pveapi.LXCConfig, error) {
	return pveapi.LXCConfig{}, nil
}
func (c *blockingPveClient) GetLXCInterfaces(context.Context, string, int) ([]pveapi.LXCInterface, error) {
	return nil, nil
}

func TestStop_BlocksUntilSupervisorExits(t *testing.T) {
	fake := &blockingPveClient{}
	p := &Proxmox{
		client:            fake,
		Zones:             []string{"hb.lan."},
		PermissiveChannel: true,
		ReconcileEvery:    10 * time.Second,
		PollNever:         1 * time.Second,
		PollKnown:         1 * time.Second,
	}
	if err := p.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// Wait for enumerate to begin — otherwise Stop would cancel before the
	// supervisor does anything interesting and the test wouldn't prove much.
	deadline := time.Now().Add(500 * time.Millisecond)
	for fake.getNodesActive.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if fake.getNodesActive.Load() == 0 {
		t.Fatal("supervisor did not call GetNodes within 500ms")
	}

	stopDone := make(chan struct{})
	go func() { _ = p.Stop(); close(stopDone) }()

	select {
	case <-stopDone:
	case <-time.After(1 * time.Second):
		t.Fatal("Stop did not return within 1s — suspect deadlock or missing drain")
	}

	// Spec: when Stop returns, the supervisor Run loop has exited, i.e.
	// s.done has been closed. A non-closed channel falls through to default.
	select {
	case <-p.sup.done:
	default:
		t.Fatal("Stop returned but supervisor.Run has not exited (done channel still open)")
	}
}

func TestStop_BeforeStart_NoPanic(t *testing.T) {
	p := &Proxmox{}
	if err := p.Stop(); err != nil {
		t.Errorf("Stop before Start: unexpected err %v", err)
	}
}
