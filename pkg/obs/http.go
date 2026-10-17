package obs

import (
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func serverPort(req *http.Request) int {
	if p := req.URL.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			return n
		}
	}
	if req.URL.Scheme == "https" {
		return 443
	}
	return 80
}

// HTTPAttrs returns server.address, server.port and http.request.method for
// req. The URL is never recorded.
func HTTPAttrs(req *http.Request) []attribute.KeyValue {
	return []attribute.KeyValue{
		AttrServerAddress.String(req.URL.Hostname()),
		AttrServerPort.Int(serverPort(req)),
		AttrHTTPRequestMethod.String(req.Method),
	}
}

// anyTemplate is the templater used when WrapHTTPTransport gets a nil one.
func anyTemplate(string) string { return "*" }

type httpTransport struct {
	base http.RoundTripper
	tmpl func(string) string
}

// WrapHTTPTransport wraps base so requests made with a traced context get a client span and a
// traceparent header, and (when metrics are on) every request records http.client.request.duration.
// tmpl reduces a request path to a bounded template (ids become "*"); nil maps every path to "*".
// A nil base means http.DefaultTransport.
func WrapHTTPTransport(base http.RoundTripper, tmpl func(path string) string) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if tmpl == nil {
		tmpl = anyTemplate
	}
	return &httpTransport{base: base, tmpl: tmpl}
}

func (t *httpTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	traced := Enabled() && trace.SpanContextFromContext(req.Context()).IsValid()
	timed := MetricsEnabled()
	if !traced && !timed {
		return t.base.RoundTrip(req)
	}
	tmpl := t.tmpl(req.URL.Path)
	start := time.Now()
	var span trace.Span = noopSpan
	if traced {
		var ctx = req.Context()
		ctx, span = cur.Load().tracer.Start(ctx, req.Method+" "+tmpl, trace.WithSpanKind(trace.SpanKindClient))
		defer span.End()
		if span.IsRecording() {
			span.SetAttributes(HTTPAttrs(req)...)
			span.SetAttributes(AttrURLTemplate.String(tmpl))
		}
		req = req.Clone(ctx)
		propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(req.Header))
	}
	resp, err := t.base.RoundTrip(req)
	var errType string
	if err != nil {
		Fail(span, err)
		errType = errTypeOf(err)
	} else {
		span.SetAttributes(AttrHTTPResponseStatus.Int(resp.StatusCode))
		if resp.StatusCode >= 400 {
			errType = strconv.Itoa(resp.StatusCode)
			span.SetAttributes(AttrErrorType.String(errType))
		}
		if resp.StatusCode >= 500 {
			span.SetStatus(codes.Error, "")
		}
	}
	if timed {
		// server.port is span-only (not in the TELEMETRY-SPEC §4.3 metric attribute list).
		attrs := []attribute.KeyValue{
			AttrHTTPRequestMethod.String(req.Method),
			AttrServerAddress.String(req.URL.Hostname()),
			AttrURLTemplate.String(tmpl),
		}
		if err == nil {
			attrs = append(attrs, AttrHTTPResponseStatus.Int(resp.StatusCode))
		}
		if errType != "" {
			attrs = append(attrs, AttrErrorType.String(errType))
		}
		M().HTTPClientDuration.Record(req.Context(), time.Since(start).Seconds(), metric.WithAttributes(attrs...))
	}
	return resp, err
}
