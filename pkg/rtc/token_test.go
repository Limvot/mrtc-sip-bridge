package rtc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
)

const (
	testAccess = "SECRET-OPENID-ACCESS-TOKEN"
	testJWT    = "eyJhbGciOi.eyJzdWIi.c2ln"
)

var testTok = &core.OpenIDToken{AccessToken: testAccess, TokenType: "Bearer", MatrixServerName: "hs.test", ExpiresIn: 3600}

func TestFetchTokenOK(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/sfu/get" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("bad request %s %s %s", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"url":"wss://lk.test","jwt":"` + testJWT + `"}`))
	}))
	defer srv.Close()
	u, jwt, err := fetchToken(context.Background(), srv.Client(), srv.URL, "!r:hs.test", "DEV", testTok)
	if err != nil || u != "wss://lk.test" || jwt != testJWT {
		t.Fatalf("got %q %q %v", u, jwt, err)
	}
	if got["room"] != "!r:hs.test" || got["device_id"] != "DEV" {
		t.Fatalf("body %v", got)
	}
	o, _ := got["openid_token"].(map[string]any)
	if o["access_token"] != testAccess || o["token_type"] != "Bearer" || o["matrix_server_name"] != "hs.test" || o["expires_in"] != float64(3600) {
		t.Fatalf("openid_token %v", o)
	}
}

func TestFetchTokenErrorsNeverLeak(t *testing.T) {
	long := strings.Repeat("x", 500)
	cases := map[string]struct {
		status int
		body   string
	}{
		"403":     {403, `{"errcode":"M_FORBIDDEN","error":"nope"}`},
		"long":    {500, long},
		"echo":    {400, "bad " + testAccess + " and " + testJWT},
		"empty":   {200, `{"url":"","jwt":""}`},
		"garbage": {200, `not json ` + testJWT},
		"nojwt":   {200, `{"url":"wss://x"}`},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			defer srv.Close()
			_, _, err := fetchToken(context.Background(), srv.Client(), srv.URL, "!r:hs.test", "DEV", testTok)
			if err == nil {
				t.Fatal("expected error")
			}
			if strings.Contains(err.Error(), testJWT) {
				t.Fatalf("error leaks jwt: %v", err)
			}
			if name == "echo" && strings.Contains(err.Error(), "eyJ") {
				t.Fatalf("jwt not scrubbed: %v", err)
			}
			if name == "long" && len(err.Error()) > 400 {
				t.Fatalf("body not truncated: %d", len(err.Error()))
			}
			je := (&JoinError{Stage: "token_error", Err: err}).Error()
			if strings.Contains(je, testJWT) {
				t.Fatalf("JoinError leaks jwt: %s", je)
			}
		})
	}
}

func TestFetchTokenTransportErrorHasNoOpenID(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	_, _, err := fetchToken(context.Background(), http.DefaultClient, url, "!r:hs.test", "DEV", testTok)
	if err == nil || strings.Contains(err.Error(), testAccess) {
		t.Fatalf("err=%v", err)
	}
}

func newTokenServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"url":"wss://lk.test","jwt":"` + testJWT + `"}`))
	}))
}

func TestFetchTokenTrimsTrailingSlashOnlyForRequest(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"url":"wss://lk.test","jwt":"` + testJWT + `"}`))
	}))
	defer srv.Close()
	if _, _, err := fetchToken(context.Background(), srv.Client(), srv.URL+"/jwt/", "!r:hs.test", "DEV", testTok); err != nil {
		t.Fatal(err)
	}
	if path != "/jwt/sfu/get" {
		t.Fatalf("path %q", path)
	}
}
