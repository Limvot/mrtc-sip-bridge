package sipua

import "github.com/williamkray/mrtc-sip-bridge/pkg/core"

// MaskUsername returns "ab***yz" (first two and last two runes); "***" if 4 runes or fewer.
func MaskUsername(u string) string { return core.MaskUsername(u) }
