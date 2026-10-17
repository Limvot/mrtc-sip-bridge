// Command s2-sip is the Phase 0 Spike S2 SIP user agent. See README.md and
// docs/spikes/S2-sip-ua-design.md. It registers to a SIP account (JMP.chat),
// auto-answers inbound calls and can place an outbound call, recording audio and
// playing a tone, to prove SIP + RTP works from behind NAT.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

type config struct {
	user, pass, registrar, domain, callerID string

	bind         string
	publicIP     string
	publicSIPort int
	stun         string
	rtpStart     int
	rtpEnd       int
	expiry       time.Duration
	reregister   time.Duration
	symmetric    bool
	earlyRTP     int
	recordDir    string
	toneHz       int
	toneMs       int
	answer       bool
	callSeconds  int
	sipDebug     bool
	rtpRangeFlag *string
}

func usage() {
	fmt.Fprintf(os.Stderr, `usage:
  s2-sip register [flags]            register and auto-answer inbound calls
  s2-sip call [flags] <number>       place an outbound call (flags go BEFORE the number)

credentials (env or -env-file): SIP_USERNAME SIP_PASSWORD SIP_REGISTRAR [SIP_DOMAIN] [SIP_CALLER_ID]

flags:
`)
	newFlagSet("x", &config{}, new(string)).PrintDefaults()
}

