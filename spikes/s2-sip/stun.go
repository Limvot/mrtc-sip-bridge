package main

import (
	"fmt"
	"net"

	"github.com/pion/stun/v3"
)

// stunPublicIP asks a STUN server for our public IPv4 address (from a throwaway
// socket, so only the IP is meaningful, not the port).
func stunPublicIP(server string) (net.IP, error) {
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
	if ip == nil {
		return nil, fmt.Errorf("stun: no address in response")
	}
	return ip, nil
}
