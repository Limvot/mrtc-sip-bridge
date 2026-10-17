package media

import (
	"math"
	"sync"
	"time"

	"github.com/rs/zerolog"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
)

// toneAmplitude is the peak of each of the two components (0.12 of full scale, about -19 dBm0 each).
const toneAmplitude = 0.12 * 32767

// toneGen produces 160-sample PCM16 call-progress tone frames. Cadence is by sample counting; frame
// allocates nothing.
type toneGen struct {
	inc1, inc2 float64 // phase increment per sample (radians)
	ph1, ph2   float64
	pos        int // sample position within the cadence period
	on, period int // samples
}

func (g *toneGen) init(p TonePattern) {
	var f1, f2 float64
	var on, off time.Duration
	switch p {
	case ToneBusy:
		f1, f2, on, off = 480, 620, 500*time.Millisecond, 500*time.Millisecond
	case ToneReorder:
		f1, f2, on, off = 480, 620, 250*time.Millisecond, 250*time.Millisecond
	default: // ToneRingback
		f1, f2, on, off = 440, 480, 2*time.Second, 4*time.Second
	}
	*g = toneGen{
		inc1:   2 * math.Pi * f1 / SampleRate,
		inc2:   2 * math.Pi * f2 / SampleRate,
		on:     int(on * SampleRate / time.Second),
		period: int((on + off) * SampleRate / time.Second),
	}
}

// frame fills pcm (FrameSamples long) with the next 20 ms of the pattern.
func (g *toneGen) frame(pcm []int16) {
	for i := range pcm {
		if g.pos < g.on {
			pcm[i] = int16(toneAmplitude * (math.Sin(g.ph1) + math.Sin(g.ph2)))
		} else {
			pcm[i] = 0
		}
		g.ph1 += g.inc1
		if g.ph1 >= 2*math.Pi {
			g.ph1 -= 2 * math.Pi
		}
		g.ph2 += g.inc2
		if g.ph2 >= 2*math.Pi {
			g.ph2 -= 2 * math.Pi
		}
		g.pos++
		if g.pos >= g.period {
			g.pos = 0
		}
	}
}

type toneRunner struct {
	gen  toneGen
	enc  *opusEncoder
	rtc  RTCLeg
	clk  clock.Clock
	log  zerolog.Logger
	pcm  [FrameSamples]int16
	out  [maxOpusPacket]byte
	stop chan struct{}
	once sync.Once
	wg   sync.WaitGroup
}

func startTone(cfg Config, rtc RTCLeg, p TonePattern) (Tone, error) {
	cfg = cfg.withDefaults()
	enc, err := newOpusEncoder()
	if err != nil {
		return nil, err
	}
	t := &toneRunner{enc: enc, rtc: rtc, clk: cfg.Clock, log: cfg.Log, stop: make(chan struct{})}
	t.gen.init(p)
	tk := t.clk.NewTicker(FrameDuration) // created before return (deterministic under a fake clock)
	t.wg.Add(1)
	go t.run(tk)
	return t, nil
}

func (t *toneRunner) run(tk clock.Ticker) {
	defer t.wg.Done()
	defer tk.Stop()
	var errLog time.Time
	for {
		select {
		case <-t.stop:
			return
		case <-tk.C():
			t.gen.frame(t.pcm[:])
			n, err := t.enc.encode(t.pcm[:], t.out[:])
			if err == nil {
				err = t.rtc.WriteOpus(t.out[:n], FrameDuration)
			}
			if err != nil {
				if now := t.clk.Now(); now.Sub(errLog) >= errLogInterval {
					errLog = now
					t.log.Warn().Err(err).Msg("media: tone frame failed")
				}
			}
		}
	}
}

func (t *toneRunner) Stop() {
	t.once.Do(func() { close(t.stop) })
	t.wg.Wait()
}
