package widget

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/internal/obstest"
	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
)

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func TestContentBuilder(t *testing.T) {
	e := newEnv(t)
	in := e.s.Installer()
	c := in.Content(roomPortal, "Call "+portalNumber)
	u := c["url"].(string)
	wantPrefix := testPublic + "?widgetId=$matrix_widget_id&theme=$org.matrix.msc2873.client_theme#room="
	if !strings.HasPrefix(u, wantPrefix) {
		t.Fatalf("url %q", u)
	}
	frag := u[strings.Index(u, "#")+1:]
	if !strings.HasPrefix(frag, "room=%21portal%3Ahs.example&k=") {
		t.Errorf("fragment %q", frag)
	}
	vals, err := url.ParseQuery(frag)
	if err != nil || vals.Get("room") != string(roomPortal) || vals.Get("k") != e.key(roomPortal) {
		t.Errorf("fragment values %v %v", vals, err)
	}
	if c["type"] != "m.custom" || c["name"] != "Dialpad" || c["id"] != "sipbridge_dialpad" ||
		c["creatorUserId"] != "@sipbot:hs.example" || c["waitForIframeLoad"] != true {
		t.Errorf("content %v", c)
	}
	if c["data"].(map[string]any)["title"] != "Call "+portalNumber {
		t.Errorf("title %v", c["data"])
	}
	if _, ok := in.Content(roomMgmt, "")["data"].(map[string]any)["title"]; ok {
		t.Error("management room has a title")
	}
	if StateKey("x") != "x_dialpad" {
		t.Error("StateKey")
	}
}

func TestEnsureIdempotentAndUpdate(t *testing.T) {
	e := newEnv(t)
	in := e.s.Installer()
	ctx := context.Background()
	if err := in.Ensure(ctx, roomPortal, "Call "+portalNumber); err != nil {
		t.Fatal(err)
	}
	if g, s := e.state.counts(); g != 1 || s != 1 {
		t.Fatalf("first ensure: gets %d sets %d", g, s)
	}
	if err := in.Ensure(ctx, roomPortal, "Call "+portalNumber); err != nil {
		t.Fatal(err)
	}
	if _, s := e.state.counts(); s != 1 {
		t.Fatalf("unchanged content was re-sent")
	}
	// Enqueue of a done room is a no-op (no queue entry).
	in.Enqueue(roomPortal, "Call "+portalNumber)
	if len(in.queue) != 0 {
		t.Error("done room enqueued")
	}
	// A secret change alters the content: the next ensure updates it.
	cfg := testCfg()
	cfg.Secret = testSecret + "-rotated"
	in2 := newInstaller(cfg, e.s.d, e.s.log)
	res, err := in2.ensure(ctx, roomPortal, "Call "+portalNumber)
	if err != nil || res != outcomeUpdated {
		t.Fatalf("rotated: %v %v", res, err)
	}
	if !strings.Contains(e.state.state[roomPortal]["url"].(string), "k="+roomKey([]byte(cfg.Secret), roomPortal)) {
		t.Error("url not updated")
	}
}

func TestEnsureForbidden(t *testing.T) {
	e := newEnv(t)
	e.state.forbid[roomMgmt] = true
	in := e.s.Installer()
	err := in.Ensure(context.Background(), roomMgmt, "")
	if !errors.Is(err, mautrix.MForbidden) {
		t.Fatalf("err %v", err)
	}
	// Marked done: a sweep does not retry it.
	in.sweep(context.Background(), TriggerPeriodic, noSC)
	if _, s := e.state.counts(); s != 2 { // forbidden attempt + the portal
		t.Errorf("sets %d", s)
	}
	in.sweep(context.Background(), TriggerPeriodic, noSC)
	if _, s := e.state.counts(); s != 2 {
		t.Errorf("forbidden room retried: sets %d", s)
	}
}

func TestEnqueueNeverBlocks(t *testing.T) {
	e := newEnv(t) // not started: nobody drains the queue
	in := e.s.Installer()
	done := make(chan struct{})
	go func() {
		for i := 0; i < queueSize+100; i++ {
			in.Enqueue(id.RoomID("!r"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+":hs"), "")
		}
		in.RequestSweep(context.Background(), TriggerLogin)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Enqueue blocked")
	}
	if len(in.queue) != queueSize {
		t.Errorf("queue %d", len(in.queue))
	}
}

func TestWorkerStartupAndPeriodicSweep(t *testing.T) {
	ot := obstest.Setup(t)
	e := newEnv(t)
	e.s.Start()
	eventually(t, "startup sweep", func() bool { _, s := e.state.counts(); return s == 2 })
	eventually(t, "startup span", func() bool { return ot.SpanByName(spanSweep) != nil })
	sp := ot.SpanByName(spanSweep)
	if sp.Parent().IsValid() || obstest.Attr(sp, "trigger").AsString() != TriggerStartup ||
		obstest.Attr(sp, "rooms.installed").AsInt64() != 2 {
		t.Errorf("startup span %v", sp.Attributes())
	}
	if !e.clk.WaitForWaiters(2) {
		t.Fatal("tickers not armed")
	}
	// Periodic pass with nothing new: no state reads, no span.
	g0, _ := e.state.counts()
	e.clk.Advance(sweepEvery)
	time.Sleep(20 * time.Millisecond)
	if g, _ := e.state.counts(); g != g0 {
		t.Errorf("periodic sweep re-read done rooms: %d -> %d", g0, g)
	}
	if n := len(ot.SpansByName(spanSweep)); n != 1 {
		t.Errorf("periodic no-op produced spans: %d", n)
	}
	// A new portal appears (created by texting): the next periodic pass installs it.
	e.setRooms(append(e.rooms, RoomRef{RoomID: "!new:hs.example", Title: "Call +15550009999"}))
	e.clk.Advance(sweepEvery)
	eventually(t, "periodic install", func() bool { _, s := e.state.counts(); return s == 3 })
	eventually(t, "periodic span", func() bool { return len(ot.SpansByName(spanSweep)) == 2 })
	if g, _ := e.state.counts(); g != g0+1 {
		t.Errorf("periodic sweep read %d rooms, want 1", g-g0)
	}
	// Enqueue from a command or a created portal goes through the worker.
	e.s.Installer().Enqueue("!created:hs.example", "Call +15550001234")
	eventually(t, "enqueued install", func() bool { _, s := e.state.counts(); return s == 4 })
	ins := ot.Sum(t, metricInstalls, "outcome", outcomeInstalled)
	if ins != 4 {
		t.Errorf("installs metric %v", ins)
	}
	obstest.AssertMetricAttrsAllowed(t, ot.Reader, widgetAllowlist())
}

