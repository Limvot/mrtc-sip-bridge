package widget

import (
	"context"
	"sync"
	"sync/atomic"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

// Span names (TELEMETRY-SPEC §5.12). GET state, POST normalize and static files get no spans.
const (
	spanSession = "widget.session"       // SERVER, root
	spanCall    = "widget.call"          // SERVER, root
	spanDTMF    = "widget.dtmf"          // SERVER, root
	spanHangup  = "widget.hangup"        // SERVER, root
	spanVerify  = "widget.openid.verify" // CLIENT, child-only under widget.session
	spanSweep   = "bridge.widget.sweep"  // INTERNAL, root / linked root

	// Dialpad PWA (PWA-SPEC §10).
	spanDevEnroll  = "widget.device.enroll"         // SERVER, root
	spanDevSession = "widget.device.session"        // SERVER, root
	spanDevConfirm = "widget.device.confirm"        // SERVER, root
	spanDevForget  = "widget.device.forget"         // SERVER, root
	spanDevJanitor = "bridge.widget.device_janitor" // INTERNAL, root, only when it did work
)

// PWA span and metric attributes (PWA-SPEC §10).
const (
	attrDeviceID       = attribute.Key("widget.device.id")          // span (link id; never on metrics)
	attrDeviceConfirm  = attribute.Key("widget.device.confirm")     // span: bool
	attrTokenState     = attribute.Key("widget.device.token_state") // span: current, unconfirmed, grace
	attrClient         = attribute.Key("widget.client")             // span and metric: widget, pwa
	attrDevicesPurged  = attribute.Key("devices.purged")            // span
	attrNoticesRedact  = attribute.Key("notices.redacted")          // span
	attrNoticesFailed  = attribute.Key("notices.failed")            // span
	attrLinkState      = attribute.Key("state")                     // metric: pending, active
	attrDeviceEvent    = attribute.Key("event")                     // metric
	metricDeviceLinks  = "mrtcsip.widget.device_links"
	metricDeviceEvents = "mrtcsip.widget.device_events"

	clientWidget = "widget"
	clientPWA    = "pwa"
)

// Events of mrtcsip.widget.device_events (closed enum).
const (
	evIssued         = "issued"
	evEnrolled       = "enrolled"
	evRotated        = "rotated"
	evGraceUsed      = "grace_used"
	evReuseRevoked   = "reuse_revoked"
	evRevoked        = "revoked"
	evExpired        = "expired"
	evOrphanPurged   = "orphan_purged"
	evNoticeRedacted = "notice_redacted"
)

// Attribute keys used only by this package (TELEMETRY-SPEC §5.12, §4.3). obs.AttrOutcome is "outcome".
const (
	attrRoomKind      = attribute.Key("widget.room.kind") // span: portal, management
	attrCallForm      = attribute.Key("widget.call.form") // span: room, number
	attrAction        = attribute.Key("widget.action")    // metric: session, state, normalize, call, dtmf, hangup
	attrTrigger       = attribute.Key("trigger")          // span: startup, login, periodic, disable
	attrRoomsChecked  = attribute.Key("rooms.checked")    // span
	attrRoomsInstall  = attribute.Key("rooms.installed")  // span
	attrRoomsUpdated  = attribute.Key("rooms.updated")    // span
	attrRoomsFailed   = attribute.Key("rooms.failed")     // span
	attrRoomsRemoved  = attribute.Key("rooms.removed")    // span (disable sweep)
	metricRequests    = "mrtcsip.widget.requests"
	metricSessions    = "mrtcsip.widget.sessions"
	metricInstalls    = "mrtcsip.widget.installs"
	outcomeOK         = "ok"
	outcomeInstalled  = "installed"
	outcomeUpdated    = "updated"
	outcomeUnchanged  = "unchanged"
	outcomeRemoved    = "removed"
	outcomeInstallErr = "failed"
)

// Metric outcomes of mrtcsip.widget.requests (closed enum).
const (
	mOK           = "ok"
	mUnauthorized = "unauthorized"
	mForbidden    = "forbidden"
	mRateLimited  = "rate_limited"
	mInvalid      = "invalid"
	mBusy         = "busy"
	mNotReady     = "not_ready"
	mError        = "error"
	mExpired      = "expired" // device_expired, link_expired
	mUsed         = "used"    // link_used
	mReuse        = "reuse"   // device token reuse detected
)

// metricOutcome maps an API error code ("" = ok) to the closed metric outcome enum.
func metricOutcome(code string) string {
	switch code {
	case "", "ok":
		return mOK
	case codeUnauthorized:
		return mUnauthorized
	case codeForbidden, codeLoginRequired:
		return mForbidden
	case codeRateLimited:
		return mRateLimited
	case "line_busy", "dtmf_busy":
		return mBusy
	case codeNotReady, "outbound_disabled":
		return mNotReady
	case codeInternal:
		return mError
	case codeDeviceInvalid, codeDeviceRevoked:
		return mUnauthorized
	case codeDeviceExpired, codeLinkExpired:
		return mExpired
	case codeLinkUsed:
		return mUsed
	case mReuse:
		return mReuse
	default:
		return mInvalid
	}
}

// instruments are the widget's metric instruments. obs owns the meter: when obs.Use swaps the registry
// (host start, tests), the instruments are rebuilt from the new meter on next use.
type instruments struct {
	reg      *obs.Instruments
	requests metric.Int64Counter
	installs metric.Int64Counter
	devEvts  metric.Int64Counter
}

var (
	instMu  sync.Mutex
	instCur atomic.Pointer[instruments]

	// live holds the running servers whose sessions the mrtcsip.widget.sessions gauge reports.
	liveMu sync.Mutex
	live   = map[*Server]struct{}{}
)

// inst returns the instruments for the current obs registry (never nil).
func inst() *instruments {
	reg := obs.M()
	if in := instCur.Load(); in != nil && in.reg == reg {
		return in
	}
	instMu.Lock()
	defer instMu.Unlock()
	if in := instCur.Load(); in != nil && in.reg == reg {
		return in
	}
	m := reg.Meter
	in := &instruments{reg: reg}
	var err error
	in.requests, err = m.Int64Counter(metricRequests,
		metric.WithUnit("{request}"), metric.WithDescription("Dialpad widget API requests by action and outcome"))
	handleErr(err)
	in.installs, err = m.Int64Counter(metricInstalls,
		metric.WithUnit("{room}"), metric.WithDescription("Dialpad widget installer results per room"))
	handleErr(err)
	gauge, err := m.Int64ObservableGauge(metricSessions,
		metric.WithUnit("{session}"), metric.WithDescription("Active Dialpad widget sessions"))
	handleErr(err)
	if err == nil {
		_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
			o.ObserveInt64(gauge, liveSessions())
			return nil
		}, gauge)
		handleErr(err)
	}
	in.devEvts, err = m.Int64Counter(metricDeviceEvents,
		metric.WithUnit("{event}"), metric.WithDescription("Dialpad PWA device lifecycle events"))
	handleErr(err)
	links, err := m.Int64ObservableGauge(metricDeviceLinks,
		metric.WithUnit("{link}"), metric.WithDescription("Unexpired Dialpad PWA links by state"))
	handleErr(err)
	if err == nil {
		_, err = m.RegisterCallback(func(_ context.Context, o metric.Observer) error {
			c, ok := liveDeviceCounts()
			if ok {
				o.ObserveInt64(links, int64(c.Pending), metric.WithAttributes(attrLinkState.String(string(DeviceStatePending))))
				o.ObserveInt64(links, int64(c.Active), metric.WithAttributes(attrLinkState.String(string(DeviceStateActive))))
			}
			return nil
		}, links)
		handleErr(err)
	}
	instCur.Store(in)
	return in
}

