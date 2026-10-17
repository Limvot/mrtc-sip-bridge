package sipua

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
)

const (
	// outboundDefaultTimeout bounds an unanswered outbound call when DialRequest.Timeout is 0.
	outboundDefaultTimeout = 5 * time.Minute
	// outboundSIPTimeout bounds each ACK and BYE the loop sends.
	outboundSIPTimeout  = 5 * time.Second
	outboundMsgBuffer   = 32
	outboundCmdBuffer   = 4
	outboundEventBuffer = 16
	// outboundEventReserve is how many event slots are kept free for EventAnswered and EventEnded:
	// EventProvisional and EventAuthChallenged are dropped when fewer than this many slots plus one are free.
	outboundEventReserve = 2
	// maxWaitAnswerReentries bounds how often WaitAnswer is re-entered after sipgo's
	// "more than 10 responses received" error (D12).
	maxWaitAnswerReentries = 10
)

// dialogClient is the subset of the diago client dialog the loop uses (outbound_diago.go adapts it).
// Invite and WaitAnswer are only called from the invite goroutine; every other method only from the run
// goroutine after the invite goroutine posted the message that makes the value valid.
type dialogClient interface {
	Info() DialInfo
	// Invite sends the INVITE and blocks until: 2xx (nil; Answer/Leg valid), 183 with SDP (errEarlyMedia;
	// Leg valid), final non-2xx (*finalError), or ctx cancellation (sipgo sent CANCEL; returns an error
	// wrapping context.Canceled, or another error if the CANCEL could not complete). onResponse is called
	// on this goroutine for every response except the 183 that establishes early media.
	Invite(ctx context.Context, onResponse func(status int)) error
	// WaitAnswer continues after errEarlyMedia with the same result contract.
	WaitAnswer(ctx context.Context, onResponse func(status int)) error
	Leg() media.SIPLeg          // early-media or call leg (same object across 183 -> 200)
	Answer() AnswerInfo         // after 2xx: codec, telephone-event, RTP addrs
	FinalStatus() (int, string) // last final response seen (incl. a 200 that raced CANCEL); 0 if none
	Ack(ctx context.Context) error
	Bye(ctx context.Context) error
	Done() <-chan struct{} // dialog context done (remote BYE, ended)
	Close()
}

// errEarlyMedia is returned by dialogClient.Invite when a 183 with SDP established early media.
var errEarlyMedia = errors.New("sipua: early media")

// finalError is a final non-2xx response to the INVITE.
type finalError struct {
	Status int
	Reason string
}

// Error deliberately leaves out the remote reason phrase (untrusted text).
func (e *finalError) Error() string { return fmt.Sprintf("sipua: INVITE rejected with %d", e.Status) }

type outMsgKind int

const (
	msgAuth outMsgKind = iota + 1
	msgProvisional
	msgEarly
	msgResult
)

type outMsg struct {
	kind   outMsgKind
	status int
	err    error
}

// outboundCall implements OutboundCall on top of a dialogClient.
type outboundCall struct {
	dlg     dialogClient
	log     zerolog.Logger
	clk     clock.Clock
	info    DialInfo
	timeout time.Duration

	msgs   chan outMsg
	cmds   chan struct{}
	events chan CallEvent

	mu     sync.Mutex
	closed bool // no more commands accepted

	// Owned by the invite goroutine; read by run only after msgResult (channel happens-before).
	authPosted bool
	okAt       time.Time
}

func newOutboundCall(dlg dialogClient, log zerolog.Logger, clk clock.Clock, req DialRequest) *outboundCall {
	info := dlg.Info()
	if info.StartedAt.IsZero() {
		info.StartedAt = clk.Now()
	}
	if info.Target == "" {
		info.Target = req.Target
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = outboundDefaultTimeout
	}
	return &outboundCall{
		dlg:     dlg,
		log:     log,
		clk:     clk,
		info:    info,
		timeout: timeout,
		msgs:    make(chan outMsg, outboundMsgBuffer),
		cmds:    make(chan struct{}, outboundCmdBuffer),
		events:  make(chan CallEvent, outboundEventBuffer),
	}
}

func (c *outboundCall) Info() DialInfo           { return c.info }
func (c *outboundCall) Events() <-chan CallEvent { return c.events }

// Hangup queues a hangup without blocking; ignored after the call ended (or when one is already queued).
func (c *outboundCall) Hangup() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	select {
	case c.cmds <- struct{}{}:
	default: // a hangup is already pending; Hangup is idempotent
	}
}

// onResponse runs on the invite goroutine for every response sipgo hands to the dialog.
func (c *outboundCall) onResponse(status int) {
	switch {
	case status == 401 || status == 407:
		if !c.authPosted {
			c.authPosted = true
			c.msgs <- outMsg{kind: msgAuth, status: status}
		}
	case status >= 100 && status < 200:
		select {
		case c.msgs <- outMsg{kind: msgProvisional, status: status}:
		default: // run is behind; provisional responses are informational
		}
	case status >= 200 && status < 300:
		c.okAt = c.clk.Now()
	}
}

