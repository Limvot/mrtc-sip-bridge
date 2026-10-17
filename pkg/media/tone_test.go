package media

import (
	"math"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
)

func goertzel(pcm []int16, freq float64) float64 {
	w := 2 * math.Pi * freq / SampleRate
	coeff := 2 * math.Cos(w)
	var s1, s2 float64
	for _, v := range pcm {
		s0 := float64(v) + coeff*s1 - s2
		s2, s1 = s1, s0
	}
	p := s1*s1 + s2*s2 - coeff*s1*s2
	return p / float64(len(pcm)) / float64(len(pcm)) * 4 // squared amplitude estimate
}

func TestToneCadenceAndFrequencies(t *testing.T) {
	cases := []struct {
		p          TonePattern
		on, off    int // frames
		fa, fb, fx float64
	}{
		{ToneRingback, 100, 200, 440, 480, 620},
		{ToneBusy, 25, 25, 480, 620, 440},
		{ToneReorder, 12, 13, 480, 620, 440}, // 0.25 s = 12.5 frames: checked on a 25-frame period
	}
	for _, c := range cases {
		var g toneGen
		g.init(c.p)
		period := c.on + c.off
		frames := make([][]int16, 600) // 12 s
		for i := range frames {
			frames[i] = make([]int16, FrameSamples)
			g.frame(frames[i])
		}
		for i, f := range frames {
			pos := i % period
			loud := rms(f) > 1000
			if c.p == ToneReorder {
				// 250 ms on / 250 ms off = 12.5 frames: only check frames clear of the edges.
				p := i % 25
				if (p < 12 && !loud) || (p > 13 && p < 25 && loud) {
					t.Fatalf("reorder frame %d loud=%v", i, loud)
				}
				continue
			}
			if (pos < c.on) != loud {
				t.Fatalf("pattern %d frame %d loud=%v", c.p, i, loud)
			}
		}
		// Measure the first on-burst spectrum.
		var burst []int16
		for _, f := range frames[:12] {
			burst = append(burst, f...)
		}
		a, b, x := goertzel(burst, c.fa), goertzel(burst, c.fb), goertzel(burst, c.fx)
		want := toneAmplitude * toneAmplitude
		for _, v := range []float64{a, b} {
			if v < want*0.7 || v > want*1.3 {
				t.Fatalf("pattern %d: component power %.0f, want about %.0f", c.p, v, want)
			}
		}
		if x > want*0.02 {
			t.Fatalf("pattern %d: stray component %.0f Hz power %.0f", c.p, c.fx, x)
		}
	}
}

func TestToneGenZeroAllocs(t *testing.T) {
	var g toneGen
	g.init(ToneRingback)
	pcm := make([]int16, FrameSamples)
	if n := testing.AllocsPerRun(1000, func() { g.frame(pcm) }); n != 0 {
		t.Fatal(n)
	}
}

func TestStartToneWritesFramesAndStops(t *testing.T) {
	clk := clock.NewFake(time.Unix(1_000_000, 0))
	rtc := &fakeRTC{}
	tone, err := StartTone(Config{Clock: clk, Log: zerolog.Nop()}, rtc, ToneBusy)
	if err != nil {
		t.Fatal(err)
	}
	if !clk.WaitForWaiters(1) {
		t.Fatal("no ticker")
	}
	for i := 1; i <= 50; i++ { // one second
		clk.Advance(FrameDuration)
		for rtc.n.Load() < int64(i) {
			time.Sleep(50 * time.Microsecond)
		}
	}
	tone.Stop()
	tone.Stop()
	if got := rtc.n.Load(); got != 50 {
		t.Fatalf("frames = %d", got)
	}
	clk.Advance(time.Second)
	time.Sleep(10 * time.Millisecond)
	if got := rtc.n.Load(); got != 50 {
		t.Fatalf("frames after Stop = %d", got)
	}
	frames := decodeFrames(t, rtc.framesCopy())
	if rms(frames[5]) < 1000 || rms(frames[30]) > 200 {
		t.Fatalf("busy cadence not audible: %.0f %.0f", rms(frames[5]), rms(frames[30]))
	}
}
