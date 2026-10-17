// Package connector is the standalone bridgev2 host for the SIP call core.
package connector

import (
	"sync/atomic"

	"go.mau.fi/util/configupgrade"
	"maunium.net/go/mautrix/bridgev2"
	"maunium.net/go/mautrix/bridgev2/database"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/calls"
	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
	"github.com/williamkray/mrtc-sip-bridge/pkg/rtc"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipaccount"
	"github.com/williamkray/mrtc-sip-bridge/pkg/widget"
)

// SIPConnector implements the framework-facing half of bridgev2.NetworkConnector for SIP.
type SIPConnector struct {
	br     *bridgev2.Bridge
	Config hostkit.Config
	RTC    *core.Ingest // created in Init with a core.LogSink; Start swaps in Calls as the sink
	Calls  *calls.Manager
	lkURL  *rtc.ServiceURLResolver

	// rt is the SIP runtime (bind/external IP, port pool, registrations gauge), set in Start.
	rt *sipaccount.Runtime

	// widget is the Dialpad widget server (nil when disabled); widgetInst is the installer handle the bot
	// commands hold from PostInit on (it delegates to widget once Start created it).
	widget     *widget.Server
	widgetInst widgetInstaller
	// dialpad is the late-bound hostkit.DialpadLinks for the dialpad-* commands (set in Start when widget.pwa is on).
	dialpad dialpadAdapter

	// configPhase is true between GetConfig and Init: mxmain renders its example-config template from
	// GetName in that window (see GetName).
	configPhase atomic.Bool
}

// Init stores the bridge and creates the MatrixRTC ingest with the log-only sink.
func (c *SIPConnector) Init(br *bridgev2.Bridge) {
	c.configPhase.Store(false)
	c.br = br
	lg := br.Log.With().Str("component", "matrixrtc").Logger()
	c.RTC = core.NewIngest(core.LogSink{Log: &lg})
}

// GetName returns the bridge identity.
func (c *SIPConnector) GetName() bridgev2.BridgeName {
	// The mxmain config template renders appservice.bot.avatar as `$<<.NetworkIcon>>`; with an empty icon
	// that is a YAML null, which the config upgrader reports as "Ignoring incorrect config field type
	// !!null" on every start. While the config is being generated or upgraded, return a literal "" instead.
	// At runtime NetworkIcon stays empty (no icon).
	var icon id.ContentURIString
	if c.configPhase.Load() {
		icon = `""`
	}
	return bridgev2.BridgeName{
		NetworkIcon:          icon,
		DisplayName:          "SIP",
		NetworkURL:           "https://github.com/williamkray/mrtc-sip-bridge",
		NetworkID:            "sip",
		BeeperBridgeType:     "github.com/williamkray/mrtc-sip-bridge",
		DefaultPort:          29319,
		DefaultCommandPrefix: "!sip",
	}
}

// GetConfig returns the example config, the struct the framework decodes into, and the upgrader.
func (c *SIPConnector) GetConfig() (string, any, configupgrade.Upgrader) {
	c.configPhase.Store(true)
	return hostkit.ExampleYAML, &c.Config, upgrader
}

// upgrader copies every key from the user's file into the new base.
var upgrader = configupgrade.SimpleUpgrader(func(h configupgrade.Helper) { hostkit.CopyKeys(h) })

// ValidateConfig applies defaults and validates the decoded network config.
func (c *SIPConnector) ValidateConfig() error {
	c.Config.ApplyDefaults()
	return c.Config.Validate("network.")
}

// GetDBMetaTypes returns the metadata constructors.
func (c *SIPConnector) GetDBMetaTypes() database.MetaTypes {
	return database.MetaTypes{
		Portal:    func() any { return &PortalMetadata{} },
		Ghost:     func() any { return &GhostMetadata{} },
		UserLogin: func() any { return &UserLoginMetadata{} },
	}
}

// GetCapabilities returns the general network capabilities (none beyond defaults).
func (c *SIPConnector) GetCapabilities() *bridgev2.NetworkGeneralCapabilities {
	return &bridgev2.NetworkGeneralCapabilities{}
}

// GetBridgeInfoVersion returns the bridge info and capability versions.
func (c *SIPConnector) GetBridgeInfoVersion() (info, capabilities int) {
	return 1, 1
}
