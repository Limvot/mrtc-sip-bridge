package media

import (
	"sync"
	"testing"
)

type legEvent struct {
	event   bool
	payload []byte
	samples uint32
	marker  bool
}

// fakeEventSIP is a SIPEventLeg that records audio and event packets in one ordered log.
type fakeEventSIP struct {
	*fakeSIP
	pt uint8
	ok bool
	mu sync.Mutex
	lg []legEvent
}

func (f *fakeEventSIP) TelephoneEventPT() (uint8, bool) { return f.pt, f.ok }

func (f *fakeEventSIP) WriteEvent(p []byte, samples uint32, marker bool) error {
	f.mu.Lock()
	f.lg = append(f.lg, legEvent{true, append([]byte(nil), p...), samples, marker})
	f.mu.Unlock()
	f.n.Add(1)
	return nil
}

func (f *fakeEventSIP) WriteRTP(p []byte, samples uint32, marker bool) error {
	f.mu.Lock()
	f.lg = append(f.lg, legEvent{false, append([]byte(nil), p...), samples, marker})
	f.mu.Unlock()
	f.n.Add(1)
	return nil
}

func (f *fakeEventSIP) log() []legEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]legEvent(nil), f.lg...)
}

func eventHarness(t *testing.T, pt uint8, ok bool) (*harness, *fakeEventSIP) {
	var leg *fakeEventSIP
	h := newHarnessLeg(t, Config{}, func(s *fakeSIP) SIPLeg {
		leg = &fakeEventSIP{fakeSIP: s, pt: pt, ok: ok}
		return leg
	})
	return h, leg
}

func TestDTMFEncodeRFC4733(t *testing.T) {
	var p [4]byte
	dtmfEncode(p[:], 5, false, 480) // digit 5, running, duration 480
	if p != [4]byte{5, 10, 0x01, 0xE0} {
		t.Fatalf("% x", p)
	}
	dtmfEncode(p[:], 11, true, 800) // '#', end bit, volume 10, duration 800
	if p != [4]byte{11, 0x8A, 0x03, 0x20} {
		t.Fatalf("% x", p)
	}
}

func TestDTMFCodes(t *testing.T) {
	want := map[byte]byte{'0': 0, '9': 9, '*': 10, '#': 11, 'A': 12, 'D': 15, 'a': 12}
	for c, w := range want {
		if got, ok := dtmfCode(c); !ok || got != w {
			t.Fatalf("%c -> %d %v", c, got, ok)
		}
	}
	for _, c := range []byte{'E', 'x', ' ', '+', 0} {
		if _, ok := dtmfCode(c); ok {
			t.Fatalf("%q accepted", c)
		}
	}
}

func TestSendDTMFErrors(t *testing.T) {
	h := newHarness(t, Config{}) // plain SIPLeg
	if err := h.b.SendDTMF("1"); err != ErrDTMFUnsupported {
		t.Fatal(err)
	}
	if err := h.b.SendDTMF("1x"); err != ErrDTMFInvalid {
		t.Fatal(err)
	}
	if err := h.b.SendDTMF(""); err != ErrDTMFInvalid {
		t.Fatal(err)
	}
	h2, _ := eventHarness(t, 101, false) // PT not negotiated
	if err := h2.b.SendDTMF("1"); err != ErrDTMFUnsupported {
		t.Fatal(err)
	}
	h3, _ := eventHarness(t, 101, true)
	if err := h3.b.SendDTMF("0123456789*#ABCD0123456789*#ABCD"); err != nil { // exactly 32
		t.Fatal(err)
	}
	if err := h3.b.SendDTMF("1"); err != ErrDTMFBusy {
		t.Fatal(err)
	}
	h4, _ := eventHarness(t, 101, true)
	if err := h4.b.SendDTMF("012345678901234567890123456789012"); err != ErrDTMFBusy { // 33
		t.Fatal(err)
	}
}

func TestDTMFSchedule(t *testing.T) {
	h, leg := eventHarness(t, 101, true)
	if err := h.b.SendDTMF("1#"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8+5+8+3; i++ {
		h.tick()
	}
	lg := leg.log()
	for i, e := range lg {
		wantEvent := i < 8 || (i >= 13 && i < 21)
		if e.event != wantEvent {
			t.Fatalf("packet %d event=%v", i, e.event)
		}
	}
	for d, code := range []byte{1, 11} {
		base := []int{0, 13}[d]
		var ts uint32
		for k := 0; k < 8; k++ {
			e := lg[base+k]
			dur := uint16(e.payload[2])<<8 | uint16(e.payload[3])
			end := e.payload[1]&0x80 != 0
			wantDur, wantEnd := uint16(160*(k+1)), false
			if k >= 5 {
				wantDur, wantEnd = 800, true
			}
			if e.payload[0] != code || e.payload[1]&0x3F != 10 || dur != wantDur || end != wantEnd {
				t.Fatalf("digit %d pkt %d: % x", d, k, e.payload)
			}
			if e.marker != (k == 0) {
				t.Fatalf("digit %d pkt %d marker=%v", d, k, e.marker)
			}
			wantSamples := uint32(0)
			if k == 7 {
				wantSamples = 1280
			}
			if e.samples != wantSamples {
				t.Fatalf("digit %d pkt %d samples=%d", d, k, e.samples)
			}
			ts += e.samples
		}
		if ts != 1280 {
			t.Fatal(ts)
		}
	}
	for _, e := range lg[8:13] { // audio gap: one frame of audio per tick
		if len(e.payload) != FrameSamples || e.samples != FrameSamples {
			t.Fatal("gap packet is not audio")
		}
	}
	if got := h.b.Stats().DTMFSent; got != 2 {
		t.Fatalf("DTMFSent = %d", got)
	}
	if err := h.b.SendDTMF("5"); err != nil { // queue works again afterwards
		t.Fatal(err)
	}
}