func newFlagSet(name string, c *config, envFile *string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	rtpRange := new(string)
	fs.StringVar(envFile, "env-file", "~/.config/mrtc-sip-bridge/spike.env", "env file with credentials (must be mode 0600)")
	fs.StringVar(&c.registrar, "registrar", "", "registrar host:port (default env SIP_REGISTRAR)")
	fs.StringVar(&c.domain, "domain", "", "SIP domain if different from registrar host (default env SIP_DOMAIN)")
	fs.StringVar(&c.bind, "bind", "0.0.0.0:5060", "local SIP UDP bind; an unspecified IP is replaced by the LAN IP (diago needs a concrete IP to share one socket)")
	fs.StringVar(&c.publicIP, "public-ip", "", "public IP for Contact and SDP c= (manual; skips STUN)")
	fs.IntVar(&c.publicSIPort, "public-sip-port", 0, "Contact port override (default: bind port, then learned from rport)")
	fs.StringVar(&c.stun, "stun", "stun.l.google.com:19302", "STUN server for public IP discovery when -public-ip is unset; \"off\" disables")
	fs.StringVar(rtpRange, "rtp-port-range", "40000-40100", "RTP port range (open it on the host firewall)")
	fs.DurationVar(&c.expiry, "expiry", 60*time.Second, "REGISTER Expires requested")
	fs.DurationVar(&c.reregister, "reregister", 25*time.Second, "forced re-register interval (keeps the NAT binding fresh)")
	fs.BoolVar(&c.symmetric, "symmetric-rtp", true, "learn remote RTP address from first packet (RTPNATSymetric)")
	fs.IntVar(&c.earlyRTP, "early-rtp", 5, "silence packets sent immediately after answer to open the NAT pinhole")
	fs.StringVar(&c.recordDir, "record-dir", "./out", "directory for WAV recordings (gitignored)")
	fs.IntVar(&c.toneHz, "tone-hz", 440, "playback tone frequency")
	fs.IntVar(&c.toneMs, "tone-ms", 2000, "playback tone duration")
	fs.BoolVar(&c.answer, "answer", true, "auto-answer inbound calls (register mode)")
	fs.IntVar(&c.callSeconds, "call-seconds", 20, "hang up an outbound call after N seconds")
	fs.BoolVar(&c.sipDebug, "sip-debug", false, "print raw SIP (Authorization/Proxy-Authorization values redacted)")
	// stash range string for parse step
	c.rtpRangeFlag = rtpRange
	return fs
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	mode := os.Args[1]
	if mode != "register" && mode != "call" {
		usage()
		os.Exit(2)
	}
	cfg := &config{}
	envFile := new(string)
	fs := newFlagSet(mode, cfg, envFile)
	_ = fs.Parse(os.Args[2:])
	number := ""
	if mode == "call" {
		if fs.NArg() != 1 {
			fmt.Fprintln(os.Stderr, "call mode needs exactly one number argument after the flags")
			os.Exit(2)
		}
		number = fs.Arg(0)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(log)

	loaded, err := loadEnvFile(*envFile)
	if err != nil {
		log.Error("env file", "err", err)
		os.Exit(1)
	}
	cfg.user = os.Getenv("SIP_USERNAME")
	cfg.pass = os.Getenv("SIP_PASSWORD")
	if cfg.registrar == "" {
		cfg.registrar = os.Getenv("SIP_REGISTRAR")
	}
	if cfg.domain == "" {
		cfg.domain = os.Getenv("SIP_DOMAIN")
	}
	cfg.callerID = os.Getenv("SIP_CALLER_ID")
	if cfg.user == "" || cfg.pass == "" || cfg.registrar == "" {
		log.Error("SIP_USERNAME, SIP_PASSWORD and SIP_REGISTRAR are required (env or -env-file)")
		os.Exit(1)
	}
	if cfg.callerID == "" {
		cfg.callerID = cfg.user
	}
	if cfg.rtpStart, cfg.rtpEnd, err = parseRange(*cfg.rtpRangeFlag); err != nil {
		log.Error("bad -rtp-port-range", "err", err)
		os.Exit(1)
	}

	log.Info("startup", "mode", mode, "username", maskUser(cfg.user), "registrar", cfg.registrar,
		"domain", cfg.domain, "env_file_loaded", loaded, "bind", cfg.bind,
		"rtp_ports", *cfg.rtpRangeFlag, "expiry", cfg.expiry, "reregister", cfg.reregister,
		"symmetric_rtp", cfg.symmetric, "early_rtp", cfg.earlyRTP, "sip_debug", cfg.sipDebug)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := run(ctx, log, cfg, mode, number); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func parseRange(s string) (int, int, error) {
	a, b, ok := strings.Cut(s, "-")
	if !ok {
		return 0, 0, fmt.Errorf("want START-END")
	}
	start, err1 := strconv.Atoi(a)
	end, err2 := strconv.Atoi(b)
	if err1 != nil || err2 != nil || start <= 0 || end <= start {
		return 0, 0, fmt.Errorf("want START-END with START < END")
	}
	return start, end, nil
}

func run(ctx context.Context, log *slog.Logger, cfg *config, mode, number string) error {
	host, portStr, err := net.SplitHostPort(cfg.bind)
	if err != nil {
		return fmt.Errorf("-bind: %w", err)
	}
	bindPort, _ := strconv.Atoi(portStr)
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		lan, _, err := sip.ResolveInterfacesIP("ip4", nil)
		if err != nil {
			return fmt.Errorf("resolve LAN IP: %w", err)
		}
		host = lan.String()
		log.Info("bind resolved to LAN IP", "ip", host, "port", bindPort)
	}

	// Public IP: manual, else STUN.
	pub := cfg.publicIP
	if pub == "" && cfg.stun != "off" && cfg.stun != "" {
		ip, err := stunPublicIP(cfg.stun)
		if err != nil {
			log.Warn("stun failed; Contact and SDP will use the LAN IP (set -public-ip)", "err", err)
		} else {
			pub = ip.String()
			log.Info("stun result", "public", pub)
		}
	}
	extHost, extPort := host, bindPort
	var mediaIP net.IP
	if pub != "" {
		extHost = pub
		mediaIP = net.ParseIP(pub)
		if mediaIP == nil {
			return fmt.Errorf("-public-ip %q is not an IP", pub)
		}
	} else {
		log.Warn("no public IP known; behind NAT this will not work")
	}
	if cfg.publicSIPort != 0 {
		extPort = cfg.publicSIPort
	}

	media.RTPPortStart, media.RTPPortEnd = cfg.rtpStart, cfg.rtpEnd

	regHost, _, _ := strings.Cut(cfg.registrar, ":")
	sipDomain := cfg.domain
	if sipDomain == "" {
		sipDomain = regHost
	}

	tracer := &sipTracer{log: log, verbose: cfg.sipDebug}
	sip.SIPDebugTracer(tracer)
	sip.SIPDebug = true // we always inspect; printing is gated by tracer.verbose

	// sipgo derives the From user (and Contact user) from the UA name.
	ua, err := sipgo.NewUA(sipgo.WithUserAgent(cfg.user), sipgo.WithUserAgentHostname(sipDomain))
	if err != nil {
		return err
	}
	dg := diago.NewDiago(ua,
		diago.WithLogger(log),
		diago.WithTransport(diago.Transport{
			Transport:       "udp4",
			BindHost:        host,
			BindPort:        bindPort,
			ExternalHost:    extHost,
			ExternalPort:    extPort,
			MediaExternalIP: mediaIP,
			RewriteContact:  true,
		}),
		diago.WithMediaConfig(diago.MediaConfig{Codecs: []media.Codec{
			media.CodecAudioUlaw, media.CodecTelephoneEvent8000}}),
	)

	if err := os.MkdirAll(cfg.recordDir, 0o700); err != nil {
		return err
	}
	tonePath := filepath.Join(cfg.recordDir, "tone.wav")
	if err := writeToneWAV(tonePath, cfg.toneHz, cfg.toneMs); err != nil {
		return err
	}

	mo := func(l *slog.Logger, label string, done <-chan struct{}) mediaOpts {
		return mediaOpts{log: l, recordDir: cfg.recordDir, tonePath: tonePath, earlyRTP: cfg.earlyRTP,
			toneDelay: 500 * time.Millisecond, label: label, dialogDone: done}
	}

	handler := func(d *diago.DialogServerSession) { handleInbound(ctx, log, cfg, mode, mo, d) }
	if err := dg.ServeBackground(ctx, handler); err != nil {
		return fmt.Errorf("serve: %w", err)
	}

	if mode == "register" {
		return registerLoop(ctx, log, cfg, dg, tracer, sipDomain)
	}
	return outbound(ctx, log, cfg, dg, mo, sipDomain, number)
}

func handleInbound(ctx context.Context, log *slog.Logger, cfg *config, mode string,
	mo func(*slog.Logger, string, <-chan struct{}) mediaOpts, d *diago.DialogServerSession) {

	callID := d.InviteRequest.CallID().Value()
	codecs, remote := sdpSummary(d.InviteRequest.Body())
	l := log.With("call_id", callID)
	l.Info("invite", "from", d.FromUser(), "offered_codecs", codecs, "remote_sdp", remote)

	if mode != "register" || !cfg.answer {
		l.Info("rejecting inbound (not answering in this mode)")
		_ = d.Respond(sip.StatusBusyHere, "Busy Here", nil)
		return
	}
	_ = d.Trying()
	_ = d.Ringing()
	rtpNAT := media.RTPNATDisabled
	if cfg.symmetric {
		rtpNAT = media.RTPNATSymetric
	}
	med, err := d.Answer(diago.AnswerOptions{
		Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecTelephoneEvent8000},
		RTPNAT: rtpNAT,
	})
	if err != nil {
		l.Error("answer failed", "err", err)
		return
	}
	answeredAt := time.Now()
	l.Info("answer", "codec", "PCMU")

	var localBye atomic.Bool
	go func() {
		select {
		case <-d.Context().Done():
			by := "remote"
			if localBye.Load() {
				by = "local"
			}
			l.Info("bye", "by", by, "after", time.Since(answeredAt).Round(time.Second))
		case <-ctx.Done():
			localBye.Store(true)
			hctx, c := context.WithTimeout(context.Background(), 5*time.Second)
			defer c()
			_ = d.Hangup(hctx)
		}
	}()

	if err := runMedia(ctx, mo(l, "in", d.Context().Done()), med); err != nil {
		l.Error("media ended with error", "err", err)
	}
}

