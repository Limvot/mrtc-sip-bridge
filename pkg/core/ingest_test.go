package core

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"

	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
)

type recordedEvent struct {
	login LoginRef
	evt   matrixrtc.Event
}

type fakeSink struct{ got []recordedEvent }

func (f *fakeSink) HandleRTCEvent(_ context.Context, login LoginRef, evt matrixrtc.Event) {
	f.got = append(f.got, recordedEvent{login, evt})
}

func knownEvent() *event.Event {
	return &event.Event{
		Type:    matrixrtc.RTCNotification,
		RoomID:  "!room:example.org",
		ID:      "$ev",
		Sender:  "@ghost:example.org",
		Content: event.Content{Raw: map[string]any{"k": "v"}},
	}
}

func TestIngestForwardsKnownEvent(t *testing.T) {
	sink := &fakeSink{}
	in := NewIngest(sink)
	if !in.HandleMatrixRTCEvent(context.Background(), "login1", knownEvent()) {
		t.Fatal("returned false")
	}
	if len(sink.got) != 1 {
		t.Fatalf("got %d events", len(sink.got))
	}
	if sink.got[0].login != "login1" || sink.got[0].evt.Kind != matrixrtc.KindRTCNotification {
		t.Fatalf("got %+v", sink.got[0])
	}
}

func TestIngestDrops(t *testing.T) {
	sink := &fakeSink{}
	in := NewIngest(sink)
	ctx := context.Background()
	unknown := &event.Event{Type: event.EventMessage}
	if in.HandleMatrixRTCEvent(ctx, "login1", unknown) {
		t.Error("unknown type forwarded")
	}
	if in.HandleMatrixRTCEvent(ctx, "", knownEvent()) {
		t.Error("empty login forwarded")
	}
	if in.HandleMatrixRTCEvent(ctx, "login1", nil) {
		t.Error("nil event forwarded")
	}
	if len(sink.got) != 0 {
		t.Fatalf("sink got %d events", len(sink.got))
	}
}

func TestSetSinkNilFallsBackToLogSink(t *testing.T) {
	in := NewIngest(&fakeSink{})
	in.SetSink(nil)
	if !in.HandleMatrixRTCEvent(context.Background(), "login1", knownEvent()) {
		t.Fatal("returned false")
	}
	if !NewIngest(nil).HandleMatrixRTCEvent(context.Background(), "login1", knownEvent()) {
		t.Fatal("NewIngest(nil) returned false")
	}
}

func TestLogSinkNeverLogsContent(t *testing.T) {
	var buf bytes.Buffer
	logger := zerolog.New(&buf)
	evt := knownEvent()
	evt.Content = event.Content{Raw: map[string]any{"secret": "hunter2-secret"}}
	if !NewIngest(LogSink{Log: &logger}).HandleMatrixRTCEvent(context.Background(), "login1", evt) {
		t.Fatal("returned false")
	}
	out := buf.String()
	for _, want := range []string{"rtc_notification", "!room:example.org", "MatrixRTC event ingested", "login1"} {
		if !strings.Contains(out, want) {
			t.Errorf("log %q missing %q", out, want)
		}
	}
	if strings.Contains(out, "hunter2-secret") {
		t.Errorf("log leaked content: %s", out)
	}
}

func TestLogSinkNilLogUsesCtxLogger(t *testing.T) {
	var buf bytes.Buffer
	ctx := zerolog.New(&buf).WithContext(context.Background())
	LogSink{}.HandleRTCEvent(ctx, "login1", matrixrtc.Event{Type: "x", Kind: matrixrtc.KindRTCNotification})
	if out := buf.String(); !strings.Contains(out, "MatrixRTC event ingested") {
		t.Errorf("ctx logger did not receive the line: %q", out)
	}
}
