package hostkit

import (
	"errors"
	"strings"
	"testing"

	"github.com/williamkray/mrtc-sip-bridge/pkg/calls"
)

func TestCleanContactName(t *testing.T) {
	long64 := strings.Repeat("é", 64)
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"  Alice  ", "Alice", true},
		{"", "", false},
		{"   ", "", false},
		{long64, long64, true},
		{long64 + "é", "", false},
		{"Zoë 🙂", "Zoë 🙂", true},
		{"a\nb", "", false},
		{"a\tb", "", false},
		{"a\x00b", "", false},
		{"a b", "", false},
		{"a b", "", false},
		{"a‮b", "", false},
		{"a⁦b", "", false},
		{"a\xffb", "", false},
	}
	for _, c := range cases {
		got, ok := CleanContactName(c.in)
		if ok != c.ok || got != c.want {
			t.Errorf("CleanContactName(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestCheckChatNumberAction(t *testing.T) {
	for _, err := range []error{nil, calls.ErrNotAllowed, calls.ErrSelfDial} {
		r := CheckChatNumberAction(&fakeCalls{checkErr: err}, "l", "+1 (555) 234-5678", "1")
		if !r.OK || r.Number != "+15552345678" {
			t.Errorf("err=%v: %+v", err, r)
		}
	}
	if r := CheckChatNumberAction(&fakeCalls{checkErr: calls.ErrInvalidNumber}, "l", "+15552345678", "1"); r.OK || r.Code != CodeInvalidNumber {
		t.Fatalf("%+v", r)
	}
	if r := CheckChatNumberAction(&fakeCalls{}, "l", "abc", "1"); r.OK || r.Code != CodeInvalidNumber {
		t.Fatalf("%+v", r)
	}
	r := CheckChatNumberAction(&fakeCalls{}, "l", "611", "1")
	if r.OK || r.Code != CodeInvalidNumber || !strings.Contains(r.Message, "$cmdprefix pm") {
		t.Fatalf("short code: %+v", r)
	}
}

func TestActionError(t *testing.T) {
	err := error(NewActionError(CodeUnavailable, "later"))
	var ae *ActionError
	if !errors.As(err, &ae) || ae.Code != CodeUnavailable || ae.Message != "later" {
		t.Fatalf("%v", err)
	}
	if err.Error() != "unavailable: later" {
		t.Fatal(err.Error())
	}
}

func TestDisplayContactName(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"plain", "Alice", "Alice"},
		{"trim", "  Alice  ", "Alice"},
		{"collapse", "Alice \t\n  Smith", "Alice Smith"},
		{"nbsp is space", "A\u00a0\u00a0B", "A B"},
		{"control removed", "A\x00li\x07ce", "Alice"},
		{"separators removed", "A\u2028B\u2029C", "ABC"},
		{"bidi removed", "A\u202eB\u2066C\u2069D", "ABCD"},
		{"all forbidden", "\u202e\x00\u2028", ""},
		{"empty", "   ", ""},
		{"invalid utf8", "A\xffB", "AB"},
		{"truncated", strings.Repeat("é", 70), strings.Repeat("é", 64)},
		{"exactly 64", strings.Repeat("x", 64), strings.Repeat("x", 64)},
		{"truncate no trailing space", strings.Repeat("x", 63) + " yyy", strings.Repeat("x", 63)},
		{"emoji", "Zoë 🙂", "Zoë 🙂"},
	}
	for _, c := range cases {
		got := DisplayContactName(c.in)
		if got != c.want {
			t.Errorf("%s: DisplayContactName(%q) = %q, want %q", c.name, c.in, got, c.want)
		}
		if again := DisplayContactName(got); again != got {
			t.Errorf("%s: not idempotent: %q -> %q", c.name, got, again)
		}
		if got != "" {
			if _, ok := CleanContactName(got); !ok {
				t.Errorf("%s: result %q fails CleanContactName", c.name, got)
			}
		}
	}
}
