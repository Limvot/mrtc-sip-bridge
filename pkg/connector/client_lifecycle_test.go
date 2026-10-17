package connector

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"

	"github.com/williamkray/mrtc-sip-bridge/pkg/sipaccount"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

type fakeUA struct {
	mu       sync.Mutex
	port     int
	startErr error
	stopped  bool
}

func (f *fakeUA) Start(context.Context) error { return f.startErr }
func (f *fakeUA) Stop(context.Context) error {
	f.mu.Lock()
	f.stopped = true
	f.mu.Unlock()
	return nil
}
func (f *fakeUA) State() sipua.StateEvent { return sipua.StateEvent{} }
func (f *fakeUA) Dial(sipua.DialRequest) (sipua.OutboundCall, error) {
	return nil, errors.New("fakeUA: Dial not implemented")
}
func (f *fakeUA) isStopped() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.stopped
}

// testConnector returns a connector with a loopback SIP runtime on ports min..max (no network I/O).
func testConnector(t *testing.T, min, max int, log zerolog.Logger) *SIPConnector {
	t.Helper()
	c := &SIPConnector{br: &bridgev2.Bridge{Log: log}}
	c.Config.ApplyDefaults()
	nc := c.Config.NetConfig()
	nc.SIPBindIP, nc.ExternalIP = "127.0.0.1", "127.0.0.1"
	nc.SIPPortMin, nc.SIPPortMax = min, max
	rt, err := sipaccount.NewRuntime(context.Background(), nc, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rt.Close)
	c.rt = rt
	return c
}

// testClient returns a client of a test connector. Set newUA before calling it.
func testClient(t *testing.T, min, max int) *SIPClient {
	t.Helper()
	c := testConnector(t, min, max, zerolog.Nop())
	return c.newClient(&UserLoginMetadata{Username: "12345678", Password: "pw", Registrar: "reg.example.com:5060"}, nil)
}

// withNewUA installs f as the UA factory for clients created during the test.
func withNewUA(t *testing.T, f func(sipua.Config) (sipua.UA, error)) {
	t.Helper()
	old := newUA
	newUA = f
	t.Cleanup(func() { newUA = old })
}

func TestConnectRetryCancelledByDisconnect(t *testing.T) {
	oldDelay := startRetryDelay
	defer func() { startRetryDelay = oldDelay }()
	startRetryDelay = 20 * time.Millisecond
	var mu sync.Mutex
	calls := 0
	withNewUA(t, func(sipua.Config) (sipua.UA, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return &fakeUA{startErr: errors.New("in use")}, nil
	})
	cl := testClient(t, 5060, 5060)
	cl.Connect(context.Background())
	cl.Disconnect()
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 1 {
		t.Fatalf("retry ran after Disconnect: %d calls", calls)
	}
}

func TestConnectRetriesOnce(t *testing.T) {
	oldDelay := startRetryDelay
	defer func() { startRetryDelay = oldDelay }()
	startRetryDelay = 10 * time.Millisecond
	var mu sync.Mutex
	calls := 0
	withNewUA(t, func(sipua.Config) (sipua.UA, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return &fakeUA{startErr: errors.New("in use")}, nil
	})
	cl := testClient(t, 5060, 5060)
	cl.Connect(context.Background())
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("calls = %d, want 2 (initial + one retry)", calls)
	}
}

// startedClient returns a test client whose UA (returned) is started.
func startedClient(t *testing.T) (*SIPClient, *fakeUA) {
	t.Helper()
	fua := &fakeUA{}
	withNewUA(t, func(sipua.Config) (sipua.UA, error) { return fua, nil })
	cl := testClient(t, 5060, 5062)
	if err := cl.start(context.Background()); err != nil {
		t.Fatal(err)
	}
	return cl, fua
}

func TestLoginCancelDuringWait(t *testing.T) {
	cl, fua := startedClient(t)

	p := &sipLoginProcess{c: cl.c}
	errCh := make(chan error, 1)
	go func() {
		_, err := p.await(context.Background(), cl, "reg.example.com:5060")
		errCh <- err
	}()
	// Wait until the process is pending, then cancel.
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.mu.Lock()
		ok := p.pending != nil
		p.mu.Unlock()
		if ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("await never became pending")
		}
		time.Sleep(time.Millisecond)
	}
	p.Cancel()
	select {
	case err := <-errCh:
		if err == nil || err.Error() != "login cancelled" {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("await did not return after Cancel")
	}
	if !fua.isStopped() {
		t.Fatal("client was not stopped")
	}
	// A late await on a cancelled process refuses and stops the client.
	cl2, f2 := startedClient(t)
	if _, err := p.await(context.Background(), cl2, "x"); err == nil || !f2.isStopped() {
		t.Fatalf("await after cancel: err=%v stopped=%v", err, f2.isStopped())
	}
}

func TestLoginAwaitContextError(t *testing.T) {
	cl, _ := startedClient(t)
	p := &sipLoginProcess{c: cl.c}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := p.await(ctx, cl, "reg.example.com:5060")
	if err == nil || !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "timed out") {
		t.Fatalf("err = %v", err)
	}
}

func TestCreateChatWithGhost(t *testing.T) {
	cl := testClient(t, 5060, 5062)
	cl.meta.DisplayNumber = "+15550001111"
	cl.login = &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: "login1"}}
	ghost := &bridgev2.Ghost{Ghost: &database.Ghost{ID: MakeUserID("+447911123456")}}
	resp, err := cl.CreateChatWithGhost(context.Background(), ghost)
	if err != nil {
		t.Fatal(err)
	}
	if resp.PortalKey != MakePortalKey("+447911123456", "login1") {
		t.Fatalf("portal key = %+v", resp.PortalKey)
	}
	if resp.PortalInfo == nil || resp.PortalInfo.Members.OtherUserID != "447911123456" {
		t.Fatalf("portal info = %+v", resp.PortalInfo)
	}
	bad := &bridgev2.Ghost{Ghost: &database.Ghost{ID: "not-a-number"}}
	if _, err = cl.CreateChatWithGhost(context.Background(), bad); err == nil {
		t.Fatal("expected error for invalid ghost ID")
	}
}

type rejectCall struct {
	sipua.InboundCall
	code int
}

func (r *rejectCall) Reject(code int, _ string) { r.code = code }

func TestOnInviteWithoutManagerRejects480(t *testing.T) {
	cl, _ := startedClient(t)
	rc := &rejectCall{}
	cl.onInvite(rc)
	if rc.code != 480 {
		t.Fatalf("reject code = %d, want 480", rc.code)
	}
}

// TestStoppingRefusesCallsAndDials checks that once stop began, inbound calls get 480 and dials fail,
// as when the UA was cleared before EndLogin.
func TestStoppingRefusesCallsAndDials(t *testing.T) {
	cl, fua := startedClient(t)
	cl.stop(context.Background())
	if !fua.isStopped() || cl.running() {
		t.Fatal("UA not stopped")
	}
	rc := &rejectCall{}
	cl.onInvite(rc)
	if rc.code != 480 {
		t.Fatalf("reject code = %d, want 480", rc.code)
	}
	if _, err := cl.dial(sipua.DialRequest{}); !errors.Is(err, sipua.ErrNotRegistered) {
		t.Fatalf("dial after stop: %v", err)
	}
}
