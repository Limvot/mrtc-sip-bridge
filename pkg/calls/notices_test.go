package calls

import (
	"testing"
	"time"
)

func TestFormatDuration(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{-5 * time.Second, "0s"},
		{0, "0s"},
		{45 * time.Second, "45s"},
		{59*time.Second + 900*time.Millisecond, "59s"},
		{60 * time.Second, "1m 0s"},
		{80 * time.Second, "1m 20s"},
		{120 * time.Second, "2m 0s"},
		{3599 * time.Second, "59m 59s"},
		{time.Hour, "1h 00m"},
		{time.Hour + 2*time.Minute + 59*time.Second, "1h 02m"},
		{12*time.Hour + 30*time.Minute, "12h 30m"},
	}
	for _, c := range cases {
		if got := formatDuration(c.d); got != c.want {
			t.Errorf("formatDuration(%v) = %q, want %q", c.d, got, c.want)
		}
	}
	if got := answeredNotice(80 * time.Second); got != "Call · 1m 20s" {
		t.Errorf("answeredNotice = %q", got)
	}
}
