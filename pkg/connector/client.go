package connector

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/bridgev2/status"
	"maunium.net/go/mautrix/event"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
	"github.com/williamkray/mrtc-sip-bridge/pkg/phone"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipaccount"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
	"github.com/williamkray/mrtc-sip-bridge/pkg/widget"
)

// roomFeatures is shared by every portal; this bridge has no message features.
var roomFeatures = &event.RoomFeatures{ID: "com.github.williamkray.mrtcsip.capabilities.v1"}

// SIPClient is one SIP account login: it keeps a REGISTER alive (through its sipaccount.Account) and
// reports bridge state.
type SIPClient struct {
	c     *SIPConnector
	meta  *UserLoginMetadata
	acct  *sipaccount.Account
	login *bridgev2.UserLogin // nil until attach
	log   *zerolog.Logger

	mu       sync.Mutex
	lastSent sipua.State // last state for which a bridge state was sent (StateIdle = none)
	stopping bool        // set by stop until the next start: inbound calls and dials are refused meanwhile
	connGen  int         // bumped on stop/Disconnect; cancels a scheduled Connect retry
}

// newUA is a seam for tests (nil = sipua.New).
var newUA func(sipua.Config) (sipua.UA, error)

// startRetryDelay is how long Connect waits before its single retry after start fails.
var startRetryDelay = 30 * time.Second

var (
	_ bridgev2.NetworkAPI                    = (*SIPClient)(nil)
	_ bridgev2.IdentifierResolvingNetworkAPI = (*SIPClient)(nil)
	_ bridgev2.GhostDMCreatingNetworkAPI     = (*SIPClient)(nil)
)

// newClient creates the client of meta and its (stopped) SIP account. ul is the stored login when known
// (LoadUserLogin), nil during the login flow; it only adds the login fields to the account's logger.
func (c *SIPConnector) newClient(meta *UserLoginMetadata, ul *bridgev2.UserLogin) *SIPClient {
	lg := c.br.Log.With().Str("component", "sip").Str("sip_user", sipua.MaskUsername(meta.Username)).Logger()
	ug := c.br.Log.With().Str("sip_user", sipua.MaskUsername(meta.Username)).Logger()
	if ul != nil {
		ug = c.br.Log.With().Stringer("user_mxid", ul.UserMXID).
			Str("login", string(ul.ID)).Str("sip_user", sipua.MaskUsername(meta.Username)).Logger()
	}
	cl := &SIPClient{c: c, meta: meta, log: &lg}
	cl.acct = c.rt.NewAccount(sipaccount.AccountConfig{
		Creds:    *meta,
		Log:      ug, // no component key: sipua adds its own, the account adds component=sip
		OnState:  cl.onState,
		OnInvite: cl.onInvite,
		NewUA:    newUA,
	})
	return cl
}

func (cl *SIPClient) attach(ul *bridgev2.UserLogin) {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	cl.login = ul
	// Built from the bridge logger, not ul.Log (which carries bridgev2's own login_id field).
	lg := cl.c.br.Log.With().Str("component", "sip").Stringer("user_mxid", ul.UserMXID).
		Str("login", string(ul.ID)).Str("sip_user", sipua.MaskUsername(cl.meta.Username)).Logger()
	cl.log = &lg
}

// start starts the account's UA (no-op if one is running).
func (cl *SIPClient) start(ctx context.Context) error {
	if cl.c.rt == nil {
		return errors.New("SIP connector is not started")
	}
	cl.mu.Lock()
	if !cl.acct.Running() {
		cl.lastSent = sipua.StateIdle
	}
	cl.stopping = false
	cl.mu.Unlock()
	return cl.acct.Start(ctx)
}

// stop ends the login's calls and stops the UA. ctx only carries the caller's span (the unregister runs
// under its own 5 s timeout, detached from ctx's cancellation).
func (cl *SIPClient) stop(ctx context.Context) {
	cl.mu.Lock()
	cl.connGen++
	cl.stopping = true
	login := cl.login
	cl.mu.Unlock()
	if !cl.acct.Running() {
		return
	}
	if login != nil && cl.c.Calls != nil {
		ectx, ecancel := context.WithTimeout(context.WithoutCancel(ctx), 8*time.Second)
		cl.c.Calls.EndLogin(ectx, core.LoginRef(login.ID))
		ecancel()
	}
	cl.acct.Stop(ctx)
}

