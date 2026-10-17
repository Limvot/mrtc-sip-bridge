package media

import "sync"

const jitterSlotSize = 1500

// Latency bound: when the buffer holds more than delay+latencySlack frames for latencyPops consecutive
// pops (the sender's clock runs faster than ours), one frame is dropped.
const (
	latencySlack = 3
	latencyPops  = 50
)

type jitterSlot struct {
	used bool
	n    int
	buf  [jitterSlotSize]byte
}

// jitterBuffer is a sequence-ordered playout buffer (PHASE2-SPEC 7.3). Timestamps are ignored.
type jitterBuffer struct {
	mu         sync.Mutex
	delay, max int
	mask       uint16
	slots      []jitterSlot // indexed by seq&mask
	count      int
	started    bool
	next       uint16 // next seq to play (started) / lowest buffered seq (not started, count>0)
	ssrc       uint32
	haveSSRC   bool
	underruns  int
	over       int // consecutive pops with count > delay+latencySlack
	lost, late uint64
	out        [jitterSlotSize]byte
}

func newJitterBuffer(delayFrames, maxFrames int) *jitterBuffer {
	if delayFrames < 1 {
		delayFrames = 1
	}
	if maxFrames < delayFrames+1 {
		maxFrames = delayFrames + 1
	}
	size := 1
	for size < maxFrames+1 {
		size <<= 1
	}
	return &jitterBuffer{delay: delayFrames, max: maxFrames, mask: uint16(size - 1), slots: make([]jitterSlot, size)}
}

func (j *jitterBuffer) flushLocked() {
	for i := range j.slots {
		j.slots[i].used = false
	}
	j.count, j.started, j.underruns, j.over = 0, false, 0, 0
}

func (j *jitterBuffer) storeLocked(p *RTPPacket) {
	s := &j.slots[p.Seq&j.mask]
	s.used = true
	s.n = copy(s.buf[:], p.Payload)
	j.count++
}

// push copies p.Payload into a free slot. Called from the producer goroutine.
func (j *jitterBuffer) push(p *RTPPacket) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.haveSSRC && p.SSRC != j.ssrc {
		j.flushLocked()
	}
	j.ssrc, j.haveSSRC = p.SSRC, true

	if !j.started {
		if j.count == 0 {
			j.next = p.Seq
		}
		d := int(int16(p.Seq - j.next))
		switch {
		case d > 1000 || d < -1000 || d > j.max || -d > j.max:
			j.flushLocked()
			j.next = p.Seq
		case d < 0:
			j.next = p.Seq
		}
		if s := &j.slots[p.Seq&j.mask]; s.used {
			j.late++ // duplicate
			return
		}
		j.storeLocked(p)
		if j.count >= j.delay {
			j.started = true
		}
		return
	}

	d := int(int16(p.Seq - j.next))
	switch {
	case d < -1000 || d > j.max:
		j.flushLocked()
		j.next = p.Seq
		j.storeLocked(p)
		if j.count >= j.delay {
			j.started = true
		}
		return
	case d < 0:
		j.late++
		return
	case d > 0 && j.count == 0:
		j.next = p.Seq // empty buffer (underrun or DTX gap): resume at this packet, nothing was lost
	}
	if j.slots[p.Seq&j.mask].used {
		j.late++
		return
	}
	if j.count >= j.max {
		// Drop the oldest buffered packet and skip past it.
		for i := 0; i < len(j.slots); i++ {
			seq := j.next + uint16(i)
			if s := &j.slots[seq&j.mask]; s.used {
				s.used = false
				j.count--
				j.late++
				j.next = seq + 1
				break
			}
		}
		if int(int16(p.Seq-j.next)) < 0 {
			j.late++
			return
		}
	}
	j.storeLocked(p)
}

// pop returns the payload for the next playout tick (valid until the next pop) or ok=false for a
// concealed frame. Called from the ticker goroutine.
func (j *jitterBuffer) pop() (payload []byte, ok bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if !j.started {
		return nil, false
	}
	if j.count > j.delay+latencySlack {
		j.over++
		if j.over >= latencyPops {
			j.over = 0
			for i := 0; i < len(j.slots); i++ {
				seq := j.next + uint16(i)
				if s := &j.slots[seq&j.mask]; s.used {
					s.used = false
					j.count--
					j.next = seq + 1
					break
				}
			}
		}
	} else {
		j.over = 0
	}
	if j.count == 0 {
		// Underrun: do not advance next, so a merely-late packet still plays (a DTX-style gap resumes
		// at the packet's own seq). Re-buffer after 10 consecutive empty ticks.
		j.underruns++
		if j.underruns >= 10 {
			j.started, j.underruns = false, 0
		}
		return nil, false
	}
	s := &j.slots[j.next&j.mask]
	j.next++
	j.underruns = 0
	if s.used {
		s.used = false
		j.count--
		n := copy(j.out[:], s.buf[:s.n])
		return j.out[:n], true
	}
	j.lost++
	return nil, false
}

func (j *jitterBuffer) stats() (lost, late uint64) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.lost, j.late
}

func (j *jitterBuffer) buffered() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.count
}