func outbound(ctx context.Context, log *slog.Logger, cfg *config, dg *diago.Diago,
	mo func(*slog.Logger, string, <-chan struct{}) mediaOpts, sipDomain, number string) error {

	target := "sip:" + number + "@" + sipDomain
	if cfg.domain == "" {
		target = "sip:" + number + "@" + cfg.registrar // keep the registrar port
	}
	var dst sip.Uri
	if err := sip.ParseUri(target, &dst); err != nil {
		return fmt.Errorf("bad target: %w", err)
	}

	opts := diago.InviteOptions{
		Username: cfg.user,
		Password: cfg.pass,
		Headers:  []sip.Header{},
	}
	if cfg.callerID != cfg.user {
		opts.Headers = append(opts.Headers, &sip.FromHeader{
			Address: sip.Uri{User: cfg.callerID, Host: sipDomain},
			Params:  sip.NewParams(),
		})
	}
	start := time.Now()
	opts.OnResponse = func(res *sip.Response) error {
		log.Info("outbound response", "status", res.StatusCode, "reason", res.Reason,
			"after", time.Since(start).Round(time.Millisecond))
		return nil
	}
	log.Info("outbound invite", "to", number)
	d, med, err := dg.Invite(ctx, dst, opts)
	if err != nil {
		return fmt.Errorf("invite: %w", err)
	}
	defer d.Close()
	answeredAt := time.Now()
	log.Info("outbound answered", "after", time.Since(start).Round(time.Millisecond))

	var localBye atomic.Bool
	go func() {
		select {
		case <-d.Context().Done():
		case <-time.After(time.Duration(cfg.callSeconds) * time.Second):
			localBye.Store(true)
			hctx, c := context.WithTimeout(context.Background(), 5*time.Second)
			defer c()
			_ = d.Hangup(hctx)
		case <-ctx.Done():
			localBye.Store(true)
			hctx, c := context.WithTimeout(context.Background(), 5*time.Second)
			defer c()
			_ = d.Hangup(hctx)
		}
		<-d.Context().Done()
		by := "remote"
		if localBye.Load() {
			by = "local"
		}
		log.Info("bye", "by", by, "after", time.Since(answeredAt).Round(time.Second))
	}()

	if err := runMedia(ctx, mo(log, "out", d.Context().Done()), med); err != nil {
		log.Error("media ended with error", "err", err)
	}
	return nil
}

