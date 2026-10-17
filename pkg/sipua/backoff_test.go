package sipua

import (
	"testing"
	"time"
)

func TestBackoffDelay(t *testing.T) {
	want := []time.Duration{2, 2, 4, 8, 16, 32, 60, 60, 60}
	for attempt, w := range want {
		if got := backoffDelay(attempt); got != w*time.Second {
			t.Errorf("backoffDelay(%d) = %v, want %v", attempt, got, w*time.Second)
		}
	}
}

func TestJitter(t *testing.T) {
	if got := jitter(10*time.Second, 0); got != 8*time.Second {
		t.Errorf("jitter r=0: %v", got)
	}
	if got := jitter(10*time.Second, 0.5); got != 10*time.Second {
		t.Errorf("jitter r=0.5: %v", got)
	}
	if got := jitter(10*time.Second, 0.999); got >= 12*time.Second {
		t.Errorf("jitter r=0.999: %v", got)
	}
}
