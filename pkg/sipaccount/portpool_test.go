package sipaccount

import (
	"errors"
	"testing"
)

func TestPortPool(t *testing.T) {
	p := newPortPool(5060, 5062)
	a, _ := p.Acquire()
	b, _ := p.Acquire()
	if a != 5060 || b != 5061 {
		t.Fatalf("got %d %d", a, b)
	}
	p.Release(a)
	if c, _ := p.Acquire(); c != 5060 {
		t.Fatalf("reuse lowest: got %d", c)
	}
	if c, err := p.Acquire(); err != nil || c != 5062 {
		t.Fatalf("got %d %v", c, err)
	}
	if _, err := p.Acquire(); !errors.Is(err, errNoFreePort) {
		t.Fatalf("err = %v", err)
	}
	p.Release(9999) // unknown: no-op
	if _, err := p.Acquire(); !errors.Is(err, errNoFreePort) {
		t.Fatalf("err = %v", err)
	}
}
