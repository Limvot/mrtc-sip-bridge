package connector

import (
	"strings"

	"maunium.net/go/mautrix/bridgev2/networkid"

	"github.com/williamkray/mrtc-sip-bridge/pkg/phone"
)

// MakePortalKey returns the key of the DM portal for one remote number per login.
func MakePortalKey(e164 string, login networkid.UserLoginID) networkid.PortalKey {
	return networkid.PortalKey{ID: networkid.PortalID(e164), Receiver: login}
}

// MakeUserID returns the ghost ID: the E.164 digits without "+".
func MakeUserID(e164 string) networkid.UserID { return networkid.UserID(phone.Digits(e164)) }

// ParseUserID is the inverse of MakeUserID; ok is false if uid is not valid E.164 digits.
func ParseUserID(uid networkid.UserID) (e164 string, ok bool) { return phone.FromDigits(string(uid)) }

// MakeLoginID returns lower(displayE164 + "@" + registrarHost); registrarHost must not include a port. The SIP
// username is deliberately not part of the ID: bridgev2 logs login IDs unmasked.
func MakeLoginID(displayE164, registrarHost string) networkid.UserLoginID {
	return networkid.UserLoginID(strings.ToLower(displayE164 + "@" + registrarHost))
}
