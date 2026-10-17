package telemetry

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/williamkray/mrtc-sip-bridge/internal/obstest"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

func resetForTest() {
	obs.Use(obs.Providers{})
	mu.Lock()
	tp, mpv = nil, nil
	mu.Unlock()
	shutdownOnce = sync.Once{}
	initOnce = sync.Once{}
	// Restore the otel globals Init may have replaced so tests do not leak state.
	otel.SetLogger(logr.Discard())
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) { log.Print(err) }))
	otel.SetTracerProvider(tracenoop.NewTracerProvider())
	otel.SetMeterProvider(metricnoop.NewMeterProvider())
}

func env(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestResolveConfig(t *testing.T) {
	const ep = "OTEL_EXPORTER_OTLP_ENDPOINT"
	tests := []struct {
		name         string
		env          map[string]string
		traces, mets bool
		tProto       string
		mProto       string
		temp         string
		warn         string
	}{
		{"nothing", nil, false, false, "", "", tempCumulative, ""},
		{"endpoint enables both", map[string]string{ep: "http://c:4318"}, true, true, protoHTTP, protoHTTP, tempCumulative, ""},
		{"traces endpoint only", map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://c:4318/v1/traces"}, true, false, protoHTTP, "", tempCumulative, ""},
		{"metrics endpoint only", map[string]string{"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT": "http://c:4318/v1/metrics"}, false, true, "", protoHTTP, tempCumulative, ""},
		{"sdk disabled", map[string]string{ep: "http://c:4318", "OTEL_SDK_DISABLED": "true"}, false, false, "", "", tempCumulative, ""},
		{"sdk disabled garbage", map[string]string{ep: "http://c:4318", "OTEL_SDK_DISABLED": "zzz"}, true, true, protoHTTP, protoHTTP, tempCumulative, "OTEL_SDK_DISABLED"},
		{"traces none", map[string]string{ep: "http://c:4318", "OTEL_TRACES_EXPORTER": "none"}, false, true, "", protoHTTP, tempCumulative, ""},
		{"metrics none", map[string]string{ep: "http://c:4318", "OTEL_METRICS_EXPORTER": "none"}, true, false, protoHTTP, "", tempCumulative, ""},
		{"metrics prometheus", map[string]string{ep: "http://c:4318", "OTEL_METRICS_EXPORTER": "prometheus"}, true, false, protoHTTP, "", tempCumulative, "unsupported OTEL_METRICS_EXPORTER=prometheus"},
		{"traces console", map[string]string{ep: "http://c:4318", "OTEL_TRACES_EXPORTER": "console"}, false, true, "", protoHTTP, tempCumulative, "unsupported OTEL_TRACES_EXPORTER=console"},
		{"exporter otlp no endpoint", map[string]string{"OTEL_TRACES_EXPORTER": "otlp", "OTEL_METRICS_EXPORTER": "otlp"}, false, false, "", "", tempCumulative, ""},
		{"grpc", map[string]string{ep: "http://c:4317", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}, true, true, protoGRPC, protoGRPC, tempCumulative, ""},
		{"http/json", map[string]string{ep: "http://c:4318", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/json"}, false, false, "", "", tempCumulative, "unsupported OTLP protocol"},
		{"per-signal protocol", map[string]string{ep: "http://c:4317", "OTEL_EXPORTER_OTLP_PROTOCOL": "http/protobuf", "OTEL_EXPORTER_OTLP_TRACES_PROTOCOL": "grpc"}, true, true, protoGRPC, protoHTTP, tempCumulative, "metrics: endpoint port looks like grpc"},
		{"metrics bad proto only", map[string]string{ep: "http://c:4318", "OTEL_EXPORTER_OTLP_METRICS_PROTOCOL": "http/json"}, true, false, protoHTTP, "", tempCumulative, "metrics disabled"},
		{"port hint", map[string]string{ep: "http://c:4317"}, true, true, protoHTTP, protoHTTP, tempCumulative, "looks like grpc"},
		{"port hint grpc", map[string]string{ep: "http://c:4318", "OTEL_EXPORTER_OTLP_PROTOCOL": "grpc"}, true, true, protoGRPC, protoGRPC, tempCumulative, "looks like http"},
		{"delta", map[string]string{ep: "http://c:4318", "OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE": "Delta"}, true, true, protoHTTP, protoHTTP, tempDelta, ""},
		{"lowmemory", map[string]string{ep: "http://c:4318", "OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE": "LOWMEMORY"}, true, true, protoHTTP, protoHTTP, tempLowMemory, ""},
		{"bad temporality", map[string]string{ep: "http://c:4318", "OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE": "x"}, true, true, protoHTTP, protoHTTP, tempCumulative, "TEMPORALITY_PREFERENCE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := resolveConfig(env(tt.env))
			if c.traces.enabled != tt.traces || c.metrics.enabled != tt.mets ||
				c.traces.protocol != tt.tProto || c.metrics.protocol != tt.mProto || c.temporality != tt.temp {
				t.Fatalf("got %+v", c)
			}
			joined := strings.Join(c.warnings, "|")
			if tt.warn == "" && joined != "" {
				t.Fatalf("unexpected warnings: %s", joined)
			}
			if tt.warn != "" && !strings.Contains(joined, tt.warn) {
				t.Fatalf("warnings %q lack %q", joined, tt.warn)
			}
		})
	}
}

func TestTemporalitySelector(t *testing.T) {
	kinds := []sdkmetric.InstrumentKind{
		sdkmetric.InstrumentKindCounter, sdkmetric.InstrumentKindHistogram, sdkmetric.InstrumentKindGauge,
		sdkmetric.InstrumentKindUpDownCounter, sdkmetric.InstrumentKindObservableCounter,
		sdkmetric.InstrumentKindObservableUpDownCounter, sdkmetric.InstrumentKindObservableGauge,
	}
	d, c := metricdata.DeltaTemporality, metricdata.CumulativeTemporality
	want := map[string][]metricdata.Temporality{
		tempCumulative: {c, c, c, c, c, c, c},
		tempDelta:      {d, d, c, c, d, c, c},
		tempLowMemory:  {d, d, c, c, c, c, c},
	}
	for pref, w := range want {
		sel := temporalitySelector(pref)
		for i, k := range kinds {
			if got := sel(k); got != w[i] {
				t.Errorf("%s kind %v: got %v want %v", pref, k, got, w[i])
			}
		}
	}
}

func TestDisabledIdentity(t *testing.T) {
	resetForTest()
	t.Cleanup(resetForTest)
	for _, k := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
	} {
		t.Setenv(k, "")
	}
	before := runtime.NumGoroutine()
	Init(context.Background(), Options{Log: zerolog.Nop()})
	if obs.Enabled() || obs.MetricsEnabled() {
		t.Fatal("must be disabled without env")
	}
	if n := runtime.NumGoroutine(); n != before {
		t.Fatalf("goroutines changed %d -> %d", before, n)
	}
	if tp != nil || mpv != nil {
		t.Fatal("providers installed")
	}
	ctx := context.WithValue(context.Background(), struct{}{}, 1)
	if c, s := obs.Start(ctx, "x"); c != ctx || s.IsRecording() {
		t.Error("obs.Start not identity")
	}
	Shutdown(context.Background())
	base := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: http.NoBody}, nil
	})
	req, _ := http.NewRequest("GET", "http://x/", nil)
	if _, err := WrapMatrixTransport(base).RoundTrip(req); err != nil || req.Header.Get("traceparent") != "" {
		t.Error("transport should pass through")
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestResourcePrecedence(t *testing.T) {
	get := func(m map[string]string) map[string]string {
		t.Setenv("OTEL_SERVICE_NAME", m["OTEL_SERVICE_NAME"])
		t.Setenv("OTEL_RESOURCE_ATTRIBUTES", m["OTEL_RESOURCE_ATTRIBUTES"])
		res, err := buildResource(context.Background(), Options{ServiceVersion: "1.0", CommitSHA: "abc123", RepositoryURL: "https://example.com/repo"}, env(m))
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, kv := range res.Attributes() {
			out[string(kv.Key)] = kv.Value.Emit()
		}
		return out
	}
	a := get(map[string]string{})
	if a["service.name"] != "mrtc-sip-bridge" || a["service.version"] != "1.0" ||
		a["git.commit.sha"] != "abc123" || a["git.repository_url"] != "https://example.com/repo" {
		t.Errorf("defaults: %v", a)
	}
	a = get(map[string]string{"DD_GIT_COMMIT_SHA": "def456", "DD_GIT_REPOSITORY_URL": "https://example.com/fork"})
	if a["git.commit.sha"] != "def456" || a["git.repository_url"] != "https://example.com/fork" {
		t.Errorf("dd git: %v", a)
	}
	a = get(map[string]string{"DD_SERVICE": "dd", "DD_VERSION": "2", "DD_ENV": "prod"})
	if a["service.name"] != "dd" || a["service.version"] != "2" ||
		a["deployment.environment.name"] != "prod" || a["deployment.environment"] != "prod" {
		t.Errorf("dd: %v", a)
	}
	a = get(map[string]string{"DD_SERVICE": "dd", "OTEL_SERVICE_NAME": "otel", "OTEL_RESOURCE_ATTRIBUTES": "service.version=9"})
	if a["service.name"] != "otel" || a["service.version"] != "9" {
		t.Errorf("otel: %v", a)
	}
}

func TestResourceServiceName(t *testing.T) {
	get := func(o Options, m map[string]string) string {
		t.Setenv("OTEL_SERVICE_NAME", m["OTEL_SERVICE_NAME"])
		t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
		res, err := buildResource(context.Background(), o, env(m))
		if err != nil {
			t.Fatal(err)
		}
		for _, kv := range res.Attributes() {
			if kv.Key == "service.name" {
				return kv.Value.Emit()
			}
		}
		return ""
	}
	if got := get(Options{}, nil); got != "mrtc-sip-bridge" {
		t.Errorf("default = %q", got)
	}
	o := Options{ServiceName: "mautrix-jabber"}
	if got := get(o, nil); got != "mautrix-jabber" {
		t.Errorf("ServiceName = %q", got)
	}
	if got := get(o, map[string]string{"DD_SERVICE": "dd"}); got != "dd" {
		t.Errorf("DD_SERVICE must win over ServiceName: %q", got)
	}
	if got := get(o, map[string]string{"DD_SERVICE": "dd", "OTEL_SERVICE_NAME": "otel"}); got != "otel" {
		t.Errorf("OTEL_SERVICE_NAME must win: %q", got)
	}
}

func TestURLTemplate(t *testing.T) {
	for in, want := range map[string]string{
		"/_matrix/client/v3/rooms/!abc:hs/send/m.room.message/mautrix-go_123": "/_matrix/client/v3/rooms/*/send/*/*",
		"/_matrix/client/v3/profile/@jabber_x=40y:hs/displayname":             "/_matrix/client/v3/profile/*/displayname",
		"/_matrix/client/v1/media/download/hs.example/AbCd":                   "/_matrix/client/v1/media/download/*/*",
	} {
		if got := urlTemplate(in); got != want {
			t.Errorf("%s: got %s want %s", in, got, want)
		}
	}
}

func TestTransport(t *testing.T) {
	resetForTest()
	rec := tracetest.NewSpanRecorder()
	reader := sdkmetric.NewManualReader()
	obs.Use(obs.Providers{
		Tracer: sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)),
		Meter:  sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)),
	})
	t.Cleanup(resetForTest)
	var gotTP string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTP = r.Header.Get("traceparent")
		w.WriteHeader(404)
	}))
	defer srv.Close()
	rt := WrapMatrixTransport(nil)

	req, _ := http.NewRequest("GET", srv.URL+"/_matrix/client/v3/rooms/!a:b/send/m.x/t1?q=secret", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if gotTP != "" || len(rec.Ended()) != 0 {
		t.Fatal("untraced request must be untouched")
	}

	ctx, root := obs.Start(context.Background(), "root")
	req, _ = http.NewRequestWithContext(ctx, "GET", srv.URL+"/_matrix/client/v3/rooms/!a:b/send/m.x/t1?q=secret", nil)
	resp, err = rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	root.End()
	if !strings.Contains(gotTP, root.SpanContext().TraceID().String()) {
		t.Errorf("traceparent %q", gotTP)
	}
	var found bool
	for _, s := range rec.Ended() {
		if s.Name() == "GET /_matrix/client/v3/rooms/*/send/*/*" {
			found = true
			for _, kv := range s.Attributes() {
				if strings.Contains(kv.Value.Emit(), "secret") {
					t.Error("query leaked")
				}
			}
		}
	}
	if !found {
		t.Errorf("transport span missing: %v", rec.Ended())
	}
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	var seen bool
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == "http.client.request.duration" {
				seen = true
			}
		}
	}
	if !seen {
		t.Error("http.client.request.duration not recorded")
	}
}

