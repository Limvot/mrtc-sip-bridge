package hostkit

import (
	"context"
	"strings"

	"go.opentelemetry.io/otel/trace"
	"maunium.net/go/mautrix/bridgev2/commands"

	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

// WithSpan wraps a bot command handler in a root CONSUMER span named "cmd."+name (with "-" replaced by
// "_"). cmd.name is the only attribute: command arguments are never recorded. ce.Ctx and ce.Log are
// swapped for the handler's duration so framework and handler log lines carry the trace fields.
func WithSpan(name string, fn func(*commands.Event)) func(*commands.Event) {
	return func(ce *commands.Event) {
		orig := ce.Ctx
		base := orig
		if base == nil {
			base = context.Background()
		}
		ctx, span := obs.Start(base, "cmd."+strings.ReplaceAll(name, "-", "_"),
			trace.WithSpanKind(trace.SpanKindConsumer),
			trace.WithAttributes(obs.AttrCmdName.String(name)))
		defer span.End()
		ce.Ctx = ctx
		defer func() { ce.Ctx = orig }()
		if ce.Log != nil {
			origLog := ce.Log
			ce.Log = obs.Logger(ctx, ce.Log)
			defer func() { ce.Log = origLog }()
		}
		fn(ce)
	}
}