func isTooManyResponses(err error) bool {
	return err != nil && strings.Contains(err.Error(), "more than 10 responses received")
}

// invite is the only goroutine that calls Invite/WaitAnswer. It always posts exactly one msgResult.
func (c *outboundCall) invite(ctx context.Context) {
	err := c.dlg.Invite(ctx, c.onResponse)
	reentries := 0
	for {
		if errors.Is(err, errEarlyMedia) {
			c.msgs <- outMsg{kind: msgEarly, status: 183}
			err = c.dlg.WaitAnswer(ctx, c.onResponse)
			continue
		}
		if isTooManyResponses(err) && reentries < maxWaitAnswerReentries {
			reentries++
			err = c.dlg.WaitAnswer(ctx, c.onResponse)
			continue
		}
		break
	}
	if err != nil && ctx.Err() == nil && c.needsCancel(err) {
		// Give up (response limit) or fail locally (e.g. diago could not apply the 183 SDP) before any final
		// response: do not leave the INVITE pending at the far end. WaitAnswer with a cancelled context makes
		// sipgo send CANCEL (and wait for the 487).
		cctx, cancel := context.WithCancel(context.Background())
		cancel()
		_ = c.dlg.WaitAnswer(cctx, c.onResponse)
	}
	c.msgs <- outMsg{kind: msgResult, err: err}
}

// needsCancel reports whether err left the INVITE pending remotely: the response limit, or a local error
// before any final response (a finalError is a final response itself).
func (c *outboundCall) needsCancel(err error) bool {
	if isTooManyResponses(err) {
		return true
	}
	var fe *finalError
	if errors.As(err, &fe) {
		return false
	}
	status, _ := c.dlg.FinalStatus()
	return status == 0
}

// run owns all call state and emits every event. It returns after EventEnded was emitted, which only
// happens once the invite goroutine posted its result.
func (c *outboundCall) run() {
	ictx, cancelInvite := context.WithCancel(context.Background())
	defer cancelInvite()

	c.log.Info().
		Str("target", c.info.Target).
		Str("request_user", c.info.RequestUser).
		Str("sip_call_id", c.info.CallID).
		Msg("Outbound INVITE")
	go c.invite(ictx)

	var (
		answered        bool
		cancelRequested bool // Hangup or safety timeout before answer: the invite context was cancelled
		safetyFired     bool
		earlySent       bool
		done            <-chan struct{}
		safety          = c.clk.NewTimer(c.timeout)
		safetyC         = safety.C()
	)
	defer safety.Stop()

	requestCancel := func() {
		if !cancelRequested {
			cancelRequested = true
			cancelInvite()
		}
	}
	// cancelCause is the end cause for a call ended by our own cancel request.
	cancelCause := func() EndCause {
		if safetyFired {
			return EndFailed
		}
		return EndLocalCancel
	}

	for {
		select {
		case m := <-c.msgs:
			switch m.kind {
			case msgAuth:
				c.emitDroppable(CallEvent{Kind: EventAuthChallenged, At: c.clk.Now(), StatusCode: m.status})
			case msgProvisional:
				c.log.Debug().Int("status", m.status).Str("sip_call_id", c.info.CallID).Msg("Outbound provisional response")
				c.emitDroppable(CallEvent{Kind: EventProvisional, At: c.clk.Now(), StatusCode: m.status})
			case msgEarly:
				if earlySent {
					continue
				}
				earlySent = true
				c.log.Debug().Str("sip_call_id", c.info.CallID).Msg("Outbound early media")
				c.emitDroppable(CallEvent{Kind: EventProvisional, At: c.clk.Now(), StatusCode: 183, EarlyMedia: true, Media: c.dlg.Leg()})
			case msgResult:
				if c.result(m.err, cancelRequested, cancelCause) {
					return
				}
				answered = true
				safety.Stop()
				safetyC = nil
				done = c.dlg.Done()
			}
		case <-c.cmds:
			if answered {
				ctx, cancel := context.WithTimeout(context.Background(), outboundSIPTimeout)
				err := c.dlg.Bye(ctx)
				cancel()
				if err != nil {
					c.log.Warn().Str("error", obs.Scrub(err.Error())).Str("sip_call_id", c.info.CallID).Msg("Sending BYE failed")
				}
				c.finish(EndLocalHangup, 200, "", nil)
				return
			}
			requestCancel()
		case <-done:
			c.finish(EndRemoteBye, 200, "", nil)
			return
		case <-safetyC:
			safetyC = nil
			if !cancelRequested {
				c.log.Warn().Str("sip_call_id", c.info.CallID).Dur("timeout", c.timeout).Msg("Outbound call not answered within the safety timeout")
				safetyFired = true
				requestCancel()
			}
		}
	}
}

