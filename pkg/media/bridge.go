package media

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/rs/zerolog"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
)

const errLogInterval = 10 * time.Second

type bridge struct {
	cfg  Config
	clk  clock.Clock
	log  zerolog.Logger
	sip  SIPLeg
	rtc  RTCLeg
	cnt  counters
	jSIP *jitterBuffer
	jRTC *jitterBuffer

	startTime time.Time
	stopCh    chan struct{}
	stopOnce  sync.Once
	done      chan struct{}
	doneOnce  sync.Once
	err       error
	wg        sync.WaitGroup

	// reader goroutine state
	jit jitterEstimator

	// sipToRTC goroutine state
	enc       *opusEncoder
	s2rPCM    [FrameSamples]int16
	s2rOut    [maxOpusPacket]byte
	s2rErrLog time.Time

	// rtcToSIP goroutine state
	dec         *opusDecoder
	r2sPCM      [maxDecodePCM]int16
	fifo        [2 * maxDecodePCM]int16
	fifoN       int
	lastSamples int
	consec      int
	frame       [FrameSamples]byte
	sentFirst   bool
	r2sErrLog   time.Time
	events      SIPEventLeg // non-nil if the SIP leg can send telephone-event
	dtmf        dtmfState

	// early-media mode (D10)
	early       atomic.Bool
	connectedAt atomic.Int64 // unix nanoseconds, 0 = never connected
	connMu      sync.Mutex   // serialises Connect and the receiver detach in Stop
	ringback    toneGen
}

func (c Config) withDefaults() Config {
	if c.Clock == nil {
		c.Clock = clock.Real()
	}
	if c.JitterDelay <= 0 {
		c.JitterDelay = 60 * time.Millisecond
	}
	if c.JitterMax <= 0 {
		c.JitterMax = 400 * time.Millisecond
	}
	if c.RTPTimeout <= 0 {
		c.RTPTimeout = 60 * time.Second
	}
	if c.EarlyFillAfter <= 0 {
		c.EarlyFillAfter = time.Second
	}
	return c
}

// newBridge builds the bridge and codecs without starting any goroutine.
func newBridge(cfg Config, sip SIPLeg, rtc RTCLeg) (*bridge, error) {
	cfg = cfg.withDefaults()
	enc, err := newOpusEncoder()
	if err != nil {
		return nil, err
	}
	dec, err := newOpusDecoder()
	if err != nil {
		return nil, err
	}
	b := &bridge{
		cfg: cfg, clk: cfg.Clock, log: cfg.Log, sip: sip, rtc: rtc, enc: enc, dec: dec,
		jSIP:      newJitterBuffer(int(cfg.JitterDelay/FrameDuration), int(cfg.JitterMax/FrameDuration)),
		jRTC:      newJitterBuffer(int(cfg.JitterDelay/FrameDuration), int(cfg.JitterMax/FrameDuration)),
		stopCh:    make(chan struct{}),
		done:      make(chan struct{}),
		startTime: cfg.Clock.Now(),
	}
	b.events, _ = sip.(SIPEventLeg)
	b.ringback.init(ToneRingback)
	b.early.Store(cfg.Early)
	return b, nil
}

func start(cfg Config, sip SIPLeg, rtc RTCLeg) (Bridge, error) {
	b, err := newBridge(cfg, sip, rtc)
	if err != nil {
		return nil, err
	}
	// Tickers are created here so they exist before Start returns (deterministic under a fake clock).
	t1 := b.clk.NewTicker(FrameDuration)
	t2 := b.clk.NewTicker(FrameDuration)
	b.wg.Add(2)
	go b.tickLoop(t1, b.sipToRTCStep)
	go b.tickLoop(t2, b.rtcToSIPStep)
	go b.sipReader()
	if !cfg.Early {
		rtc.SetReceiver(b)
	}
	return b, nil
}

func (b *bridge) tickLoop(t clock.Ticker, step func()) {
	defer b.wg.Done()
	defer t.Stop()
	for {
		select {
		case <-b.stopCh:
			return
		case <-b.done:
			return
		case <-t.C():
			step()
		}
	}
}

func (b *bridge) finish(err error) {
	b.doneOnce.Do(func() {
		b.err = err
		close(b.done)
	})
}

func (b *bridge) sipReader() {
	var pkt RTPPacket
	for {
		pkt = RTPPacket{}
		if err := b.sip.ReadRTP(&pkt); err != nil {
			b.finish(err)
			return
		}
		select {
		case <-b.stopCh:
			continue // discard until the SIP media closes
		default:
		}
		b.handleSIPPacket(&pkt)
	}
}

// handleSIPPacket is the per-packet reader logic (atomics only, no allocation).
func (b *bridge) handleSIPPacket(p *RTPPacket) {
	now := b.clk.Now()
	b.cnt.sipRx.Add(1)
	b.cnt.lastSIPRx.Store(now.UnixNano())
	if p.PayloadType != PayloadPCMU {
		return
	}
	b.cnt.sipJitter.Store(int64(b.jit.update(now, p.Timestamp)))
	b.jSIP.push(p)
}

