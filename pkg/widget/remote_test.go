package widget

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/federation"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
)

func TestRemoteAllowIP(t *testing.T) {
	for ip, want := range map[string]bool{
		"8.8.8.8":         true,
		"2606:4700::1111": true,
		"127.0.0.1":       false,
		"::1":             false,
		"10.1.2.3":        false,
		"192.168.0.1":     false,
		"169.254.1.1":     false,
		"100.64.0.1":      false,
		"100.127.255.1":   false,
		"100.128.0.1":     true,
		"0.0.0.0":         false,
	} {
		if got := remoteAllowIP(net.ParseIP(ip)); got != want {
			t.Errorf("%s: %v want %v", ip, got, want)
		}
	}
}

func TestDiscoveryClientIsIPFilteredAndNoRedirects(t *testing.T) {
	var hits int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		http.Redirect(w, nil, "/x", http.StatusFound)
	}))
	defer srv.Close()
	fc, srt := newFederationClient()
	if srt == nil || srt.ResolveOpts == nil || srt.ResolveOpts.HTTPClient == nil {
		t.Fatal("discovery client not installed")
	}
	if fc.AllowIP(net.ParseIP("100.64.0.1")) || !fc.AllowIP(net.ParseIP("8.8.8.8")) {
		t.Error("AllowIP not tightened")
	}
	hc := srt.ResolveOpts.HTTPClient
	if hc.Timeout <= 0 {
		t.Error("no timeout")
	}
	if err := hc.CheckRedirect(nil, nil); !errors.Is(err, http.ErrUseLastResponse) {
		t.Errorf("CheckRedirect: %v", err)
	}
	// The test server listens on loopback: the filtering dialer must refuse it.
	_, err := hc.Get(srv.URL)
	if err == nil || !strings.Contains(err.Error(), federation.ErrIPFiltered.Error()) {
		t.Fatalf("loopback fetch: %v", err)
	}
	if hits != 0 {
		t.Errorf("server was contacted %d times", hits)
	}
}

func TestRemoteVerifierMapsFilteredDialToTransient(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("filtered address was contacted")
	}))
	defer srv.Close()
	verify := defaultRemoteVerifier(clock.NewFake(t0))
	_, err := verify(context.Background(), strings.TrimPrefix(srv.URL, "https://"), "tok")
	var tr errOpenIDTransient
	if !errors.As(err, &tr) || errors.Is(err, ErrOpenIDInvalid) {
		t.Fatalf("want transient, got %v", err)
	}
	if strings.Contains(err.Error(), "tok") && strings.Contains(err.Error(), "access_token") {
		t.Errorf("error leaks the request: %v", err)
	}
}

func TestSubMismatchPattern(t *testing.T) {
	if !subMismatchRE.MatchString("user ID @a:b.org does not belong to server c.org") {
		t.Error("mismatch error not recognized")
	}
	if subMismatchRE.MatchString(`Get "https://x/?access_token=user ID @a:b does not belong to server": dial`) {
		t.Error("transport error recognized as mismatch")
	}
}

func TestServerNamesCap(t *testing.T) {
	clk := clock.NewFake(t0)
	n := newServerNames(clk, 3)
	for _, s := range []string{"a.org", "b.org", "c.org"} {
		if !n.admit(s) {
			t.Fatalf("%s refused", s)
		}
	}
	if n.admit("d.org") {
		t.Error("new name over the cap admitted")
	}
	if !n.admit("a.org") {
		t.Error("known name refused")
	}
	clk.Advance(remoteNamesWindow + time.Minute)
	if !n.admit("d.org") {
		t.Error("name refused after the window passed")
	}
}

func TestRemoteTooManyServersIsTransientNotNegCached(t *testing.T) {
	e := newEnv(t, func(c *hostkit.WidgetConfig, d *Deps) {
		c.AllowRemoteUsers = true
		d.RemoteVerify = func(context.Context, string, string) (id.UserID, error) {
			return "", errOpenIDTransient{"too many servers"}
		}
	})
	sub, out := e.s.verifyOpenID(context.Background(), "far.example", "t1")
	if sub != "" || out != outcomeOpenIDError {
		t.Fatalf("got %q %q", sub, out)
	}
	if e.s.neg.has(tokenHash("far.example", "t1")) {
		t.Error("transient failure was negative-cached")
	}
}

func TestPrivateAmong(t *testing.T) {
	ghost := func(u id.UserID) bool { return strings.HasPrefix(string(u), "@ghost_") }
	bot := id.UserID("@bot:x")
	allowed := []id.UserID{"@me:x"}
	for name, tc := range map[string]struct {
		m    []id.UserID
		want bool
	}{
		"owner bot ghost": {[]id.UserID{"@me:x", bot, "@ghost_1:x"}, true},
		"stranger":        {[]id.UserID{"@me:x", bot, "@you:x"}, false},
		"empty":           {nil, true},
	} {
		if got := privateAmong(tc.m, bot, allowed, ghost); got != tc.want {
			t.Errorf("%s: %v", name, got)
		}
	}
	if _, err := PrivateRoom(context.Background(), nil, "!r:x"); err == nil {
		t.Error("nil bridge must error")
	}
}
