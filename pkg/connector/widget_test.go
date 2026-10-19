package connector

import (
	"context"
	"errors"
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/widget"
)

const (
	wAlice = id.UserID("@alice:example.org")
	wBob   = id.UserID("@bob:example.org")
	wMgmt  = id.RoomID("!mgmt:example.org")
	wRoom  = id.RoomID("!room:example.org")
)

func wLogin(id string, owner id.UserID, ready bool) *bridgev2.UserLogin {
	ul := &bridgev2.UserLogin{UserLogin: &database.UserLogin{ID: networkid.UserLoginID(id), UserMXID: owner}}
	if ready {
		ul.Client = &SIPClient{}
	} else {
		ul.Client = &stubClient{}
	}
	return ul
}

func wPortal(id string, receiver networkid.UserLoginID) *bridgev2.Portal {
	return &bridgev2.Portal{Portal: &database.Portal{PortalKey: networkid.PortalKey{ID: networkid.PortalID(id), Receiver: receiver}}}
}

func wUser(mxid id.UserID) *bridgev2.User {
	return &bridgev2.User{User: &database.User{MXID: mxid, ManagementRoom: wMgmt}}
}

func TestResolveWidgetRoom(t *testing.T) {
	aliceL := wLogin("l1", wAlice, true)
	stubL := wLogin("l2", wAlice, false)
	logins := map[networkid.UserLoginID]*bridgev2.UserLogin{"l1": aliceL, "l2": stubL}
	portals := map[id.RoomID]*bridgev2.Portal{
		wRoom:      wPortal("+15551234567", "l1"),
		"!stub":    wPortal("+15551234567", "l2"),
		"!group":   wPortal("not-a-number", "l1"),
		"!norecv":  wPortal("+15551234567", ""),
		"!shared":  wPortal("+15551234567", "l1"),
		"!permerr": wPortal("+15551234567", "l1"),
	}
	var dflt *bridgev2.UserLogin = aliceL
	lk := widgetLookup{
		portalByMXID: func(_ context.Context, r id.RoomID) (*bridgev2.Portal, error) {
			if r == "!err" {
				return nil, errors.New("db down")
			}
			return portals[r], nil
		},
		loginByID:    func(i networkid.UserLoginID) *bridgev2.UserLogin { return logins[i] },
		defaultLogin: func(*bridgev2.User) *bridgev2.UserLogin { return dflt },
		private: func(_ context.Context, r id.RoomID, owner id.UserID) (bool, error) {
			switch r {
			case "!shared":
				return false, nil
			case "!permerr":
				return false, errors.New("no state store")
			}
			return true, nil
		},
	}
	ctx := context.Background()

	refused := func(room id.RoomID, user id.UserID) {
		t.Helper()
		_, err := resolveWidgetRoom(ctx, lk, "!sip", wUser(user), room)
		var re *widget.RefusalError
		if !errors.As(err, &re) || re.Message != widget.MsgNotBridgeRoom {
			t.Errorf("%s as %s: err = %v, want refusal", room, user, err)
		}
	}

	rc, err := resolveWidgetRoom(ctx, lk, "!sip", wUser(wAlice), wRoom)
	if err != nil || rc.Kind != widget.KindPortal || rc.Number != "+15551234567" || rc.Login != aliceL || rc.Refusal != "" {
		t.Errorf("portal: %+v %v", rc, err)
	}
	refused(wRoom, wBob)        // someone else's portal
	refused("!unknown", wAlice) // not a bridge room
	refused("!group", wAlice)   // not a number portal
	refused("!norecv", wAlice)  // no receiver
	refused("!unknown", wBob)

	_, err = resolveWidgetRoom(ctx, lk, "!sip", wUser(wAlice), "!shared")
	var re *widget.RefusalError
	if !errors.As(err, &re) || re.Message != widget.MsgNotPrivateRoom {
		t.Errorf("shared room: err = %v, want not-private refusal", err)
	}
	if _, err = resolveWidgetRoom(ctx, lk, "!sip", wUser(wAlice), "!permerr"); err == nil || errors.As(err, new(*widget.RefusalError)) {
		t.Errorf("privacy check error must be internal, got %v", err)
	}
	if owners := roomOwners(ctx, lk, func(context.Context, id.RoomID) id.UserID { return "" }, wRoom); len(owners) != 1 || owners[0] != wAlice {
		t.Errorf("portal owners = %v", owners)
	}
	if owners := roomOwners(ctx, lk, func(context.Context, id.RoomID) id.UserID { return wAlice }, wMgmt); len(owners) != 1 || owners[0] != wAlice {
		t.Errorf("management owners = %v", owners)
	}
	if owners := roomOwners(ctx, lk, func(context.Context, id.RoomID) id.UserID { return "" }, "!unknown"); owners != nil {
		t.Errorf("unknown room owners = %v", owners)
	}

	rc, err = resolveWidgetRoom(ctx, lk, "!sip", wUser(wAlice), "!stub")
	if err != nil || rc.Login != nil || rc.Refusal == "" || rc.Number != "+15551234567" {
		t.Errorf("stub login: %+v %v", rc, err)
	}
	if _, err = resolveWidgetRoom(ctx, lk, "!sip", wUser(wAlice), "!err"); err == nil || errors.As(err, new(*widget.RefusalError)) {
		t.Errorf("db error must be internal, got %v", err)
	}

	rc, err = resolveWidgetRoom(ctx, lk, "!sip", wUser(wAlice), wMgmt)
	if err != nil || rc.Kind != widget.KindManagement || rc.Number != "" || rc.Login != aliceL {
		t.Errorf("management: %+v %v", rc, err)
	}
	dflt = nil
	rc, err = resolveWidgetRoom(ctx, lk, "!sip", wUser(wAlice), wMgmt)
	if err != nil || rc.Login != nil || rc.Refusal == "" {
		t.Errorf("management without login: %+v %v", rc, err)
	}
}

