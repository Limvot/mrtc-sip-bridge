package sipaccount

import (
	"strings"
	"testing"
)

func TestNormalizeRegistrar(t *testing.T) {
	good := map[string]string{
		"Reg.Example.com":          "reg.example.com:5060",
		"sip:reg.example.com:5008": "reg.example.com:5008",
		"SIP:Reg.Example.com":      "reg.example.com:5060",
		"1.2.3.4:5008":             "1.2.3.4:5008",
		"  host.example:5070 ":     "host.example:5070",
	}
	for in, want := range good {
		got, err := NormalizeRegistrar(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"[::1]:5060", "user@host", "host:99999", "host:0", "", ":5060", "host/path", "host:abc"} {
		if got, err := NormalizeRegistrar(in); err == nil {
			t.Errorf("%q: expected error, got %q", in, got)
		}
	}
}

func TestParseInput(t *testing.T) {
	in := map[string]string{
		FieldUsername:      " 12345 ",
		FieldPassword:      "  s3cret pw  ",
		FieldRegistrar:     "Reg.Example.com",
		FieldDisplayNumber: "(555) 123-4567",
	}
	m, err := ParseInput(in, "1")
	if err != nil {
		t.Fatal(err)
	}
	if m.Username != "12345" || m.Password != "  s3cret pw  " || m.Registrar != "reg.example.com:5060" ||
		m.Transport != "udp" || m.DisplayNumber != "+15551234567" {
		t.Fatalf("meta = %+v", m)
	}

	with := func(k, v string) map[string]string {
		c := map[string]string{}
		for kk, vv := range in {
			c[kk] = vv
		}
		c[k] = v
		return c
	}
	bad := []map[string]string{
		with(FieldUsername, "  "),
		with(FieldPassword, ""),
		with(FieldTransport, "tls"),
		with(FieldDisplayNumber, "nope"),
		with(FieldRegistrar, "user@host"),
	}
	for i, b := range bad {
		_, err := ParseInput(b, "1")
		if err == nil {
			t.Errorf("case %d: expected error", i)
			continue
		}
		if strings.Contains(err.Error(), "s3cret") {
			t.Errorf("case %d: error leaks password: %v", i, err)
		}
	}
	m, err = ParseInput(with(FieldTransport, "TCP"), "1")
	if err != nil || m.Transport != "tcp" {
		t.Fatalf("tcp: %+v %v", m, err)
	}
}
