package connector

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/bridgev2/status"

	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
	"github.com/williamkray/mrtc-sip-bridge/pkg/obs"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipaccount"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

const (
	loginFlowPassword   = "password"
	loginStepCredential = "com.github.williamkray.mrtcsip.credentials"
	loginStepComplete   = "com.github.williamkray.mrtcsip.complete"
	loginRegisterWait   = 30 * time.Second
)

// Provisioning API error codes (the bot command shows only the message).
const (
	errCodeBadCredentials  = "COM.GITHUB.WILLIAMKRAY.MRTCSIP.BAD_CREDENTIALS"
	errCodeRegisterTimeout = "COM.GITHUB.WILLIAMKRAY.MRTCSIP.REGISTER_TIMEOUT"
	errCodeRegisterFailed  = "COM.GITHUB.WILLIAMKRAY.MRTCSIP.REGISTER_FAILED"
)

// loginRespError builds a bridgev2.RespError: Error() is just msg (bot commands), while the provisioning
// API returns code and status.
func loginRespError(code string, status int, format string, args ...any) error {
	return bridgev2.RespError{ErrCode: code, Err: fmt.Sprintf(format, args...), StatusCode: status}
}

// sipLoginProcess is the SIP username/password/registrar login.
type sipLoginProcess struct {
	c    *SIPConnector
	user *bridgev2.User

	// mu guards the fields below: bridgev2 runs Cancel and SubmitUserInput in separate goroutines.
	mu         sync.Mutex
	pending    *SIPClient
	cancelled  bool
	cancelWait context.CancelFunc // cancels the ctx used by WaitFirstResult
}

var (
	_ bridgev2.LoginProcess          = (*sipLoginProcess)(nil)
	_ bridgev2.LoginProcessUserInput = (*sipLoginProcess)(nil)
)

// Start returns the credentials prompt.
func (p *sipLoginProcess) Start(_ context.Context) (*bridgev2.LoginStep, error) {
	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeUserInput,
		StepID: loginStepCredential,
		Instructions: "Enter your SIP account details. Answer each prompt as its own message (passwords may contain spaces). " +
			"Registering will take over incoming calls for this account; if a desk phone or ATA uses the same " +
			"account, calls may stop reaching it.",
		UserInputParams: &bridgev2.LoginUserInputParams{Fields: hostkit.LoginFields(p.c.Config.DefaultCountryCode)},
	}, nil
}

// clearPending forgets the pending client (after it was stopped or saved).
func (p *sipLoginProcess) clearPending() {
	p.mu.Lock()
	p.pending = nil
	p.cancelWait = nil
	p.mu.Unlock()
}

func (p *sipLoginProcess) isCancelled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cancelled
}

// await waits for the first registration result of an already started client. It records the client as
// pending so Cancel can stop it and abort the wait. On error the client has been stopped.
func (p *sipLoginProcess) await(ctx context.Context, client *SIPClient, registrar string) (sipua.StateEvent, error) {
	waitCtx, cancelWait := context.WithCancel(ctx)
	defer cancelWait()
	p.mu.Lock()
	if p.cancelled {
		p.mu.Unlock()
		client.stop(ctx)
		return sipua.StateEvent{}, errors.New("login cancelled")
	}
	p.pending = client
	p.cancelWait = cancelWait
	p.mu.Unlock()

	ev, err := client.acct.WaitFirstResult(waitCtx, loginRegisterWait)
	if err == nil {
		return ev, nil
	}
	p.clearPending()
	client.stop(ctx)
	switch {
	case p.isCancelled():
		return sipua.StateEvent{}, errors.New("login cancelled")
	case ctx.Err() != nil:
		return sipua.StateEvent{}, fmt.Errorf("login interrupted: %w", ctx.Err())
	}
	return sipua.StateEvent{}, registerTimeoutError{registrar: registrar}
}

// registerTimeoutError is the await error for a registration that produced no result in time.
type registerTimeoutError struct{ registrar string }

func (e registerTimeoutError) Error() string { return "timed out registering with " + e.registrar }