func TestWidgetInstallerBeforeStart(t *testing.T) {
	var w widgetInstaller
	w.Enqueue("!r:x", "Call +1") // must not panic or block
	if err := w.Ensure(context.Background(), "!r:x", ""); !errors.Is(err, errWidgetNotRunning) {
		t.Errorf("Ensure before start = %v", err)
	}
}

func TestWidgetInstallerRequestSweep(t *testing.T) {
	var w widgetInstaller
	w.RequestSweep(context.Background(), widget.TriggerLogin) // nil-safe before Start
	var got []string
	w.sweepHook = func(_ context.Context, trig string) { got = append(got, trig) }
	w.RequestSweep(context.Background(), widget.TriggerLogin)
	if len(got) != 1 || got[0] != widget.TriggerLogin {
		t.Errorf("sweeps = %v", got)
	}
}

func TestWidgetTitle(t *testing.T) {
	if got := widgetTitle("+15551234567"); got != "Call +15551234567" {
		t.Errorf("title = %q", got)
	}
}

func TestResolveWidgetDevice(t *testing.T) {
	logins := map[networkid.UserLoginID]*bridgev2.UserLogin{
		"l1": wLogin("l1", wAlice, true),
		"l2": wLogin("l2", wAlice, false),
	}
	lookup := func(i networkid.UserLoginID) *bridgev2.UserLogin { return logins[i] }
	rc, err := resolveWidgetDevice(lookup, "!sip", wUser(wAlice), "l1")
	if err != nil || rc.Kind != widget.KindDevice || rc.Login != logins["l1"] || rc.Refusal != "" {
		t.Errorf("ready: %+v %v", rc, err)
	}
	rc, err = resolveWidgetDevice(lookup, "!sip", wUser(wAlice), "l2")
	if err != nil || rc.Kind != widget.KindDevice || rc.Login != nil || rc.Refusal == "" {
		t.Errorf("stub: %+v %v", rc, err)
	}
	for _, c := range []struct {
		u id.UserID
		l networkid.UserLoginID
	}{{wAlice, "gone"}, {wBob, "l1"}} {
		if _, err = resolveWidgetDevice(lookup, "!sip", wUser(c.u), c.l); !errors.Is(err, widget.ErrDeviceLoginGone) {
			t.Errorf("%s/%s: err = %v, want ErrDeviceLoginGone", c.u, c.l, err)
		}
	}
}

func TestDialpadAdapterBeforeStart(t *testing.T) {
	var d dialpadAdapter
	ctx := context.Background()
	if _, err := d.Issue(ctx, wAlice, "l1", wMgmt, "x"); !errors.Is(err, errDialpadNotRunning) {
		t.Errorf("Issue = %v", err)
	}
	if _, err := d.List(ctx, wAlice); !errors.Is(err, errDialpadNotRunning) {
		t.Errorf("List = %v", err)
	}
	if _, err := d.Revoke(ctx, wAlice, "all"); !errors.Is(err, errDialpadNotRunning) {
		t.Errorf("Revoke = %v", err)
	}
	if err := d.AttachNotice(ctx, "id", wMgmt, "$e"); !errors.Is(err, errDialpadNotRunning) {
		t.Errorf("AttachNotice = %v", err)
	}
	c := &SIPConnector{}
	if c.pwaEnabled() {
		t.Error("pwa enabled by default")
	}
}

// The standalone host has no optional features: the widget shows none of those controls.
func TestWidgetDepsHaveNoFeatures(t *testing.T) {
	c := &SIPConnector{br: &bridgev2.Bridge{Log: zerolog.Nop()}}
	if d := c.widgetDeps(); d.Features != nil {
		t.Errorf("standalone widgetDeps().Features = %v, want nil", d.Features)
	}
}
