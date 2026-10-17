// Command mrtc-sip-bridge is the standalone Matrix-SIP calling bridge.
package main

import (
	"context"

	"go.opentelemetry.io/otel/trace"
	"maunium.net/go/mautrix/bridgev2/commands"
	"maunium.net/go/mautrix/bridgev2/matrix/mxmain"

	"github.com/williamkray/mrtc-sip-bridge/pkg/connector"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
	"github.com/williamkray/mrtc-sip-bridge/pkg/telemetry"
)

// Filled at build time with -X (see Dockerfile / build-args.sh).
var (
	Tag           = "unknown"
	Commit        = "unknown"
	BuildTime     = "unknown"
	RepositoryURL = "unknown"
)

// knownOrEmpty maps the "unknown" linker-flag default to "".
func knownOrEmpty(s string) string {
	if s == "unknown" {
		return ""
	}
	return s
}

func main() {
	c := &connector.SIPConnector{}
	br := &mxmain.BridgeMain{
		Name:        "mrtc-sip-bridge",
		Description: "A Matrix-SIP calling bridge using MatrixRTC (Element Call / LiveKit).",
		URL:         "https://github.com/williamkray/mrtc-sip-bridge",
		Version:     "0.1.0",
		Connector:   c,
	}
	encryptionForcedOff := false
	br.PostInit = func() {
		// Telemetry is configured only through OTEL_* env vars; nothing is installed unless an OTLP
		// endpoint is set. Init calls obs.Use itself and never fails the bridge.
		telemetry.Init(context.Background(), telemetry.Options{
			ServiceVersion: br.Version,
			CommitSHA:      knownOrEmpty(Commit),
			RepositoryURL:  knownOrEmpty(RepositoryURL),
			Log:            *br.Log,
		})
		if obs.Enabled() || obs.MetricsEnabled() {
			hc := br.Matrix.AS.HTTPClient
			hc.Transport = telemetry.WrapMatrixTransport(hc.Transport)
		}
		if br.Config.Encryption.Default {
			br.Log.Warn().Msg("encryption.default is true, but calls need unencrypted rooms; overriding to false")
			br.Config.Encryption.Default = false
			encryptionForcedOff = true
		}
		c.RegisterMatrixRTCHandlers(br.Matrix.EventProcessor)
		c.RegisterCommands(br.Bridge.Commands.(*commands.Processor))
	}
	br.PostStart = func() {
		_, span := obs.Start(context.Background(), "bridge.start",
			trace.WithAttributes(c.StartAttrs(br.Version, encryptionForcedOff)...))
		span.End()
	}
	br.InitVersion(Tag, Commit, BuildTime)
	br.Run()
}
