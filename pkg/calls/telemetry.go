package calls

import (
	"context"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"

	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

// Pre-built metric options for the fixed attribute combinations of the 5 s stats path (PHASE2-SPEC
// §10.3), so the flush allocates no attribute sets per call.
var (
	attrInbound  = obs.AttrCallDirection.String(dirInbound)
	attrOutbound = obs.AttrCallDirection.String(dirOutbound)

	optInbound  = metric.WithAttributeSet(attribute.NewSet(attrInbound))
	optOutbound = metric.WithAttributeSet(attribute.NewSet(attrOutbound))
	optDTMF     = metric.WithAttributeSet(attribute.NewSet(obs.AttrDTMFMethod.String("rfc4733")))
	optRx       = metric.WithAttributeSet(attribute.NewSet(obs.AttrMediaDirection.String("rx")))
	optTx       = metric.WithAttributeSet(attribute.NewSet(obs.AttrMediaDirection.String("tx")))
	optSIPToRTC = metric.WithAttributeSet(attribute.NewSet(obs.AttrMediaLeg.String("sip_to_rtc")))
	optRTCToSIP = metric.WithAttributeSet(attribute.NewSet(obs.AttrMediaLeg.String("rtc_to_sip")))
	optNone     = metric.WithAttributeSet(attribute.NewSet())
)

// pastEvent is a mid-call event replayed onto call.session with its original timestamp.
type pastEvent struct {
	name  string
	at    time.Time
	attrs []attribute.KeyValue
}

// child starts a child-only span timestamped with the call clock.
func (c *call) child(ctx context.Context, name string, kind trace.SpanKind, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	return obs.StartChild(ctx, name,
		trace.WithSpanKind(kind),
		trace.WithTimestamp(c.now()),
		trace.WithAttributes(attrs...))
}

// endSpan ends span at the call clock's now.
func (c *call) endSpan(span trace.Span) { span.End(trace.WithTimestamp(c.now())) }

// spanEvent adds a span event at t.
func (c *call) spanEvent(span trace.Span, name string, t time.Time, attrs ...attribute.KeyValue) {
	if span == nil || !span.IsRecording() {
		return
	}
	span.AddEvent(name, trace.WithTimestamp(t), trace.WithAttributes(attrs...))
}

// addPast buffers a mid-call event for call.session (at most maxPastEvents).
func (c *call) addPast(name string, t time.Time, attrs ...attribute.KeyValue) {
	if len(c.past) < maxPastEvents {
		c.past = append(c.past, pastEvent{name, t, attrs})
	}
}

func (c *call) recordStarted() {
	m := obs.M()
	m.CallStarted.Add(context.Background(), 1, c.optDir)
	m.CallActive.Add(context.Background(), 1, c.optDir)
}

func (c *call) recordSetupDuration(d time.Duration, outcome string) {
	obs.M().CallSetupDuration.Record(context.Background(), d.Seconds(), metric.WithAttributeSet(
		attribute.NewSet(c.dirAttr, obs.AttrCallOutcome.String(outcome))))
}

// statusClass maps a final SIP status to the sip.response.status_class enum.
func statusClass(code int) string {
	if code < 100 || code > 699 {
		return "none"
	}
	return strconv.Itoa(code/100) + "xx"
}

// codecAttr maps the negotiated codec to the media.codec metric enum.
func codecAttr(codec string) string {
	switch codec {
	case "", "PCMU":
		return "PCMU"
	case "PCMA":
		return "PCMA"
	default:
		return "other"
	}
}

// recordEnd records the call-end metrics (actor only, once per call).
func (c *call) recordEnd() {
	ctx := context.Background()
	m := obs.M()
	status := c.sipStatus
	if c.answered {
		status = 200
	}
	m.CallEnded.Add(ctx, 1, metric.WithAttributeSet(attribute.NewSet(c.dirAttr,
		obs.AttrCallOutcome.String(c.outcome),
		obs.AttrSIPStatusClass.String(statusClass(status)))))
	m.CallActive.Add(ctx, -1, c.optDir)
	outcomeSet := metric.WithAttributeSet(attribute.NewSet(c.dirAttr, obs.AttrCallOutcome.String(c.outcome)))
	if !c.setupRecorded {
		m.CallSetupDuration.Record(ctx, c.endedAt.Sub(c.startedAt).Seconds(), outcomeSet)
		c.setupRecorded = true
	}
	switch {
	case c.isOutbound():
		// Outbound: first 18x to 200 OK or failure; the command path's callback ring is not recorded.
		if !c.ringbackAt.IsZero() && !c.ringbackEnd.IsZero() {
			m.CallRingDuration.Record(ctx, c.ringbackEnd.Sub(c.ringbackAt).Seconds(), outcomeSet)
		}
	case !c.ringSentAt.IsZero() && !c.ringEndedAt.IsZero():
		m.CallRingDuration.Record(ctx, c.ringEndedAt.Sub(c.ringSentAt).Seconds(), outcomeSet)
	}
	if c.answered {
		m.CallDuration.Record(ctx, c.endedAt.Sub(c.answeredAt).Seconds(), metric.WithAttributeSet(
			attribute.NewSet(c.dirAttr, obs.AttrMediaCodec.String(codecAttr(c.answer.Codec)))))
	}
}

// onStatsTick flushes media stats and runs the degraded check (Active only).
func (c *call) onStatsTick() {
	if c.state != stActive || c.bridge == nil {
		return
	}
	prev := c.lastStats
	st := c.bridge.Stats()
	c.flushStats(st, true)
	c.checkDegraded(prev, st)
}

// flushStats records the deltas since the last flush. The jitter histogram is recorded on ticks only,
// when at least one packet arrived in the window.
func (c *call) flushStats(st media.Stats, tick bool) {
	ctx := context.Background()
	m := obs.M()
	prev := c.lastStats
	if d := st.SIPRx - prev.SIPRx; d > 0 {
		m.RTPPackets.Add(ctx, int64(d), optRx)
		if tick {
			m.RTPJitter.Record(ctx, st.SIPJitter.Seconds(), optNone)
			c.jitterSum += st.SIPJitter
			c.jitterN++
			if st.SIPJitter > c.jitterMax {
				c.jitterMax = st.SIPJitter
			}
		}
	}
	if d := st.SIPTx - prev.SIPTx; d > 0 {
		m.RTPPackets.Add(ctx, int64(d), optTx)
	}
	if d := st.SIPLost - prev.SIPLost; d > 0 {
		m.RTPLost.Add(ctx, int64(d), optNone)
	}
	if d := st.SIPLate - prev.SIPLate; d > 0 {
		m.RTPLate.Add(ctx, int64(d), optNone)
	}
	if d := st.ConcealedSIPToRTC - prev.ConcealedSIPToRTC; d > 0 {
		m.MediaConcealed.Add(ctx, int64(d), optSIPToRTC)
	}
	if d := st.ConcealedRTCToSIP - prev.ConcealedRTCToSIP; d > 0 {
		m.MediaConcealed.Add(ctx, int64(d), optRTCToSIP)
	}
	if d := st.DTMFSent - prev.DTMFSent; d > 0 {
		m.DTMFSent.Add(ctx, int64(d), optDTMF)
	}
	c.lastStats = st
}

// checkDegraded emits call.media.degraded once per call: loss above 5 % in the last window, or no SIP
// RTP for 5 s.
func (c *call) checkDegraded(prev, st media.Stats) {
	if c.degraded {
		return
	}
	now := c.now()
	last := st.LastSIPRx
	if last.IsZero() {
		last = c.answeredAt
	}
	silent := now.Sub(last) >= degradedSilence
	dRx, dLost := st.SIPRx-prev.SIPRx, st.SIPLost-prev.SIPLost
	lossy := dLost > 0 && float64(dLost)/float64(dRx+dLost) > degradedLossRatio
	if !silent && !lossy {
		return
	}
	c.degraded = true
	attrs := []attribute.KeyValue{
		obs.AttrRTPLost.Int64(int64(st.SIPLost)),
		obs.AttrRTPPacketsRx.Int64(int64(st.SIPRx)),
	}
	ctx, span := obs.StartLinkedRoot(context.Background(), "call.media.degraded", c.setupCtx,
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithTimestamp(now),
		trace.WithAttributes(append([]attribute.KeyValue{obs.AttrCallID.String(c.id)}, attrs...)...))
	c.endSpan(span)
	c.addPast("call.media.degraded", now, attrs...)
	c.logFor(ctx).Warn().Bool("no_rtp", silent).Uint64("rtp_lost", st.SIPLost).Uint64("rtp_rx", st.SIPRx).
		Msg("Call media degraded")
}

// rtpSummary is the rtp.* session summary.
func (c *call) rtpSummary() []attribute.KeyValue {
	st := c.lastStats
	mean := time.Duration(0)
	if c.jitterN > 0 {
		mean = c.jitterSum / time.Duration(c.jitterN)
	}
	return []attribute.KeyValue{
		obs.AttrRTPPacketsRx.Int64(int64(st.SIPRx)),
		obs.AttrRTPPacketsTx.Int64(int64(st.SIPTx)),
		obs.AttrRTPLost.Int64(int64(st.SIPLost)),
		obs.AttrRTPLate.Int64(int64(st.SIPLate)),
		obs.AttrRTPJitterMaxMS.Float64(float64(c.jitterMax) / float64(time.Millisecond)),
		obs.AttrRTPJitterMeanMS.Float64(float64(mean) / float64(time.Millisecond)),
	}
}

// emitSession emits call.session: a zero-length post hoc linked root at INVITE receipt, linked to the
// setup span and the teardown span (if any), carrying the call summary and the buffered past events.
func (c *call) emitSession() {
	if !obs.Enabled() {
		return
	}
	duration := 0.0
	if c.answered {
		duration = c.endedAt.Sub(c.answeredAt).Seconds()
	}
	attrs := []attribute.KeyValue{
		obs.AttrCallID.String(c.id),
		c.dirAttr,
		obs.AttrCallOutcome.String(c.outcome),
		obs.AttrCallDurationS.Float64(duration),
		obs.AttrCallEndReason.String(c.endReason),
	}
	if c.answered {
		attrs = append(attrs, obs.AttrMediaCodec.String(codecAttr(c.answer.Codec)), obs.AttrCallHangupBy.String(c.hangupBy))
		attrs = append(attrs, c.rtpSummary()...)
	}
	opts := []trace.SpanStartOption{
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithTimestamp(c.startedAt),
		trace.WithAttributes(attrs...),
	}
	if c.teardownSC.IsValid() {
		opts = append(opts, trace.WithLinks(trace.Link{SpanContext: c.teardownSC}))
	}
	_, span := obs.StartLinkedRoot(context.Background(), "call.session", c.setupCtx, opts...)
	for _, e := range c.past {
		span.AddEvent(e.name, trace.WithTimestamp(e.at), trace.WithAttributes(e.attrs...))
	}
	span.End(trace.WithTimestamp(c.startedAt))
}
