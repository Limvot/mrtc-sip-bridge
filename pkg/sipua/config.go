package sipua

import (
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/rs/zerolog"
)

// Transport is the SIP transport protocol.
type Transport string

const (
	// TransportUDP is SIP over UDP (default, proven against JMP).
	TransportUDP Transport = "udp"
	// TransportTCP is SIP over TCP (accepted, untested).
	TransportTCP Transport = "tcp"
)

// ParseTransport is case-insensitive and trims spaces. "" yields TransportUDP. "tls" and anything
// else are errors.
func ParseTransport(s string) (Transport, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "udp":
		return TransportUDP, nil
	case "tcp":
		return TransportTCP, nil
	case "tls":
		return "", fmt.Errorf("TLS transport is not supported yet")
	default:
		return "", fmt.Errorf("unknown SIP transport %q (want udp or tcp)", s)
	}
}

// Config configures one UA.
type Config struct {
	Username  string    // SIP auth + AOR user. Required.
	Password  string    // Required. Never logged.
	Registrar string    // "host:port", port required (connector normalizes). Required.
	Domain    string    // Optional SIP domain for AOR/Request-URI; "" = registrar host.
	Transport Transport // TransportUDP or TransportTCP; "" = UDP.

	BindIP       string // Concrete local IPv4 to bind (not 0.0.0.0). Required.
	BindPort     int    // Local SIP port, shared by REGISTER and inbound requests. Required.
	ExternalIP   string // Public IPv4 for Contact and SDP c=; "" = BindIP.
	ExternalPort int    // 0 = BindPort.

	Expiry          time.Duration // REGISTER Expires requested; 0 = 60s.
	RefreshInterval time.Duration // Re-REGISTER interval after success; 0 = 25s. Must be >= 5s and < Expiry.

	Log           zerolog.Logger   // Zero value is fine (disabled).
	OnStateChange func(StateEvent) // Called on every State change, from the UA goroutine. Must not block.
	Resolver      Resolver         // nil = net.DefaultResolver.

	// OnInvite receives every new inbound call (not re-INVITEs). It runs on the diago handler
	// goroutine and MUST return quickly (hand the call to another goroutine). nil means every INVITE is
	// rejected with 480 Temporarily Unavailable (Phase 1 behaviour).
	OnInvite InviteHandler
}

// applyDefaults fills zero fields.
func (c *Config) applyDefaults() {
	if c.Transport == "" {
		c.Transport = TransportUDP
	}
	if c.Expiry == 0 {
		c.Expiry = 60 * time.Second
	}
	if c.RefreshInterval == 0 {
		c.RefreshInterval = 25 * time.Second
	}
	if c.ExternalPort == 0 {
		c.ExternalPort = c.BindPort
	}
	if c.ExternalIP == "" {
		c.ExternalIP = c.BindIP
	}
	if c.Resolver == nil {
		c.Resolver = net.DefaultResolver
	}
}

func validPort(p int) bool { return p >= 1 && p <= 65535 }

func isIPv4(s string) (net.IP, bool) {
	ip := net.ParseIP(s)
	if ip == nil || ip.To4() == nil {
		return nil, false
	}
	return ip, true
}

// validate checks a defaulted Config. Errors name the offending field.
func (c *Config) validate() error {
	if c.Username == "" {
		return fmt.Errorf("sipua: Username is required")
	}
	if c.Password == "" {
		return fmt.Errorf("sipua: Password is required")
	}
	host, portStr, err := net.SplitHostPort(c.Registrar)
	if err != nil || host == "" {
		return fmt.Errorf("sipua: Registrar must be host:port")
	}
	if port, err := strconv.Atoi(portStr); err != nil || !validPort(port) {
		return fmt.Errorf("sipua: Registrar port must be 1-65535")
	}
	if c.Transport != TransportUDP && c.Transport != TransportTCP {
		return fmt.Errorf("sipua: Transport must be udp or tcp")
	}
	if ip, ok := isIPv4(c.BindIP); !ok || ip.IsUnspecified() {
		return fmt.Errorf("sipua: BindIP must be a concrete IPv4 address")
	}
	if !validPort(c.BindPort) {
		return fmt.Errorf("sipua: BindPort must be 1-65535")
	}
	if !validPort(c.ExternalPort) {
		return fmt.Errorf("sipua: ExternalPort must be 1-65535")
	}
	if _, ok := isIPv4(c.ExternalIP); !ok {
		return fmt.Errorf("sipua: ExternalIP must be an IPv4 address")
	}
	if c.RefreshInterval < 5*time.Second {
		return fmt.Errorf("sipua: RefreshInterval must be at least 5s")
	}
	if c.RefreshInterval >= c.Expiry {
		return fmt.Errorf("sipua: RefreshInterval must be less than Expiry")
	}
	return nil
}
