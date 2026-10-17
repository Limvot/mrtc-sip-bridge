package rtc

import (
	"strings"

	"github.com/go-logr/logr"
	protoLogger "github.com/livekit/protocol/logger"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/rs/zerolog"

	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

// sink is a logr.LogSink that writes only the scrubbed message (never key/values) to zerolog.
// LiveKit's Warnw arrives as V(0) Info and Errorw as Error; Debugw is V(1) and dropped. Infow is also
// V(0) at the logr level, so quietLogger drops it before it reaches the sink.
type sink struct{ log zerolog.Logger }

func (s *sink) Init(logr.RuntimeInfo)          {}
func (s *sink) Enabled(level int) bool         { return level <= 0 }
func (s *sink) WithValues(...any) logr.LogSink { return s }
func (s *sink) WithName(string) logr.LogSink   { return s }

func (s *sink) Info(level int, msg string, _ ...any) {
	if level > 0 {
		return
	}
	if isExpectedICENoise(msg) {
		s.log.Debug().Msg(obs.Scrub(msg))
		return
	}
	s.log.Warn().Msg(obs.Scrub(msg))
}

// iceNoise lists pion warnings that are expected and harmless in containers without IPv6 routing:
// ICE gathers udp6 candidates that can never work (lksdk v2.18.1 has WithIPv6Only but no IPv4-only
// option, so they cannot be disabled), and the agent pings before any candidate pair exists.
var iceNoise = []string{
	"Failed to ping without candidate pairs",
	"failed to get server reflexive address udp6",
}

func isExpectedICENoise(msg string) bool {
	for _, n := range iceNoise {
		if strings.Contains(msg, n) {
			return true
		}
	}
	return false
}

func (s *sink) Error(err error, msg string, _ ...any) {
	e := s.log.Error()
	if err != nil {
		e = e.Str("error", obs.Scrub(err.Error()))
	}
	e.Msg(obs.Scrub(msg))
}

// quietLogger is a LiveKit logger that drops Infow (logr cannot tell it from Warnw).
type quietLogger struct{ protoLogger.LogRLogger }

func (quietLogger) Infow(string, ...any) {}

func (q quietLogger) WithValues(...any) protoLogger.Logger    { return q }
func (q quietLogger) WithName(string) protoLogger.Logger      { return q }
func (q quietLogger) WithComponent(string) protoLogger.Logger { return q }
func (q quietLogger) WithCallDepth(int) protoLogger.Logger    { return q }
func (q quietLogger) WithItemSampler() protoLogger.Logger     { return q }
func (q quietLogger) WithoutSampler() protoLogger.Logger      { return q }
func (q quietLogger) WithDeferredValues() (protoLogger.Logger, protoLogger.DeferredFieldResolver) {
	_, res := q.LogRLogger.WithDeferredValues()
	return q, res
}

func newLibraryLogger(log zerolog.Logger) protoLogger.Logger {
	l := log.With().Str("component", "livekit").Logger()
	return quietLogger{protoLogger.LogRLogger(logr.New(&sink{log: l}))}
}

func setLibraryLogger(log zerolog.Logger) { lksdk.SetLogger(newLibraryLogger(log)) }
