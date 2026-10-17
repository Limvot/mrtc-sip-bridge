package core

import (
	"context"
	"time"

	"maunium.net/go/mautrix/event"

	"github.com/williamkray/mrtc-sip-bridge/pkg/matrixrtc"
)

// EventRegistrar is satisfied by *appservice.EventProcessor (appservice/eventprocessor.go:63;
// appservice.EventHandler is an alias for this func type, :30).
type EventRegistrar interface {
	On(evtType event.Type, handler func(ctx context.Context, evt *event.Event))
}

// LoginResolver maps an incoming event to the login whose room it is. Return ok=false to drop the
// event (unknown room, sender is a bridge ghost or the bridge bot, ...). Must not block on network I/O
// (a DB lookup is fine).
type LoginResolver func(ctx context.Context, evt *event.Event) (login LoginRef, ok bool)

// RegisterMatrixRTCHandlers registers one handler per matrixrtc.IngestEventTypes() entry on reg.
// Each handler: login, ok := resolve(ctx, evt); if ok { in.HandleMatrixRTCEvent(ctx, login, evt) }.
// Events the resolver drops are counted (routed=false) but create no span; accepted ones get a
// matrixrtc.ingest span backdated to handler entry (TELEMETRY-SPEC §5.5).
// Must be called before the EventProcessor starts (bridgev2/matrix/connector.go:229), i.e. in PostInit.
func RegisterMatrixRTCHandlers(reg EventRegistrar, resolve LoginResolver, in *Ingest) {
	for _, t := range matrixrtc.IngestEventTypes() {
		reg.On(t, func(ctx context.Context, evt *event.Event) {
			t0 := time.Now()
			login, ok := resolve(ctx, evt)
			in.ingest(ctx, login, ok && login != "", evt, t0)
		})
	}
}
