package connector

import "github.com/williamkray/mrtc-sip-bridge/pkg/sipaccount"

// UserLoginMetadata is stored as JSON in user_login.metadata: the login's SIP credentials. Password is
// stored in plain text in the bridge DB, like jmpchat-matrix-bridge's JabberLoginMeta. Never log it.
type UserLoginMetadata = sipaccount.Credentials

// PortalMetadata is reserved for Phase 2+ (e.g. last call info). Empty for now.
type PortalMetadata struct{}

// GhostMetadata is reserved. Empty for now.
type GhostMetadata struct{}
