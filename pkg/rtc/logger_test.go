package rtc

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/rs/zerolog"
)

func TestSinkScrubsAndDropsKeyValues(t *testing.T) {
	var buf bytes.Buffer
	l := newLibraryLogger(zerolog.New(&buf))
	const secret = "eyJhbGciOi.eyJzdWIi.sig"
	l.Debugw("debug dropped")
	l.Infow("info dropped")
	l.Warnw("connect wss://x/rtc?access_token="+secret+" failed", errors.New("dial wss://y/rtc?access_token=abc"), "token", "KVSECRET", "jwt", secret)
	l.WithValues("k", "KVSECRET").WithComponent("x").Infow("also dropped")
	l.Errorw("boom "+secret, errors.New("bad access_token=hunter2"), "password", "KVSECRET")
	out := buf.String()
	for _, bad := range []string{secret, "KVSECRET", "access_token=abc", "hunter2", "/rtc?", "dropped"} {
		if strings.Contains(out, bad) {
			t.Fatalf("output contains %q: %s", bad, out)
		}
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"level":"warn"`) || !strings.Contains(lines[1], `"level":"error"`) {
		t.Fatalf("lines: %v", lines)
	}
	if !strings.Contains(lines[0], "wss://x") {
		t.Fatalf("host should survive scrubbing: %s", lines[0])
	}
}

func TestSinkDemotesICENoise(t *testing.T) {
	var buf bytes.Buffer
	l := newLibraryLogger(zerolog.New(&buf))
	l.Warnw("Failed to ping without candidate pairs. Connection is not possible yet.", nil)
	l.Warnw("failed to get server reflexive address udp6 stun:stun.example.org:3478: i/o timeout", nil)
	if !strings.Contains(buf.String(), `"level":"debug"`) || strings.Contains(buf.String(), `"level":"warn"`) {
		t.Fatalf("noise not demoted: %s", buf.String())
	}
	buf.Reset()
	l.Warnw("ICE failed for a real reason", nil)
	if !strings.Contains(buf.String(), `"level":"warn"`) {
		t.Fatalf("real warning dropped: %s", buf.String())
	}
}
