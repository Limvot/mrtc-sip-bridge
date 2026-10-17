package connector

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
	"github.com/williamkray/mrtc-sip-bridge/pkg/widget"
)

// errWidgetNotRunning is returned by the command-side installer before Start created the widget server.
var errWidgetNotRunning = errors.New("widget: not started yet")

// widgetInstaller is the hostkit.WidgetInstaller handed to the commands in PostInit, before the widget
// server exists (it is created in Start). It delegates to the server once set.
type widgetInstaller struct {
	mu  sync.RWMutex
	srv *widget.Server
	// sweepHook replaces the server's sweep request (test seam).
	sweepHook func(ctx context.Context, trigger string)
}

var _ hostkit.WidgetInstaller = (*widgetInstaller)(nil)

func (w *widgetInstaller) set(s *widget.Server) {
	w.mu.Lock()
	w.srv = s
	w.mu.Unlock()
}

func (w *widgetInstaller) get() *widget.Server {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.srv
}

func (w *widgetInstaller) Ensure(ctx context.Context, roomID id.RoomID, title string) error {
	s := w.get()
	if s == nil {
		return errWidgetNotRunning
	}
	return s.Installer().Ensure(ctx, roomID, title)
}

func (w *widgetInstaller) Enqueue(roomID id.RoomID, title string) {
	if s := w.get(); s != nil {
		s.Installer().Enqueue(roomID, title)
	}
}

// RequestSweep asks the installer to sweep rooms not yet done (nil-safe before Start). A login that
// connects after the startup sweep ran would otherwise miss the widget until the periodic sweep.
func (w *widgetInstaller) RequestSweep(ctx context.Context, trigger string) {
	w.mu.RLock()
	hook, s := w.sweepHook, w.srv
	w.mu.RUnlock()
	if hook != nil {
		hook(ctx, trigger)
		return
	}
	if s != nil {
		s.Installer().RequestSweep(ctx, trigger)
	}
}

// errDialpadNotRunning is returned by the command-side Dialpad adapter before Start created the server.
var errDialpadNotRunning = errors.New("dialpad: the Dialpad app is not enabled on this bridge")

// dialpadAdapter is the hostkit.DialpadLinks handed to the commands in PostInit, before the widget
// server exists (it is created in Start). It delegates to the server's DialpadLinks once set.
type dialpadAdapter struct {
	mu    sync.RWMutex
	links hostkit.DialpadLinks
}

var _ hostkit.DialpadLinks = (*dialpadAdapter)(nil)

func (d *dialpadAdapter) set(l hostkit.DialpadLinks) {
	d.mu.Lock()
	d.links = l
	d.mu.Unlock()
}

func (d *dialpadAdapter) get() hostkit.DialpadLinks {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return d.links
}

func (d *dialpadAdapter) Issue(ctx context.Context, user id.UserID, login networkid.UserLoginID, room id.RoomID, name string) (hostkit.LinkIssue, error) {
	l := d.get()
	if l == nil {
		return hostkit.LinkIssue{}, errDialpadNotRunning
	}
	return l.Issue(ctx, user, login, room, name)
}

func (d *dialpadAdapter) AttachNotice(ctx context.Context, linkID string, room id.RoomID, evt id.EventID) error {
	l := d.get()
	if l == nil {
		return errDialpadNotRunning
	}
	return l.AttachNotice(ctx, linkID, room, evt)
}

func (d *dialpadAdapter) List(ctx context.Context, user id.UserID) ([]hostkit.LinkInfo, error) {
	l := d.get()
	if l == nil {
		return nil, errDialpadNotRunning
	}
	return l.List(ctx, user)
}

func (d *dialpadAdapter) Revoke(ctx context.Context, user id.UserID, linkID string) (int, error) {
	l := d.get()
	if l == nil {
		return 0, errDialpadNotRunning
	}
	return l.Revoke(ctx, user, linkID)
}

// pwaEnabled reports whether the Dialpad PWA is configured (it needs the widget).
func (c *SIPConnector) pwaEnabled() bool {
	return c.Config.Widget.Enabled && c.Config.Widget.PWA.Enabled
}

// widgetDeps builds the widget.Deps for this host.
func (c *SIPConnector) widgetDeps() widget.Deps {
	return widget.Deps{
		Bridge: c.br,
		Calls: func() hostkit.CallAPI {
			if c.Calls == nil {
				return nil
			}
			return c.Calls
		},
		ResolveRoom:        c.widgetResolveRoom,
		RoomOwners:         c.widgetRoomOwners,
		EligibleRooms:      c.widgetEligibleRooms,
		DefaultCountryCode: c.Config.DefaultCountryCode,
		Log:                c.br.Log.With().Str("component", "widget").Logger(),
	}
}

