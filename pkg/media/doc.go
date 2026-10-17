// Package media bridges audio between a SIP leg (RTP PCMU, 8 kHz) and a LiveKit leg (Opus) for one call
// (PHASE2-SPEC section 7). It is a core package: it never imports host packages, and it is the only
// package allowed to import the Opus codec library (internal/importcheck). Metrics are recorded by
// pkg/calls from Bridge.Stats, never here.
package media
