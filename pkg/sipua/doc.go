// Package sipua is a registering SIP user agent for one SIP account (for example a JMP.chat
// credential). It keeps a REGISTER alive behind NAT, reports state changes through a callback, hands
// inbound INVITEs to Config.OnInvite (InboundCall) and places outbound calls with UA.Dial (OutboundCall:
// digest auth, provisional responses and early media, CANCEL, and the CANCEL-vs-200 race resolved inside
// this package). Outbound INVITEs go to the registrar IP the register loop pinned, from the same socket.
//
// The implementation sits on diago and sipgo, but those types never appear in the exported API: the
// UA interface is the only seam, so the SIP stack can be swapped later (ADR-009). Behaviors proven
// against the live JMP registrar in spike S2 are preserved: the REGISTER To header is the
// address-of-record, the registrar is pinned to one resolved IP (re-resolved only after repeated
// failures) so the NAT mapping stays stable, and shutdown unregisters only our own Contact.
//
// Credentials, SDP and raw SIP are never logged. Only diago.go, inbound_diago.go, outbound_diago.go and
// net.go import diago, sipgo or pion/stun (tests excepted).
package sipua