// startWidget mounts and starts the widget server (widget.enabled), or runs the one-shot removal sweep
// (disabled with remove_when_disabled). Called from Start after the call stack exists.
func (c *SIPConnector) startWidget(ctx context.Context) error {
	cfg := c.Config.Widget
	if !cfg.Enabled {
		if cfg.RemoveWhenDisabled {
			d := c.widgetDeps()
			go func() {
				if err := widget.Remove(c.br.Log.WithContext(context.Background()), d); err != nil {
					c.br.Log.Warn().Err(err).Msg("Widget removal sweep failed")
				}
			}()
		}
		return nil
	}
	deps := c.widgetDeps()
	if c.pwaEnabled() {
		log := c.br.Log.With().Str("component", "widget").Logger()
		store, err := widget.OpenDeviceStore(ctx, c.br, cfg, log)
		if err != nil {
			return fmt.Errorf("start widget: device store: %w", err)
		}
		deps.Devices = store
		deps.ResolveDevice = c.widgetResolveDevice
		deps.CommandPrefix = c.commandPrefix()
	}
	srv, err := widget.New(cfg, deps)
	if err != nil {
		return fmt.Errorf("start widget: %w", err)
	}
	if err = widget.Mount(c.br, srv); err != nil {
		return fmt.Errorf("start widget: %w", err)
	}
	srv.Start()
	c.widget = srv
	c.widgetInst.set(srv)
	if c.pwaEnabled() {
		c.dialpad.set(srv.DialpadLinks())
	}
	return nil
}

func (c *SIPConnector) widgetResolveDevice(ctx context.Context, user *bridgev2.User, login networkid.UserLoginID) (widget.RoomContext, error) {
	return resolveWidgetDevice(c.br.GetCachedUserLoginByID, c.commandPrefix(), user, login)
}

// resolveWidgetDevice applies the commands' login rules to a device's pinned login: it must exist and
// belong to the user (else widget.ErrDeviceLoginGone); a stub (credential-less) login yields no Login
// and the "not logged in" refusal.
func resolveWidgetDevice(loginByID func(networkid.UserLoginID) *bridgev2.UserLogin, prefix string, user *bridgev2.User, login networkid.UserLoginID) (widget.RoomContext, error) {
	ul := loginByID(login)
	if ul == nil || user == nil || ul.UserMXID != user.MXID {
		return widget.RoomContext{}, widget.ErrDeviceLoginGone
	}
	rc := widget.RoomContext{Kind: widget.KindDevice}
	if _, ok := loginClient(ul); !ok {
		rc.Refusal = fmt.Sprintf("You are not logged in to this bridge. Use `%s login`.", prefix)
		return rc, nil
	}
	rc.Login = ul
	return rc, nil
}

// stopWidget stops the widget server, if any.
func (c *SIPConnector) stopWidget() {
	if c.widget != nil {
		c.widget.Stop()
	}
}

// enqueueWidget schedules the widget install for a room ResolveCallRoom just created.
func (c *SIPConnector) enqueueWidget(roomID id.RoomID, e164 string) {
	c.widgetInst.Enqueue(roomID, widgetTitle(e164))
}

func widgetTitle(e164 string) string { return "Call " + e164 }

// widgetLookup is the bridge access of room resolution (a seam for tests).
type widgetLookup struct {
	portalByMXID func(ctx context.Context, roomID id.RoomID) (*bridgev2.Portal, error)
	loginByID    func(id networkid.UserLoginID) *bridgev2.UserLogin
	defaultLogin func(user *bridgev2.User) *bridgev2.UserLogin
	// private reports whether room has no member besides the bot, ghosts and owner.
	private func(ctx context.Context, room id.RoomID, owner id.UserID) (bool, error)
}

func (c *SIPConnector) widgetLookup() widgetLookup {
	return widgetLookup{
		portalByMXID: c.br.GetPortalByMXID,
		loginByID:    c.br.GetCachedUserLoginByID,
		defaultLogin: (*bridgev2.User).GetDefaultLogin,
		private: func(ctx context.Context, room id.RoomID, owner id.UserID) (bool, error) {
			return widget.PrivateRoom(ctx, c.br, room, owner)
		},
	}
}

func (c *SIPConnector) widgetResolveRoom(ctx context.Context, user *bridgev2.User, roomID id.RoomID) (widget.RoomContext, error) {
	return resolveWidgetRoom(ctx, c.widgetLookup(), c.commandPrefix(), user, roomID)
}

