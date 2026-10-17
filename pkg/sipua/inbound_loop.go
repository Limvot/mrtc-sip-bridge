package sipua

import (
	"context"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
)

const (
	// inboundSafetyTimeout bounds an unanswered call; longer than the 300 s maximum ring.
	inboundSafetyTimeout = 6 * time.Minute
	inboundByeTimeout    = 5 * time.Second
	inboundCmdBuffer     = 8
	inboundEventBuffer   = 4
)

// dialogServer is the subset of *diago.DialogServerSession the loop uses. inbound_diago.go adapts it.
type dialogServer interface {
	Info() InviteInfo // built once from the INVITE (offer parsed)
	Trying() error
	Ringing() error
	Answer() (media.SIPLeg, AnswerInfo, error) // blocks until ACK (or error)
	Respond(code int, reason string) error     // final non-2xx; blocks until ACK/tx end
	Bye(ctx context.Context) error
	Done() <-chan struct{} // dialog context done (CANCEL, BYE, ended)
	Cancelled() bool       // context.Cause(Done) is sip.ErrTransactionCanceled
	Confirmed() bool       // dialog state >= Confirmed
}

type cmdKind int

const (
	cmdRinging cmdKind = iota + 1
	cmdAnswer
	cmdReject
	cmdHangup
)

type inboundCmd struct {
	kind   cmdKind
	code   int
	reason string
}

// inboundCall implements InboundCall on top of a dialogServer.
type inboundCall struct {
	dlg  dialogServer
	log  zerolog.Logger
	clk  clock.Clock
	info InviteInfo

	cmds   chan inboundCmd
	events chan CallEvent

	mu     sync.Mutex
	closed bool // no more commands accepted
}

func newInboundCall(dlg dialogServer, log zerolog.Logger, clk clock.Clock) *inboundCall {
	return &inboundCall{
		dlg:    dlg,
		log:    log,
		clk:    clk,
		info:   dlg.Info(),
		cmds:   make(chan inboundCmd, inboundCmdBuffer),
		events: make(chan CallEvent, inboundEventBuffer),
	}
}

func (c *inboundCall) Info() InviteInfo { return c.info }

func (c *inboundCall) Events() <-chan CallEvent { return c.events }

func (c *inboundCall) Ringing() { c.send(inboundCmd{kind: cmdRinging}) }
func (c *inboundCall) Answer()  { c.send(inboundCmd{kind: cmdAnswer}) }
func (c *inboundCall) Hangup()  { c.send(inboundCmd{kind: cmdHangup}) }
func (c *inboundCall) Reject(code int, reason string) {
	c.send(inboundCmd{kind: cmdReject, code: code, reason: reason})
}

// send queues a command without blocking; it is dropped after the call ended or when the queue is full.
func (c *inboundCall) send(cmd inboundCmd) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	select {
	case c.cmds <- cmd:
	default:
		c.log.Warn().Msg("Inbound call command queue full; command dropped")
	}
}

// serve runs the call to completion on the calling goroutine (the diago handler goroutine).
func (c *inboundCall) serve(onInvite InviteHandler) {
	c.info.ReceivedAt = c.clk.Now()
	if err := c.dlg.Trying(); err != nil {
		c.log.Warn().Err(err).Msg("Sending 100 Trying failed")
	} else {
		c.info.TryingSentAt = c.clk.Now()
	}
	c.log.Info().
		Str("sip_call_id", c.info.CallID).
		Str("from", c.info.FromUser).
		Str("asserted", c.info.AssertedUser).
		Strs("offered_codecs", c.info.Offer.Codecs).
		Str("remote_media", c.info.Offer.MediaAddr).
		Msg("Inbound INVITE")

	onInvite(c)

	var (
		finalSent bool // a final response (2xx or non-2xx) was sent or attempted
		answered  bool
		done      = c.dlg.Done()
		safety    = c.clk.NewTimer(inboundSafetyTimeout)
		safetyC   = safety.C()
	)
	defer safety.Stop()

	for {
		select {
		case cmd := <-c.cmds:
			switch cmd.kind {
			case cmdRinging:
				if !finalSent {
					if err := c.dlg.Ringing(); err != nil {
						c.log.Warn().Err(err).Msg("Sending 180 Ringing failed")
					}
				}
			case cmdAnswer:
				if finalSent {
					continue
				}
				finalSent = true
				leg, ans, err := c.dlg.Answer()
				if err != nil {
					c.emit(CallEvent{Kind: EventAnswerFailed, At: c.clk.Now(), Err: err})
					if c.dlg.Cancelled() {
						c.finish(EndCancelled, 487)
					} else {
						c.finish(EndFailed, 0)
					}
					return
				}
				answered = true
				safetyC = nil
				c.emit(CallEvent{Kind: EventAnswered, At: ans.AckAt, Media: leg, Answer: ans})
			case cmdReject:
				if answered || finalSent {
					continue
				}
				c.reject(cmd.code, cmd.reason)
				return
			case cmdHangup:
				if answered {
					ctx, cancel := context.WithTimeout(context.Background(), inboundByeTimeout)
					err := c.dlg.Bye(ctx)
					cancel()
					if err != nil {
						c.log.Warn().Err(err).Msg("Sending BYE failed")
					}
					c.finish(EndLocalHangup, 200)
					return
				}
				if finalSent {
					continue
				}
				c.reject(480, "Temporarily Unavailable")
				return
			}
		case <-done:
			switch {
			case answered:
				c.finish(EndRemoteBye, 200)
			case c.dlg.Cancelled():
				c.finish(EndCancelled, 487)
			default:
				c.finish(EndFailed, 0)
			}
			return
		case <-safetyC:
			c.log.Warn().Msg("Inbound call not answered within the safety timeout")
			if !finalSent {
				_ = c.dlg.Respond(480, "Temporarily Unavailable")
			}
			c.finish(EndFailed, 480)
			return
		}
	}
}

// reject sends a final non-2xx response and finishes the call.
func (c *inboundCall) reject(code int, reason string) {
	if err := c.dlg.Respond(code, reason); err != nil {
		c.log.Warn().Err(err).Int("status", code).Msg("Sending final response failed")
	}
	c.finish(EndRejected, code)
}

func (c *inboundCall) emit(ev CallEvent) {
	select {
	case c.events <- ev:
	default: // cannot happen: at most 2 events are ever sent into a buffer of 4
	}
}

// finish emits the single EventEnded, closes the event channel and stops accepting commands.
func (c *inboundCall) finish(cause EndCause, status int) {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
	c.emit(CallEvent{Kind: EventEnded, At: c.clk.Now(), Cause: cause, StatusCode: status})
	close(c.events)
}
