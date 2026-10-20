// Package core is the host-independent call library (ADR-006).
//
// A host (the standalone bridgev2 bridge, or jmpchat-matrix-bridge in embedded mode) implements CallHost, which
// tells core how to find or create the Matrix room and ghost for a remote phone number, post notices and
// mark rooms read as the owner. The host then feeds MatrixRTC events into core, either by passing its
// appservice event processor to RegisterMatrixRTCHandlers or by calling Ingest.HandleMatrixRTCEvent
// directly, and owns the login lifecycle (which LoginRef belongs to which Matrix user).
//
// Core must not import host packages or bridgev2. It talks to Matrix only through the small interfaces
// declared here, using the id and event packages for plain types. internal/importcheck enforces this.
package core
