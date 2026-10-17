package connector

import (
	"context"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"
)

// stubClient serves logins that have no usable SIP credentials (list and logout only).
type stubClient struct {
	login *bridgev2.UserLogin
}

var _ bridgev2.NetworkAPI = (*stubClient)(nil)

// Connect reports that the login needs to be redone.
func (s *stubClient) Connect(_ context.Context) {
	s.login.BridgeState.Send(status.BridgeState{
		StateEvent: status.StateBadCredentials,
		Error:      "sip-no-credentials",
		Message:    "This login has no SIP credentials; log in again",
		UserAction: status.UserActionRelogin,
	})
}

// Disconnect does nothing.
func (s *stubClient) Disconnect() {}

// IsLoggedIn is always false.
func (s *stubClient) IsLoggedIn() bool { return false }

// LogoutRemote does nothing.
func (s *stubClient) LogoutRemote(_ context.Context) {}

// IsThisUser is always false.
func (s *stubClient) IsThisUser(_ context.Context, _ networkid.UserID) bool { return false }

// GetChatInfo is unsupported.
func (s *stubClient) GetChatInfo(_ context.Context, _ *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	return nil, bridgev2.ErrNotLoggedIn
}

// GetUserInfo is unsupported.
func (s *stubClient) GetUserInfo(_ context.Context, _ *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	return nil, bridgev2.ErrNotLoggedIn
}

// GetCapabilities returns the shared room features.
func (s *stubClient) GetCapabilities(_ context.Context, _ *bridgev2.Portal) *event.RoomFeatures {
	return roomFeatures
}

// HandleMatrixMessage is unsupported.
func (s *stubClient) HandleMatrixMessage(_ context.Context, _ *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	return nil, bridgev2.ErrNotLoggedIn
}