func (b *bridge) sipToRTCStep() {
	now := b.clk.Now()
	last := b.startTime
	if n := b.cnt.lastSIPRx.Load(); n != 0 {
		last = time.Unix(0, n)
	}
	early := b.early.Load()
	if !early {
		if n := b.connectedAt.Load(); n != 0 {
			if c := time.Unix(0, n); c.After(last) {
				last = c
			}
		}
		if now.Sub(last) > b.cfg.RTPTimeout {
			b.finish(ErrRTPTimeout)
			return
		}
	}
	pcm := b.s2rPCM[:]
	if early && now.Sub(last) >= b.cfg.EarlyFillAfter {
		b.ringback.frame(pcm)
	} else if payload, ok := b.jSIP.pop(); ok {
		if len(payload) > FrameSamples {
			payload = payload[:FrameSamples]
		}
		ULawDecode(pcm[:len(payload)], payload)
		clear(pcm[len(payload):])
	} else {
		clear(pcm)
		b.cnt.concealedS2R.Add(1)
	}
	n, err := b.enc.encode(pcm, b.s2rOut[:])
	if err == nil {
		err = b.rtc.WriteOpus(b.s2rOut[:n], FrameDuration)
	}
	if err != nil && now.Sub(b.s2rErrLog) >= errLogInterval {
		b.s2rErrLog = now
		b.log.Warn().Err(err).Msg("media: sip to rtc frame failed")
	}
}

// ReceiveRTP implements PacketReceiver (LiveKit track goroutine).
func (b *bridge) ReceiveRTP(p *RTPPacket) {
	b.cnt.rtcRx.Add(1)
	b.jRTC.push(p)
}

func (b *bridge) rtcToSIPStep() {
	if b.early.Load() {
		b.sendSilence()
		return
	}
	if b.dtmfStep() {
		return
	}
	if b.fifoN < FrameSamples {
		b.refill()
	}
	if b.fifoN < FrameSamples {
		clear(b.fifo[b.fifoN:FrameSamples])
		b.fifoN = FrameSamples
	}
	ULawEncode(b.frame[:], b.fifo[:FrameSamples])
	b.fifoN -= FrameSamples
	copy(b.fifo[:b.fifoN], b.fifo[FrameSamples:FrameSamples+b.fifoN])

	b.writeAudio()
}

// sendSilence sends one mu-law silence frame (early mode): the owner's audio is never sent or consumed.
func (b *bridge) sendSilence() {
	for i := range b.frame {
		b.frame[i] = ULawSilence
	}
	b.writeAudio()
}

// writeAudio sends b.frame as the next PCMU packet (marker on the first packet of the call).
func (b *bridge) writeAudio() {
	marker := !b.sentFirst
	b.sentFirst = true
	err := b.sip.WriteRTP(b.frame[:], FrameSamples, marker)
	b.cnt.sipTx.Add(1)
	b.logR2S(err)
}

func (b *bridge) logR2S(err error) {
	if err != nil {
		if now := b.clk.Now(); now.Sub(b.r2sErrLog) >= errLogInterval {
			b.r2sErrLog = now
			b.log.Warn().Err(err).Msg("media: rtc to sip frame failed")
		}
	}
}

// refill appends decoded or concealed owner audio to the FIFO.
func (b *bridge) refill() {
	if payload, ok := b.jRTC.pop(); ok {
		m, err := b.dec.decode(payload, b.r2sPCM[:])
		if err == nil && m > 0 {
			copy(b.fifo[b.fifoN:], b.r2sPCM[:m])
			b.fifoN += m
			b.lastSamples, b.consec = m, 0
			return
		}
	}
	b.cnt.concealedR2S.Add(1)
	if b.consec < 3 && b.lastSamples > 0 {
		b.consec++
		if err := b.dec.plc(b.r2sPCM[:b.lastSamples]); err == nil {
			copy(b.fifo[b.fifoN:], b.r2sPCM[:b.lastSamples])
			b.fifoN += b.lastSamples
			return
		}
	}
	b.consec++
	clear(b.fifo[b.fifoN : b.fifoN+FrameSamples])
	b.fifoN += FrameSamples
}

// Connect leaves early mode: owner audio starts flowing to SIP, ringback fill stops and the RTP timeout
// is measured from now. Idempotent, non-blocking.
func (b *bridge) Connect() {
	if !b.early.Load() {
		return
	}
	b.connMu.Lock()
	defer b.connMu.Unlock()
	select {
	case <-b.stopCh:
		return
	default:
	}
	if b.early.Load() {
		b.connectedAt.Store(b.clk.Now().UnixNano()) // before the flag so the timeout never sees a stale value
		b.early.Store(false)
		b.rtc.SetReceiver(b)
	}
}

func (b *bridge) Stats() Stats {
	lost, late := b.jSIP.stats()
	s := Stats{
		SIPRx:             b.cnt.sipRx.Load(),
		SIPTx:             b.cnt.sipTx.Load(),
		SIPLost:           lost,
		SIPLate:           late,
		SIPJitter:         time.Duration(b.cnt.sipJitter.Load()),
		ConcealedSIPToRTC: b.cnt.concealedS2R.Load(),
		ConcealedRTCToSIP: b.cnt.concealedR2S.Load(),
		RTCRx:             b.cnt.rtcRx.Load(),
		DTMFSent:          b.cnt.dtmfSent.Load(),
	}
	if n := b.cnt.lastSIPRx.Load(); n != 0 {
		s.LastSIPRx = time.Unix(0, n)
	}
	return s
}

func (b *bridge) Done() <-chan struct{} { return b.done }

func (b *bridge) Err() error {
	select {
	case <-b.done:
		return b.err
	default:
		return nil
	}
}

func (b *bridge) Stop() {
	b.stopOnce.Do(func() {
		close(b.stopCh)
		b.wg.Wait()
		b.connMu.Lock()
		b.rtc.SetReceiver(nil)
		b.connMu.Unlock()
		b.finish(nil)
	})
}
