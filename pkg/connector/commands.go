package connector

import (
	"fmt"

	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/commands"

	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
	"github.com/williamkray/mrtc-sip-bridge/pkg/phone"
)

// RegisterCommands adds the call, dtmf and hangup bot commands to proc (hostkit.RegisterCallCommands).
// Call from PostInit (after the bridge is initialised, so br.Bridge.Commands exists). Every handler is
// wrapped in hostkit.WithSpan, which records only the command name, never its arguments.
func (c *SIPConnector) RegisterCommands(proc *commands.Processor) {
	deps := hostkit.CommandDeps{
		Calls: func() hostkit.CallAPI {
			if c.Calls == nil {
				return nil
			}
			return c.Calls
		},
		Login:      commandLoginOrRefusal,
		RoomNumber: portalNumber,
		RedactDTMF: c.Config.RedactDTMFCommand(),
	}
	if c.Config.Widget.Enabled {
		deps.Widget = &c.widgetInst
	}
	if c.pwaEnabled() {
		deps.Dialpad = &c.dialpad
	}
	hostkit.RegisterCallCommands(proc, deps)
}

// commandPrefix returns the configured bot command prefix.
func (c *SIPConnector) commandPrefix() string {
	if c.br != nil && c.br.Config != nil && c.br.Config.CommandPrefix != "" {
		return c.br.Config.CommandPrefix
	}
	return "!sip"
}

// commandLogin picks the login a command acts on: the receiver of the portal the command was sent in
// (it must belong to the sender), else the sender's default login. nil if there is none.
func commandLogin(ce *commands.Event) *bridgev2.UserLogin {
	if ce.Portal != nil && ce.Portal.Receiver != "" {
		ul := ce.Bridge.GetCachedUserLoginByID(ce.Portal.Receiver)
		if ul == nil || ul.UserMXID != ce.User.MXID {
			return nil
		}
		return ul
	}
	return ce.User.GetDefaultLogin()
}

// commandLoginOrRefusal is commandLogin in the hostkit.CommandDeps.Login form.
func commandLoginOrRefusal(ce *commands.Event) (*bridgev2.UserLogin, string) {
	if ul := commandLogin(ce); ul != nil {
		return ul, ""
	}
	return nil, msgNoLogin(ce)
}

func msgNoLogin(ce *commands.Event) string {
	return fmt.Sprintf("You are not logged in to this bridge. Use `%s login`.", ce.Bridge.Config.CommandPrefix)
}

// portalNumber returns the remote E.164 of a standalone portal (its ID); every portal is a 1:1 number room.
func portalNumber(p *bridgev2.Portal) (string, bool) {
	if e164 := string(p.ID); phone.IsE164(e164) {
		return e164, true
	}
	return "", false
}
