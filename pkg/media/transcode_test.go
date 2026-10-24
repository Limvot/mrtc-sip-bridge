package media

import (
	"math"
	"testing"
)

func TestOpusPCMURoundTrip(t *testing.T) {
	tr, err := NewOpusPCMU()
	if err != nil {
		t.Fatal(err)
	}
	// 20 frames of a 400 Hz sine through PCMU -> Opus -> PCMU.
	for n := 0; n < 20; n++ {
		pcm := make([]int16, FrameSamples)
		for i := range pcm {
			pcm[i] = int16(8000 * math.Sin(2*math.Pi*400*float64(n*FrameSamples+i)/SampleRate))
		}
		pcmu := make([]byte, FrameSamples)
		ULawEncode(pcmu, pcm)

		opus, err := tr.PCMUToOpus(pcmu)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		frames, err := tr.OpusToPCMU(opus)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(frames) != 1 || len(frames[0]) != FrameSamples {
			t.Fatalf("frame count/size: %d %d", len(frames), len(frames[0]))
		}
		// The signal must survive with its energy intact (mu-law + opus are lossy and opus has
		// lookahead, so sample-wise comparison is meaningless).
		back := make([]int16, FrameSamples)
		ULawDecode(back, frames[0])
		rmsIn, rmsOut := rms(pcm), rms(back)
		if rmsIn > 0 && (rmsOut < 0.5*rmsIn || rmsOut > 2*rmsIn) {
			t.Fatalf("round trip energy mismatch: in=%v out=%v", rmsIn, rmsOut)
		}
	}
}

func TestOpusToPCMUMultiFrame(t *testing.T) {
	tr, err := NewOpusPCMU()
	if err != nil {
		t.Fatal(err)
	}
	// 40 ms of silence in one Opus packet yields two 20 ms PCMU frames.
	var pcm [320]int16
	_ = pcm
	enc, err := newOpusEncoder()
	if err != nil {
		t.Fatal(err)
	}
	out := make([]byte, 4000)
	n, err := enc.encode(make([]int16, 320), out)
	if err != nil {
		t.Fatal(err)
	}
	frames, err := tr.OpusToPCMU(out[:n])
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 {
		t.Fatalf("want 2 frames, got %d", len(frames))
	}
}

func TestPCMUToOpusRejectsShortFrame(t *testing.T) {
	tr, err := NewOpusPCMU()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tr.PCMUToOpus(make([]byte, 10)); err != ErrShortFrame {
		t.Fatalf("want ErrShortFrame, got %v", err)
	}
}
