package calls

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
)

// HandleInvite returns while the actor is blocked in a host call.
func TestHandleInviteNonBlocking(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		gate := make(chan struct{})
		h.host.resolveGate = gate
		sc := h.newSIPCall()
		h.m.HandleInvite(testLogin, testLocal, sc) // must return although resolve blocks
		synctest.Wait()
		if len(h.host.resolveList()) != 1 {
			t.Fatal("actor did not reach ResolveCallRoom")
		}
		close(gate)
		synctest.Wait()
		sc.cancel()
		h.assertEnded("cancelled", "4xx")
	})
}

// HandleRTCEvent never blocks; the 33rd queued event is dropped and logged.
func TestHandleRTCEventNeverBlocks(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		gate := make(chan struct{})
		h.ghost.stateGate = gate // actor blocks in membership publish, after the call became routable
		sc := h.invite()
		evt, ok := matrixrtc.FromMatrixEvent(declineEvent(testOwner, "$not-the-ring", testRoom))
		if !ok {
			t.Fatal("bad fixture")
		}
		for i := 0; i < callEventBuffer+1; i++ {
			h.m.HandleRTCEvent(context.Background(), testLogin, evt)
		}
		if n := strings.Count(h.logs.String(), "Call event queue full"); n != 1 {
			t.Errorf("drop logged %d times, want 1", n)
		}
		h.ghost.mu.Lock()
		h.ghost.stateGate = nil
		h.ghost.mu.Unlock()
		close(gate)
		synctest.Wait()
		if sc.has("reject") {
			t.Fatalf("ignored declines ended the call: %v", sc.commands())
		}
		sc.cancel()
		h.assertEnded("cancelled", "4xx")
	})
}

// Stop ends calls and rejects later INVITEs with 480; it is idempotent.
func TestStopRejectsLaterInvites(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc1 := h.invite()
		h.m.Stop(context.Background())
		if !sc1.has("reject:480") || h.callCount() != 0 {
			t.Fatalf("call 1 not ended: %v", sc1.commands())
		}
		sc2 := h.invite()
		if cmds := sc2.commands(); len(cmds) != 1 || cmds[0] != "reject:480" {
			t.Errorf("INVITE after Stop: %v", cmds)
		}
		h.m.Stop(context.Background())
		if h.callCount() != 0 {
			t.Error("calls left after Stop")
		}
	})
}

// EndLogin only ends the given login's calls and honours ctx.
func TestEndLoginOtherLogin(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		h.m.EndLogin(context.Background(), "other@login")
		if sc.has("reject") || h.callCount() != 1 {
			t.Fatal("EndLogin ended another login's call")
		}
		sc.cancel()
		h.assertEnded("cancelled", "4xx")
	})
}
