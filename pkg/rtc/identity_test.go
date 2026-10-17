package rtc

import (
	"testing"

	"maunium.net/go/mautrix/id"
)

func TestIsOwner(t *testing.T) {
	const owner = id.UserID("@o:s")
	for identity, want := range map[string]bool{
		"@o:s:DEV":   true,
		"@o:s2:DEV":  false,
		"@oo:s:DEV":  false,
		"@o:s":       false,
		"@ghost:s:D": false,
	} {
		if got := isOwner(identity, owner); got != want {
			t.Errorf("isOwner(%q)=%v want %v", identity, got, want)
		}
	}
}

func TestHashJSONVector(t *testing.T) {
	const want = "O8437W3+jmzMVjoIP3tNwbm+XxHQk2iKpOA7aqw3qSc"
	if got := hashJSON("!roomid:example.com", "slot1234"); got != want {
		t.Fatalf("got %s", got)
	}
	if LiveKitRoomName("!r:s") != hashJSON("!r:s", "m.call#ROOM") {
		t.Fatal("LiveKitRoomName mismatch")
	}
}

func TestPathTemplate(t *testing.T) {
	for in, want := range map[string]string{
		"/sfu/get":                   "/sfu/get",
		"/livekit/jwt/sfu/get":       "/sfu/get",
		"/.well-known/matrix/client": "/.well-known/matrix/client",
		"/other":                     "*",
		"/sfu/get/x":                 "*",
	} {
		if got := PathTemplate(in); got != want {
			t.Errorf("PathTemplate(%q)=%q want %q", in, got, want)
		}
	}
}
