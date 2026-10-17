package connector

import (
	"context"
	"net"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"maunium.net/go/mautrix/bridgev2"

	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
	"github.com/williamkray/mrtc-sip-bridge/pkg/telemetry"
)

var _ bridgev2.StoppableNetwork = (*SIPConnector)(nil)

// Stop flushes telemetry. The framework calls it after all clients disconnected, so the unregister
// spans are included.
func (c *SIPConnector) Stop() {
	c.stopWidget()
	c.rt.Close()
	ctx := context.Background()
	if c.br != nil {
		ctx = c.br.Log.WithContext(ctx)
	}
	if c.Calls != nil {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		c.Calls.Stop(cctx)
		cancel()
	}
	telemetry.Shutdown(ctx)
}

// logFor returns the client's clean logger enriched with the trace fields of the span in ctx.
func (cl *SIPClient) logFor(ctx context.Context) *zerolog.Logger {
	cl.mu.Lock()
	l := cl.log
	cl.mu.Unlock()
	if l == nil {
		nop := zerolog.Nop()
		l = &nop
	}
	return obs.Logger(ctx, l)
}

// spanAttrs are the account attributes of connect/disconnect/logout spans (never the raw username).
func (cl *SIPClient) spanAttrs() []attribute.KeyValue {
	return accountAttrs(cl.meta)
}

func accountAttrs(meta *UserLoginMetadata) []attribute.KeyValue {
	host, _, err := net.SplitHostPort(meta.Registrar)
	if err != nil {
		host = meta.Registrar
	}
	transport := meta.Transport
	if transport == "" {
		transport = string(sipua.TransportUDP)
	}
	return []attribute.KeyValue{
		obs.AttrSIPUserMasked.String(sipua.MaskUsername(meta.Username)),
		obs.AttrServerAddress.String(host),
		obs.AttrNetworkTransport.String(transport),
	}
}

// StartAttrs are the attributes of the zero-length bridge.start span emitted by PostStart.
func (c *SIPConnector) StartAttrs(version string, encryptionForcedOff bool) []attribute.KeyValue {
	cfg := &c.Config
	lkSource := ""
	if c.lkURL != nil {
		lkSource = c.lkURL.Source()
	}
	return []attribute.KeyValue{
		obs.AttrBridgeVersion.String(version),
		attribute.Bool("stun.enabled", cfg.STUNServer != ""),
		attribute.Bool("external_ip.set", cfg.ExternalIP != ""),
		attribute.Int("ring_timeout_s", cfg.RingTimeoutSeconds),
		attribute.String("livekit.service_url.source", lkSource),
		attribute.Int("rtp.port_range_size", cfg.RTPPortMax-cfg.RTPPortMin+1),
		attribute.Int("sip.port_range_size", cfg.SIPPortMax-cfg.SIPPortMin+1),
		attribute.Bool("encryption.forced_off", encryptionForcedOff),
	}
}
