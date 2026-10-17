package sipua

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/emiago/diago"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"

	"github.com/williamkray/mrtc-sip-bridge/pkg/media"
	"github.com/williamkray/mrtc-sip-bridge/pkg/phone"
)

// requestUser returns the Request-URI user part for an E.164 target.
func requestUser(target string, f NumberFormat) string {
	if f == FormatE164 {
		return target
	}
	return phone.Digits(target)
}

// newCallID returns a random SIP Call-ID (set before sending so DialInfo carries it from the start).
func newCallID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// diagoClient adapts *diago.DialogClientSession to dialogClient.
type diagoClient struct {
	d    *diago.DialogClientSession
	opts diago.InviteClientOptions // without OnResponse (set per call)
	info DialInfo

	// Written by the invite goroutine; read by run only after the message that makes them valid.
	med       *diago.DialogMedia
	leg       *diagoLeg
	ans       AnswerInfo
	early     Offer // parsed early-media SDP (183); fallback for a 200 without SDP
	responses int   // responses seen by the current Invite call (invite goroutine only)
	restart   bool  // onResp ended Invite early to dodge sipgo's response limit (see onResp)
}

// errResponseLimit mirrors sipgo's WaitAnswer error text so the loop re-enters WaitAnswer (D12).
var errResponseLimit = errors.New("sipua: more than 10 responses received")

// inviteResponseLimit is the response count at which Invite is ended early (sipgo fails the 12th read).
const inviteResponseLimit = 10

// newDiagoClient builds (but does not send) the INVITE dialog: Request-URI sip:<user>@<aorHost>, sent to
// dest (the pinned registrar "ip:port") from the UA's bound SIP socket. From is sipgo's default
// <sip:<username>@<aorHost>> unless req.CallerID is set.
func newDiagoClient(dg *diago.Diago, cfg Config, aorHost string, req DialRequest, dest string) (*diagoClient, error) {
	user := requestUser(req.Target, req.Format)
	uri := "sip:" + user + "@" + aorHost
	if cfg.Transport == TransportTCP {
		uri += ";transport=tcp"
	}
	var ruri sip.Uri
	if err := sip.ParseUri(uri, &ruri); err != nil {
		return nil, fmt.Errorf("sipua: build request URI: %w", err)
	}
	d, err := dg.NewDialog(ruri, diago.NewDialogOptions{})
	if err != nil {
		return nil, fmt.Errorf("sipua: create dialog: %w", err)
	}
	d.InviteRequest.SetDestination(dest)
	callID, err := newCallID()
	if err != nil {
		_ = d.Close()
		return nil, fmt.Errorf("sipua: generate Call-ID: %w", err)
	}
	cid := sip.CallIDHeader(callID)
	d.InviteRequest.AppendHeader(&cid)

	opts := diago.InviteClientOptions{
		Username:         cfg.Username,
		Password:         cfg.Password,
		Headers:          []sip.Header{},
		EarlyMediaDetect: true,
	}
	if req.CallerID != "" {
		opts.WithCaller("", req.CallerID, aorHost)
	}
	return &diagoClient{
		d:    d,
		opts: opts,
		info: DialInfo{CallID: callID, Target: req.Target, RequestUser: user},
	}, nil
}

func (a *diagoClient) Info() DialInfo { return a.info }

// onResp wraps the loop's callback.
//
// It skips the 183 that diago turns into early media (the one with an SDP body while no early leg exists
// yet): the loop reports it as one early-media EventProvisional. Its SDP address is remembered (only
// parseOffer sees the body) so the 200 can be compared with it.
//
// During Invite (early) it also dodges sipgo's response limit: WaitAnswer fails after 11 responses
// ("more than 10 responses received", sipgo dialog_client.go:246), and diago's Invite then closes the
// offered media session, so a re-entered WaitAnswer could never answer. Returning
// diago.ErrClientEarlyMedia from OnResponse makes diago return early while keeping the media session;
// mapResult turns that into errResponseLimit and the loop continues with WaitAnswer.
func (a *diagoClient) onResp(early bool, onResponse func(int)) func(*sip.Response) error {
	return func(r *sip.Response) error {
		earlySDP := false
		if r.StatusCode == sip.StatusSessionInProgress && r.Body() != nil {
			if ct := r.ContentType(); ct != nil && ct.Value() == "application/sdp" {
				earlySDP = true
			}
		}
		if early && earlySDP && a.leg == nil {
			a.early = parseOffer(r.Body())
			return nil
		}
		onResponse(r.StatusCode)
		if early {
			a.responses++
			if a.responses >= inviteResponseLimit && r.IsProvisional() {
				a.restart = true
				return diago.ErrClientEarlyMedia
			}
		}
		return nil
	}
}

func (a *diagoClient) Invite(ctx context.Context, onResponse func(status int)) error {
	opts := a.opts
	opts.OnResponse = a.onResp(true, onResponse)
	med, err := a.d.Invite(ctx, opts)
	if med != nil {
		a.med = med
	}
	return a.mapResult(err)
}

