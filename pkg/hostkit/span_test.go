package hostkit

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/trace"
	"maunium.net/go/mautrix/bridgev2/commands"

	"github.com/williamkray/mrtc-sip-bridge/internal/obstest"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

func TestWithSpanCommand(t *testing.T) {
	env := obstest.Setup(t)
	var logBuf bytes.Buffer
	base := zerolog.New(&logBuf)
	orig := context.Background()
	ce := &commands.Event{Ctx: orig, Log: &base, Args: []string{"1234#", "+15559876543"}}
	var inCtx context.Context
	WithSpan("my-cmd", func(ce *commands.Event) {
		inCtx = ce.Ctx
		ce.Log.Info().Msg("handling")
	})(ce)

	if !trace.SpanContextFromContext(inCtx).IsValid() {
		t.Fatal("handler ctx has no span")
	}
	if ce.Ctx != orig || ce.Log != &base {
		t.Error("ce.Ctx/ce.Log not restored")
	}
	s := env.SpanByName("cmd.my_cmd")
	if s == nil || s.Parent().IsValid() || s.SpanKind() != trace.SpanKindConsumer {
		t.Fatalf("span = %+v", s)
	}
	if attrs := s.Attributes(); len(attrs) != 1 || string(attrs[0].Key) != "cmd.name" || attrs[0].Value.AsString() != "my-cmd" {
		t.Errorf("attributes = %v, want only cmd.name", attrs)
	}
	if n := strings.Count(logBuf.String(), `"trace_id"`); n != 1 {
		t.Errorf("handler log has %d trace_id fields: %s", n, logBuf.String())
	}
	obstest.AssertNoSecrets(t, env.Rec, &logBuf, "1234#", "+15559876543")
}

func TestWithSpanDisabledIdentity(t *testing.T) {
	obs.Use(obs.Providers{})
	base := zerolog.Nop()
	orig := context.Background()
	ce := &commands.Event{Ctx: orig, Log: &base}
	WithSpan("x", func(ce *commands.Event) {
		if ce.Ctx != orig || ce.Log != &base {
			t.Error("disabled telemetry changed ctx or logger")
		}
	})(ce)
}
