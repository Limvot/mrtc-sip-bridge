package connector

import (
	"go.mau.fi/util/ptr"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/event"

	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
)

// buildUserInfo returns the ghost profile for a remote number.
func buildUserInfo(e164 string) *bridgev2.UserInfo {
	return &bridgev2.UserInfo{
		Name:        ptr.Ptr(e164),
		Identifiers: []string{"tel:" + e164},
		IsBot:       ptr.Ptr(false),
	}
}

// buildChatInfo returns the info for the DM portal between the login owner and remoteE164.
// The owner is a login-only member (no Sender), so bridgev2 never invites a ghost of the owner's own number.
func buildChatInfo(loginID networkid.UserLoginID, ownerE164, remoteE164 string) *bridgev2.ChatInfo {
	m := bridgev2.ChatMemberMap{}
	m.Set(bridgev2.ChatMember{
		EventSender: bridgev2.EventSender{IsFromMe: true, SenderLogin: loginID},
		Membership:  event.MembershipJoin,
	})
	m.Set(bridgev2.ChatMember{
		EventSender: bridgev2.EventSender{Sender: MakeUserID(remoteE164)},
		Membership:  event.MembershipJoin,
		UserInfo:    buildUserInfo(remoteE164),
	})
	return &bridgev2.ChatInfo{
		Type:        ptr.Ptr(database.RoomTypeDM),
		CanBackfill: false,
		Members: &bridgev2.ChatMemberList{
			IsFull:      true,
			OtherUserID: MakeUserID(remoteE164),
			MemberMap:   m,
			PowerLevels: hostkit.CallPowerLevelOverrides(),
		},
	}
}