func (a *diagoClient) WaitAnswer(ctx context.Context, onResponse func(status int)) error {
	if a.med == nil {
		return errors.New("sipua: no dialog media to wait on")
	}
	opts := sipgo.AnswerOptions{
		Username:   a.opts.Username,
		Password:   a.opts.Password,
		OnResponse: a.onResp(false, onResponse),
	}
	if a.leg == nil {
		// No early media: diago applies the 200's SDP and creates the RTP session.
		return a.mapResult(a.d.WaitAnswer(ctx, a.med, opts))
	}
	// Early media is flowing. diago's WaitAnswer would re-apply the 200's SDP to the live media session
	// (checkEarlyMedia -> MediaSession.RemoteSDP), racing with the RTP reader, writer and RTCP goroutines
	// (diago v0.41.0). Wait with sipgo directly and only switch sessions (race-free fork) if the 200 moves
	// the media.
	err := a.d.DialogClientSession.WaitAnswer(ctx, opts)
	if err == nil {
		err = a.applyAnswerAfterEarly()
	}
	return a.mapResult(err)
}

// applyAnswerAfterEarly keeps the early-media RTP session when the 200 has no SDP or the same media
// address as the 183, and otherwise moves the reader and writer to a fork of the session with the 200's
// SDP applied (the way diago handles a re-INVITE: replaceRTPSessionUnsafe). The DialogMedia keeps the
// original MediaSession (same sockets), which is only used for Ack's finalize and Close.
func (a *diagoClient) applyAnswerAfterEarly() error {
	res := a.d.InviteResponse
	if res == nil {
		return errors.New("sipua: answered without a response")
	}
	body := res.Body()
	if len(body) == 0 {
		return nil
	}
	if addr := parseOffer(body).MediaAddr; addr != "" && addr == a.early.MediaAddr {
		return nil
	}
	ms, old := a.med.MediaSession(), a.med.RTPSession()
	if ms == nil || old == nil {
		return errors.New("sipua: early media session missing")
	}
	fork := ms.Fork()
	if err := fork.RemoteSDP(body); err != nil {
		return fmt.Errorf("sipua: apply answer SDP: %w", err)
	}
	if err := old.MonitorClose(); err != nil {
		return fmt.Errorf("sipua: stop early RTCP: %w", err)
	}
	rs := old.Fork(fork)
	if err := rs.MonitorBackground(); err != nil {
		return fmt.Errorf("sipua: start RTCP: %w", err)
	}
	a.med.RTPPacketReader.UpdateRTPSession(rs)
	a.med.RTPPacketWriter.UpdateRTPSession(rs)
	// UpdateRTPSession stops a blocked read with a past read deadline (the reader then clears it); if no
	// read was in flight the deadline would stick and fail the next read, so clear it here too.
	_ = fork.StartRTP(1)
	return nil
}

// mapResult converts a diago/sipgo result into the dialogClient contract and builds the leg and the
// answer facts on success.
func (a *diagoClient) mapResult(err error) error {
	if errors.Is(err, diago.ErrClientEarlyMedia) && a.restart {
		a.restart = false
		a.responses = 0
		if a.med == nil {
			return errors.New("sipua: no dialog media after response limit")
		}
		return errResponseLimit
	}
	if errors.Is(err, diago.ErrClientEarlyMedia) {
		if a.med == nil {
			return errors.New("sipua: early media without dialog media")
		}
		a.ensureLeg()
		return errEarlyMedia
	}
	var de *sipgo.ErrDialogResponse
	if errors.As(err, &de) && de.Res != nil {
		return &finalError{Status: de.Res.StatusCode, Reason: de.Res.Reason}
	}
	var dv sipgo.ErrDialogResponse
	if errors.As(err, &dv) && dv.Res != nil {
		return &finalError{Status: dv.Res.StatusCode, Reason: dv.Res.Reason}
	}
	if err != nil {
		return err
	}
	if a.med == nil {
		return errors.New("sipua: answered without dialog media")
	}
	leg := a.ensureLeg()
	ans := AnswerInfo{Codec: "PCMU", PtimeMS: 20}
	if res := a.d.InviteResponse; res != nil {
		o := parseOffer(res.Body()) // the SDP answer; never logged
		if !o.HasSDP && a.early.HasSDP {
			o = a.early // a 200 without SDP keeps the 183's media description
		}
		ans.TelephoneEvent = o.TelephoneEvent
		ans.TelephoneEventPT = o.TelephoneEventPT
		ans.RemoteRTP = o.MediaAddr
	}
	if ms := a.med.MediaSession(); ms != nil && ms.Laddr.IP != nil {
		ans.LocalRTP = ms.Laddr.String()
	}
	if ans.TelephoneEvent {
		leg.setTelephoneEventPT(ans.TelephoneEventPT)
	}
	a.ans = ans
	return nil
}

func (a *diagoClient) ensureLeg() *diagoLeg {
	if a.leg == nil {
		a.leg = &diagoLeg{med: a.med, buf: make([]byte, 1500)}
	}
	return a.leg
}

func (a *diagoClient) Leg() media.SIPLeg {
	if a.leg == nil {
		return nil
	}
	return a.leg
}

func (a *diagoClient) Answer() AnswerInfo { return a.ans }

func (a *diagoClient) FinalStatus() (int, string) {
	res := a.d.InviteResponse
	if res == nil || res.StatusCode < 200 {
		return 0, ""
	}
	return res.StatusCode, res.Reason
}

func (a *diagoClient) Ack(ctx context.Context) error { return a.d.Ack(ctx) }
func (a *diagoClient) Bye(ctx context.Context) error { return a.d.Hangup(ctx) }
func (a *diagoClient) Done() <-chan struct{}         { return a.d.Context().Done() }

func (a *diagoClient) Close() {
	_ = a.d.Close()
	if a.med != nil {
		_ = a.med.Close()
	}
}
