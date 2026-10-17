package main

import (
	"encoding/binary"
	"math"
	"os"
)

// writeToneWAV writes an 8 kHz, 16-bit, mono PCM WAV with a sine tone and short
// fades (diago's PlayFile needs exactly this format for PCMU).
func writeToneWAV(path string, hz, ms int) error {
	const rate = 8000
	n := rate * ms / 1000
	data := make([]byte, n*2)
	fade := rate / 100 // 10 ms
	for i := 0; i < n; i++ {
		amp := 0.3
		if i < fade {
			amp *= float64(i) / float64(fade)
		} else if n-i < fade {
			amp *= float64(n-i) / float64(fade)
		}
		s := int16(amp * 32767 * math.Sin(2*math.Pi*float64(hz)*float64(i)/rate))
		binary.LittleEndian.PutUint16(data[i*2:], uint16(s))
	}
	hdr := make([]byte, 44)
	copy(hdr[0:], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:], uint32(36+len(data)))
	copy(hdr[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(hdr[16:], 16)
	binary.LittleEndian.PutUint16(hdr[20:], 1) // PCM
	binary.LittleEndian.PutUint16(hdr[22:], 1) // mono
	binary.LittleEndian.PutUint32(hdr[24:], rate)
	binary.LittleEndian.PutUint32(hdr[28:], rate*2)
	binary.LittleEndian.PutUint16(hdr[32:], 2)
	binary.LittleEndian.PutUint16(hdr[34:], 16)
	copy(hdr[36:], "data")
	binary.LittleEndian.PutUint32(hdr[40:], uint32(len(data)))
	return os.WriteFile(path, append(hdr, data...), 0o600)
}