func registerLoop(ctx context.Context, log *slog.Logger, cfg *config, dg *diago.Diago,
	tracer *sipTracer, sipDomain string) error {

	regTarget := "sip:" + cfg.registrar
	opts := diago.RegisterOptions{
		Username:      cfg.user,
		Password:      cfg.pass,
		Expiry:        cfg.expiry,
		RetryInterval: cfg.reregister,
	}
	if cfg.domain != "" {
		regTarget = "sip:" + cfg.domain
		opts.ProxyHost = cfg.registrar
	}
	// The JMP registrar name resolves to several IPs and sipgo re-resolves per request,
	// so consecutive REGISTERs hit different IPs and get different NAT mappings, which
	// leaves the Contact stale. Pin every request to one resolved IP.
	if host, port, err := net.SplitHostPort(cfg.registrar); err == nil && net.ParseIP(host) == nil {
		if ips, err := net.DefaultResolver.LookupIP(ctx, "ip4", host); err == nil && len(ips) > 0 {
			opts.ProxyHost = net.JoinHostPort(ips[0].String(), port)
			log.Info("registrar pinned", "host", host, "ip", ips[0].String(), "candidates", len(ips))
		}
	}
	var uri sip.Uri
	if err := sip.ParseUri(regTarget, &uri); err != nil {
		return fmt.Errorf("bad registrar: %w", err)
	}
	t, err := dg.RegisterTransaction(ctx, uri, opts)
	if err != nil {
		return err
	}
	// diago builds To from the Request-URI (no user part); registrars expect To to be
	// the address-of-record being registered. Bandwidth answers 500 without it.
	if t.Origin.To() == nil {
		t.Origin.AppendHeader(&sip.ToHeader{Address: sip.Uri{User: cfg.user, Host: uri.Host}})
	}

	describe := func(err error) (status int, reason string) {
		var rr *diago.RegisterResponseError
		if errors.As(err, &rr) {
			return rr.RegisterRes.StatusCode, rr.RegisterRes.Reason
		}
		return 0, ""
	}
	contact := func() string {
		c := t.Origin.Contact()
		if c == nil {
			return ""
		}
		return net.JoinHostPort(c.Address.Host, strconv.Itoa(c.Address.Port))
	}

	registered := false
	attempt := 0
	tick := time.NewTicker(cfg.reregister)
	defer tick.Stop()
	defer func() {
		if !registered {
			return
		}
		uctx, c := context.WithTimeout(context.Background(), 5*time.Second)
		defer c()
		if err := t.Unregister(uctx); err != nil {
			log.Warn("unregister failed", "err", err)
		} else {
			log.Info("unregistered")
		}
	}()

	doRegister := func() {
		attempt++
		log.Info("register attempt", "n", attempt, "username", maskUser(cfg.user))
		var err error
		first := !registered
		if first {
			err = t.Register(ctx)
		} else {
			err = t.Qualify(ctx)
		}
		if err != nil {
			status, reason := describe(err)
			if status != 0 {
				log.Error("register failed", "n", attempt, "status", status, "reason", reason)
			} else if ctx.Err() == nil {
				log.Error("register failed", "n", attempt, "err", err)
			}
			registered = false
			return
		}
		registered = true
		if first {
			log.Info("register ok", "expires", tracer.registerExpires(), "contact", contact())
		} else {
			log.Info("reregister ok", "expires", tracer.registerExpires(), "contact", contact())
		}
	}

	doRegister()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
			doRegister()
		}
	}
}
