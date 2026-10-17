package sipua

import (
	"strings"
	"testing"

	"github.com/emiago/sipgo/sip"
)

func TestBuildTargets(t *testing.T) {
	base := Config{Username: "u", Registrar: "reg.example.com:5008", Transport: TransportUDP}
	tg, err := buildTargets(base)
	if err != nil {
		t.Fatal(err)
	}
	if tg.RequestURI.String() != "sip:reg.example.com:5008" || tg.AORHost != "reg.example.com" || tg.AORUser != "u" || tg.DestPort != 5008 {
		t.Errorf("no domain: %+v (%s)", tg, tg.RequestURI.String())
	}

	d := base
	d.Domain = "example.org"
	tg, err = buildTargets(d)
	if err != nil {
		t.Fatal(err)
	}
	if tg.RequestURI.String() != "sip:example.org" || tg.AORHost != "example.org" || tg.DestPort != 5008 {
		t.Errorf("domain: %+v (%s)", tg, tg.RequestURI.String())
	}

	tc := base
	tc.Transport = TransportTCP
	tg, err = buildTargets(tc)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := tg.RequestURI.UriParams.Get("transport"); !ok || v != "tcp" {
		t.Errorf("tcp transport param missing: %s", tg.RequestURI.String())
	}
}

func newRegister(t *testing.T) *sip.Request {
	t.Helper()
	var uri sip.Uri
	if err := sip.ParseUri("sip:reg.example.com:5008", &uri); err != nil {
		t.Fatal(err)
	}
	return sip.NewRequest(sip.REGISTER, uri)
}

func TestEnsureAORTo(t *testing.T) {
	req := newRegister(t)
	if req.To() != nil {
		t.Skip("NewRequest already sets To")
	}
	ensureAORTo(req, "user", "reg.example.com")
	to := req.To()
	if to == nil || to.Address.User != "user" || to.Address.Host != "reg.example.com" || to.Address.Port != 0 {
		t.Fatalf("To = %+v", to)
	}
	ensureAORTo(req, "other", "elsewhere")
	if n := len(req.GetHeaders("To")); n != 1 {
		t.Errorf("To headers = %d", n)
	}
	if req.To().Address.User != "user" {
		t.Error("existing To was modified")
	}
}

func TestEnsureAORToKeepsExisting(t *testing.T) {
	req := newRegister(t)
	req.AppendHeader(&sip.ToHeader{Address: sip.Uri{Scheme: "sip", User: "keep", Host: "h"}, Params: sip.NewParams()})
	ensureAORTo(req, "user", "reg.example.com")
	if req.To().Address.User != "keep" || len(req.GetHeaders("To")) != 1 {
		t.Errorf("To = %+v", req.To())
	}
}

func TestPrepareUnregister(t *testing.T) {
	req := newRegister(t)
	var c sip.Uri
	if err := sip.ParseUri("sip:user@203.0.113.5:40123", &c); err != nil {
		t.Fatal(err)
	}
	req.AppendHeader(&sip.ContactHeader{Address: c, Params: sip.NewParams()})
	before := req.Contact().String()
	e1, e2 := sip.ExpiresHeader(60), sip.ExpiresHeader(120)
	req.AppendHeader(&e1)
	req.AppendHeader(&e2)

	prepareUnregister(req)

	if got := req.Contact().String(); got != before || strings.Contains(got, "*") {
		t.Errorf("Contact %q, before %q", got, before)
	}
	hs := req.GetHeaders("Expires")
	if len(hs) != 1 || hs[0].Value() != "0" {
		t.Errorf("Expires headers: %v", hs)
	}
}
