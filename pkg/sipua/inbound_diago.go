package sipua

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/emiago/diago"
	dmedia "github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo/sip"

	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
)

// diagoDialog adapts *diago.DialogServerSession to dialogServer.
type diagoDialog struct {
	d    *diago.DialogServerSession
	info InviteInfo
}

func newDiagoDialog(d *diago.DialogServerSession) *diagoDialog {
	req := d.InviteRequest
	info := InviteInfo{
		RemoteAddr: req.Source(),
		Offer:      parseOffer(req.Body()),
	}
	if h := req.CallID(); h != nil {
		info.CallID = h.Value()
	}
	if h := req.From(); h != nil {
		info.FromUser = h.Address.User
		info.FromDisplay = h.DisplayName
	}
	if h := req.To(); h != nil {
		info.ToUser = h.Address.User
	}
	if h := req.GetHeader("P-Asserted-Identity"); h != nil {
		info.AssertedUser = parseAddressUser(h.Value())
	}
	return &diagoDialog{d: d, info: info}
}

// parseAddressUser returns the user part of a name-addr value, "" on any parse error.
func parseAddressUser(v string) (user string) {
	defer func() {
		if recover() != nil {
			user = ""
		}
	}()
	// P-Asserted-Identity may carry several comma-separated values; the first is used.
	if i := strings.Index(v, ">"); i >= 0 {
		v = v[:i+1]
	}
	var uri sip.Uri
	params := sip.NewParams()
	if _, err := sip.ParseAddressValue(v, &uri, &params); err != nil {
		return ""
	}
	return uri.User
}

func (a *diagoDialog) Info() InviteInfo { return a.info }
func (a *diagoDialog) Trying() error    { return a.d.Trying() }
func (a *diagoDialog) Ringing() error   { return a.d.Ringing() }

func (a *diagoDialog) Answer() (media.SIPLeg, AnswerInfo, error) {
	ans := AnswerInfo{Codec: "PCMU", PtimeMS: 20, TelephoneEvent: a.info.Offer.TelephoneEvent}
	ans.OKSentAt = time.Now()
	med, err := a.d.Answer(diago.AnswerOptions{
		Codecs: []dmedia.Codec{dmedia.CodecAudioUlaw, dmedia.CodecTelephoneEvent8000},
		RTPNAT: dmedia.RTPNATSymetric,
	})
	ans.AckAt = time.Now()
	if err != nil {
		return nil, ans, err
	}
	if ms := med.MediaSession(); ms != nil {
		if ms.Laddr.IP != nil {
			ans.LocalRTP = ms.Laddr.String()
		}
		if ms.Raddr.IP != nil {
			ans.RemoteRTP = ms.Raddr.String()
		}
	}
	leg := &diagoLeg{med: med, buf: make([]byte, 1500)}
	if a.info.Offer.TelephoneEvent {
		ans.TelephoneEventPT = a.info.Offer.TelephoneEventPT
		leg.setTelephoneEventPT(a.info.Offer.TelephoneEventPT)
	}
	return leg, ans, nil
}

func (a *diagoDialog) Respond(code int, reason string) error {
	return a.d.Respond(code, reason, nil)
}

func (a *diagoDialog) Bye(ctx context.Context) error { return a.d.Hangup(ctx) }
func (a *diagoDialog) Done() <-chan struct{}         { return a.d.Context().Done() }

func (a *diagoDialog) Cancelled() bool {
	return errors.Is(context.Cause(a.d.Context()), sip.ErrTransactionCanceled)
}

func (a *diagoDialog) Confirmed() bool {
	return a.d.LoadState() >= sip.DialogStateConfirmed
}

// diagoLeg implements media.SIPLeg over a diago DialogMedia. The reader and writer are looked up on
// every call (not cached) so diago's media updates on re-INVITE keep working.
// legHeader is the subset of the RTP header the leg forwards (keeps pion/rtp out of this package's imports).
type legHeader struct {
	SSRC           uint32
	SequenceNumber uint16
	Timestamp      uint32
	PayloadType    uint8
	Marker         bool
}

type diagoLeg struct {
	med *diago.DialogMedia
	buf []byte // used by ReadRTP only (single reader goroutine)

	// read, when set (tests), replaces the DialogMedia reader.
	read func(buf []byte) (int, legHeader, error)
	// badRTP counts consecutive non-fatal read errors (ReadRTP goroutine only).
	badRTP int
	// te is the negotiated telephone-event payload type + 1 (0 = not negotiated).
	te atomic.Uint32
}

var _ media.SIPEventLeg = (*diagoLeg)(nil)

// errNoTelephoneEvent is returned by WriteEvent when telephone-event was not negotiated.
var errNoTelephoneEvent = errors.New("sipua: telephone-event not negotiated")

