package sipua

import (
	"context"
	"fmt"
	"net"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/stun/v3"
)

// DefaultBindIP returns the first non-loopback IPv4 address of this host.
func DefaultBindIP() (net.IP, error) {
	ip, _, err := sip.ResolveInterfacesIP("ip4", nil)
	if err != nil {
		return nil, fmt.Errorf("resolve local IPv4: %w", err)
	}
	return ip, nil
}

// DiscoverPublicIP asks a STUN server ("host:port") for our public IPv4 address. It honors ctx
// cancellation. Only the IP is meaningful, not the port (throwaway socket).
func DiscoverPublicIP(ctx context.Context, stunServer string) (net.IP, error) {
	type result struct {
		ip  net.IP
		err error
	}
	ch := make(chan result, 1)
	go func() {
		ip, err := stunBinding(stunServer)
		ch <- result{ip, err}
	}()
	select {
	case r := <-ch:
		return r.ip, r.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func stunBinding(server string) (net.IP, error) {
	c, err := stun.Dial("udp4", server)
	if err != nil {
		return nil, fmt.Errorf("stun dial: %w", err)
	}
	defer c.Close()
	var (
		ip    net.IP
		cbErr error
		msg   = stun.MustBuild(stun.TransactionID, stun.BindingRequest)
	)
	doErr := c.Do(msg, func(res stun.Event) {
		if res.Error != nil {
			cbErr = res.Error
			return
		}
		var xor stun.XORMappedAddress
		if err := xor.GetFrom(res.Message); err != nil {
			cbErr = err
			return
		}
		ip = xor.IP
	})
	if doErr != nil {
		return nil, doErr
	}
	if cbErr != nil {
		return nil, cbErr
	}
	if ip == nil || ip.To4() == nil {
		return nil, fmt.Errorf("stun: no IPv4 address in response")
	}
	return ip.To4(), nil
}

// SetRTPPortRange sets diago's process-wide RTP port range. Call once at startup.
func SetRTPPortRange(min, max int) {
	media.RTPPortStart, media.RTPPortEnd = min, max
}
