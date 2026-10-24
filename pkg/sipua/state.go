package sipua

// The shared call-leg state types (State, StateEvent) live in pkg/leg (ADR-014) and are
// re-exported from alias.go. This file keeps the SIP-specific plumbing.

import (
	"context"
	"fmt"
	"net"
)

// ResponseError is returned by the transaction layer for non-200 final responses.
type ResponseError struct {
	StatusCode int
	Reason     string
}

// Error implements error.
func (e *ResponseError) Error() string { return fmt.Sprintf("SIP %d %s", e.StatusCode, e.Reason) }

// Resolver looks up IPs; *net.Resolver satisfies it.
type Resolver interface {
	LookupIP(ctx context.Context, network, host string) ([]net.IP, error)
}
