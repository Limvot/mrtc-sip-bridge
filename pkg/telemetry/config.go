package telemetry

import (
	"net/url"
	"strconv"
	"strings"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const (
	protoHTTP = "http/protobuf"
	protoGRPC = "grpc"

	tempCumulative = "cumulative"
	tempDelta      = "delta"
	tempLowMemory  = "lowmemory"
)

// signalConfig is the resolved configuration of one signal (traces or metrics).
type signalConfig struct {
	enabled  bool
	protocol string
	endpoint string // effective endpoint as configured (may be empty when disabled)
}

// config is the result of resolving the environment.
type config struct {
	traces      signalConfig
	metrics     signalConfig
	temporality string
	warnings    []string
}

// resolveSignal resolves one signal. kind is "TRACES" or "METRICS", name the word used in warnings.
func resolveSignal(get func(string) string, kind, name string, warn func(string)) signalConfig {
	var c signalConfig
	if v := strings.ToLower(get("OTEL_" + kind + "_EXPORTER")); v != "" && v != "otlp" {
		if v != "none" {
			warn("unsupported OTEL_" + kind + "_EXPORTER=" + v + ", " + name + " disabled")
		}
		return c
	}
	ep := get("OTEL_EXPORTER_OTLP_" + kind + "_ENDPOINT")
	if ep == "" {
		ep = get("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if ep == "" {
		return c
	}
	c.endpoint = ep

	proto := get("OTEL_EXPORTER_OTLP_" + kind + "_PROTOCOL")
	if proto == "" {
		proto = get("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	if proto == "" {
		proto = protoHTTP
	}
	switch proto {
	case protoHTTP, protoGRPC:
		c.protocol = proto
	default:
		warn("unsupported OTLP protocol " + proto + ", " + name + " disabled")
		return c
	}
	c.enabled = true

	if u, err := url.Parse(ep); err == nil {
		switch {
		case c.protocol == protoHTTP && u.Port() == "4317":
			warn(name + ": endpoint port looks like grpc; set OTEL_EXPORTER_OTLP_PROTOCOL")
		case c.protocol == protoGRPC && u.Port() == "4318":
			warn(name + ": endpoint port looks like http/protobuf; set OTEL_EXPORTER_OTLP_PROTOCOL")
		}
	}
	return c
}

// resolveConfig decides, from the environment only, which signals are enabled and how. It does no I/O.
func resolveConfig(getenv func(string) string) config {
	var c config
	get := func(k string) string { return strings.TrimSpace(getenv(k)) }
	warn := func(s string) { c.warnings = append(c.warnings, s) }
	c.temporality = tempCumulative

	if v := get("OTEL_SDK_DISABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			warn("unparseable OTEL_SDK_DISABLED, treating as false")
		} else if b {
			return c
		}
	}
	c.traces = resolveSignal(get, "TRACES", "tracing", warn)
	c.metrics = resolveSignal(get, "METRICS", "metrics", warn)

	if v := strings.ToLower(get("OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE")); v != "" {
		switch v {
		case tempCumulative, tempDelta, tempLowMemory:
			c.temporality = v
		default:
			warn("unsupported OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=" + v + ", using cumulative")
		}
	}
	return c
}

// temporalitySelector maps the preference to an explicit selector so behaviour does not depend on
// the exporter version's own env parsing.
func temporalitySelector(pref string) sdkmetric.TemporalitySelector {
	switch pref {
	case tempDelta:
		return sdkmetric.DeltaTemporalitySelector
	case tempLowMemory:
		return func(k sdkmetric.InstrumentKind) metricdata.Temporality {
			switch k {
			case sdkmetric.InstrumentKindCounter, sdkmetric.InstrumentKindHistogram:
				return metricdata.DeltaTemporality
			}
			return metricdata.CumulativeTemporality
		}
	}
	return sdkmetric.CumulativeTemporalitySelector
}

// safeEndpoint reduces an endpoint to scheme://host:port.
func safeEndpoint(ep string) string {
	u, err := url.Parse(ep)
	if err != nil || u.Host == "" {
		return "unknown"
	}
	return u.Scheme + "://" + u.Host
}
