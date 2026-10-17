package sipaccount

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/metric"

	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

// NetConfig is the process-wide network part of the host config.
type NetConfig struct {
	// KeyPrefix is the config key path used in error messages ("" means "network.").
	KeyPrefix              string
	SIPBindIP              string
	SIPPortMin, SIPPortMax int
	ExternalIP, STUNServer string
	RTPPortMin, RTPPortMax int
	RegisterExpiry         time.Duration
	RegisterRefresh        time.Duration
}

// Runtime is the process-wide SIP state. Exactly one per process: NewRuntime sets package-level state in
// sipua (RTP port range, library logger).
type Runtime struct {
	cfg        NetConfig
	bindIP     net.IP
	externalIP net.IP
	ports      *portPool
	plain      zerolog.Logger // the logger passed to NewRuntime (no component key)

	// UAs read by the registrations gauge and its callback handle.
	uaMu     sync.Mutex
	uas      map[sipua.UA]struct{}
	gaugeReg metric.Registration
}

// NewRuntime resolves the bind IP (DefaultBindIP when empty or 0.0.0.0) and the external IP
// (ExternalIP, else STUN with a 5 s timeout, else the bind IP; a STUN failure is a warning), calls
// sipua.SetLibraryLogger(log) and sipua.SetRTPPortRange, creates the SIP port pool and registers the
// mrtcsip.sip.registrations gauge callback. Must be called after obs.Use (Use rebuilds instruments and
// drops earlier callbacks). log is a plain logger (sipua adds component=sipgo; Runtime logs with
// component=sip).
func NewRuntime(ctx context.Context, cfg NetConfig, log zerolog.Logger) (*Runtime, error) {
	keyPrefix := cfg.KeyPrefix
	if keyPrefix == "" {
		keyPrefix = "network."
	}
	slog := log.With().Str("component", "sip").Logger()

	// sipgo's package-level logger would otherwise write to stderr (including raw SIP on parse failures).
	// Library logs are capped at Warn (D21).
	sipua.SetLibraryLogger(log) // plain logger: it adds component=sipgo itself

	rt := &Runtime{cfg: cfg, plain: log}
	if cfg.SIPBindIP == "" || cfg.SIPBindIP == "0.0.0.0" {
		ip, err := sipua.DefaultBindIP()
		if err != nil {
			return nil, fmt.Errorf("%ssip_bind_ip: %w", keyPrefix, err)
		}
		rt.bindIP = ip
	} else {
		rt.bindIP = net.ParseIP(cfg.SIPBindIP)
		if rt.bindIP == nil {
			return nil, fmt.Errorf("%ssip_bind_ip: %q is not a valid IP address", keyPrefix, cfg.SIPBindIP)
		}
	}

	switch {
	case cfg.ExternalIP != "":
		rt.externalIP = net.ParseIP(cfg.ExternalIP)
		if rt.externalIP == nil {
			return nil, fmt.Errorf("%sexternal_ip: %q is not a valid IP address", keyPrefix, cfg.ExternalIP)
		}
	case cfg.STUNServer != "":
		stunCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		ip, err := sipua.DiscoverPublicIP(stunCtx, cfg.STUNServer)
		cancel()
		if err != nil {
			slog.Warn().Err(err).Msg("STUN failed; using bind IP as external IP (set network.external_ip)")
			rt.externalIP = rt.bindIP
		} else {
			rt.externalIP = ip
		}
	default:
		slog.Info().Msg("no external_ip or stun_server configured; using bind IP as external IP")
		rt.externalIP = rt.bindIP
	}

	// The host already ran obs.Use (telemetry.Init), so the instruments exist now.
	rt.registerMetrics()

	sipua.SetRTPPortRange(cfg.RTPPortMin, cfg.RTPPortMax)
	rt.ports = newPortPool(cfg.SIPPortMin, cfg.SIPPortMax)

	slog.Info().
		Str("bind_ip", rt.bindIP.String()).
		Str("external_ip", rt.externalIP.String()).
		Str("sip_ports", fmt.Sprintf("%d-%d", cfg.SIPPortMin, cfg.SIPPortMax)).
		Str("rtp_ports", fmt.Sprintf("%d-%d", cfg.RTPPortMin, cfg.RTPPortMax)).
		Msg("SIP connector started")
	return rt, nil
}

// BindIP returns the local IP the SIP UAs bind to.
func (rt *Runtime) BindIP() net.IP { return rt.bindIP }

// ExternalIP returns the IP advertised in Contact/Via and SDP.
func (rt *Runtime) ExternalIP() net.IP { return rt.externalIP }

// Close unregisters the gauge callback. Idempotent (and nil-safe). Accounts must be stopped first.
func (rt *Runtime) Close() {
	if rt == nil {
		return
	}
	rt.unregisterMetrics()
}

// trackUA adds ua to the set read by the registrations gauge.
func (rt *Runtime) trackUA(ua sipua.UA) {
	rt.uaMu.Lock()
	defer rt.uaMu.Unlock()
	if rt.uas == nil {
		rt.uas = map[sipua.UA]struct{}{}
	}
	rt.uas[ua] = struct{}{}
}

func (rt *Runtime) untrackUA(ua sipua.UA) {
	rt.uaMu.Lock()
	delete(rt.uas, ua)
	rt.uaMu.Unlock()
}

var registrationStates = []sipua.State{
	sipua.StateRegistering, sipua.StateRegistered, sipua.StateAuthFailed, sipua.StateTransientError,
}

// registerMetrics registers the mrtcsip.sip.registrations gauge callback on the current instruments.
// obs.Use must be called before NewRuntime: Use rebuilds the instruments, which drops any callback
// registered earlier. The callback reads in-memory UA state only.
func (rt *Runtime) registerMetrics() {
	rt.unregisterMetrics()
	m := obs.M()
	reg, err := m.Meter.RegisterCallback(func(_ context.Context, o metric.Observer) error {
		counts := make(map[sipua.State]int64, len(registrationStates))
		rt.uaMu.Lock()
		for ua := range rt.uas {
			counts[ua.State().State]++
		}
		rt.uaMu.Unlock()
		for _, st := range registrationStates {
			o.ObserveInt64(m.Registrations, counts[st], metric.WithAttributes(obs.AttrState.String(st.String())))
		}
		return nil
	}, m.Registrations)
	if err != nil {
		rt.plain.Warn().Msg("Failed to register the SIP registrations gauge callback")
		return
	}
	rt.uaMu.Lock()
	rt.gaugeReg = reg
	rt.uaMu.Unlock()
}

func (rt *Runtime) unregisterMetrics() {
	rt.uaMu.Lock()
	reg := rt.gaugeReg
	rt.gaugeReg = nil
	rt.uaMu.Unlock()
	if reg != nil {
		_ = reg.Unregister()
	}
}
