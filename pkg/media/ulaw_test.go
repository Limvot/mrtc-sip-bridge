package media

import (
	"math"
	"testing"
)

func TestULawKnownValues(t *testing.T) {
	var d [1]int16
	for _, c := range []struct {
		b byte
		v int16
	}{{0xFF, 0}, {0x7F, 0}, {0x00, -32124}, {0x80, 32124}, {0xFE, 8}, {0x7E, -8}} {
		ULawDecode(d[:], []byte{c.b})
		if d[0] != c.v {
			t.Errorf("decode(%#x)=%d want %d", c.b, d[0], c.v)
		}
	}
	var e [1]byte
	ULawEncode(e[:], []int16{0})
	if e[0] != ULawSilence {
		t.Errorf("encode(0)=%#x", e[0])
	}
	ULawEncode(e[:], []int16{32767})
	if e[0] != 0x80 {
		t.Errorf("encode(max)=%#x", e[0])
	}
	ULawEncode(e[:], []int16{-32768})
	if e[0] != 0x00 {
		t.Errorf("encode(min)=%#x", e[0])
	}
}

func TestULawTableSymmetry(t *testing.T) {
	for i := 0; i < 128; i++ {
		if ulawDecodeTable[i] != -ulawDecodeTable[i|0x80] && !(i == 0x7F) {
			t.Errorf("asymmetry at %#x: %d vs %d", i, ulawDecodeTable[i], ulawDecodeTable[i|0x80])
		}
		if ulawDecodeTable[i] > 0 {
			t.Errorf("entry %#x should be non-positive", i)
		}
	}
	// every code decodes then re-encodes to itself (0x7F and 0xFF both map to 0)
	for i := 0; i < 256; i++ {
		var e [1]byte
		ULawEncode(e[:], []int16{ulawDecodeTable[i]})
		if e[0] != byte(i) && !(i == 0x7F && e[0] == 0xFF) {
			t.Errorf("code %#x re-encodes to %#x", i, e[0])
		}
	}
}

func TestULawRoundTrip(t *testing.T) {
	for x := -32124; x <= 32124; x += 7 {
		var e [1]byte
		var d [1]int16
		ULawEncode(e[:], []int16{int16(x)})
		ULawDecode(d[:], e[:])
		if ax := math.Abs(float64(x)); ax > 1000 {
			if err := math.Abs(float64(d[0])-float64(x)) / ax; err > 0.04 {
				t.Fatalf("x=%d got %d err %.3f", x, d[0], err)
			}
		}
	}
}