// TestInitUnreachableShutdown covers T6 acceptance 3: unreachable endpoint, Init succeeds,
// shutdown bounded, for both protocols.
func TestInitUnreachableShutdown(t *testing.T) {
	for _, proto := range []string{protoHTTP, protoGRPC} {
		t.Run(proto, func(t *testing.T) {
			resetForTest()
			t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://127.0.0.1:1")
			t.Setenv("OTEL_EXPORTER_OTLP_PROTOCOL", proto)
			t.Cleanup(resetForTest)
			start := time.Now()
			initWith(context.Background(), Options{Log: zerolog.Nop()}, osGetenv)
			if d := time.Since(start); d > time.Second {
				t.Fatalf("init took %s", d)
			}
			if !obs.Enabled() || !obs.MetricsEnabled() {
				t.Fatal("expected both signals enabled")
			}
			_, s := obs.Start(context.Background(), "x")
			s.End()
			obs.M().Logins.Add(context.Background(), 1)
			start = time.Now()
			Shutdown(context.Background())
			Shutdown(context.Background())
			if d := time.Since(start); d > 3500*time.Millisecond {
				t.Fatalf("shutdown took %s", d)
			}
		})
	}
}

func TestInitMetricsOnlyHTTP(t *testing.T) {
	resetForTest()
	var hits atomic.Int32
	var path atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		path.Store(r.URL.Path)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", srv.URL+"/v1/metrics")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE", "delta")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Cleanup(resetForTest)
	initWith(context.Background(), Options{Log: zerolog.Nop()}, osGetenv)
	if obs.Enabled() || !obs.MetricsEnabled() {
		t.Fatalf("want metrics only: traces=%v metrics=%v", obs.Enabled(), obs.MetricsEnabled())
	}
	obs.M().Logins.Add(context.Background(), 1)
	Shutdown(context.Background()) // final flush
	if hits.Load() == 0 || path.Load() != "/v1/metrics" {
		t.Fatalf("collector not hit as expected: hits=%d path=%v", hits.Load(), path.Load())
	}
}

