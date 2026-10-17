package connector

import (
	"testing"
	"time"

	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

func TestBridgeStateFor(t *testing.T) {
	tests := []struct {
		name    string
		ev      sipua.StateEvent
		ok      bool
		state   status.BridgeStateEvent
		errCode status.BridgeStateErrorCode
		message string
		action  status.BridgeStateUserAction
	}{
		{name: "idle", ev: sipua.StateEvent{State: sipua.StateIdle}},
		{name: "stopped", ev: sipua.StateEvent{State: sipua.StateStopped}},
		{name: "registering", ev: sipua.StateEvent{State: sipua.StateRegistering}, ok: true, state: status.StateConnecting},
		{name: "registered", ev: sipua.StateEvent{State: sipua.StateRegistered}, ok: true, state: status.StateConnected},
		{
			name: "auth failed",
			ev:   sipua.StateEvent{State: sipua.StateAuthFailed, StatusCode: 403, Reason: "Forbidden"},
			ok:   true, state: status.StateBadCredentials, errCode: "sip-auth-failed",
			message: "SIP registrar rejected the credentials (403 Forbidden)", action: status.UserActionRelogin,
		},
		{
			name: "transient with status",
			ev:   sipua.StateEvent{State: sipua.StateTransientError, StatusCode: 503, Reason: "Service Unavailable", NextRetry: 4 * time.Second},
			ok:   true, state: status.StateTransientDisconnect, errCode: "sip-register-failed",
			message: "SIP registration failed (503 Service Unavailable); retrying in 4s",
		},
		{
			name: "transient without status",
			ev:   sipua.StateEvent{State: sipua.StateTransientError, Reason: "timeout", NextRetry: 2 * time.Second},
			ok:   true, state: status.StateTransientDisconnect, errCode: "sip-register-failed",
			message: "SIP registration failed (timeout); retrying in 2s",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bs, ok := bridgeStateFor(tt.ev)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if bs.StateEvent != tt.state || bs.Error != tt.errCode || bs.Message != tt.message || bs.UserAction != tt.action {
				t.Errorf("got %+v", bs)
			}
		})
	}
}
