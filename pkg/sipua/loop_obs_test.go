package sipua

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"

	"github.com/williamkray/mrtc-sip-bridge/internal/obstest"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

func TestRegisterLoopSpans(t *testing.T) {
	env := obstest.Setup(t)
	var logBuf bytes.Buffer
	boom := errors.New("timeout talking to 203.0.113.9")
	// full ok, refresh ok, refresh fails (then the loop is cancelled in the sleep).
	h := newHarness(t, "reg.example.com:5008", twoIPs(), []error{nil, nil, boom}, 3)
	h.l.log = zerolog.New(&logBuf)
	hostCtx, host := obs.Start(context.Background(), "host.start")
	h.l.startSC = trace.SpanContextFromContext(hostCtx)
	h.l.run(h.ctx)
	host.End()

	eq(t, "calls", h.txn.calls, []string{"Register", "Refresh", "Refresh"})
	regs := env.SpansByName("sip.register")
	if len(regs) != 2 {
		t.Fatalf("got %d sip.register spans, want 2 (full ok + failed refresh; successful refresh has none)", len(regs))
	}
	full, failed := regs[0], regs[1]
	if got := obstest.Attr(full, "sip.register.kind").AsString(); got != "full" {
		t.Errorf("first span kind = %q", got)
	}
	if full.Parent().SpanID() != host.SpanContext().SpanID() {
		t.Error("first sip.register is not a child of the Start caller's span")
	}
	if got := obstest.Attr(failed, "sip.register.kind").AsString(); got != "refresh" {
		t.Errorf("failed span kind = %q", got)
	}
	if failed.Parent().IsValid() {
		t.Error("later sip.register must be a root")
	}
	if failed.Status().Code != codes.Error {
		t.Error("failed refresh span has no error status")
	}
	if len(env.SpansByName("sip.register.resolve")) != 1 {
		t.Error("want one sip.register.resolve child span for the initial lookup")
	}
	var sawState bool
	for _, ev := range full.Events() {
		if ev.Name == "sip.registration.state" {
			sawState = true
		}
	}
	if !sawState {
		t.Error("sip.registration.state event missing on first register span")
	}

	if got := env.Sum(t, "mrtcsip.sip.register.attempts", "sip.register.kind", "refresh", "outcome", "ok"); got != 1 {
		t.Errorf("refresh/ok attempts = %v, want 1", got)
	}
	if got := env.Sum(t, "mrtcsip.sip.register.attempts", "sip.register.kind", "refresh", "outcome", "transient"); got != 1 {
		t.Errorf("refresh/transient attempts = %v, want 1", got)
	}
	if got := env.Sum(t, "mrtcsip.sip.register.attempts", "sip.register.kind", "full", "outcome", "ok"); got != 1 {
		t.Errorf("full/ok attempts = %v, want 1", got)
	}
	if got := env.Sum(t, "mrtcsip.sip.register.duration", "sip.register.kind", "refresh"); got != 2 {
		t.Errorf("refresh duration observations = %v, want 2", got)
	}
	obstest.AssertMetricAttrsAllowed(t, env.Reader, nil)
	obstest.AssertNoSecrets(t, env.Rec, &logBuf, "secret", "12345678")
}

func TestRefreshOKProducesNoSpan(t *testing.T) {
	env := obstest.Setup(t)
	h := newHarness(t, "reg.example.com:5008", twoIPs(), nil, 4)
	h.l.run(h.ctx)
	if n := len(env.SpansByName("sip.register")); n != 1 {
		t.Fatalf("got %d sip.register spans, want only the initial full register", n)
	}
	if got := env.Sum(t, "mrtcsip.sip.register.attempts", "sip.register.kind", "refresh", "outcome", "ok"); got != 3 {
		t.Errorf("refresh/ok attempts = %v, want 3", got)
	}
}

func TestUnregisterSpan(t *testing.T) {
	env := obstest.Setup(t)
	h := newHarness(t, "reg.example.com:5008", twoIPs(), nil, 1)
	h.l.run(h.ctx)
	if err := h.l.shutdown(context.Background(), StateRegistered); err != nil {
		t.Fatal(err)
	}
	s := env.SpanByName("sip.unregister")
	if s == nil || s.Parent().IsValid() {
		t.Fatalf("want a root sip.unregister span, got %v", s)
	}
	if got := env.Sum(t, "mrtcsip.sip.register.attempts", "sip.register.kind", "unregister", "outcome", "ok"); got != 1 {
		t.Errorf("unregister attempts = %v", got)
	}
}

func TestUnregisterLogs(t *testing.T) {
	obstest.Setup(t)
	for _, fail := range []bool{false, true} {
		h := newHarness(t, "reg.example.com:5008", twoIPs(), nil, 1)
		var buf bytes.Buffer
		h.l.log = zerolog.New(&buf)
		if fail {
			h.txn.unregErr = errors.New("boom for sip:user:hunter2@reg.example.com")
		}
		h.l.run(h.ctx)
		buf.Reset()
		_ = h.l.shutdown(context.Background(), StateRegistered)
		out := buf.String()
		want, level := "SIP unregistered", `"level":"info"`
		if fail {
			want, level = "SIP unregister failed", `"level":"warn"`
		}
		if !strings.Contains(out, want) || !strings.Contains(out, level) {
			t.Errorf("fail=%v: log = %q, want %q at %s", fail, out, want, level)
		}
		if !strings.Contains(out, `"username":"12***78"`) || strings.Contains(out, "12345678") || strings.Contains(out, "hunter2") {
			t.Errorf("fail=%v: username not masked or secret leaked: %q", fail, out)
		}
		if !fail && (!strings.Contains(out, `"contact":"203.0.113.5:40123"`) || !strings.Contains(out, `"registrar_addr"`)) {
			t.Errorf("missing contact/registrar_addr: %q", out)
		}
	}
}
