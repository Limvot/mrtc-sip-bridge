// Package telemetry is the host side of the bridge's OpenTelemetry support: it installs the SDK
// (providers, OTLP exporters, resource, error handler) from the standard OTEL_* environment and hands
// the providers to the core through obs.Use (TELEMETRY-SPEC §2, §3).
//
// Everything is off unless an OTLP endpoint is configured. When neither signal is enabled nothing is
// installed (no provider, goroutine, socket, transport wrapping or global registration).
package telemetry

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"

	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

// Options configures Init.
type Options struct {
	ServiceName    string         // default service.name; "" = "mrtc-sip-bridge" (DD_SERVICE / OTEL_* still override)
	ServiceVersion string         // br.Version
	CommitSHA      string         // git commit the binary was built from (empty if unknown)
	RepositoryURL  string         // source repository URL (empty if unknown)
	Log            zerolog.Logger // br.Log, used for the enable/warn lines and the OTel error handler
}

const (
	shutdownTimeout = 3 * time.Second
	errLogInterval  = 5 * time.Minute
)

var (
	mu           sync.Mutex
	tp           *sdktrace.TracerProvider
	mpv          *sdkmetric.MeterProvider
	initOnce     sync.Once
	shutdownOnce sync.Once
)

// Init configures tracing and metrics from the environment and calls obs.Use when at least one signal
// is enabled. It never fails the bridge: any problem results in one warning and that signal staying
// off. Only the first call has an effect.
func Init(ctx context.Context, o Options) {
	initOnce.Do(func() { initWith(ctx, o, osGetenv) })
}

func initWith(ctx context.Context, o Options, getenv func(string) string) {
	log := o.Log
	cfg := resolveConfig(getenv)
	for _, w := range cfg.warnings {
		log.Warn().Msg(w)
	}
	if !cfg.traces.enabled && !cfg.metrics.enabled {
		return
	}
	// Replace the SDK's default stderr logger before the exporters parse their env: its key/value
	// pairs can contain raw env values (e.g. a malformed OTEL_EXPORTER_OTLP_HEADERS with an API key).
	otel.SetLogger(logr.New(otelLogSink{log: log}))

	res, err := buildResource(ctx, o, getenv)
	if err != nil {
		log.Warn().Str("error", obs.Scrub(err.Error())).Msg("Problem building OpenTelemetry resource")
	}
	if res == nil {
		res = resource.Default()
	}

	var providers obs.Providers
	var tprov *sdktrace.TracerProvider
	var mprov *sdkmetric.MeterProvider

	if cfg.traces.enabled {
		var exp *otlptrace.Exporter
		var err error
		switch cfg.traces.protocol {
		case protoGRPC:
			exp, err = otlptracegrpc.New(ctx)
		default:
			exp, err = otlptracehttp.New(ctx)
		}
		if err != nil {
			log.Warn().Str("error", obs.Scrub(err.Error())).Msg("Failed to create OTLP trace exporter, tracing disabled")
		} else {
			tprov = sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithBatcher(exp))
			providers.Tracer = tprov
		}
	}
	if cfg.metrics.enabled {
		var exp sdkmetric.Exporter
		var err error
		sel := temporalitySelector(cfg.temporality)
		switch cfg.metrics.protocol {
		case protoGRPC:
			exp, err = otlpmetricgrpc.New(ctx, otlpmetricgrpc.WithTemporalitySelector(sel))
		default:
			exp, err = otlpmetrichttp.New(ctx, otlpmetrichttp.WithTemporalitySelector(sel))
		}
		if err != nil {
			log.Warn().Str("error", obs.Scrub(err.Error())).Msg("Failed to create OTLP metric exporter, metrics disabled")
		} else {
			// The periodic reader honors OTEL_METRIC_EXPORT_INTERVAL / _TIMEOUT.
			mprov = sdkmetric.NewMeterProvider(sdkmetric.WithResource(res),
				sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)))
			providers.Meter = mprov
		}
	}
	if tprov == nil && mprov == nil {
		return
	}

	otel.SetErrorHandler(newErrorHandler(log))
	if tprov != nil {
		otel.SetTracerProvider(tprov)
	}
	if mprov != nil {
		otel.SetMeterProvider(mprov)
	}
	mu.Lock()
	tp, mpv = tprov, mprov
	mu.Unlock()
	obs.Use(providers)

	if tprov != nil {
		sampler := getenv("OTEL_TRACES_SAMPLER")
		if sampler == "" {
			sampler = "default"
		}
		log.Info().
			Str("protocol", cfg.traces.protocol).
			Str("endpoint", safeEndpoint(cfg.traces.endpoint)).
			Str("sampler", sampler).
			Msg("OpenTelemetry tracing enabled")
	}
	if mprov != nil {
		interval := getenv("OTEL_METRIC_EXPORT_INTERVAL")
		if interval == "" {
			interval = "default"
		}
		log.Info().
			Str("protocol", cfg.metrics.protocol).
			Str("endpoint", safeEndpoint(cfg.metrics.endpoint)).
			Str("temporality", cfg.temporality).
			Str("interval_ms", interval).
			Msg("OpenTelemetry metrics enabled")
		if err := runtime.Start(runtime.WithMeterProvider(metric.MeterProvider(mprov))); err != nil {
			log.Warn().Str("error", obs.Scrub(err.Error())).Msg("Failed to start Go runtime metrics")
		}
	}
}