// resolveWidgetRoom applies the commandLogin rules: a number portal of one of the user's logins, or the
// user's management room (default login, dial by number only). Anything else is refused. A valid room
// whose login is not ready yields no error, a nil Login and the refusal text.
func resolveWidgetRoom(ctx context.Context, lk widgetLookup, prefix string, user *bridgev2.User, roomID id.RoomID) (widget.RoomContext, error) {
	notLoggedIn := fmt.Sprintf("You are not logged in to this bridge. Use `%s login`.", prefix)
	ready := func(rc widget.RoomContext, ul *bridgev2.UserLogin) widget.RoomContext {
		if _, ok := loginClient(ul); !ok {
			rc.Refusal = notLoggedIn
			return rc
		}
		rc.Login = ul
		return rc
	}
	if roomID != "" && user.ManagementRoom == roomID {
		rc := widget.RoomContext{Kind: widget.KindManagement}
		if err := requirePrivate(ctx, lk, roomID, user.MXID); err != nil {
			return widget.RoomContext{}, err
		}
		ul := lk.defaultLogin(user)
		if ul == nil {
			rc.Refusal = notLoggedIn
			return rc, nil
		}
		return ready(rc, ul), nil
	}
	portal, err := lk.portalByMXID(ctx, roomID)
	if err != nil {
		return widget.RoomContext{}, fmt.Errorf("get portal: %w", err)
	}
	if portal == nil || portal.Receiver == "" {
		return widget.RoomContext{}, widget.Refuse(widget.MsgNotBridgeRoom)
	}
	ul := lk.loginByID(portal.Receiver)
	if ul == nil || ul.UserMXID != user.MXID {
		return widget.RoomContext{}, widget.Refuse(widget.MsgNotBridgeRoom)
	}
	number, ok := portalNumber(portal)
	if !ok {
		return widget.RoomContext{}, widget.Refuse(widget.MsgNotBridgeRoom)
	}
	if err = requirePrivate(ctx, lk, roomID, user.MXID); err != nil {
		return widget.RoomContext{}, err
	}
	return ready(widget.RoomContext{Kind: widget.KindPortal, Number: number}, ul), nil
}

// requirePrivate refuses rooms with members other than the bot, ghosts and owner (WIDGET-SPEC §5.2).
func requirePrivate(ctx context.Context, lk widgetLookup, roomID id.RoomID, owner id.UserID) error {
	ok, err := lk.private(ctx, roomID, owner)
	if err != nil {
		return fmt.Errorf("check room privacy: %w", err)
	}
	if !ok {
		return widget.Refuse(widget.MsgNotPrivateRoom)
	}
	return nil
}

// widgetRoomOwners names the only user allowed in a room for the installer's privacy rule: the
// receiver's user for a portal, the user for a management room.
func (c *SIPConnector) widgetRoomOwners(ctx context.Context, room id.RoomID) []id.UserID {
	return roomOwners(ctx, c.widgetLookup(), func(ctx context.Context, room id.RoomID) id.UserID {
		users, err := c.br.DB.UserLogin.GetAllUserIDsWithLogins(ctx)
		if err != nil {
			return ""
		}
		for _, uid := range users {
			if u, err := c.br.GetExistingUserByMXID(ctx, uid); err == nil && u != nil && u.ManagementRoom == room {
				return uid
			}
		}
		return ""
	}, room)
}

func roomOwners(ctx context.Context, lk widgetLookup, mgmtOwner func(context.Context, id.RoomID) id.UserID, room id.RoomID) []id.UserID {
	if portal, err := lk.portalByMXID(ctx, room); err == nil && portal != nil && portal.Receiver != "" {
		if ul := lk.loginByID(portal.Receiver); ul != nil {
			return []id.UserID{ul.UserMXID}
		}
		return nil
	}
	if uid := mgmtOwner(ctx, room); uid != "" {
		return []id.UserID{uid}
	}
	return nil
}

// loginClient reports whether ul has a real SIP client (not the credential-less stub).
func loginClient(ul *bridgev2.UserLogin) (*SIPClient, bool) {
	cl, ok := ul.Client.(*SIPClient)
	return cl, ok && cl != nil
}

func (c *SIPConnector) widgetEligibleRooms(ctx context.Context) ([]widget.RoomRef, error) {
	return eligibleWidgetRooms(ctx, c.br)
}

// eligibleWidgetRooms lists every number portal (with a room and a live SIP login as receiver) and the
// existing management room of every user with a SIP login.
func eligibleWidgetRooms(ctx context.Context, br *bridgev2.Bridge) ([]widget.RoomRef, error) {
	var refs []widget.RoomRef
	portals, err := br.DB.Portal.GetAllWithMXID(ctx)
	if err != nil {
		return nil, fmt.Errorf("list portals: %w", err)
	}
	for _, p := range portals {
		if p.MXID == "" || p.Receiver == "" {
			continue
		}
		ul := br.GetCachedUserLoginByID(p.Receiver)
		if ul == nil {
			continue
		}
		if _, ok := loginClient(ul); !ok {
			continue
		}
		bp := &bridgev2.Portal{Portal: p}
		if n, ok := portalNumber(bp); ok {
			refs = append(refs, widget.RoomRef{RoomID: p.MXID, Title: widgetTitle(n)})
		}
	}
	users, err := br.DB.UserLogin.GetAllUserIDsWithLogins(ctx)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	for _, uid := range users {
		u, err := br.GetExistingUserByMXID(ctx, uid)
		if err != nil || u == nil || u.ManagementRoom == "" {
			continue
		}
		for _, ul := range u.GetUserLogins() {
			if _, ok := loginClient(ul); ok {
				refs = append(refs, widget.RoomRef{RoomID: u.ManagementRoom})
				break
			}
		}
	}
	return refs, nil
}