func handleErr(err error) {
	if err != nil {
		otel.Handle(err)
	}
}

// liveSessions sums the unexpired sessions of every running server.
func liveSessions() int64 {
	liveMu.Lock()
	defer liveMu.Unlock()
	var n int64
	for s := range live {
		n += int64(s.sessions.count())
	}
	return n
}

// liveDeviceCounts sums the cached device counts of the running servers' stores (each store once;
// no I/O). ok is false when no running server has the PWA enabled.
func liveDeviceCounts() (DeviceCounts, bool) {
	liveMu.Lock()
	defer liveMu.Unlock()
	var c DeviceCounts
	seen := map[*DeviceStore]bool{}
	for s := range live {
		st := s.d.Devices
		if st == nil || !s.pwa || seen[st] {
			continue
		}
		seen[st] = true
		x := st.Counts()
		c.Pending += x.Pending
		c.Active += x.Active
	}
	return c, len(seen) > 0
}

// recordRequest counts one API request (no identities on metrics). client is clientWidget or clientPWA.
func recordRequest(ctx context.Context, action, client, code string) {
	inst().requests.Add(ctx, 1, metric.WithAttributes(attrAction.String(action), attrClient.String(client),
		obs.AttrOutcome.String(metricOutcome(code))))
}

// recordDeviceEvent counts one device lifecycle event (closed enum, no identities).
func recordDeviceEvent(ctx context.Context, event string, n int) {
	if n <= 0 {
		return
	}
	inst().devEvts.Add(ctx, int64(n), metric.WithAttributes(attrDeviceEvent.String(event)))
}

// recordInstall counts one installer result.
func recordInstall(ctx context.Context, outcome string) {
	inst().installs.Add(ctx, 1, metric.WithAttributes(obs.AttrOutcome.String(outcome)))
}
