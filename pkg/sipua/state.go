package sipua

import (
	"context"
	"fmt"
	"net"
	"time"
)

// State is the registration state of a UA.
type State int

const (
	// StateIdle is the state before Start.
	StateIdle State = iota
	// StateRegistering is the state while the first REGISTER is in flight.
	StateRegistering
	// StateRegistered means the registrar accepted our REGISTER.
	StateRegistered
	// StateAuthFailed means the registrar rejected the credentials (401/403/404/407).
	StateAuthFailed
	// StateTransientError means a network, DNS, timeout or server error; retrying with backoff.
	StateTransientError
	// StateStopped is the final state after Stop.
	StateStopped
)

// String returns the lowercase state name.
func (s State) String() string {
	switch s {
	case StateIdle:
		return "idle"
	case StateRegistering:
		return "registering"
	case StateRegistered:
		return "registered"
	case StateAuthFailed:
		return "auth_failed"
	case StateTransientError:
		return "transient_error"
	case StateStopped:
		return "stopped"
	}
	return fmt.Sprintf("state(%d)", int(s))
}

// StateEvent describes the UA state and the details of the latest attempt.
type StateEvent struct {
	State         State
	StatusCode    int           // SIP status of the failing final response; 0 for timeouts/network/DNS errors
	Reason        string        // SIP reason phrase or short error text; never contains credentials
	Err           error         // underlying error for logs; may be nil
	RegistrarAddr string        // pinned "ip:port" used for the attempt ("" if DNS failed)
	Contact       string        // our Contact "host:port" after success (NAT-rewritten by rport/received)
	NextRetry     time.Duration // for AuthFailed/TransientError: delay before the next attempt
}

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
