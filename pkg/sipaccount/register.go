package sipaccount

import (
	"context"
	"errors"
	"time"

	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

// ErrRegisterTimeout is returned by Register when no result arrived in time.
var ErrRegisterTimeout = errors.New("sipaccount: timed out waiting for SIP registration result")

// Register starts a, waits up to timeout for the first result and stops a unless the result is
// StateRegistered. On ctx cancellation it stops a and returns ctx.Err(). Used by both login flows.
func Register(ctx context.Context, a *Account, timeout time.Duration) (sipua.StateEvent, error) {
	if err := a.Start(ctx); err != nil {
		return sipua.StateEvent{}, err
	}
	ev, err := a.WaitFirstResult(ctx, timeout)
	if err != nil {
		a.Stop(ctx)
		if ctx.Err() != nil {
			return sipua.StateEvent{}, ctx.Err()
		}
		return sipua.StateEvent{}, err
	}
	if ev.State != sipua.StateRegistered {
		a.Stop(ctx)
	}
	return ev, nil
}

// Outcome maps a Register result to the closed enum used for mrtcsip.logins and span attributes:
// "ok", "auth_failed", "transient_error", "timeout", "error".
func Outcome(ev sipua.StateEvent, err error) string {
	switch {
	case errors.Is(err, ErrRegisterTimeout):
		return "timeout"
	case err != nil:
		return "error"
	case ev.State == sipua.StateRegistered:
		return "ok"
	case ev.State == sipua.StateAuthFailed:
		return "auth_failed"
	case ev.State == sipua.StateTransientError:
		return "transient_error"
	default:
		return "error"
	}
}