func TestLoginSweepIsLinkedRoot(t *testing.T) {
	ot := obstest.Setup(t)
	e := newEnv(t)
	in := e.s.Installer()
	pctx, parent := startTestSpan(context.Background(), "bridge.calls.pl_sweep")
	parent.End()
	in.sweep(context.Background(), TriggerLogin, spanContextOf(pctx))
	sp := ot.SpanByName(spanSweep)
	if sp == nil || sp.Parent().IsValid() || len(sp.Links()) != 1 {
		t.Fatalf("login sweep span: %+v", sp)
	}
}

func TestRemoveSweep(t *testing.T) {
	ot := obstest.Setup(t)
	e := newEnv(t)
	in := e.s.Installer()
	if err := in.Ensure(context.Background(), roomPortal, "Call "+portalNumber); err != nil {
		t.Fatal(err)
	}
	d := e.s.d
	if err := Remove(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	if c := e.state.state[roomPortal]; len(c) != 0 {
		t.Errorf("portal widget not removed: %v", c)
	}
	if _, ok := e.state.state[roomMgmt]; ok {
		t.Error("removal wrote to a room without the widget")
	}
	sp := ot.SpanByName(spanSweep)
	if sp == nil || obstest.Attr(sp, "trigger").AsString() != TriggerDisable || obstest.Attr(sp, "rooms.removed").AsInt64() != 1 {
		t.Errorf("disable span %+v", sp)
	}
	if ot.Sum(t, metricInstalls, "outcome", outcomeRemoved) != 1 {
		t.Error("removed metric")
	}
	if err := Remove(context.Background(), Deps{}); err == nil {
		t.Error("Remove without deps should fail")
	}
}

func TestInstallerImplementsHostkit(t *testing.T) {
	var _ hostkit.WidgetInstaller = newEnv(t).s.Installer()
}

func TestInstallerSkipsNonPrivateRooms(t *testing.T) {
	members := map[id.RoomID][]id.UserID{
		roomPortal: {"@sipbot:hs.example", alice},
		roomMgmt:   {"@sipbot:hs.example", alice, "@stranger:far.example"},
	}
	e := newEnv(t, func(_ *hostkit.WidgetConfig, d *Deps) {
		d.RoomMembers = func(_ context.Context, r id.RoomID) ([]id.UserID, error) { return members[r], nil }
	})
	in := e.s.Installer()
	if err := in.Ensure(context.Background(), roomPortal, "Call "+portalNumber); err != nil {
		t.Fatalf("private room: %v", err)
	}
	err := in.Ensure(context.Background(), roomMgmt, "")
	if !errors.Is(err, ErrNotPrivateRoom) || !errors.Is(err, hostkit.ErrWidgetRoomNotPrivate) {
		t.Fatalf("shared room: %v", err)
	}
	if e.state.state[roomMgmt] != nil || e.state.sets != 1 {
		t.Errorf("widget written to a shared room (sets=%d)", e.state.sets)
	}
	if err.Error() != MsgNotPrivateRoom || MsgNotPrivateRoom != "This room has other members; the Dialpad works only in private bridge rooms." {
		t.Errorf("message %q", err)
	}
	// Once the stranger leaves, a later ensure installs it (the room was not marked done).
	members[roomMgmt] = []id.UserID{"@sipbot:hs.example", alice}
	if err := in.Ensure(context.Background(), roomMgmt, ""); err != nil || e.state.state[roomMgmt] == nil {
		t.Fatalf("after leave: %v", err)
	}
}

func TestInstallerPrivacyDefaultRule(t *testing.T) {
	var members []id.UserID
	e := newEnv(t, func(_ *hostkit.WidgetConfig, d *Deps) {
		d.RoomMembers = func(context.Context, id.RoomID) ([]id.UserID, error) { return members, nil }
	})
	in := e.s.Installer()
	for _, tc := range []struct {
		name string
		m    []id.UserID
		want bool
	}{
		{"owner only", []id.UserID{alice}, true},
		{"two bridge users", []id.UserID{alice, bob}, false},
		{"unknown user", []id.UserID{alice, "@x:y.org"}, false},
		{"empty", nil, true},
	} {
		members = tc.m
		if ok, err := in.roomPrivate(context.Background(), roomPortal); err != nil || ok != tc.want {
			t.Errorf("%s: %v %v", tc.name, ok, err)
		}
	}
	in.d.RoomOwners = func(context.Context, id.RoomID) []id.UserID { return []id.UserID{alice, bob} }
	members = []id.UserID{alice, bob}
	if ok, _ := in.roomPrivate(context.Background(), roomPortal); !ok {
		t.Error("explicit owners not honoured")
	}
}
