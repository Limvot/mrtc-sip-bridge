package sipua

import (
	"fmt"
	"net"
	"strconv"

	"github.com/emiago/sipgo/sip"
)

// targets are the derived REGISTER addressing values.
type targets struct {
	RequestURI sip.Uri // REGISTER Request-URI
	AORUser    string  // = cfg.Username
	AORHost    string  // = cfg.Domain if set, else registrar host (no port)
	DestPort   int     // registrar port, used with the pinned IP
}

func buildTargets(cfg Config) (targets, error) {
	host, portStr, err := net.SplitHostPort(cfg.Registrar)
	if err != nil {
		return targets{}, fmt.Errorf("registrar: %w", err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return targets{}, fmt.Errorf("registrar port: %w", err)
	}
	t := targets{AORUser: cfg.Username, AORHost: host, DestPort: port}
	uri := "sip:" + cfg.Registrar
	if cfg.Domain != "" {
		t.AORHost = cfg.Domain
		uri = "sip:" + cfg.Domain
	}
	if cfg.Transport == TransportTCP {
		uri += ";transport=tcp"
	}
	if err := sip.ParseUri(uri, &t.RequestURI); err != nil {
		return targets{}, fmt.Errorf("request URI %q: %w", uri, err)
	}
	return t, nil
}

// ensureAORTo sets To to the address-of-record sip:<user>@<host> (no port) if req has no To header.
// diago builds To from the Request-URI, which has no user part; Bandwidth/JMP answers 500 to that (S2).
func ensureAORTo(req *sip.Request, user, host string) {
	if req.To() == nil {
		req.AppendHeader(&sip.ToHeader{Address: sip.Uri{Scheme: "sip", User: user, Host: host}, Params: sip.NewParams()})
	}
}

// prepareUnregister turns the REGISTER Origin into an unregister for OUR Contact only: it replaces
// Expires with 0 and leaves the Contact untouched (it holds our rport/received-rewritten binding).
func prepareUnregister(req *sip.Request) {
	for req.RemoveHeader("Expires") {
	}
	e := sip.ExpiresHeader(0)
	req.AppendHeader(&e)
}
