package core

import (
	"bytes"
	"context"
	"testing"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"maunium.net/go/mautrix/event"

	"github.com/williamkray/mrtc-sip-bridge/internal/obstest"
	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

func TestIngestSpan(t *testing.T) {
	env := obstest.Setup(t)
	var logBuf bytes.Buffer
	lg := zerolog.New(&logBuf)
	in := NewIngest(LogSink{Log: &lg})
	reg := &fakeRegistrar{}
	resolveOK := false
	RegisterMatrixRTCHandlers(reg, func(context.Context, *event.Event) (LoginRef, bool) {
		return "+15551234567@reg.example.com", resolveOK
	}, in)

	evt := knownEvent()
	evt.Content = event.Content{Raw: map[string]any{"secret": "hunter2-secret", "livekit_service_url": "https://lk.example.org/x"}}

	// Dropped by the resolver: no span, counted routed=false.
	reg.handlers[2](context.Background(), evt)
	if n := len(env.Spans()); n != 0 {
		t.Fatalf("dropped event created %d spans", n)
	}
	if got := env.Sum(t, "mrtcsip.matrixrtc.events", "routed", "false", "matrixrtc.kind", "rtc_notification"); got != 1 {
		t.Errorf("routed=false count = %v, want 1", got)
	}

	// Accepted: one CONSUMER root span with ids and no content.
	resolveOK = true
	reg.handlers[2](context.Background(), evt)
	spans := env.SpansByName("matrixrtc.ingest")
	if len(spans) != 1 {
		t.Fatalf("got %d matrixrtc.ingest spans", len(spans))
	}
	s := spans[0]
	if s.Parent().IsValid() {
		t.Error("matrixrtc.ingest must be a root without a host span")
	}
	if s.SpanKind() != trace.SpanKindConsumer {
		t.Errorf("kind = %v", s.SpanKind())
	}
	if got := obstest.Attr(s, "matrix.room_id").AsString(); got != "!room:example.org" {
		t.Errorf("room_id = %q", got)
	}
	if got := obstest.Attr(s, "matrixrtc.kind").AsString(); got != "rtc_notification" {
		t.Errorf("kind attr = %q", got)
	}
	if got := obstest.Attr(s, "call.local.e164").AsString(); got != "+15551234567" {
		t.Errorf("call.local.e164 = %q", got)
	}
	if got := env.Sum(t, "mrtcsip.matrixrtc.events", "routed", "true"); got != 1 {
		t.Errorf("routed=true count = %v, want 1", got)
	}
	obstest.AssertNoSecrets(t, env.Rec, &logBuf, "hunter2-secret", "lk.example.org")
	obstest.AssertMetricAttrsAllowed(t, env.Reader, nil)

	// The sink line logged under the span carries the correlation fields exactly once.
	if n := bytes.Count(logBuf.Bytes(), []byte(`"trace_id"`)); n != 1 {
		t.Errorf("log line has %d trace_id fields, want 1: %s", n, logBuf.String())
	}
}

func TestIngestSpanChildOfHostSpan(t *testing.T) {
	env := obstest.Setup(t)
	in := NewIngest(&fakeSink{})
	ctx, host := obsStart(context.Background())
	if !in.HandleMatrixRTCEvent(ctx, "login1", knownEvent()) {
		t.Fatal("not handled")
	}
	host.End()
	s := env.SpanByName("matrixrtc.ingest")
	if s == nil || s.Parent().SpanID() != host.SpanContext().SpanID() {
		t.Fatalf("ingest span is not a child of the host span: %+v", s)
	}
	if len(env.Roots()) != 1 {
		t.Errorf("want only the host root, got %d roots", len(env.Roots()))
	}
	_ = matrixrtc.KindUnknown
}

func obsStart(ctx context.Context) (context.Context, trace.Span) {
	return obs.Start(ctx, "host.span")
}

type routingSink struct{}

func (routingSink) HandleRTCEvent(ctx context.Context, _ LoginRef, _ matrixrtc.Event) {
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("matrixrtc.routed_to", "call"))
}

func TestIngestSinkOverridesRoutedTo(t *testing.T) {
	env := obstest.Setup(t)
	in := NewIngest(routingSink{})
	if !in.HandleMatrixRTCEvent(context.Background(), "login1", knownEvent()) {
		t.Fatal("not handled")
	}
	s := env.SpanByName("matrixrtc.ingest")
	if s == nil {
		t.Fatal("no ingest span")
	}
	if got := obstest.Attr(s, "matrixrtc.routed_to").AsString(); got != "call" {
		t.Errorf("routed_to = %q, want call", got)
	}

	// Default stays "none" when the sink does not set it.
	in2 := NewIngest(&fakeSink{})
	in2.HandleMatrixRTCEvent(context.Background(), "login1", knownEvent())
	spans := env.SpansByName("matrixrtc.ingest")
	if got := obstest.Attr(spans[1], "matrixrtc.routed_to").AsString(); got != "none" {
		t.Errorf("default routed_to = %q, want none", got)
	}
}

// TestIngestSetLocalNumber checks that SetLocalNumber overrides call.local.e164 and that nil restores the
// standalone rule (LoginRef up to the first '@').
func TestIngestSetLocalNumber(t *testing.T) {
	env := obstest.Setup(t)
	in := NewIngest(&fakeSink{})
	var asked LoginRef
	in.SetLocalNumber(func(l LoginRef) string { asked = l; return "+15557654321" })
	if !in.HandleMatrixRTCEvent(context.Background(), "alice@example.org", knownEvent()) {
		t.Fatal("event not handled")
	}
	spans := env.SpansByName("matrixrtc.ingest")
	if len(spans) != 1 {
		t.Fatalf("got %d spans", len(spans))
	}
	if got := obstest.Attr(spans[0], "call.local.e164").AsString(); got != "+15557654321" || asked != "alice@example.org" {
		t.Errorf("call.local.e164 = %q (asked for %q)", got, asked)
	}

	in.SetLocalNumber(nil)
	if !in.HandleMatrixRTCEvent(context.Background(), "+15551234567@reg.example.com", knownEvent()) {
		t.Fatal("event not handled")
	}
	spans = env.SpansByName("matrixrtc.ingest")
	if len(spans) != 2 {
		t.Fatalf("got %d spans", len(spans))
	}
	if got := obstest.Attr(spans[1], "call.local.e164").AsString(); got != "+15551234567" {
		t.Errorf("nil SetLocalNumber: call.local.e164 = %q", got)
	}
}
