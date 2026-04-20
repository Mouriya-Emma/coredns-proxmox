package proxmox

import (
	"context"
	"testing"
	"time"

	"github.com/Mouriya-Emma/coredns-proxmox/internal/pveapi"
)

// blockingInterfacesFake is a supervisorFake variant whose GetQEMUInterfaces
// blocks on a channel and returns a pre-configured success payload once
// released — so tests can interleave "fetch succeeded" with a parallel ctx
// cancel. We wait with a 2-second timeout on the block channel so a
// misconfigured test fails fast rather than hanging.
type blockingInterfacesFake struct {
	*supervisorFake
	blockCh chan struct{}
	payload pveapi.AgentInterfacesResponse
}

func (f *blockingInterfacesFake) GetQEMUInterfaces(_ context.Context, _ string, _ int) (pveapi.AgentInterfacesResponse, error) {
	select {
	case <-f.blockCh:
	case <-time.After(2 * time.Second):
	}
	return f.payload, nil
}

// TestPollGuest_SkipsUpsertIfCtxCancelledAfterFetch exercises the
// ctx.Err() recheck in pollGuest between fetchGuestIPs returning and the
// store.Upsert call. The reconcile-side rename path cancels an old
// goroutine and deletes its store record; the old goroutine may have a
// fetch in flight and, without the recheck, would Upsert the stale record
// back moments after Delete. This test puts the fetch in that exact window.
func TestPollGuest_SkipsUpsertIfCtxCancelledAfterFetch(t *testing.T) {
	blockCh := make(chan struct{})
	fake := &blockingInterfacesFake{
		supervisorFake: &supervisorFake{
			nodes: []pveapi.Node{{Node: "pve"}},
			qemu: map[string][]pveapi.QEMU{
				"pve": {{VMID: 100, Name: "app01", Status: "running"}},
			},
		},
		blockCh: blockCh,
		payload: pveapi.AgentInterfacesResponse{
			Result: []pveapi.AgentInterface{{
				Name:            "eth0",
				HardwareAddress: "aa:bb:cc:dd:ee:ff",
				IPAddresses: []pveapi.AgentInterfaceAddress{
					{Type: "ipv4", Address: "192.168.1.50"},
				},
			}},
		},
	}
	s := newTestSupervisor(fake)
	ctx, cancel := context.WithCancel(context.Background())
	defer s.stopAll()

	gid := guestID{Node: "pve", Type: "qemu", VMID: 100}
	meta := guestMeta{ID: gid, Name: "app01"}

	s.wg.Add(1)
	go s.pollGuest(ctx, meta)

	// pollGuest is now blocked inside fetchGuestIPs → GetQEMUInterfaces.
	// Cancel the parent ctx; the fake doesn't observe it, so it stays
	// blocked on blockCh.
	cancel()

	// Release the fake. fetchGuestIPs returns success with IPs. pollGuest's
	// ctx.Err() check must see Canceled and return BEFORE Upsert.
	close(blockCh)

	// Wait for pollGuest to exit (wg.Done in its defer).
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("pollGuest did not exit within 3s of cancel + release")
	}

	if ips := s.store.Lookup("app01.hb.lan."); len(ips) != 0 {
		t.Errorf("pollGuest Upserted despite ctx cancelled between fetch and upsert: %v", ips)
	}
}