// SubmitUserInput registers with the SIP server and, on success, saves the login.
func (p *sipLoginProcess) SubmitUserInput(ctx context.Context, input map[string]string) (step *bridgev2.LoginStep, err error) {
	// The input map (password) is never recorded; account attributes are added once it is parsed.
	ctx, span := obs.Start(ctx, "bridge.login.submit", trace.WithSpanKind(trace.SpanKindConsumer))
	defer span.End()
	outcome := "error"
	defer func() {
		span.SetAttributes(obs.AttrOutcome.String(outcome))
		obs.Fail(span, err)
		obs.M().Logins.Add(ctx, 1, metric.WithAttributes(obs.AttrOutcome.String(outcome)))
	}()
	meta, err := sipaccount.ParseInput(input, p.c.Config.DefaultCountryCode)
	if err != nil {
		return nil, err
	}
	span.SetAttributes(accountAttrs(meta)...)
	span.SetAttributes(obs.AttrCallLocalE164.String(meta.DisplayNumber))
	host, _, _ := net.SplitHostPort(meta.Registrar)
	loginID := MakeLoginID(meta.DisplayNumber, host)
	if p.isCancelled() {
		return nil, errors.New("login cancelled")
	}
	// Re-login on an account that is already registered here: release its registration and SIP port first,
	// otherwise we would register a second Contact and only then unregister the old one.
	if old := p.c.br.GetCachedUserLoginByID(loginID); old != nil && old.UserMXID == p.user.MXID {
		if oldClient, ok := old.Client.(*SIPClient); ok && oldClient.running() {
			oldClient.stop(ctx)
		}
	}

	client := p.c.newClient(meta, nil)
	if err = client.start(ctx); err != nil {
		return nil, fmt.Errorf("could not start SIP client: %w", err)
	}
	ev, err := p.await(ctx, client, meta.Registrar)
	if err != nil {
		var te registerTimeoutError
		if errors.As(err, &te) {
			outcome = "timeout"
			return nil, loginRespError(errCodeRegisterTimeout, http.StatusGatewayTimeout, "%s", err.Error())
		}
		return nil, err
	}
	switch ev.State {
	case sipua.StateRegistered:
	case sipua.StateAuthFailed:
		outcome = "auth_failed"
		p.clearPending()
		client.stop(ctx)
		return nil, loginRespError(errCodeBadCredentials, http.StatusForbidden,
			"the registrar rejected the credentials (SIP %d %s)", ev.StatusCode, ev.Reason)
	default:
		outcome = "transient_error"
		p.clearPending()
		client.stop(ctx)
		return nil, loginRespError(errCodeRegisterFailed, http.StatusBadGateway,
			"could not register with %s: %s", meta.Registrar, ev.Reason)
	}

	// A Cancel racing with the registration result must not leave a saved login with a stopped client.
	p.mu.Lock()
	cancelled := p.cancelled
	p.mu.Unlock()
	if cancelled {
		client.stop(ctx)
		return nil, errors.New("login cancelled")
	}
	saveCtx, saveSpan := obs.StartChild(ctx, "bridge.login.save")
	ul, err := p.user.NewLogin(saveCtx, &database.UserLogin{
		ID:            loginID,
		RemoteName:    meta.DisplayNumber,
		RemoteProfile: status.RemoteProfile{Phone: meta.DisplayNumber},
		Metadata:      meta,
	}, &bridgev2.NewLoginParams{LoadUserLogin: func(_ context.Context, ul *bridgev2.UserLogin) error {
		client.attach(ul)
		ul.Client = client
		return nil
	}})
	obs.Fail(saveSpan, err)
	saveSpan.End()
	if err != nil {
		p.clearPending()
		client.stop(ctx)
		return nil, fmt.Errorf("failed to save login: %w", err)
	}
	client.sendBridgeState()
	p.clearPending()
	outcome = "ok"
	return &bridgev2.LoginStep{
		Type:   bridgev2.LoginStepTypeComplete,
		StepID: loginStepComplete,
		Instructions: fmt.Sprintf("Registered %s via %s. Incoming calls ring your Matrix clients; start a call in a number's room or use `%s call <number>` to call out.",
			meta.DisplayNumber, meta.Registrar, p.c.commandPrefix()),
		CompleteParams: &bridgev2.LoginCompleteParams{UserLoginID: loginID, UserLogin: ul},
	}, nil
}

// Cancel stops any client that registered but was not saved, and aborts a registration wait in progress.
func (p *sipLoginProcess) Cancel() {
	p.mu.Lock()
	p.cancelled = true
	pending := p.pending
	p.pending = nil
	cancel := p.cancelWait
	p.cancelWait = nil
	p.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if pending != nil {
		pending.stop(context.Background())
	}
}
