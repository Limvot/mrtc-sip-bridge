package sipua

import "testing"

func TestMaskUsername(t *testing.T) {
	for in, want := range map[string]string{"12345678": "12***78", "abcd": "***", "": "***"} {
		if got := MaskUsername(in); got != want {
			t.Errorf("MaskUsername(%q) = %q, want %q", in, got, want)
		}
	}
}
