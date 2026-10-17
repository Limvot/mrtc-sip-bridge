package calls

import (
	"time"

	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/core"
)

// Snapshot is a read-only view of a login's active call (WIDGET-SPEC §8.2). It never carries DTMF
// digits, SIP identifiers or credentials.
type Snapshot struct {
	ID        string
	Direction string // "inbound" or "outbound"
	State     string // state.String(): ringing, answering, joining, dialing, early, active, ...
	Outbound  bool
	RoomID    id.RoomID
	Remote    string // E.164, "" until known
	StartedAt time.Time
	// AnsweredAt is the zero time until the call is answered.
	AnsweredAt time.Time
}

// setState moves the actor to s and publishes the new snapshot. Actor goroutine only.
func (c *call) setState(s state) {
	c.state = s
	c.publish()
}

// publish stores an immutable snapshot of the actor's current view. Actor goroutine only; lock-free.
func (c *call) publish() {
	c.pub.Store(&Snapshot{
		ID:         c.id,
		Direction:  c.direction,
		State:      c.state.String(),
		Outbound:   c.isOutbound(),
		RoomID:     c.roomID,
		Remote:     c.e164,
		StartedAt:  c.startedAt,
		AnsweredAt: c.answeredAt,
	})
}

// Snapshot returns the login's active (non-busy) call, if any. It takes only m.mu and loads the
// published pointer: no actor locks, no I/O. A call that has not published yet (still in early setup)
// or that started ending (the active slot is released) is not reported.
func (m *Manager) Snapshot(login core.LoginRef) (Snapshot, bool) {
	m.mu.Lock()
	c := m.active[login]
	m.mu.Unlock()
	if c == nil {
		return Snapshot{}, false
	}
	p := c.pub.Load()
	if p == nil {
		return Snapshot{}, false
	}
	return *p, true
}
