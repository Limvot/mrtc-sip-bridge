package rtc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
)

const wellKnownOK = `{"m.homeserver":{"base_url":"https://hs"},"org.matrix.msc4143.rtc_foci":[{"type":"other"},{"type":"livekit","livekit_service_url":"https://rtc.test/jwt/"}]}`

func TestResolverOverrideWins(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer srv.Close()
	r := NewServiceURLResolver("https://override.test/x/", "hs.test", srv.Client(), clock.NewFake(time.Unix(0, 0)))
	r.base = srv.URL
	u, err := r.Get(context.Background())
	if err != nil || u != "https://override.test/x/" || r.Source() != "config" || hits.Load() != 0 {
		t.Fatalf("%q %v %q hits=%d", u, err, r.Source(), hits.Load())
	}
}

func TestResolverDiscoveryCachedForever(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/.well-known/matrix/client" {
			t.Errorf("path %s", req.URL.Path)
		}
		hits.Add(1)
		_, _ = w.Write([]byte(wellKnownOK))
	}))
	defer srv.Close()
	clk := clock.NewFake(time.Unix(0, 0))
	r := NewServiceURLResolver("", "hs.test", srv.Client(), clk)
	r.base = srv.URL
	if r.Source() != "" {
		t.Fatalf("source before: %q", r.Source())
	}
	for i := 0; i < 3; i++ {
		u, err := r.Get(context.Background())
		if err != nil || u != "https://rtc.test/jwt/" {
			t.Fatalf("%q %v", u, err)
		}
		clk.Advance(24 * time.Hour)
	}
	if hits.Load() != 1 || r.Source() != "well_known" {
		t.Fatalf("hits=%d source=%q", hits.Load(), r.Source())
	}
}

func TestResolverNegativeCache(t *testing.T) {
	var hits atomic.Int32
	var ok atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		if ok.Load() {
			_, _ = w.Write([]byte(wellKnownOK))
			return
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	clk := clock.NewFake(time.Unix(0, 0))
	r := NewServiceURLResolver("", "hs.test", srv.Client(), clk)
	r.base = srv.URL
	_, err := r.Get(context.Background())
	if err == nil || !strings.Contains(err.Error(), "network.livekit_service_url") {
		t.Fatalf("err=%v", err)
	}
	clk.Advance(59 * time.Second)
	if _, err2 := r.Get(context.Background()); err2 == nil || hits.Load() != 1 {
		t.Fatalf("cached error expected, hits=%d err=%v", hits.Load(), err2)
	}
	ok.Store(true)
	clk.Advance(2 * time.Second)
	u, err := r.Get(context.Background())
	if err != nil || u != "https://rtc.test/jwt/" || hits.Load() != 2 {
		t.Fatalf("%q %v hits=%d", u, err, hits.Load())
	}
}

func TestResolverHTTPErrorAndTLS(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		http.Error(w, "no", http.StatusNotFound)
	}))
	defer srv.Close()
	r := NewServiceURLResolver("", "hs.test", srv.Client(), clock.NewFake(time.Unix(0, 0)))
	r.base = srv.URL
	if _, err := r.Get(context.Background()); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("err=%v", err)
	}
}

func TestResolverConcurrentSharesOneFetch(t *testing.T) {
	var hits atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		hits.Add(1)
		<-release
		_, _ = w.Write([]byte(wellKnownOK))
	}))
	defer srv.Close()
	r := NewServiceURLResolver("", "hs.test", srv.Client(), clock.NewFake(time.Unix(0, 0)))
	r.base = srv.URL
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if u, err := r.Get(context.Background()); err != nil || u != "https://rtc.test/jwt/" {
				t.Errorf("%q %v", u, err)
			}
		}()
	}
	time.Sleep(100 * time.Millisecond)
	close(release)
	wg.Wait()
	if hits.Load() != 1 {
		t.Fatalf("hits=%d", hits.Load())
	}
}

func TestResolverCallerCancel(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)
	r := NewServiceURLResolver("", "hs.test", srv.Client(), clock.NewFake(time.Unix(0, 0)))
	r.base = srv.URL
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := r.Get(ctx); err == nil {
		t.Fatal("expected ctx error")
	}
}
