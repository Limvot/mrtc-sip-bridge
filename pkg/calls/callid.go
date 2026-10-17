package calls

import (
	"crypto/rand"
	"encoding/binary"
	"time"
)

// crockford is the Crockford base32 alphabet used by ULIDs.
const crockford = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// newCallID returns a ULID (48-bit millisecond timestamp, 80 random bits, 26 Crockford base32
// characters) for now (PHASE2-SPEC D20).
func newCallID(now time.Time) string {
	var b [16]byte
	ms := uint64(now.UnixMilli())
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	_, _ = rand.Read(b[6:]) // crypto/rand.Read never returns an error (Go 1.24+)
	return encodeULID(b)
}

// encodeULID encodes 128 bits as 26 base32 characters (the first carries the top 3 bits).
func encodeULID(b [16]byte) string {
	var out [26]byte
	hi := binary.BigEndian.Uint64(b[:8])
	lo := binary.BigEndian.Uint64(b[8:])
	for i := 25; i >= 0; i-- {
		out[i] = crockford[lo&31]
		lo = lo>>5 | hi<<59
		hi >>= 5
	}
	return string(out[:])
}

// deviceIDFor returns the ghost membership device id for a call id: "SIP" + the last 8 characters
// (only [0-9A-Z], wire §1.2).
func deviceIDFor(callID string) string {
	if len(callID) < 8 {
		return "SIP" + callID
	}
	return "SIP" + callID[len(callID)-8:]
}