// onState runs on the UA goroutine (events of stale UAs are already filtered) and must not block.
func (cl *SIPClient) onState(sipua.StateEvent) {
	cl.sendBridgeState()
}

func (cl *SIPClient) sendBridgeState() {
	cl.mu.Lock()
	login := cl.login
	last := cl.acct.State()
	if login == nil || last.State == cl.lastSent {
		cl.mu.Unlock()
		return
	}
	bs, ok := bridgeStateFor(last)
	if !ok {
		cl.mu.Unlock()
		return
	}
	cl.lastSent = last.State
	cl.mu.Unlock()
	login.BridgeState.Send(bs)
}

// dial places an outbound call on the account's UA (sipua.ErrNotRegistered while stopped or stopping).
func (cl *SIPClient) dial(req sipua.DialRequest) (sipua.OutboundCall, error) {
	cl.mu.Lock()
	stopping := cl.stopping
	cl.mu.Unlock()
	if stopping {
		return nil, sipua.ErrNotRegistered
	}
	return cl.acct.Dial(req)
}

// Connect starts the SIP registration (or re-sends state if already running).
func (cl *SIPClient) Connect(ctx context.Context) {
	ctx, span := obs.Start(ctx, "bridge.connect", trace.WithAttributes(cl.spanAttrs()...))
	defer span.End()
	cl.connect(ctx, span, true)
}

// running reports whether a UA is currently started.
func (cl *SIPClient) running() bool { return cl.acct.Running() }

func (cl *SIPClient) connect(ctx context.Context, span trace.Span, retry bool) {
	running := cl.acct.Running()
	cl.mu.Lock()
	login := cl.login
	gen := cl.connGen
	cl.mu.Unlock()
	if running {
		cl.sendBridgeState()
		return
	}
	err := cl.start(ctx)
	if err == nil {
		// The startup sweep ran before logins loaded: install the widget in this login's rooms now.
		cl.c.widgetInst.RequestSweep(ctx, widget.TriggerLogin)
	}
	if err != nil {
		obs.Fail(span, err)
		cl.logFor(ctx).Err(err).Msg("Failed to start SIP client")
		if retry {
			// The timer goroutine never gets the live span: the retry is a linked root.
			from := obs.CarrySpan(context.Background(), ctx)
			time.AfterFunc(startRetryDelay, func() {
				cl.mu.Lock()
				stale := cl.connGen != gen
				cl.mu.Unlock()
				if !stale {
					rctx, rspan := obs.StartLinkedRoot(context.Background(), "bridge.connect", from,
						trace.WithAttributes(cl.spanAttrs()...), trace.WithAttributes(attribute.Bool("retry", true)))
					defer rspan.End()
					cl.connect(rctx, rspan, false)
				}
			})
		}
		if login != nil {
			login.BridgeState.Send(status.BridgeState{
				StateEvent: status.StateUnknownError,
				Error:      "sip-start-failed",
				Message:    err.Error(),
			})
		}
	}
}

// Disconnect stops the SIP registration.
func (cl *SIPClient) Disconnect() {
	ctx, span := obs.Start(context.Background(), "bridge.disconnect", trace.WithAttributes(cl.spanAttrs()...))
	defer span.End()
	cl.stop(ctx)
}

// IsLoggedIn reports whether the account is believed to have valid credentials.
func (cl *SIPClient) IsLoggedIn() bool {
	return cl.meta != nil && cl.acct.State().State != sipua.StateAuthFailed
}

// LogoutRemote stops the registration; SIP has no remote session to invalidate.
func (cl *SIPClient) LogoutRemote(ctx context.Context) {
	ctx, span := obs.Start(ctx, "bridge.logout", trace.WithAttributes(cl.spanAttrs()...))
	defer span.End()
	cl.stop(ctx)
}

// IsThisUser reports whether uid is the account's own number.
func (cl *SIPClient) IsThisUser(_ context.Context, uid networkid.UserID) bool {
	return uid == MakeUserID(cl.meta.DisplayNumber)
}

