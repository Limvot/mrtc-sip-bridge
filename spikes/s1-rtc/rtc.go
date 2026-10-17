package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/livekit/protocol/livekit"
	lksdk "github.com/livekit/server-sdk-go/v2"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"gopkg.in/hraban/opus.v2"
	"maunium.net/go/mautrix"
)

// livekitRoomName mirrors lk-jwt-service (spec §4.5), diagnostics only.
func livekitRoomName(roomID string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode([]string{roomID, slotIDRoom})
	sum := sha256.Sum256(bytes.TrimSuffix(buf.Bytes(), []byte("\n")))
	return base64.RawStdEncoding.EncodeToString(sum[:])
}

type sfuResponse struct {
	URL string `json:"url"`
	JWT string `json:"jwt"`
}

// getSFUToken asks lk-jwt-service for a LiveKit URL + JWT (spec §4.4). The OpenID
// token is minted for the ghost via mautrix RequestOpenIDToken (client.go:1995,
// POST /user/{userID}/openid/request_token, masqueraded by ?user_id=).
func getSFUToken(ctx context.Context, cfg *envConfig, ghost *mautrix.Client, roomID, deviceID string, log *slog.Logger) (*sfuResponse, error) {
	oid, err := ghost.RequestOpenIDToken(ctx)
	if err != nil {
		return nil, fmt.Errorf("openid request_token as ghost: %w", err)
	}
	log.Info("openid token minted for ghost", "matrix_server_name", oid.MatrixServerName, "expires_in", oid.ExpiresIn)
	body, _ := json.Marshal(map[string]any{
		"room":         roomID,
		"openid_token": oid, // fields: access_token, token_type, matrix_server_name, expires_in
		"device_id":    deviceID,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.LiveKitSvcURL+sfuGetPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 20 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", sfuGetPath, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		// Error bodies from lk-jwt are small Matrix-style JSON errors, no secrets.
		return nil, fmt.Errorf("lk-jwt-service %s returned %d: %s", sfuGetPath, resp.StatusCode, truncate(string(raw), 300))
	}
	var out sfuResponse
	if err := json.Unmarshal(raw, &out); err != nil || out.URL == "" || out.JWT == "" {
		return nil, fmt.Errorf("lk-jwt-service response not understood (err=%v)", err)
	}
	return &out, nil
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// inspectJWT decodes the payload WITHOUT verification and logs sub + video.room
// (spec §9 Q2). The token itself is never logged.
func inspectJWT(jwt, wantSub, roomID string, log *slog.Logger) {
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		log.Warn("jwt is not a 3-part token")
		return
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		log.Warn("jwt payload decode failed", "err", err)
		return
	}
	var c struct {
		Sub   string `json:"sub"`
		Video struct {
			Room         string `json:"room"`
			CanPublish   *bool  `json:"canPublish"`
			CanSubscribe *bool  `json:"canSubscribe"`
		} `json:"video"`
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		log.Warn("jwt payload parse failed", "err", err)
		return
	}
	wantRoom := livekitRoomName(roomID)
	log.Info("jwt claims", "sub", c.Sub, "sub_matches_expected", c.Sub == wantSub,
		"video.room", c.Video.Room, "room_matches_expected_hash", c.Video.Room == wantRoom,
		"canPublish", c.Video.CanPublish, "canSubscribe", c.Video.CanSubscribe)
	if c.Sub != wantSub {
		log.Warn("LiveKit identity differs from membership-derived identity: owner clients will not render this participant", "want", wantSub)
	}
	if c.Video.Room != wantRoom {
		log.Warn("LiveKit room differs from unpaddedBase64(sha256([room_id, m.call#ROOM]))", "want", wantRoom)
	}
	if c.Video.CanPublish != nil && !*c.Video.CanPublish {
		log.Warn("jwt has canPublish=false: add the server name to LIVEKIT_FULL_ACCESS_HOMESERVERS")
	}
}

// rtcSession is a connected LiveKit participant publishing a beep tone.
type rtcSession struct {
	log   *slog.Logger
	room  *lksdk.Room
	track *lksdk.LocalTrack
	owner string

	txFrames atomic.Int64
	rxPkts   atomic.Int64
	rxTracks atomic.Int64

	stop      chan struct{}
	stopOnce  sync.Once
	ended     chan string // closed-with-reason signal: receives first end reason
	endedOnce sync.Once
}

// signalEnded reports (once) that the session is over, e.g. disconnect or owner leaving.
func (s *rtcSession) signalEnded(reason string) {
	s.endedOnce.Do(func() { s.ended <- reason })
}

// connectRTC connects to LiveKit with the given jwt and publishes the tone.
func connectRTC(url, jwt, ownerMXID string, log *slog.Logger) (*rtcSession, error) {
	s := &rtcSession{log: log, owner: ownerMXID, stop: make(chan struct{}), ended: make(chan string, 1)}

	// Simplest correct path: a sample track carrying our own Opus packets.
	// (server-sdk-go has no built-in tone generator; hraban/opus encodes below.)
	track, err := lksdk.NewLocalSampleTrack(webrtc.RTPCodecCapability{
		MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2,
	})
	if err != nil {
		return nil, fmt.Errorf("new sample track: %w", err)
	}
	s.track = track

	cb := &lksdk.RoomCallback{
		OnDisconnectedWithReason: func(reason lksdk.DisconnectionReason) {
			log.Info("livekit disconnected", "reason", string(reason))
			s.signalEnded("livekit disconnected: " + string(reason))
		},
		OnParticipantConnected: func(rp *lksdk.RemoteParticipant) {
			log.Info("livekit participant joined", "identity", rp.Identity())
		},
		OnParticipantDisconnected: func(rp *lksdk.RemoteParticipant) {
			log.Info("livekit participant left", "identity", rp.Identity())
			if strings.HasPrefix(rp.Identity(), ownerMXID+":") {
				s.signalEnded("owner left LiveKit: " + rp.Identity())
			}
		},
		ParticipantCallback: lksdk.ParticipantCallback{
			OnTrackSubscribed: func(t *webrtc.TrackRemote, pub *lksdk.RemoteTrackPublication, rp *lksdk.RemoteParticipant) {
				log.Info("subscribed to remote track", "identity", rp.Identity(), "kind", t.Kind().String(),
					"codec", t.Codec().MimeType, "source", pub.Source().String())
				if t.Kind() != webrtc.RTPCodecTypeAudio {
					return
				}
				s.rxTracks.Add(1)
				go func() {
					for {
						if _, _, err := t.ReadRTP(); err != nil {
							return
						}
						s.rxPkts.Add(1)
					}
				}()
			},
		},
	}
	room, err := lksdk.ConnectToRoomWithToken(url, jwt, cb, lksdk.WithAutoSubscribe(true))
	if err != nil {
		return nil, fmt.Errorf("connect to livekit: %w", err)
	}
	s.room = room
	log.Info("livekit connected", "room_name", room.Name(), "local_identity", room.LocalParticipant.Identity())
	for _, rp := range room.GetRemoteParticipants() {
		log.Info("livekit participant already present", "identity", rp.Identity())
	}

	// Source MICROPHONE: Element Call maps tracks by source (spec §5). No video track.
	if _, err := room.LocalParticipant.PublishTrack(track, &lksdk.TrackPublicationOptions{
		Name: "audio", Source: livekit.TrackSource_MICROPHONE,
	}); err != nil {
		room.Disconnect()
		return nil, fmt.Errorf("publish audio track: %w", err)
	}
	log.Info("published tone track", "source", "MICROPHONE")

	go s.toneLoop()
	go s.statsLoop()
	return s, nil
}

// toneLoop sends 20 ms Opus frames (48 kHz mono) of a 440 Hz beep: 400 ms on, 600 ms off.
func (s *rtcSession) toneLoop() {
	const (
		rate    = 48000
		frame   = 960 // 20 ms
		hz      = 440.0
		amp     = 0.3
		cycle   = 50 // frames per 1 s beep cycle
		onCount = 20
	)
	enc, err := opus.NewEncoder(rate, 1, opus.AppVoIP)
	if err != nil {
		s.log.Error("opus encoder", "err", err)
		s.signalEnded("opus encoder failed")
		return
	}
	pcm := make([]int16, frame)
	out := make([]byte, 1500)
	var n int64 // running sample index for phase continuity
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for i := 0; ; i++ {
		select {
		case <-s.stop:
			return
		case <-tick.C:
		}
		on := i%cycle < onCount
		for j := range pcm {
			if on {
				pcm[j] = int16(amp * 32767 * math.Sin(2*math.Pi*hz*float64(n)/rate))
			} else {
				pcm[j] = 0
			}
			n++
		}
		k, err := enc.Encode(pcm, out)
		if err != nil {
			s.log.Error("opus encode", "err", err)
			continue
		}
		data := append([]byte(nil), out[:k]...)
		if err := s.track.WriteSample(media.Sample{Data: data, Duration: 20 * time.Millisecond}, &lksdk.SampleWriteOptions{}); err != nil {
			s.log.Warn("write sample", "err", err)
			continue
		}
		s.txFrames.Add(1)
	}
}

func (s *rtcSession) statsLoop() {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-t.C:
		}
		var ids []string
		for _, rp := range s.room.GetRemoteParticipants() {
			ids = append(ids, rp.Identity())
		}
		s.log.Info("rtc stats", "tx_frames", s.txFrames.Load(), "rx_packets", s.rxPkts.Load(),
			"rx_audio_tracks", s.rxTracks.Load(), "remote_participants", ids)
	}
}

// Close stops the tone and leaves LiveKit. Safe to call more than once.
func (s *rtcSession) Close() {
	s.stopOnce.Do(func() {
		close(s.stop)
		s.room.Disconnect()
		s.log.Info("left livekit", "tx_frames", s.txFrames.Load(), "rx_packets", s.rxPkts.Load())
	})
}
