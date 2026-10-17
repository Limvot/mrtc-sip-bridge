package calls

import (
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rs/zerolog"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

func wantSnap(t *testing.T, h *harness, state, dir, remote string, answered bool) Snapshot {
	t.Helper()
	s, ok := h.m.Snapshot(testLogin)
	if !ok {
		t.Fatalf("no snapshot, want state %s", state)
	}
	if s.State != state || s.Direction != dir || s.Outbound != (dir == dirOutbound) || s.Remote != remote {
		t.Fatalf("snapshot %+v, want state=%s dir=%s remote=%q", s, state, dir, remote)
	}
	if s.ID == "" || s.RoomID != testRoom || s.StartedAt.IsZero() {
		t.Fatalf("snapshot identity fields %+v", s)
	}
	if answered == s.AnsweredAt.IsZero() {
		t.Fatalf("AnsweredAt %v, answered=%v", s.AnsweredAt, answered)
	}
	return s
}

func TestSnapshotNone(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		if _, ok := h.m.Snapshot(testLogin); ok {
			t.Fatal("snapshot without a call")
		}
	})
}

func TestSnapshotInbound(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		sc := h.invite()
		wantSnap(t, h, "ringing", dirInbound, "+15552345678", false)
		if _, ok := h.m.Snapshot("other@login"); ok {
			t.Fatal("snapshot for another login")
		}
		h.answer(sc, "DEV")
		h.advance(time.Second)
		s := wantSnap(t, h, "active", dirInbound, "+15552345678", true)
		if s.AnsweredAt.Before(s.StartedAt) {
			t.Errorf("answered %v before start %v", s.AnsweredAt, s.StartedAt)
		}
		sc.bye()
		synctest.Wait()
		if _, ok := h.m.Snapshot(testLogin); ok {
			t.Fatal("snapshot after the call ended")
		}
	})
}

func TestSnapshotInboundBusyNotReported(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t)
		h.invite()
		h.invite(func(i *sipua.InviteInfo) { i.CallID = "in2"; i.FromUser = "+15559998888" })
		s := wantSnap(t, h, "ringing", dirInbound, "+15552345678", false)
		if s.Remote == "+15559998888" {
			t.Fatal("busy call reported")
		}
	})
}

func TestSnapshotOutbound(t *testing.T) {
	run(t, func(t *testing.T) {
		h := newHarness(t, withOutbound(""))
		h.ownerStart("OWNERDEV")
		oc := h.dialed()
		wantSnap(t, h, "dialing", dirOutbound, "+15552345678", false)
		h.ownerJoinedLK("OWNERDEV")
		oc.auth()
		oc.provisional(183, true)
		synctest.Wait()
		wantSnap(t, h, "early", dirOutbound, "+15552345678", false)
		h.advance(2 * time.Second)
		oc.answer()
		synctest.Wait()
		wantSnap(t, h, "active", dirOutbound, "+15552345678", true)
		oc.bye()
		synctest.Wait()
		if _, ok := h.m.Snapshot(testLogin); ok {
			t.Fatal("snapshot after the call ended")
		}
	})
}

// Concurrent readers against an actor-style writer (run with -race).
func TestSnapshotConcurrent(t *testing.T) {
	m := NewManager(Config{Log: zerolog.Nop()})
	login := core.LoginRef("x@y")
	c := newActor(m, login, "cid", dirOutbound, zerolog.Nop())
	m.mu.Lock()
	m.active[login] = c
	m.mu.Unlock()

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20000 {
				if s, ok := m.Snapshot(login); ok && (s.ID != "cid" || !s.Outbound) {
					t.Errorf("torn snapshot %+v", s)
					return
				}
			}
		}()
	}
	for i := range 20000 {
		c.e164 = "+15550001111"
		c.answeredAt = time.Unix(int64(i), 0)
		c.setState(stActive)
	}
	wg.Wait()
	s, ok := m.Snapshot(login)
	if !ok || s.State != "active" || s.Remote != "+15550001111" {
		t.Fatalf("final %+v %v", s, ok)
	}
}
