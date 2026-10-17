package media

import (
	"sync"
	"sync/atomic"
)

// RFC 4733 timing: 8 packets over 8 ticks (160 ms), then 5 audio ticks (100 ms) between digits.
const (
	dtmfQueueLen     = 32
	dtmfVolume       = 10 // -10 dBm0
	dtmfPackets      = 8  // 5 running packets + 3 end packets
	dtmfRunning      = 5
	dtmfEndDuration  = 800 // 160 * dtmfRunning
	dtmfGapTicks     = 5
	dtmfEndBit       = 0x80
	dtmfPayloadBytes = 4
)

// dtmfState is the SendDTMF queue (mutex, slow path) and the rtcToSIP-goroutine-only schedule state.
type dtmfState struct {
	mu      sync.Mutex
	ring    [dtmfQueueLen]byte // event codes
	head, n int
	pending atomic.Int32 // mirrors n, lets the tick skip the mutex when idle

	// rtcToSIP goroutine only
	active bool
	code   byte
	pkt    int // packets sent for the current digit
	gap    int // audio ticks remaining before the next digit may start
	buf    [dtmfPayloadBytes]byte
}

// dtmfCode maps a digit character to its RFC 4733 event code.
func dtmfCode(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c == '*':
		return 10, true
	case c == '#':
		return 11, true
	case c >= 'A' && c <= 'D':
		return 12 + c - 'A', true
	case c >= 'a' && c <= 'd':
		return 12 + c - 'a', true
	}
	return 0, false
}

// dtmfEncode writes the 4-byte telephone-event payload (RFC 4733 section 2.3) into p.
func dtmfEncode(p []byte, code byte, end bool, duration uint16) {
	p[0] = code
	p[1] = dtmfVolume
	if end {
		p[1] |= dtmfEndBit
	}
	p[2], p[3] = byte(duration>>8), byte(duration)
}

// SendDTMF queues digits for RFC 4733 transmission. It never logs the digits.
func (b *bridge) SendDTMF(digits string) error {
	if digits == "" {
		return ErrDTMFInvalid
	}
	var codes [dtmfQueueLen]byte
	for i := 0; i < len(digits); i++ {
		c, ok := dtmfCode(digits[i])
		if !ok {
			return ErrDTMFInvalid
		}
		if i < dtmfQueueLen {
			codes[i] = c
		}
	}
	if b.early.Load() {
		return ErrDTMFNotConnected
	}
	if b.events == nil {
		return ErrDTMFUnsupported
	}
	if _, ok := b.events.TelephoneEventPT(); !ok {
		return ErrDTMFUnsupported
	}
	d := &b.dtmf
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(digits) > dtmfQueueLen || d.n+len(digits) > dtmfQueueLen {
		return ErrDTMFBusy
	}
	for i := 0; i < len(digits); i++ {
		d.ring[(d.head+d.n)%dtmfQueueLen] = codes[i]
		d.n++
	}
	d.pending.Store(int32(d.n))
	return nil
}

func (d *dtmfState) pop() (byte, bool) {
	if d.pending.Load() == 0 {
		return 0, false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.n == 0 {
		return 0, false
	}
	c := d.ring[d.head]
	d.head = (d.head + 1) % dtmfQueueLen
	d.n--
	d.pending.Store(int32(d.n))
	return c, true
}

// dtmfStep runs on the rtcToSIP tick. It returns true if it sent an event packet (so the tick must not
// send audio).
func (b *bridge) dtmfStep() bool {
	d := &b.dtmf
	if !d.active {
		if d.gap > 0 {
			d.gap--
			return false
		}
		code, ok := d.pop()
		if !ok {
			return false
		}
		d.active, d.code, d.pkt = true, code, 0
		b.fifoN = 0 // owner audio decoded so far is dropped
	}
	// Owner audio arriving during the digit is dropped.
	b.jRTC.pop()
	d.pkt++
	var samples uint32
	end := d.pkt > dtmfRunning
	dur := uint16(160 * d.pkt)
	if end {
		dur = dtmfEndDuration
	}
	last := d.pkt == dtmfPackets
	if last {
		samples = FrameSamples * dtmfPackets
	}
	dtmfEncode(d.buf[:], d.code, end, dur)
	err := b.events.WriteEvent(d.buf[:], samples, d.pkt == 1)
	b.cnt.sipTx.Add(1)
	b.logR2S(err)
	if last {
		d.active = false
		d.gap = dtmfGapTicks
		b.cnt.dtmfSent.Add(1)
	}
	return true
}
