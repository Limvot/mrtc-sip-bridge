// Package calls is the inbound call state machine (PHASE2-SPEC section 8). A Manager owns every call of
// every login; each call is one actor goroutine that owns all of its state (no mutex guards call state,
// only the manager's map). Inputs arrive on channels: the call's own event channel (Matrix events routed
// by HandleRTCEvent, LiveKit join results), the shutdown channel, the SIP call's events, the LiveKit
// session's events, the media bridge's Done channel and clock timers.
//
// The package depends only on interfaces (sipua.InboundCall, rtc.Joiner/Session, media.Bridge,
// core.CallHost/GhostIntent, clock.Clock), so its tests run against fakes with a fake clock.
//
// Telemetry (TELEMETRY-SPEC §5.6, §5.10): the actor starts the root call.inbound span backdated to
// INVITE receipt and ends it at ACK (or after the failure branch); answered calls get a call.teardown
// linked root and every call ends with a zero-length, post hoc call.session linked root. No live span
// crosses goroutines; the LiveKit join goroutine receives only the carried span context and the actor
// waits for it before ending the setup span.
package calls
