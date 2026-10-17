package connector

import "testing"

func TestIDs(t *testing.T) {
	if got := MakeUserID("+15551234567"); got != "15551234567" {
		t.Fatalf("MakeUserID = %q", got)
	}
	e, ok := ParseUserID(MakeUserID("+15551234567"))
	if !ok || e != "+15551234567" {
		t.Fatalf("round trip = %q %v", e, ok)
	}
	if _, ok := ParseUserID("abc"); ok {
		t.Fatal("abc should not parse")
	}
	k := MakePortalKey("+15551234567", "login1")
	if k.ID != "+15551234567" || k.Receiver != "login1" {
		t.Fatalf("portal key = %+v", k)
	}
	if got := MakeLoginID("+15551234567", "Reg.Example.com"); got != "+15551234567@reg.example.com" {
		t.Fatalf("MakeLoginID = %q", got)
	}
}
