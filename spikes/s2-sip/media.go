package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
)

var reRtpmap = regexp.MustCompile(`(?m)^a=rtpmap:(\d+) ([^\r\n/]+)`)

// sdpSummary extracts offered codecs and the remote media address from an SDP
// body, for logging only.
func sdpSummary(sdp []byte) (codecs []string, remote string) {
	s := string(sdp)
	for _, m := range reRtpmap.FindAllStringSubmatch(s, -1) {
		codecs = append(codecs, m[2]+"/"+m[1])
	}
	var ip, port string
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "c=IN IP4 ") && ip == "" {
			ip = strings.TrimPrefix(l, "c=IN IP4 ")
		}
		if strings.HasPrefix(l, "m=audio ") {
			f := strings.Fields(l)
			if len(f) > 1 {
				port = f[1]
			}
			// static payload types without an rtpmap line
			if len(codecs) == 0 {
				for _, pt := range f[3:] {
					codecs = append(codecs, "pt"+pt)
				}
			}
		}
	}
	return codecs, ip + ":" + port
}

// mediaOpts carries what runMedia needs.
type mediaOpts struct {
	log        *slog.Logger
	recordDir  string
	tonePath   string
	earlyRTP   int
	toneDelay  time.Duration
	label      string // "in" or "out"
	dialogDone <-chan struct{}
}

// runMedia drives one answered call: early silence, DTMF counting, WAV
// recording of both directions, tone playback, RTP counters. It returns when the
// inbound audio stream ends (BYE / media closed).
func runMedia(ctx context.Context, o mediaOpts, med *diago.DialogMedia) error {
	log := o.log
	ms := med.MediaSession()
	if ms != nil {
		log.Info("media negotiated", "local_rtp", ms.Laddr.String(), "remote_rtp", ms.Raddr.String())
	}

	// 1. Early RTP: silence to open the NAT pinhole toward the carrier's media IP.
	if o.earlyRTP > 0 {
		w, err := med.AudioWriter()
		if err != nil {
			return fmt.Errorf("audio writer: %w", err)
		}
		silence := make([]byte, 160) // 20 ms of PCMU
		for i := range silence {
			silence[i] = 0xFF // mu-law digital silence
		}
		for i := 0; i < o.earlyRTP; i++ {
			if _, err := w.Write(silence); err != nil {
				return fmt.Errorf("early rtp write: %w", err)
			}
		}
		log.Info("early rtp sent", "packets", o.earlyRTP)
	}

	// 2. DTMF: count only, never log digits.
	var dtmfCount atomic.Int64
	dr, err := med.AudioReaderDTMF()
	if err != nil {
		return fmt.Errorf("dtmf reader: %w", err)
	}
	dr.OnDTMF(func(rune) error {
		n := dtmfCount.Add(1)
		log.Info("dtmf received", "digit-count", n)
		return nil
	})
	med.SetAudioReader(dr)

	// 3. Recording (stereo: inbound on one channel, our playback on the other).
	if err := os.MkdirAll(o.recordDir, 0o700); err != nil {
		return err
	}
	wavPath := filepath.Join(o.recordDir, fmt.Sprintf("%s-%s.wav", o.label, time.Now().Format("20060102-150405")))
	f, err := os.OpenFile(wavPath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	rec, err := med.AudioStereoRecordingCreate(f)
	if err != nil {
		return fmt.Errorf("recording: %w", err)
	}
	started := time.Now()
	log.Info("recording started", "path", wavPath)
	defer func() {
		if err := rec.Close(); err != nil {
			log.Error("recording close failed", "err", err)
		}
		log.Info("wav closed", "path", wavPath, "seconds", int(time.Since(started).Seconds()))
	}()
	med.SetAudioWriter(rec.AudioWriter())

	// 4. Tone playback, once, shortly after the call is up.
	go func() {
		select {
		case <-time.After(o.toneDelay):
		case <-o.dialogDone:
			return
		case <-ctx.Done():
			return
		}
		pb, err := med.PlaybackCreate()
		if err != nil {
			log.Error("playback create failed", "err", err)
			return
		}
		if _, err := pb.PlayFile(o.tonePath); err != nil {
			log.Error("tone playback failed", "err", err)
			return
		}
		log.Info("tone sent")
	}()

	// 5. RTP counters every 5 s (+ report when the remote media address changes,
	// which is the symmetric-RTP latching evidence).
	statsDone := make(chan struct{})
	defer close(statsDone)
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		lastRemote := ""
		if ms != nil {
			lastRemote = ms.Raddr.String()
		}
		for {
			select {
			case <-t.C:
			case <-statsDone:
				return
			}
			rs := med.RTPSession()
			if rs == nil {
				continue
			}
			r, w := rs.ReadStats(), rs.WriteStats()
			log.Info("rtp stats", "rx_packets", r.PacketsCount, "rx_bytes", r.OctetCount,
				"tx_packets", w.PacketsCount, "tx_bytes", w.OctetCount)
			if ms != nil {
				if cur := ms.Raddr.String(); cur != lastRemote {
					log.Info("rtp remote address changed (latching)", "from", lastRemote, "to", cur)
					lastRemote = cur
				}
			}
		}
	}()

	// 6. Read inbound audio until the stream ends.
	rd := rec.AudioReader()
	buf := make([]byte, media.RTPBufSize)
	for {
		if _, err := rd.Read(buf); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, os.ErrClosed) || ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}
