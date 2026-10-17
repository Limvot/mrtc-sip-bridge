package core

import (
	"context"
	"testing"

	"maunium.net/go/mautrix/event"

	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
)

type fakeRegistrar struct {
	types    []event.Type
	handlers []func(ctx context.Context, evt *event.Event)
}

func (f *fakeRegistrar) On(t event.Type, h func(ctx context.Context, evt *event.Event)) {
	f.types = append(f.types, t)
	f.handlers = append(f.handlers, h)
}

func TestRegisterMatrixRTCHandlers(t *testing.T) {
	sink := &fakeSink{}
	in := NewIngest(sink)
	reg := &fakeRegistrar{}
	resolveOK := false
	resolve := func(ctx context.Context, evt *event.Event) (LoginRef, bool) {
		return "resolved", resolveOK
	}
	RegisterMatrixRTCHandlers(reg, resolve, in)

	want := matrixrtc.IngestEventTypes()
	if len(reg.types) != len(want) {
		t.Fatalf("registered %d types, want %d", len(reg.types), len(want))
	}
	for i := range want {
		if reg.types[i] != want[i] {
			t.Errorf("type[%d] = %v, want %v", i, reg.types[i], want[i])
		}
	}

	ctx := context.Background()
	reg.handlers[2](ctx, knownEvent())
	if len(sink.got) != 0 {
		t.Fatalf("event reached sink despite resolve=false")
	}
	resolveOK = true
	reg.handlers[2](ctx, knownEvent())
	if len(sink.got) != 1 || sink.got[0].login != "resolved" {
		t.Fatalf("sink got %+v", sink.got)
	}
}