func (l *diagoLeg) setTelephoneEventPT(pt uint8) { l.te.Store(uint32(pt) + 1) }

// TelephoneEventPT implements media.SIPEventLeg.
func (l *diagoLeg) TelephoneEventPT() (uint8, bool) {
	v := l.te.Load()
	if v == 0 {
		return 0, false
	}
	return uint8(v - 1), true
}

// WriteEvent implements media.SIPEventLeg: one RFC 4733 packet on the call's RTP stream (same SSRC,
// sequence and timestamp clock as WriteRTP; the caller must use the same goroutine as WriteRTP).
func (l *diagoLeg) WriteEvent(payload []byte, samples uint32, marker bool) error {
	pt, ok := l.TelephoneEventPT()
	if !ok {
		return errNoTelephoneEvent
	}
	_, err := l.med.RTPPacketWriter.WriteSamples(payload, samples, marker, pt)
	return err
}

// maxConsecutiveBadRTP is how many consecutive RTP read failures (e.g. unparsable stray datagrams)
// ReadRTP tolerates before giving up on the leg.
const maxConsecutiveBadRTP = 100

func (l *diagoLeg) readOne() (int, legHeader, error) {
	if l.read != nil {
		return l.read(l.buf)
	}
	r := l.med.RTPPacketReader
	n, err := r.Read(l.buf)
	h := r.PacketHeader
	return n, legHeader{SSRC: h.SSRC, SequenceNumber: h.SequenceNumber, Timestamp: h.Timestamp, PayloadType: h.PayloadType, Marker: h.Marker}, err
}

func (l *diagoLeg) ReadRTP(p *media.RTPPacket) error {
	for {
		n, h, err := l.readOne()
		if err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
				return io.EOF
			}
			// diago returns the RTP unmarshal error for stray datagrams (short, STUN, port scans): skip
			// them rather than ending the bridge and hanging up an answered call.
			l.badRTP++
			if l.badRTP >= maxConsecutiveBadRTP {
				return err
			}
			continue
		}
		l.badRTP = 0
		if n == 0 {
			continue
		}
		p.SSRC = h.SSRC
		p.Seq = h.SequenceNumber
		p.Timestamp = h.Timestamp
		p.PayloadType = h.PayloadType
		p.Marker = h.Marker
		p.Payload = l.buf[:n]
		return nil
	}
}

func (l *diagoLeg) WriteRTP(payload []byte, samples uint32, marker bool) error {
	_, err := l.med.RTPPacketWriter.WriteSamples(payload, samples, marker, 0)
	return err
}

var staticPayloadNames = map[int]string{0: "PCMU", 8: "PCMA", 9: "G722", 18: "G729"}

// parseOffer reduces an SDP offer to the facts the core needs. It never logs and never panics.
func parseOffer(body []byte) Offer {
	o := Offer{HasSDP: len(body) > 0}
	if !o.HasSDP {
		return o
	}
	sd := sdp.SessionDescription{}
	if err := sdp.Unmarshal(body, &sd); err != nil {
		return Offer{HasSDP: true}
	}
	// The first audio m-line (the library's MediaDescription rejects offers with several m-lines).
	var port string
	var formats []string
	for _, m := range sd.Values("m") {
		f := strings.Fields(m)
		if len(f) >= 4 && f[0] == "audio" {
			port = strings.Split(f[1], "/")[0]
			formats = f[3:]
			break
		}
	}
	if formats == nil {
		return Offer{HasSDP: true}
	}
	names := map[string]string{}
	for _, a := range sd.Values("a") {
		rest, ok := strings.CutPrefix(a, "rtpmap:")
		if !ok {
			continue
		}
		pt, enc, ok := strings.Cut(strings.TrimSpace(rest), " ")
		if !ok {
			continue
		}
		enc = strings.TrimSpace(enc)
		names[pt] = strings.SplitN(enc, "/", 2)[0]
		if strings.EqualFold(enc, "telephone-event/8000") && !o.TelephoneEvent {
			if n, err := strconv.ParseUint(pt, 10, 8); err == nil && n <= 127 {
				o.TelephoneEvent = true
				o.TelephoneEventPT = uint8(n)
			}
		}
	}
	for _, pt := range formats {
		if pt == "0" {
			o.PCMU = true
		}
		name := names[pt]
		if name == "" {
			if n, err := strconv.Atoi(pt); err == nil {
				name = staticPayloadNames[n]
			}
		}
		if name == "" {
			name = "pt" + pt
		}
		o.Codecs = append(o.Codecs, name+"/"+pt)
	}
	if fields := strings.Fields(sd.Value("c")); len(fields) >= 3 && port != "" {
		host := strings.Split(fields[2], "/")[0]
		if net.ParseIP(host) != nil {
			o.MediaAddr = net.JoinHostPort(host, port)
		}
	}
	return o
}
