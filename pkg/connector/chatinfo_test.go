package connector

import (
	"testing"

	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/event"

	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
)

func TestBuildUserInfo(t *testing.T) {
	ui := buildUserInfo("+15551234567")
	if ui.Name == nil || *ui.Name != "+15551234567" {
		t.Fatalf("name = %v", ui.Name)
	}
	if len(ui.Identifiers) != 1 || ui.Identifiers[0] != "tel:+15551234567" {
		t.Fatalf("identifiers = %v", ui.Identifiers)
	}
	if ui.IsBot == nil || *ui.IsBot {
		t.Fatal("IsBot should be false")
	}
}

func TestBuildChatInfo(t *testing.T) {
	ci := buildChatInfo("login1", "+15550001111", "+15551234567")
	if ci.Type == nil || *ci.Type != database.RoomTypeDM {
		t.Fatal("not a DM")
	}
	if ci.Name != nil || ci.CanBackfill {
		t.Fatal("name must be nil and backfill off")
	}
	ml := ci.Members
	if !ml.IsFull || ml.OtherUserID != "15551234567" {
		t.Fatalf("member list = %+v", ml)
	}
	if len(ml.MemberMap) != 2 {
		t.Fatalf("members = %d", len(ml.MemberMap))
	}
	owner := ml.MemberMap[""]
	if owner.Sender != "" || !owner.IsFromMe || owner.SenderLogin != "login1" || owner.Membership != event.MembershipJoin {
		t.Fatalf("owner = %+v", owner)
	}
	ghost := ml.MemberMap["15551234567"]
	if ghost.IsFromMe || ghost.UserInfo == nil || *ghost.UserInfo.Name != "+15551234567" {
		t.Fatalf("ghost = %+v", ghost)
	}
	for _, typ := range []event.Type{matrixrtc.CallMember, matrixrtc.CallMemberAlias} {
		if lvl, ok := ml.PowerLevels.Events[typ]; !ok || lvl != 0 {
			t.Fatalf("power level for %v = %d, %v", typ, lvl, ok)
		}
	}
}
