package connector

import (
	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
	"github.com/williamkray/mrtc-sip-bridge/pkg/hostkit"
)

// RegisterMatrixRTCHandlers wires call.member / rtc.notification / rtc.decline into c.RTC.
// Call from PostInit (before the EventProcessor starts). reg is br.Matrix.EventProcessor.
func (c *SIPConnector) RegisterMatrixRTCHandlers(reg core.EventRegistrar) {
	core.RegisterMatrixRTCHandlers(reg, hostkit.ResolveRTCLogin(c.br, nil), c.RTC)
}
