package media

const (
	ulawBias = 0x84
	ulawClip = 32635
)

// ulawDecodeTable maps a µ-law byte to its PCM16 value (ITU-T G.711).
var ulawDecodeTable = func() (t [256]int16) {
	for i := range t {
		u := ^byte(i)
		exp := (u >> 4) & 7
		mant := int(u & 0x0F)
		v := ((mant<<3 + ulawBias) << exp) - ulawBias
		if u&0x80 != 0 {
			v = -v
		}
		t[i] = int16(v)
	}
	return
}()

func ulawEncodeSample(x int16) byte {
	s := int(x)
	var sign byte
	if s < 0 {
		s = -s
		sign = 0x80
	}
	if s > ulawClip {
		s = ulawClip
	}
	s += ulawBias
	exp := 7
	for mask := 0x4000; exp > 0 && s&mask == 0; mask >>= 1 {
		exp--
	}
	mant := byte(s>>(uint(exp)+3)) & 0x0F
	return ^(sign | byte(exp)<<4 | mant)
}

// ULawEncode encodes len(src) PCM16 samples into dst (len(dst) >= len(src)). G.711 µ-law, bias 0x84.
func ULawEncode(dst []byte, src []int16) {
	dst = dst[:len(src)]
	for i, x := range src {
		dst[i] = ulawEncodeSample(x)
	}
}

// ULawDecode decodes len(src) µ-law bytes into dst.
func ULawDecode(dst []int16, src []byte) {
	dst = dst[:len(src)]
	for i, b := range src {
		dst[i] = ulawDecodeTable[b]
	}
}
