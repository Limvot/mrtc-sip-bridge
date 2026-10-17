package sipua

import (
	"context"
	"fmt"
	"net"
	"strconv"
)

// pinner keeps the registrar pinned to one resolved IP so the NAT mapping stays stable (S2).
type pinner struct {
	host     string
	port     int
	resolver Resolver
	ips      []net.IP
	idx      int
}

func newPinner(registrar string, r Resolver) (*pinner, error) {
	host, portStr, err := net.SplitHostPort(registrar)
	if err != nil {
		return nil, fmt.Errorf("registrar %q: %w", registrar, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return nil, fmt.Errorf("registrar port: %w", err)
	}
	return &pinner{host: host, port: port, resolver: r}, nil
}

// indexOf returns the index of ip in ips, or -1.
func indexOf(ips []net.IP, ip net.IP) int {
	for i, c := range ips {
		if c.Equal(ip) {
			return i
		}
	}
	return -1
}

func (p *pinner) currentIP() net.IP {
	if p.idx < len(p.ips) {
		return p.ips[p.idx]
	}
	return nil
}

// resolve refreshes the IP list, keeping the pinned IP if it is still present. On error the previous
// list is kept.
func (p *pinner) resolve(ctx context.Context) error {
	if ip := net.ParseIP(p.host); ip != nil {
		p.ips = []net.IP{ip}
		p.idx = 0
		return nil
	}
	ips, err := p.resolver.LookupIP(ctx, "ip4", p.host)
	if err != nil {
		return err
	}
	if len(ips) == 0 {
		return fmt.Errorf("no IPv4 addresses for %s", p.host)
	}
	cur := p.currentIP()
	p.ips = ips
	p.idx = 0
	if cur != nil {
		if i := indexOf(ips, cur); i >= 0 {
			p.idx = i
		}
	}
	return nil
}

// advance moves to the next IP after a fresh lookup. Rotation happens even if the lookup fails.
func (p *pinner) advance(ctx context.Context) error {
	cur := p.currentIP()
	err := p.resolve(ctx)
	if len(p.ips) == 0 {
		return err
	}
	if i := indexOf(p.ips, cur); cur != nil && i >= 0 {
		p.idx = (i + 1) % len(p.ips)
	} else {
		p.idx = 0
	}
	return err
}

// current returns "ip:port" of the pinned IP, or "" if none.
func (p *pinner) current() string {
	ip := p.currentIP()
	if ip == nil {
		return ""
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(p.port))
}