// result handles the invite goroutine's result. It returns true when the call ended (EventEnded emitted)
// and false when the call is answered (EventAnswered emitted).
func (c *outboundCall) result(err error, cancelRequested bool, cancelCause func() EndCause) bool {
	if err == nil {
		ackErr := c.ack()
		ackAt := c.clk.Now()
		if cancelRequested {
			// The 200 won the race against our CANCEL: confirm the dialog, then end it at once.
			c.bye()
			c.finish(cancelCause(), 200, "", nil)
			return true
		}
		if ackErr != nil {
			c.bye()
			c.finish(EndFailed, 200, "", fmt.Errorf("send ACK: %w", ackErr))
			return true
		}
		ans := c.dlg.Answer()
		if ans.OKReceivedAt.IsZero() {
			ans.OKReceivedAt = c.okAt
		}
		ans.AckSentAt = ackAt
		c.log.Info().Str("sip_call_id", c.info.CallID).Bool("telephone_event", ans.TelephoneEvent).Msg("Outbound call answered")
		c.emit(CallEvent{Kind: EventAnswered, At: ackAt, Media: c.dlg.Leg(), Answer: ans})
		return false
	}

	var fe *finalError
	if errors.As(err, &fe) {
		if cancelRequested && fe.Status == 487 {
			c.finish(cancelCause(), 487, "", nil)
		} else {
			c.finish(EndRemoteRejected, fe.Status, fe.Reason, nil)
		}
		return true
	}

	// Cancelled, transport error, timeout or protocol error. A 2xx may still have arrived (it raced the
	// CANCEL, or diago rejected its SDP): it must be ACKed and BYEd so the far end does not keep the call.
	status, _ := c.dlg.FinalStatus()
	if status >= 200 && status < 300 {
		_ = c.ack()
		c.bye()
		status = 200
	}
	switch {
	case cancelRequested && cancelCause() == EndLocalCancel:
		if status == 0 && !errors.Is(err, context.Canceled) {
			// The CANCEL itself failed (non-200 answer, or sent before sipgo saw a provisional on a re-sent
			// INVITE): the callee may still be ringing.
			c.log.Warn().Str("error", obs.Scrub(err.Error())).Str("sip_call_id", c.info.CallID).
				Msg("CANCEL did not complete; the remote side may still be ringing")
			c.finish(EndFailed, status, "", err)
			return true
		}
		c.finish(EndLocalCancel, status, "", nil)
	default:
		c.finish(EndFailed, status, "", err)
	}
	return true
}

func (c *outboundCall) ack() error {
	ctx, cancel := context.WithTimeout(context.Background(), outboundSIPTimeout)
	defer cancel()
	err := c.dlg.Ack(ctx)
	if err != nil {
		c.log.Warn().Str("error", obs.Scrub(err.Error())).Str("sip_call_id", c.info.CallID).Msg("Sending ACK failed")
	}
	return err
}

func (c *outboundCall) bye() {
	ctx, cancel := context.WithTimeout(context.Background(), outboundSIPTimeout)
	defer cancel()
	if err := c.dlg.Bye(ctx); err != nil {
		c.log.Warn().Str("error", obs.Scrub(err.Error())).Str("sip_call_id", c.info.CallID).Msg("Sending BYE failed")
	}
}

// emitDroppable sends a provisional-class event unless that would eat into the slots reserved for
// EventAnswered and EventEnded. Only run sends on events, so the free-slot check cannot go stale
// in the unsafe direction (the consumer can only free slots).
func (c *outboundCall) emitDroppable(ev CallEvent) {
	if cap(c.events)-len(c.events) <= outboundEventReserve {
		return
	}
	c.events <- ev
}

func (c *outboundCall) emit(ev CallEvent) {
	select {
	case c.events <- ev:
	default: // cannot happen: outboundEventReserve slots are kept for EventAnswered and EventEnded
	}
}

// finish emits the single EventEnded, closes the event channel, stops accepting commands and releases
// the dialog.
func (c *outboundCall) finish(cause EndCause, status int, reason string, err error) {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	ev := c.log.Info().
		Str("sip_call_id", c.info.CallID).
		Int("status", status).
		Str("cause", cause.String())
	if reason != "" {
		ev = ev.Str("reason", obs.Scrub(reason))
	}
	if err != nil {
		ev = ev.Str("error", obs.Scrub(err.Error()))
	}
	ev.Msg("Outbound call ended")
	c.emit(CallEvent{Kind: EventEnded, At: c.clk.Now(), Cause: cause, StatusCode: status, Reason: reason, Err: err})
	close(c.events)
	c.dlg.Close()
}
