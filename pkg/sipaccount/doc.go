// Package sipaccount is the SIP account runtime shared by both hosts (the standalone bridge and the
// embedded mautrix-jabber host): one process-wide Runtime (bind and external IP, STUN, RTP port range,
// SIP port pool, registrations gauge) and one Account per login (UA start with port retry, stop with a
// detached unregister, state, first-result wait, outbound dial), plus the stored Credentials and the
// login form parsing.
//
// It is a core-class package: it must not import bridgev2, the appservice, the host packages or the
// OTel SDK (internal/importcheck enforces this).
package sipaccount
