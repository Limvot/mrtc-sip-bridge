package connector

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"

	"github.com/williamkray/mrtc-sip-bridge/internal/obstest"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipaccount"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

const testSIPPassword = "s3cret-sip-pw"

// fakeRegistrar answers every SIP request on a UDP socket with the given final status.
type fakeRegistrar struct {
	conn   *net.UDPConn
	status int
	wg     sync.WaitGroup
}

func newFakeRegistrar(t *testing.T, status int) *fakeRegistrar {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRegistrar{conn: conn, status: status}
	f.wg.Add(1)
	go func() {
		defer f.wg.Done()
		buf := make([]byte, 65535)
		for {
			n, from, err := conn.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if resp := f.respond(string(buf[:n])); resp != "" {
				_, _ = conn.WriteToUDP([]byte(resp), from)
			}
		}
	}()
	t.Cleanup(func() { _ = conn.Close(); f.wg.Wait() })
	return f
}

func (f *fakeRegistrar) addr() string { return f.conn.LocalAddr().String() }

func (f *fakeRegistrar) respond(req string) string {
	lines := strings.Split(strings.ReplaceAll(req, "\r\n", "\n"), "\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "REGISTER ") {
		return ""
	}
	reason := map[int]string{200: "OK", 403: "Forbidden"}[f.status]
	var b strings.Builder
	b.WriteString("SIP/2.0 " + strconv.Itoa(f.status) + " " + reason + "\r\n")
	for _, l := range lines[1:] {
		name, _, _ := strings.Cut(l, ":")
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "via", "from", "call-id", "cseq":
			b.WriteString(l + "\r\n")
		case "to":
			if !strings.Contains(l, "tag=") {
				l += ";tag=fake"
			}
			b.WriteString(l + "\r\n")
		case "contact":
			if f.status == 200 {
				b.WriteString(l + "\r\n")
			}
		}
	}
	b.WriteString("Content-Length: 0\r\n\r\n")
	return b.String()
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// telemetryConnector returns a connector with a loopback SIP runtime on one free port. Call it after
// obstest.Setup: the runtime registers the registrations gauge on the current instruments.
func telemetryConnector(t *testing.T, logBuf *bytes.Buffer) *SIPConnector {
	t.Helper()
	port := freeUDPPort(t)
	return testConnector(t, port, port, zerolog.New(logBuf))
}

func inTraceNames(env *obstest.Env, root string) map[string]int {
	out := map[string]int{}
	s := env.SpanByName(root)
	if s == nil {
		return out
	}
	for _, x := range env.InTrace(s) {
		out[x.Name()]++
	}
	return out
}

func TestLoginSpan(t *testing.T) {
	env := obstest.Setup(t)
	var logBuf bytes.Buffer
	reg := newFakeRegistrar(t, 403) // auth failure ends the login without needing a database
	c := telemetryConnector(t, &logBuf)
	p := &sipLoginProcess{c: c}

	_, err := p.SubmitUserInput(context.Background(), map[string]string{
		sipaccount.FieldUsername:      "12345678",
		sipaccount.FieldPassword:      testSIPPassword,
		sipaccount.FieldRegistrar:     reg.addr(),
		sipaccount.FieldTransport:     "udp",
		sipaccount.FieldDisplayNumber: "+15551234567",
	})
	if err == nil || !strings.Contains(err.Error(), "rejected the credentials") {
		t.Fatalf("err = %v", err)
	}
	var re bridgev2.RespError
	if !errors.As(err, &re) || re.ErrCode != errCodeBadCredentials || re.StatusCode != http.StatusForbidden ||
		!strings.HasPrefix(err.Error(), "the registrar rejected the credentials (SIP 403") {
		t.Errorf("want BAD_CREDENTIALS/403 RespError with the plain message, got %#v (%q)", err, err.Error())
	}

	roots := env.Roots()
	if len(roots) != 1 || roots[0].Name() != "bridge.login.submit" {
		var names []string
		for _, r := range roots {
			names = append(names, r.Name())
		}
		t.Fatalf("want exactly one bridge.login.submit root, got %v", names)
	}
	names := inTraceNames(env, "bridge.login.submit")
	for _, want := range []string{"sip.ua.start", "sip.register", "sip.register.resolve"} {
		if names[want] == 0 {
			t.Errorf("trace is missing %s: %v", want, names)
		}
	}
	root := roots[0]
	if got := obstest.Attr(root, "outcome").AsString(); got != "auth_failed" {
		t.Errorf("outcome = %q", got)
	}
	if got := obstest.Attr(root, "sip.user.masked").AsString(); got != "12***78" {
		t.Errorf("sip.user.masked = %q", got)
	}
	if got := env.Sum(t, "mrtcsip.bridge.logins", "outcome", "auth_failed"); got != 1 {
		t.Errorf("logins auth_failed = %v", got)
	}
	if got := env.Sum(t, "mrtcsip.sip.register.attempts", "sip.register.kind", "full", "outcome", "auth"); got < 1 {
		t.Errorf("register attempts full/auth = %v", got)
	}
	obstest.AssertNoSecrets(t, env.Rec, &logBuf, testSIPPassword, "12345678@")
	obstest.AssertMetricAttrsAllowed(t, env.Reader, nil)
}

func TestConnectDisconnectSpans(t *testing.T) {
	env := obstest.Setup(t)
	var logBuf bytes.Buffer
	reg := newFakeRegistrar(t, 200)
	c := telemetryConnector(t, &logBuf)
	cl := c.newClient(&UserLoginMetadata{
		Username: "12345678", Password: testSIPPassword, Registrar: reg.addr(), Transport: "udp",
		DisplayNumber: "+15551234567",
	}, nil)

	cl.Connect(context.Background())
	ev, err := cl.acct.WaitFirstResult(context.Background(), 5*time.Second)
	if err != nil || ev.State != sipua.StateRegistered {
		t.Fatalf("first result %+v, %v", ev, err)
	}
	if got := env.Sum(t, "mrtcsip.sip.registrations", "state", "registered"); got != 1 {
		t.Errorf("registrations{registered} = %v, want 1", got)
	}
	cl.Disconnect()

	if got := env.Sum(t, "mrtcsip.sip.registrations", "state", "registered"); got != 0 {
		t.Errorf("registrations{registered} after Disconnect = %v, want 0", got)
	}
	names := inTraceNames(env, "bridge.connect")
	if names["sip.ua.start"] != 1 || names["sip.register"] != 1 {
		t.Errorf("bridge.connect trace: %v", names)
	}
	dn := inTraceNames(env, "bridge.disconnect")
	if dn["sip.unregister"] != 1 {
		t.Errorf("bridge.disconnect trace: %v", dn)
	}
	if got := env.Sum(t, "mrtcsip.sip.register.attempts", "sip.register.kind", "unregister", "outcome", "ok"); got != 1 {
		t.Errorf("unregister attempts = %v", got)
	}
	obstest.AssertNoSecrets(t, env.Rec, &logBuf, testSIPPassword, "12345678@")
	obstest.AssertMetricAttrsAllowed(t, env.Reader, nil)
}
