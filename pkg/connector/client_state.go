package connector

import (
	"fmt"
	"time"

	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

// bridgeStateFor maps a SIP UA state to a bridge state. ok is false for states that are not reported.
func bridgeStateFor(ev sipua.StateEvent) (status.BridgeState, bool) {
	switch ev.State {
	case sipua.StateRegistering:
		return status.BridgeState{StateEvent: status.StateConnecting}, true
	case sipua.StateRegistered:
		return status.BridgeState{StateEvent: status.StateConnected}, true
	case sipua.StateAuthFailed:
		return status.BridgeState{
			StateEvent: status.StateBadCredentials,
			Error:      "sip-auth-failed",
			Message:    fmt.Sprintf("SIP registrar rejected the credentials (%d %s)", ev.StatusCode, ev.Reason),
			UserAction: status.UserActionRelogin,
		}, true
	case sipua.StateTransientError:
		reason := ev.Reason
		if ev.StatusCode != 0 {
			reason = fmt.Sprintf("%d %s", ev.StatusCode, ev.Reason)
		}
		return status.BridgeState{
			StateEvent: status.StateTransientDisconnect,
			Error:      "sip-register-failed",
			Message:    fmt.Sprintf("SIP registration failed (%s); retrying in %s", reason, ev.NextRetry.Round(time.Second)),
		}, true
	default:
		return status.BridgeState{}, false
	}
}
