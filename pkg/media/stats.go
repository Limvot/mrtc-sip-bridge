package media

import (
	"math"
	"sync/atomic"
	"time"
)

// counters are the bridge's lock-free counters; the hot path touches only these.
type counters struct {
	sipRx, sipTx               atomic.Uint64
	concealedS2R, concealedR2S atomic.Uint64
	rtcRx, dtmfSent            atomic.Uint64
	lastSIPRx                  atomic.Int64 // unix nanoseconds, 0 = none
	sipJitter                  atomic.Int64 // nanoseconds
}

// jitterEstimator is the RFC 3550 interarrival jitter estimator (single goroutine).
type jitterEstimator struct {
	have        bool
	prevArrival time.Time
	prevTS      uint32
	j           float64 // seconds
}

// update folds in one packet and returns the current estimate.
func (e *jitterEstimator) update(arrival time.Time, ts uint32) time.Duration {
	if e.have {
		d := arrival.Sub(e.prevArrival).Seconds() - float64(int32(ts-e.prevTS))/SampleRate
		e.j += (math.Abs(d) - e.j) / 16
	}
	e.have, e.prevArrival, e.prevTS = true, arrival, ts
	return time.Duration(e.j * float64(time.Second))
}
