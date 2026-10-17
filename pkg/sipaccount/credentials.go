package sipaccount

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/williamkray/mrtc-sip-bridge/pkg/phone"
	"github.com/williamkray/mrtc-sip-bridge/pkg/sipua"
)

// Field IDs of the SIP login form, shared by both hosts.
const (
	FieldUsername      = "username"
	FieldPassword      = "password"
	FieldRegistrar     = "registrar"
	FieldTransport     = "transport"
	FieldDisplayNumber = "display_number"
)

const defaultSIPPort = "5060"

// Credentials is one SIP account. The JSON tags are the storage format of both hosts (identical to the
// standalone UserLoginMetadata, which is an alias of this type). Password is stored in plain text in the
// bridge DB, like the XMPP password; never log it.
type Credentials struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	Registrar     string `json:"registrar"`        // "host:port", normalized
	Domain        string `json:"domain,omitempty"` // reserved: SIP domain if different from registrar host; not settable yet
	Transport     string `json:"transport"`        // "udp" | "tcp"
	DisplayNumber string `json:"display_number"`   // E.164 of the account's own number
}

// Complete reports whether c has everything needed to register (username, password and registrar).
func (c *Credentials) Complete() bool {
	return c != nil && c.Username != "" && c.Password != "" && c.Registrar != ""
}

// NormalizeRegistrar trims s, strips a leading "sip:", rejects userinfo/paths/IPv6 and returns
// a lower-cased "host:port", defaulting the port to 5060.
func NormalizeRegistrar(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) >= 4 && strings.EqualFold(s[:4], "sip:") {
		s = s[4:]
	}
	if s == "" {
		return "", errors.New("registrar is required")
	}
	if strings.ContainsAny(s, "@/[") {
		return "", errors.New("registrar must be host or host:port (no user, path or IPv6 address)")
	}
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		var addrErr *net.AddrError
		if errors.As(err, &addrErr) && addrErr.Err == "missing port in address" {
			host, port = s, defaultSIPPort
		} else {
			return "", fmt.Errorf("invalid registrar: %w", err)
		}
	}
	if host == "" {
		return "", errors.New("registrar host is empty")
	}
	if strings.Contains(host, ":") {
		return "", errors.New("registrar must be host or host:port (no user, path or IPv6 address)")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return "", fmt.Errorf("registrar port %q is not a valid port (1..65535)", port)
	}
	return strings.ToLower(net.JoinHostPort(host, strconv.Itoa(p))), nil
}

// ParseInput validates the submitted login form (keyed by the Field* IDs) and returns credentials ready
// to store. Errors never contain the password.
func ParseInput(input map[string]string, defaultCC string) (*Credentials, error) {
	username := strings.TrimSpace(input[FieldUsername])
	if username == "" {
		return nil, errors.New("username is required")
	}
	password := input[FieldPassword]
	if password == "" {
		return nil, errors.New("password is required")
	}
	registrar, err := NormalizeRegistrar(input[FieldRegistrar])
	if err != nil {
		return nil, err
	}
	transport, err := sipua.ParseTransport(input[FieldTransport])
	if err != nil {
		return nil, err
	}
	number, err := phone.Normalize(input[FieldDisplayNumber], defaultCC)
	if err != nil {
		return nil, fmt.Errorf("invalid phone number: %w", err)
	}
	return &Credentials{
		Username:      username,
		Password:      password,
		Registrar:     registrar,
		Transport:     string(transport),
		DisplayNumber: number,
	}, nil
}