// GetChatInfo returns the DM info for a portal keyed by E.164.
func (cl *SIPClient) GetChatInfo(_ context.Context, portal *bridgev2.Portal) (*bridgev2.ChatInfo, error) {
	remote := string(portal.ID)
	if !phone.IsE164(remote) {
		return nil, fmt.Errorf("portal ID %q is not an E.164 number", remote)
	}
	return buildChatInfo(cl.login.ID, cl.meta.DisplayNumber, remote), nil
}

// GetUserInfo returns the profile of a ghost.
func (cl *SIPClient) GetUserInfo(_ context.Context, ghost *bridgev2.Ghost) (*bridgev2.UserInfo, error) {
	e164, ok := ParseUserID(ghost.ID)
	if !ok {
		return nil, fmt.Errorf("ghost ID %q is not a phone number", ghost.ID)
	}
	return buildUserInfo(e164), nil
}

// GetCapabilities returns the shared room features.
func (cl *SIPClient) GetCapabilities(_ context.Context, _ *bridgev2.Portal) *event.RoomFeatures {
	return roomFeatures
}

// HandleMatrixMessage rejects all messages: this bridge only handles calls.
func (cl *SIPClient) HandleMatrixMessage(_ context.Context, _ *bridgev2.MatrixMessage) (*bridgev2.MatrixMessageResponse, error) {
	return nil, bridgev2.WrapErrorInStatus(errors.New("this bridge only handles calls; text messages are not sent")).
		WithIsCertain(true).
		WithErrorAsMessage().
		WithSendNotice(false).
		WithErrorReason(event.MessageStatusUnsupported)
}

// ResolveIdentifier resolves a phone number to a ghost and (optionally) its portal.
func (cl *SIPClient) ResolveIdentifier(ctx context.Context, identifier string, createChat bool) (resp *bridgev2.ResolveIdentifierResponse, err error) {
	ctx, span := obs.Start(ctx, "bridge.resolve_identifier", trace.WithAttributes(
		obs.AttrCallLocalE164.String(cl.meta.DisplayNumber),
		attribute.Bool("create_chat", createChat)))
	defer span.End()
	defer func() { obs.Fail(span, err) }()
	e164, err := phone.Normalize(identifier, cl.c.Config.DefaultCountryCode)
	if err != nil {
		return nil, fmt.Errorf("invalid phone number: %w", err)
	}
	span.SetAttributes(obs.AttrCallRemoteE164.String(e164))
	ghost, err := cl.c.br.GetGhostByID(ctx, MakeUserID(e164))
	if err != nil {
		return nil, fmt.Errorf("failed to get ghost: %w", err)
	}
	resp = &bridgev2.ResolveIdentifierResponse{
		Ghost:    ghost,
		UserID:   MakeUserID(e164),
		UserInfo: buildUserInfo(e164),
	}
	if createChat {
		resp.Chat = cl.chatResponse(e164)
	}
	return resp, nil
}

// CreateChatWithGhost starts a DM with a pre-validated ghost.
func (cl *SIPClient) CreateChatWithGhost(_ context.Context, ghost *bridgev2.Ghost) (*bridgev2.CreateChatResponse, error) {
	e164, ok := ParseUserID(ghost.ID)
	if !ok {
		return nil, fmt.Errorf("ghost ID %q is not a phone number", ghost.ID)
	}
	return cl.chatResponse(e164), nil
}

func (cl *SIPClient) chatResponse(e164 string) *bridgev2.CreateChatResponse {
	return &bridgev2.CreateChatResponse{
		PortalKey:  MakePortalKey(e164, cl.login.ID),
		PortalInfo: buildChatInfo(cl.login.ID, cl.meta.DisplayNumber, e164),
	}
}

// onInvite hands an inbound INVITE to the call manager (runs on the diago handler goroutine).
func (cl *SIPClient) onInvite(call sipua.InboundCall) {
	cl.mu.Lock()
	login, stopping := cl.login, cl.stopping
	cl.mu.Unlock()
	if stopping || login == nil || cl.c.Calls == nil || !cl.acct.Running() {
		call.Reject(480, "Temporarily Unavailable")
		return
	}
	cl.c.Calls.HandleInvite(core.LoginRef(login.ID), cl.meta.DisplayNumber, call)
}
