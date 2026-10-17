// Package rtc is the LiveKit participant for one call: it requests an OpenID token as the ghost, gets a
// LiveKit JWT from the MatrixRTC auth service (lk-jwt-service), joins the room, publishes one Opus
// microphone track and forwards the owner's audio (PHASE2-SPEC section 6). It is a core package and the
// only one allowed to import LiveKit and pion (internal/importcheck); no LiveKit or pion type appears in
// its exported API.
package rtc
