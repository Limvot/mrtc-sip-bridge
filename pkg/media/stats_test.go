package media

import (
	"testing"
	"time"
)

func TestJitterEstimator(t *testing.T) {
	var e jitterEstimator
	t0 := time.Unix(1000, 0)
	var j time.Duration
	for i := 0; i < 50; i++ {
		j = e.update(t0.Add(time.Duration(i)*20*time.Millisecond), uint32(i*160))
	}
	if j != 0 {
		t.Fatalf("steady stream jitter %v", j)
	}
	// One packet 10 ms late.
	j = e.update(t0.Add(50*20*time.Millisecond+10*time.Millisecond), 50*160)
	want := 10 * time.Millisecond / 16
	if d := j - want; d < -time.Microsecond || d > time.Microsecond {
		t.Fatalf("jitter %v want %v", j, want)
	}
}