// buildResource builds the resource: defaults < DD_* < OTEL_* env. git.commit.sha and
// git.repository_url are what Datadog's source code integration reads from OTel resources.
func buildResource(ctx context.Context, o Options, getenv func(string) string) (*resource.Resource, error) {
	trim := func(k string) string { return strings.TrimSpace(getenv(k)) }
	name := o.ServiceName
	if name == "" {
		name = "mrtc-sip-bridge"
	}
	version := o.ServiceVersion
	commit, repo := o.CommitSHA, o.RepositoryURL
	if v := trim("DD_GIT_COMMIT_SHA"); v != "" {
		commit = v
	}
	if v := trim("DD_GIT_REPOSITORY_URL"); v != "" {
		repo = v
	}
	if v := trim("DD_SERVICE"); v != "" {
		name = v
	}
	if v := trim("DD_VERSION"); v != "" {
		version = v
	}
	attrs := []attribute.KeyValue{attribute.String("service.name", name)}
	if version != "" {
		attrs = append(attrs, attribute.String("service.version", version))
	}
	if v := trim("DD_ENV"); v != "" {
		attrs = append(attrs,
			attribute.String("deployment.environment.name", v),
			attribute.String("deployment.environment", v))
	}
	if commit != "" {
		attrs = append(attrs, attribute.String("git.commit.sha", commit))
	}
	if repo != "" {
		attrs = append(attrs, attribute.String("git.repository_url", repo))
	}
	return resource.New(ctx,
		resource.WithAttributes(attrs...),
		resource.WithTelemetrySDK(),
		resource.WithFromEnv(),
	)
}

// Shutdown flushes and stops both providers, bounded to 3 s in total. It is idempotent and a no-op
// when telemetry is off.
func Shutdown(ctx context.Context) {
	shutdownOnce.Do(func() {
		mu.Lock()
		t, m := tp, mpv
		mu.Unlock()
		if t == nil && m == nil {
			return
		}
		sctx, cancel := context.WithTimeout(ctx, shutdownTimeout)
		defer cancel()
		var wg sync.WaitGroup
		stop := func(what string, f func(context.Context) error) {
			defer wg.Done()
			if err := f(sctx); err != nil {
				zerolog.Ctx(ctx).Warn().Str("error", obs.Scrub(err.Error())).Msg("OpenTelemetry " + what + " shutdown error")
			}
		}
		if t != nil {
			wg.Add(1)
			go stop("trace", t.Shutdown)
		}
		if m != nil {
			wg.Add(1)
			go stop("metric", m.Shutdown)
		}
		wg.Wait()
	})
}

// newErrorHandler returns an OTel error handler that logs the first error and
// then at most one line per errLogInterval with a suppressed count.
func newErrorHandler(log zerolog.Logger) otel.ErrorHandler {
	var (
		mu         sync.Mutex
		last       time.Time
		suppressed int
	)
	return otel.ErrorHandlerFunc(func(err error) {
		if err == nil {
			return
		}
		mu.Lock()
		now := time.Now()
		if !last.IsZero() && now.Sub(last) < errLogInterval {
			suppressed++
			mu.Unlock()
			return
		}
		n := suppressed
		suppressed = 0
		last = now
		mu.Unlock()
		ev := log.Warn()
		if n > 0 {
			ev = ev.Int("suppressed", n)
		}
		ev.Str("error", obs.Scrub(fmt.Sprint(err))).Msg("OpenTelemetry export error")
	})
}

// otelLogSink routes the OTel SDK's internal error log to zerolog. Key/value pairs are dropped
// (they may carry header values or URLs); only the message and the scrubbed error are kept.
type otelLogSink struct{ log zerolog.Logger }

func (otelLogSink) Init(logr.RuntimeInfo)            {}
func (otelLogSink) Enabled(int) bool                 { return false }
func (otelLogSink) Info(int, string, ...any)         {}
func (s otelLogSink) WithValues(...any) logr.LogSink { return s }
func (s otelLogSink) WithName(string) logr.LogSink   { return s }
func (s otelLogSink) Error(err error, msg string, _ ...any) {
	ev := s.log.Warn()
	if err != nil {
		ev = ev.Str("error", obs.Scrub(err.Error()))
	}
	ev.Msg("OpenTelemetry: " + msg)
}