func TestOTelLogSinkDropsValues(t *testing.T) {
	var buf strings.Builder
	l := logr.New(otelLogSink{log: zerolog.New(&buf)})
	l.Error(errors.New("missing '="), "parse headers", "input", "DD-API-KEY secretvalue")
	if strings.Contains(buf.String(), "secretvalue") || !strings.Contains(buf.String(), "parse headers") {
		t.Errorf("unexpected log: %s", buf.String())
	}
}

func TestErrorHandlerRateLimitAndScrub(t *testing.T) {
	var buf strings.Builder
	h := newErrorHandler(zerolog.New(&buf))
	h.Handle(errors.New("post https://host.example/v1/traces?k=secret failed"))
	h.Handle(errors.New("again"))
	out := buf.String()
	if strings.Count(out, "export error") != 1 || strings.Contains(out, "secret") {
		t.Errorf("bad output: %s", out)
	}
}

func TestWrapMatrixTransportMetricAttrsAllowed(t *testing.T) {
	env := obstest.Setup(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()
	client := &http.Client{Transport: WrapMatrixTransport(nil)}
	path := "/_matrix/client/v3/rooms/!abc:example.org/state/m.room.member/@u:example.org"
	req, err := http.NewRequest(http.MethodPut, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(env.MetricPoints(t, "http.client.request.duration")) == 0 {
		t.Fatal("no http.client.request.duration points recorded")
	}
	obstest.AssertMetricAttrsAllowed(t, env.Reader, nil)
}
