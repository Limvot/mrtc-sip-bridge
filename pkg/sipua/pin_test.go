package sipua

import (
	"context"
	"errors"
	"net"
	"testing"
)

type fakeResolver struct {
	results [][]net.IP
	errs    []error
	calls   int
	network string
}

func (f *fakeResolver) LookupIP(ctx context.Context, network, host string) ([]net.IP, error) {
	i := f.calls
	f.calls++
	f.network = network
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	return f.results[i], err
}

func ips(s ...string) []net.IP {
	var out []net.IP
	for _, x := range s {
		out = append(out, net.ParseIP(x))
	}
	return out
}

func mustPin(t *testing.T, reg string, r Resolver) *pinner {
	t.Helper()
	p, err := newPinner(reg, r)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPinIPLiteral(t *testing.T) {
	r := &fakeResolver{results: [][]net.IP{nil}}
	p := mustPin(t, "1.2.3.4:5008", r)
	if err := p.resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p.current() != "1.2.3.4:5008" || r.calls != 0 {
		t.Errorf("current=%q calls=%d", p.current(), r.calls)
	}
}

func TestPinAdvanceWraps(t *testing.T) {
	r := &fakeResolver{results: [][]net.IP{ips("1.1.1.1", "2.2.2.2")}}
	p := mustPin(t, "reg.example.com:5008", r)
	ctx := context.Background()
	if err := p.resolve(ctx); err != nil {
		t.Fatal(err)
	}
	if p.current() != "1.1.1.1:5008" {
		t.Fatalf("current %q", p.current())
	}
	_ = p.advance(ctx)
	if p.current() != "2.2.2.2:5008" {
		t.Errorf("after advance %q", p.current())
	}
	_ = p.advance(ctx)
	if p.current() != "1.1.1.1:5008" {
		t.Errorf("after wrap %q", p.current())
	}
	if r.network != "ip4" {
		t.Errorf("network %q", r.network)
	}
}

func TestPinReresolveKeepsPinned(t *testing.T) {
	r := &fakeResolver{results: [][]net.IP{ips("1.1.1.1"), ips("3.3.3.3", "1.1.1.1")}}
	p := mustPin(t, "reg.example.com:5008", r)
	ctx := context.Background()
	_ = p.resolve(ctx)
	_ = p.resolve(ctx)
	if p.current() != "1.1.1.1:5008" || p.idx != 1 {
		t.Errorf("current %q idx %d", p.current(), p.idx)
	}
}

func TestPinReresolveDropsPinned(t *testing.T) {
	r := &fakeResolver{results: [][]net.IP{ips("1.1.1.1"), ips("3.3.3.3")}}
	p := mustPin(t, "reg.example.com:5008", r)
	ctx := context.Background()
	_ = p.resolve(ctx)
	_ = p.resolve(ctx)
	if p.current() != "3.3.3.3:5008" || p.idx != 0 {
		t.Errorf("current %q idx %d", p.current(), p.idx)
	}
}

func TestPinAdvanceResolverError(t *testing.T) {
	boom := errors.New("dns down")
	r := &fakeResolver{results: [][]net.IP{ips("1.1.1.1", "2.2.2.2"), nil}, errs: []error{nil, boom}}
	p := mustPin(t, "reg.example.com:5008", r)
	ctx := context.Background()
	_ = p.resolve(ctx)
	err := p.advance(ctx)
	if !errors.Is(err, boom) {
		t.Errorf("err %v", err)
	}
	if p.current() != "2.2.2.2:5008" {
		t.Errorf("rotation within old list failed: %q", p.current())
	}
}

func TestPinEmptyResult(t *testing.T) {
	r := &fakeResolver{results: [][]net.IP{nil}}
	p := mustPin(t, "reg.example.com:5008", r)
	if err := p.resolve(context.Background()); err == nil {
		t.Error("want error for empty result")
	}
	if p.current() != "" {
		t.Errorf("current %q", p.current())
	}
}
