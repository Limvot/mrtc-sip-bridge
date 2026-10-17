package main

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

var (
	reAuthHdr  = regexp.MustCompile(`(?im)^((?:Proxy-)?Authorization:[ \t]*).*$`)
	reCSeq     = regexp.MustCompile(`(?im)^CSeq:[ \t]*\d+[ \t]+([A-Z]+)`)
	reRealm    = regexp.MustCompile(`(?i)realm="([^"]*)"`)
	reAlgo     = regexp.MustCompile(`(?i)algorithm=([A-Za-z0-9-]+)`)
	reExpires  = regexp.MustCompile(`(?im)^Expires:[ \t]*(\d+)`)
	reContactX = regexp.MustCompile(`(?im)^Contact:.*?;expires=(\d+)`)
)

// redactSIP strips the values of Authorization and Proxy-Authorization headers.
func redactSIP(msg string) string {
	return reAuthHdr.ReplaceAllString(msg, "${1}[REDACTED]")
}

// sipTracer plugs into sipgo's raw SIP hook. It always inspects messages (to
// log digest challenges and the registrar's Expires, which diago does not
// expose) and prints the redacted message only when verbose is set.
type sipTracer struct {
	log     *slog.Logger
	verbose bool
	mu      sync.Mutex
	// last REGISTER 200 OK expiry seen, as text ("" if none).
	lastExpires atomic.Value
}

func (t *sipTracer) SIPTraceRead(transport, laddr, raddr string, b []byte) {
	t.inspect(string(b))
	t.print("read", transport, laddr, raddr, b)
}

func (t *sipTracer) SIPTraceWrite(transport, laddr, raddr string, b []byte) {
	t.print("write", transport, laddr, raddr, b)
}

func (t *sipTracer) print(dir, transport, laddr, raddr string, b []byte) {
	if !t.verbose {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	arrow := "->"
	if dir == "read" {
		arrow = "<-"
	}
	fmt.Fprintf(os.Stderr, "=== SIP %s %s %s %s %s (auth redacted) ===\n%s\n", dir, transport, laddr, arrow, raddr, redactSIP(string(b)))
}

func (t *sipTracer) inspect(msg string) {
	if !strings.HasPrefix(msg, "SIP/2.0 ") {
		return
	}
	line, _, _ := strings.Cut(msg, "\r\n")
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return
	}
	status := fields[1]
	method := ""
	if m := reCSeq.FindStringSubmatch(msg); m != nil {
		method = m[1]
	}
	switch status {
	case "401", "407":
		realm, algo := "", "MD5"
		if m := reRealm.FindStringSubmatch(msg); m != nil {
			realm = m[1]
		}
		if m := reAlgo.FindStringSubmatch(msg); m != nil {
			algo = m[1]
		}
		t.log.Info("digest challenge", "method", method, "status", status, "realm", realm, "algo", algo)
	case "200":
		if method == "REGISTER" {
			exp := ""
			if m := reExpires.FindStringSubmatch(msg); m != nil {
				exp = m[1]
			} else if m := reContactX.FindStringSubmatch(msg); m != nil {
				exp = m[1]
			}
			t.lastExpires.Store(exp)
		}
	}
}

func (t *sipTracer) registerExpires() string {
	if v, ok := t.lastExpires.Load().(string); ok && v != "" {
		return v
	}
	return "unknown"
}
