package calls

import (
	"strings"
	"testing"
	"time"
)

func TestULIDEncoding(t *testing.T) {
	// ULID spec example: timestamp 1469918176385 encodes as "01ARYZ6S41".
	var b [16]byte
	ms := uint64(1469918176385)
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (40 - 8*i))
	}
	if got := encodeULID(b); got != "01ARYZ6S41"+"0000000000000000" {
		t.Errorf("encodeULID = %q", got)
	}
	var max [16]byte
	for i := range max {
		max[i] = 0xFF
	}
	if got := encodeULID(max); got != "7ZZZZZZZZZZZZZZZZZZZZZZZZZ" {
		t.Errorf("max ULID = %q", got)
	}
}

func TestNewCallID(t *testing.T) {
	now := time.UnixMilli(1469918176385)
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		id := newCallID(now)
		if len(id) != 26 || !strings.HasPrefix(id, "01ARYZ6S41") {
			t.Fatalf("call id %q", id)
		}
		for _, r := range id {
			if !strings.ContainsRune(crockford, r) {
				t.Fatalf("call id %q has %q", id, r)
			}
		}
		if seen[id] {
			t.Fatalf("duplicate call id %q", id)
		}
		seen[id] = true
	}
}

func TestDeviceID(t *testing.T) {
	if got := deviceIDFor("01ARYZ6S41ABCDEFGH12345678"); got != "SIP12345678" {
		t.Errorf("deviceIDFor = %q", got)
	}
}
