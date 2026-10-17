package sipaccount

import (
	"errors"
	"sync"
)

// errNoFreePort is returned when every port in the SIP range is in use.
var errNoFreePort = errors.New("no free SIP port in sip_port_min..sip_port_max")

// portPool hands out one SIP port per active login.
type portPool struct {
	mu       sync.Mutex
	min, max int
	used     map[int]bool
}

func newPortPool(min, max int) *portPool {
	return &portPool{min: min, max: max, used: map[int]bool{}}
}

// Acquire returns the lowest free port and marks it used.
func (p *portPool) Acquire() (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for port := p.min; port <= p.max; port++ {
		if !p.used[port] {
			p.used[port] = true
			return port, nil
		}
	}
	return 0, errNoFreePort
}

// Release frees a port; it is a no-op for unknown or unused ports.
func (p *portPool) Release(port int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.used, port)
}

// Size returns the number of ports in the range.
func (p *portPool) Size() int { return p.max - p.min + 1 }
