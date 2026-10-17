package connector

import (
	"context"
	"fmt"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipaccount"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

var (
	_ bridgev2.NetworkConnector        = (*SIPConnector)(nil)
	_ bridgev2.ConfigValidatingNetwork = (*SIPConnector)(nil)
)

// Start creates the SIP runtime (bind and external IPs, RTP port range, SIP port pool, registrations
// gauge) and the call stack.
func (c *SIPConnector) Start(ctx context.Context) error {
	// The host's PostInit already ran telemetry.Init (obs.Use), so the runtime's gauge callback lands on
	// the current instruments.
	rt, err := sipaccount.NewRuntime(ctx, c.Config.NetConfig(), c.br.Log)
	if err != nil {
		return err
	}
	c.rt = rt
	c.startCalls()
	return c.startWidget(ctx)
}

// LoadUserLogin attaches a client to a stored login. The client is not started here; the framework
// calls Connect.
func (c *SIPConnector) LoadUserLogin(_ context.Context, login *bridgev2.UserLogin) error {
	meta, _ := login.Metadata.(*UserLoginMetadata)
	if !meta.Complete() {
		login.Client = &stubClient{login: login}
		return nil
	}
	cl := c.newClient(meta, login)
	cl.attach(login)
	login.Client = cl
	return nil
}

// GetLoginFlows lists the supported login flows.
func (c *SIPConnector) GetLoginFlows() []bridgev2.LoginFlow {
	return []bridgev2.LoginFlow{{
		ID:          loginFlowPassword,
		Name:        "SIP account",
		Description: "Log in with a SIP username, password and registrar",
	}}
}

// CreateLogin starts a login process for the given flow.
func (c *SIPConnector) CreateLogin(_ context.Context, user *bridgev2.User, flowID string) (bridgev2.LoginProcess, error) {
	if flowID != loginFlowPassword {
		return nil, fmt.Errorf("unknown login flow %q", flowID)
	}
	return &sipLoginProcess{c: c, user: user}, nil
}

// startCalls creates the call stack (LiveKit joiner, service URL resolver, call manager) and routes
// MatrixRTC events to the manager.
func (c *SIPConnector) startCalls() {
	c.Calls, c.lkURL = hostkit.NewCallStack(c.br, &c.Config, hostkit.CallStackDeps{
		Host:        c.CallHost(),
		Dial:        c.dial,
		LoginNumber: c.loginNumber,
		Log:         c.br.Log,
	})
	c.RTC.SetSink(c.Calls)
}

// dial places an outbound call on the SIP UA of login's client (calls.Config.Dial).
func (c *SIPConnector) dial(login core.LoginRef, req sipua.DialRequest) (sipua.OutboundCall, error) {
	ul := c.br.GetCachedUserLoginByID(networkid.UserLoginID(login))
	if ul == nil {
		return nil, sipua.ErrNotRegistered
	}
	cl, ok := ul.Client.(*SIPClient)
	if !ok {
		return nil, sipua.ErrNotRegistered
	}
	return cl.dial(req)
}

// loginNumber returns the login's own E.164 number (calls.Config.LoginNumber), or "" if unknown.
func (c *SIPConnector) loginNumber(login core.LoginRef) string {
	ul := c.br.GetCachedUserLoginByID(networkid.UserLoginID(login))
	if ul == nil {
		return ""
	}
	if meta, ok := ul.Metadata.(*UserLoginMetadata); ok {
		return meta.DisplayNumber
	}
	return ""
}
