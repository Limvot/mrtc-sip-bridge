// Command s1-rtc is the Phase 0 Spike S1 program. See README.md and
// docs/spikes/S1-matrixrtc-wire-spec.md. An appservice ghost rings the owner via
// MatrixRTC, and after the owner answers joins LiveKit, publishes a tone and
// counts the owner's audio. Outbound only: no HTTP listener, polling instead.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/id"
)

const botLocalpart = "sipspikebot"

func usage() {
	fmt.Fprint(os.Stderr, `usage:
  s1-rtc gen-registration -server-name X [-out out/registration.yaml]
  s1-rtc ring [flags]

ring settings (env or -env-file): MATRIX_HOMESERVER_URL MATRIX_SERVER_NAME MATRIX_AS_TOKEN OWNER_MXID LIVEKIT_SERVICE_URL
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	var err error
	switch os.Args[1] {
	case "gen-registration":
		err = genRegistration(os.Args[2:], log)
	case "ring":
		err = ring(os.Args[2:], log)
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		log.Error("failed", "err", err)
		os.Exit(1)
	}
}

func genRegistration(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("gen-registration", flag.ExitOnError)
	server := fs.String("server-name", "", "Matrix server name (required)")
	out := fs.String("out", "out/registration.yaml", "output path (keep it under a gitignored out/ dir)")
	fs.Parse(args)
	if *server == "" {
		return fmt.Errorf("-server-name is required")
	}
	rnd := func() string {
		b := make([]byte, 32)
		_, _ = rand.Read(b)
		return hex.EncodeToString(b)
	}
	// Single-quoted YAML scalars keep the regex backslashes literal.
	yaml := fmt.Sprintf(`id: mrtc-sip-spike
url: null
as_token: '%s'
hs_token: '%s'
sender_localpart: %s
rate_limited: false
namespaces:
  users:
    - exclusive: true
      regex: '%s'
  aliases: []
  rooms: []
`, rnd(), rnd(), botLocalpart, regexp.QuoteMeta("@sipspike_")+".*:"+regexp.QuoteMeta(*server))
	if err := os.MkdirAll(filepath.Dir(*out), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(*out, []byte(yaml), 0o600); err != nil {
		return err
	}
	log.Info("registration written (contains tokens; do not commit or share)", "path", *out)
	log.Info("next: copy as_token into MATRIX_AS_TOKEN in your env file, add the file to Synapse app_service_config_files")
	return nil
}

func ring(args []string, log *slog.Logger) error {
	fs := flag.NewFlagSet("ring", flag.ExitOnError)
	envFile := fs.String("env-file", "~/.config/mrtc-sip-bridge/s1.env", "env file (must be mode 0600)")
	ghostLP := fs.String("ghost", "sipspike_15550001234", "ghost localpart (must match the registration namespace)")
	roomFlag := fs.String("room", "", "existing room id to reuse (default: create an unencrypted DM-style room)")
	deviceID := fs.String("device-id", "MRTCSPIKE", "invented membership device id ([A-Z0-9])")
	ringTimeout := fs.Duration("ring-timeout", 30*time.Second, "how long to ring before giving up")
	prejoin := fs.Bool("prejoin", false, "join LiveKit before the owner answers (spec §9 Q3)")
	maxDur := fs.Duration("max-duration", 5*time.Minute, "hard limit for the whole answered call")
	fs.Parse(args)

	if loaded, err := loadEnvFile(*envFile); err != nil {
		return err
	} else if loaded {
		log.Info("env file loaded", "path", *envFile)
	}
	cfg, err := readEnvConfig()
	if err != nil {
		return err
	}
	owner := id.UserID(cfg.OwnerMXID)
	if _, _, perr := owner.Parse(); perr != nil {
		return fmt.Errorf("OWNER_MXID is not a valid user id: %w", perr)
	}
	ghostID := id.NewUserID(*ghostLP, cfg.ServerName)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Step 1: ghost.
	log.Info("step 1: ensure ghost", "ghost", ghostID, "owner", owner)
	if err := ensureGhost(ctx, cfg, ghostID, "+1 555 000 1234 (spike)", log); err != nil {
		return err
	}
	ghost, err := newMatrixClient(cfg, ghostID)
	if err != nil {
		return err
	}

	// Step 2: room.
	var room id.RoomID
	if *roomFlag != "" {
		room = id.RoomID(*roomFlag)
		log.Info("step 2: reusing room (ghost must be, or be invitable to, joined)", "room", room)
		if _, err := ghost.JoinRoomByID(ctx, room); err != nil {
			return fmt.Errorf("ghost cannot join %s (invite the ghost %s first): %w", room, ghostID, err)
		}
	} else {
		log.Info("step 2: creating unencrypted room, inviting owner")
		resp, err := ghost.CreateRoom(ctx, &mautrix.ReqCreateRoom{
			Preset:   "private_chat",
			IsDirect: true,
			Invite:   []id.UserID{owner},
			Name:     "SIP spike +1 555 000 1234",
		})
		if err != nil {
			return fmt.Errorf("create room: %w", err)
		}
		room = resp.RoomID
		log.Info("room created", "room", room)
	}
	if err := ensureCallPowerLevels(ctx, ghost, room, log); err != nil {
		return err
	}

	// Step 3: wait for the owner.
	log.Info("step 3: waiting for owner to be joined")
	if err := waitOwnerJoined(ctx, ghost, room, owner, log); err != nil {
		return err
	}

	// Step 4: snapshot pre-existing owner memberships (stale calls are not an "answer").
	pre, err := liveMembersOf(ctx, ghost, room, owner, time.Now())
	if err != nil {
		return fmt.Errorf("read room state: %w", err)
	}
	ignore := map[id.EventID]bool{}
	for _, m := range pre {
		ignore[m.EventID] = true
		log.Warn("owner already has a live call membership; ignoring it for answer detection", "state_key", m.StateKey)
	}

	stateKey := memberStateKey(ghostID.String(), *deviceID)
	memberJoined := false
	var notifID id.EventID
	answered := false
	declined := false
	var rtc *rtcSession

	// Cleanup always runs, on a fresh context so Ctrl-C does not cancel it.
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		log.Info("cleanup")
		if rtc != nil {
			rtc.Close()
		}
		if notifID != "" && !answered {
			if _, err := ghost.RedactEvent(cctx, room, notifID); err != nil {
				log.Warn("redact notification failed", "err", err)
			} else {
				log.Info("notification redacted (stops Element Web ringing)")
			}
		}
		if memberJoined {
			if _, err := ghost.SendStateEvent(cctx, room, stateType(EventCallMember), stateKey, leaveContent()); err != nil {
				log.Warn("clear membership failed", "err", err)
			} else {
				log.Info("ghost membership cleared")
			}
		}
	}()

	// Step 5: membership then notification (order matters, spec §2.2).
	log.Info("step 4: sending ghost call membership", "state_key", stateKey)
	memberResp, err := ghost.SendStateEvent(ctx, room, stateType(EventCallMember), stateKey,
		joinContent(ghostID.String(), *deviceID, room.String(), cfg.LiveKitSvcURL, time.Now()))
	if err != nil {
		return fmt.Errorf("send call.member (403: power levels, M_FORBIDDEN on state key: room version?): %w", err)
	}
	memberJoined = true
	log.Info("membership sent", "event_id", memberResp.EventID)

	ringStart := time.Now()
	log.Info("step 5: sending ring notification", "lifetime", *ringTimeout)
	notifResp, err := ghost.SendMessageEvent(ctx, room, msgType(EventRTCNotification),
		ringContent(owner.String(), memberResp.EventID.String(), *ringTimeout, ringStart))
	if err != nil {
		return fmt.Errorf("send ring notification: %w", err)
	}
	notifID = notifResp.EventID
	log.Info("ring sent: the owner's devices should ring now", "event_id", notifID)

	sfuAndConnect := func() error {
		sfu, err := getSFUToken(ctx, cfg, ghost, room.String(), *deviceID, log)
		if err != nil {
			return err
		}
		log.Info("lk-jwt-service issued token", "livekit_url", sfu.URL)
		inspectJWT(sfu.JWT, ghostID.String()+":"+*deviceID, room.String(), log)
		rtc, err = connectRTC(sfu.URL, sfu.JWT, owner.String(), log)
		return err
	}

	if *prejoin {
		log.Info("step 6 (-prejoin): joining LiveKit before the owner answers")
		if err := sfuAndConnect(); err != nil {
			return err
		}
	}

	// Step 7: poll for answer / decline / timeout.
	log.Info("step 7: polling for answer or decline", "timeout", *ringTimeout)
	deadline := time.After(*ringTimeout)
	var answerKey string
poll:
	for {
		select {
		case <-ctx.Done():
			log.Info("interrupted while ringing")
			return nil
		case <-deadline:
			log.Info("ring timeout, no answer")
			return nil
		case <-time.After(time.Second):
		}
		if rtc != nil {
			select {
			case why := <-rtc.ended:
				return fmt.Errorf("livekit session ended while ringing: %s", why)
			default:
			}
		}
		now := time.Now()
		ms, err := liveMembersOf(ctx, ghost, room, owner, now)
		if err != nil {
			log.Warn("poll state failed", "err", err)
			continue
		}
		for _, m := range ms {
			if !ignore[m.EventID] {
				answered = true
				answerKey = m.StateKey
				log.Info("owner ANSWERED (call.member appeared)", "state_key", m.StateKey,
					"ring_to_answer", time.Since(ringStart).Round(time.Millisecond))
				break poll
			}
		}
		if declined, err = findDecline(ctx, ghost, room, owner, notifID, ringStart); err != nil {
			log.Warn("poll messages failed", "err", err)
		} else if declined {
			log.Info("owner DECLINED")
			return nil
		}
	}

	// Step 8: answered; connect if not pre-joined.
	if rtc == nil {
		log.Info("step 8: joining LiveKit after answer")
		if err := sfuAndConnect(); err != nil {
			return err
		}
	}

	// Step 9: run until the call ends.
	log.Info("step 9: call running", "max_duration", *maxDur)
	limit := time.After(*maxDur)
	for {
		select {
		case <-ctx.Done():
			log.Info("interrupted, hanging up")
			return nil
		case <-limit:
			log.Info("max duration reached, hanging up")
			return nil
		case why := <-rtc.ended:
			log.Info("call ended", "reason", why)
			return nil
		case <-time.After(2 * time.Second):
		}
		ms, err := liveMembersOf(ctx, ghost, room, owner, time.Now())
		if err != nil {
			log.Warn("poll state failed", "err", err)
			continue
		}
		stillThere := false
		for _, m := range ms {
			if m.StateKey == answerKey {
				stillThere = true
			}
		}
		if !stillThere {
			log.Info("call ended: owner's membership cleared or expired", "state_key", answerKey)
			return nil
		}
	}
}
