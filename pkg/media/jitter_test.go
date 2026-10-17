package media

import "testing"

func pk(seq uint16) *RTPPacket {
	return &RTPPacket{SSRC: 1, Seq: seq, Payload: []byte{byte(seq), byte(seq >> 8)}}
}

func popSeq(t *testing.T, j *jitterBuffer, want int) {
	t.Helper()
	p, ok := j.pop()
	if want < 0 {
		if ok {
			t.Fatalf("expected concealment, got %v", p)
		}
		return
	}
	if !ok || p[0] != byte(want) {
		t.Fatalf("pop=%v,%v want seq %d", p, ok, want)
	}
}

func TestJitterInOrder(t *testing.T) {
	j := newJitterBuffer(2, 10)
	popSeq(t, j, -1)
	j.push(pk(10))
	popSeq(t, j, -1) // not started yet
	j.push(pk(11))
	for s := 10; s < 12; s++ {
		popSeq(t, j, s)
	}
	if l, late := j.stats(); l != 0 || late != 0 {
		t.Fatal(l, late)
	}
}

func TestJitterReorder(t *testing.T) {
	j := newJitterBuffer(3, 10)
	j.push(pk(2))
	j.push(pk(1))
	j.push(pk(3))
	for s := 1; s <= 3; s++ {
		popSeq(t, j, s)
	}
}

func TestJitterLossCountedOnce(t *testing.T) {
	j := newJitterBuffer(2, 10)
	j.push(pk(1))
	j.push(pk(3))
	j.push(pk(4))
	popSeq(t, j, 1)
	popSeq(t, j, -1)
	popSeq(t, j, 3)
	popSeq(t, j, 4)
	if l, late := j.stats(); l != 1 || late != 0 {
		t.Fatal(l, late)
	}
}

func TestJitterLateAndDuplicate(t *testing.T) {
	j := newJitterBuffer(2, 10)
	j.push(pk(1))
	j.push(pk(2))
	j.push(pk(2)) // duplicate while buffered
	popSeq(t, j, 1)
	popSeq(t, j, 2)
	j.push(pk(1)) // late
	j.push(pk(2)) // late duplicate
	if _, late := j.stats(); late != 3 {
		t.Fatal(late)
	}
}

func TestJitterUnderrunRebuffer(t *testing.T) {
	j := newJitterBuffer(2, 10)
	j.push(pk(1))
	j.push(pk(2))
	popSeq(t, j, 1)
	popSeq(t, j, 2)
	for i := 0; i < 10; i++ {
		popSeq(t, j, -1)
	}
	if l, _ := j.stats(); l != 0 {
		t.Fatal("underrun counted as loss", l)
	}
	j.push(pk(500))
	popSeq(t, j, -1) // re-buffering
	j.push(pk(501))
	popSeq(t, j, 500)
	popSeq(t, j, 501)
}

func TestJitterSeqWrap(t *testing.T) {
	j := newJitterBuffer(2, 10)
	for _, s := range []uint16{65534, 65535, 0, 1} {
		j.push(pk(s))
	}
	for _, w := range []int{254, 255, 0, 1} {
		popSeq(t, j, w)
	}
	if l, late := j.stats(); l != 0 || late != 0 {
		t.Fatal(l, late)
	}
}

func TestJitterSSRCReset(t *testing.T) {
	j := newJitterBuffer(1, 10)
	j.push(pk(5))
	j.push(pk(6))
	p := pk(900)
	p.SSRC = 2
	j.push(p)
	popSeq(t, j, 900&0xFF)
	if _, ok := j.pop(); ok {
		t.Fatal("old packets should be flushed")
	}
}

func TestJitterOverflowDropsOldest(t *testing.T) {
	j := newJitterBuffer(1, 4)
	for s := 1; s <= 4; s++ {
		j.push(pk(uint16(s)))
	}
	j.push(pk(5)) // buffer full: drop 1
	if _, late := j.stats(); late != 1 {
		t.Fatal(late)
	}
	popSeq(t, j, 2)
}

func TestJitterFarAheadResets(t *testing.T) {
	j := newJitterBuffer(1, 4)
	j.push(pk(1))
	j.push(pk(100))
	popSeq(t, j, 100)
}

func TestJitterZeroAllocs(t *testing.T) {
	j := newJitterBuffer(3, 20)
	var seq uint16
	p := &RTPPacket{SSRC: 1, Payload: make([]byte, 160)}
	for i := 0; i < 10; i++ {
		p.Seq = seq
		seq++
		j.push(p)
		j.pop()
	}
	if n := testing.AllocsPerRun(1000, func() {
		p.Seq = seq
		seq++
		j.push(p)
		j.pop()
	}); n != 0 {
		t.Fatalf("allocs=%v", n)
	}
}

func TestJitterLateAfterUnderrunPlays(t *testing.T) {
	j := newJitterBuffer(2, 10)
	j.push(pk(1))
	j.push(pk(2))
	popSeq(t, j, 1)
	popSeq(t, j, 2)
	for i := 0; i < 3; i++ { // underrun, fewer than 10 ticks
		popSeq(t, j, -1)
	}
	j.push(pk(3)) // merely late: must not be dropped
	popSeq(t, j, 3)
	if l, late := j.stats(); l != 0 || late != 0 {
		t.Fatal(l, late)
	}
}

func TestJitterDTXGapPlays(t *testing.T) {
	j := newJitterBuffer(2, 10)
	j.push(pk(1))
	j.push(pk(2))
	popSeq(t, j, 1)
	popSeq(t, j, 2)
	for i := 0; i < 5; i++ {
		popSeq(t, j, -1)
	}
	j.push(pk(10)) // sender skipped seqs during silence
	popSeq(t, j, 10)
}

func TestJitterLatencyBound(t *testing.T) {
	j := newJitterBuffer(2, 20)
	seq := 0
	for i := 0; i < 8; i++ {
		j.push(pk(uint16(seq)))
		seq++
	}
	// Hold the depth at 8 frames (one in, one out per tick) for 50 pops: one frame is dropped.
	for i := 0; i < latencyPops; i++ {
		j.push(pk(uint16(seq)))
		seq++
		if _, ok := j.pop(); !ok {
			t.Fatal("unexpected concealment")
		}
	}
	if got := j.buffered(); got != 7 {
		t.Fatalf("buffered %d, want 7 after latency drop", got)
	}
}
