package sipua

import (
	"strings"
	"testing"
	"time"
)

func validConfig() Config {
	return Config{
		Username: "user", Password: "pw", Registrar: "reg.example.com:5008",
		BindIP: "192.0.2.10", BindPort: 5060,
	}
}

func TestNewValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
		field  string
	}{
		{"no username", func(c *Config) { c.Username = "" }, "Username"},
		{"no password", func(c *Config) { c.Password = "" }, "Password"},
		{"no registrar", func(c *Config) { c.Registrar = "" }, "Registrar"},
		{"registrar no port", func(c *Config) { c.Registrar = "host" }, "Registrar"},
		{"registrar port 0", func(c *Config) { c.Registrar = "host:0" }, "Registrar"},
		{"registrar no host", func(c *Config) { c.Registrar = ":5060" }, "Registrar"},
		{"bad transport", func(c *Config) { c.Transport = "sctp" }, "Transport"},
		{"no bind ip", func(c *Config) { c.BindIP = "" }, "BindIP"},
		{"unspecified bind ip", func(c *Config) { c.BindIP = "0.0.0.0" }, "BindIP"},
		{"ipv6 bind ip", func(c *Config) { c.BindIP = "::1" }, "BindIP"},
		{"no bind port", func(c *Config) { c.BindPort = 0 }, "BindPort"},
		{"bad external port", func(c *Config) { c.ExternalPort = 70000 }, "ExternalPort"},
		{"bad external ip", func(c *Config) { c.ExternalIP = "nope" }, "ExternalIP"},
		{"refresh >= expiry", func(c *Config) { c.Expiry = 30 * time.Second; c.RefreshInterval = 30 * time.Second }, "RefreshInterval"},
		{"refresh < 5s", func(c *Config) { c.RefreshInterval = 4 * time.Second }, "RefreshInterval"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validConfig()
			tc.mutate(&c)
			_, err := New(c)
			if err == nil {
				t.Fatal("want error")
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error %q does not name %s", err, tc.field)
			}
		})
	}
}

func TestNewDefaults(t *testing.T) {
	ua, err := New(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	cfg := ua.(*diagoUA).cfg
	if cfg.Expiry != 60*time.Second || cfg.RefreshInterval != 25*time.Second {
		t.Errorf("expiry/refresh: %v %v", cfg.Expiry, cfg.RefreshInterval)
	}
	if cfg.ExternalIP != cfg.BindIP || cfg.ExternalPort != cfg.BindPort || cfg.Transport != TransportUDP || cfg.Resolver == nil {
		t.Errorf("defaults not applied: %+v", cfg)
	}
	if ua.State().State != StateIdle {
		t.Errorf("state before Start = %v", ua.State().State)
	}
}

func TestParseTransport(t *testing.T) {
	for in, want := range map[string]Transport{"": TransportUDP, "UDP": TransportUDP, " tcp ": TransportTCP} {
		got, err := ParseTransport(in)
		if err != nil || got != want {
			t.Errorf("ParseTransport(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := ParseTransport("tls"); err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Errorf("tls: %v", err)
	}
	if _, err := ParseTransport("sctp"); err == nil {
		t.Error("sctp: want error")
	}
}
