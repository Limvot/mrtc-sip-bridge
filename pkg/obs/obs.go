// Package obs is the core's single choke point for OpenTelemetry: tracer and meter access, span and
// log-correlation helpers, scrubbing and the instrument registry. It imports only the OTel API (never
// the SDK or exporters); the host installs providers with Use (TELEMETRY-SPEC §3). When nothing is
// installed every helper is a no-op that returns its input unchanged.
package obs

import (
	"context"
	"runtime/debug"
	"sync/atomic"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// scopeName is the instrumentation scope (tracer and meter) and this module's path.
const scopeName = "github.com/williamkray/mrtc-sip-bridge"

// Providers are installed by the host once at startup, before any login starts.
// A nil Tracer disables tracing; a nil Meter disables metrics. Never calling Use is valid (all off).
type Providers struct {
	Tracer trace.TracerProvider
	Meter  metric.MeterProvider
}

// state is the immutable snapshot swapped in by Use.
type state struct {
	tracer  trace.Tracer
	tracing bool
	metrics bool
	inst    *Instruments
}

var (
	cur atomic.Pointer[state]

	// noopSpan is a non-recording span returned by helpers when tracing is off.
	noopSpan = trace.SpanFromContext(context.Background())
)

func init() {
	cur.Store(&state{tracer: noop.NewTracerProvider().Tracer(scopeName), inst: newInstruments(nil)})
}

// Use installs p. Safe to call once; a second call replaces the providers (tests). It creates the
// tracer and (re)creates every instrument of the registry from p.Meter. Callbacks registered on the
// previous instruments (for example the registrations gauge) are not carried over.
func Use(p Providers) {
	s := &state{tracer: noop.NewTracerProvider().Tracer(scopeName)}
	if p.Tracer != nil {
		var opts []trace.TracerOption
		if v := moduleVersion(); v != "" {
			opts = append(opts, trace.WithInstrumentationVersion(v))
		}
		s.tracer = p.Tracer.Tracer(scopeName, opts...)
		s.tracing = true
	}
	s.metrics = p.Meter != nil
	s.inst = newInstruments(p.Meter)
	cur.Store(s)
}

// Enabled reports whether tracing is on.
func Enabled() bool { return cur.Load().tracing }

// MetricsEnabled reports whether metrics are on.
func MetricsEnabled() bool { return cur.Load().metrics }

// moduleVersion returns this module's version from the build info ("" when unknown or a devel build).
func moduleVersion() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	v := ""
	if bi.Main.Path == scopeName {
		v = bi.Main.Version
	} else {
		for _, d := range bi.Deps {
			if d.Path == scopeName {
				v = d.Version
				break
			}
		}
	}
	if v == "(devel)" {
		return ""
	}
	return v
}
