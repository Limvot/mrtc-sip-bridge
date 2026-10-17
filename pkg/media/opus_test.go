package media

import (
	"math"
	"testing"
)

func sine(freq float64, start, n int, amp float64) []int16 {
	out := make([]int16, n)
	for i := range out {
		out[i] = int16(amp * math.Sin(2*math.Pi*freq*float64(start+i)/SampleRate))
	}
	return out
}

func rms(x []int16) float64 {
	var s float64
	for _, v := range x {
		s += float64(v) * float64(v)
	}
	return math.Sqrt(s / float64(len(x)))
}

func zeroCrossFreq(x []int16) float64 {
	c := 0
	for i := 1; i < len(x); i++ {
		if (x[i-1] < 0) != (x[i] < 0) {
			c++
		}
	}
	return float64(c) / 2 / (float64(len(x)) / SampleRate)
}

func checkTone(t *testing.T, pcm []int16, amp float64) {
	t.Helper()
	in := amp / math.Sqrt2
	if db := 20 * math.Log10(rms(pcm)/in); math.Abs(db) > 3 {
		t.Errorf("rms off by %.2f dB", db)
	}
	if f := zeroCrossFreq(pcm); math.Abs(f-440) > 20 {
		t.Errorf("freq %.1f", f)
	}
}

func TestOpusRoundTrip(t *testing.T) {
	enc, err := newOpusEncoder()
	if err != nil {
		t.Fatal(err)
	}
	dec, err := newOpusDecoder()
	if err != nil {
		t.Fatal(err)
	}
	var out []int16
	buf := make([]byte, maxOpusPacket)
	pcm := make([]int16, maxDecodePCM)
	for f := 0; f < 50; f++ {
		n, err := enc.encode(sine(440, f*FrameSamples, FrameSamples, 10000), buf)
		if err != nil {
			t.Fatal(err)
		}
		m, err := dec.decode(buf[:n], pcm)
		if err != nil || m != FrameSamples {
			t.Fatal(m, err)
		}
		if f >= 3 {
			out = append(out, pcm[:m]...)
		}
	}
	checkTone(t, out, 10000)
	if err := dec.plc(pcm[:FrameSamples]); err != nil {
		t.Fatal(err)
	}
}
