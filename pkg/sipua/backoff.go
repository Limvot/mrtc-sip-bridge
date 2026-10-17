package sipua

import "time"

const (
	backoffBase       = 2 * time.Second
	backoffMax        = 60 * time.Second
	authRetryInterval = 5 * time.Minute
	attemptTimeout    = 10 * time.Second
	ipSwitchAfter     = 2 // consecutive transient failures on one IP before switching
)

// backoffDelay returns min(backoffBase * 2^(attempt-1), backoffMax); attempt < 1 counts as 1.
func backoffDelay(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := backoffBase
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= backoffMax {
			return backoffMax
		}
	}
	return d
}

// jitter scales d by 0.8 + 0.4*r for r in [0,1).
func jitter(d time.Duration, r float64) time.Duration {
	return time.Duration(float64(d) * (0.8 + 0.4*r))
}
