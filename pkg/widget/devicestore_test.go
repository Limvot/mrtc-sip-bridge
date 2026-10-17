package widget

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	_ "go.mau.fi/util/dbutil/litestream" // registers sqlite3-fk-wal, the bridges' SQLite driver
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
)

const (
	dsBridge = networkid.BridgeID("mrtcsip")
	dsLogin  = networkid.UserLoginID("login-alice")
	dsLogin2 = networkid.UserLoginID("login-alice-2")
	dsLoginB = networkid.UserLoginID("login-bob")
	day      = 24 * time.Hour
)

type dsEnv struct {
	t      *testing.T
	db     *dbutil.Database
	clk    *clock.Fake
	st     *DeviceStore
	logMu  sync.Mutex
	logBuf bytes.Buffer
}

// newDSDB opens a temp-file SQLite database the way the bridges do (sqlite3-fk-wal,
// _txlock=immediate, a connection pool), logging every query with its args at Trace into buf.
func newDSDB(t *testing.T, buf *bytes.Buffer, mu *sync.Mutex) *dbutil.Database {
	t.Helper()
	path := filepath.Join(t.TempDir(), "devices.db")
	db, err := dbutil.NewWithDialect("file:"+path+"?_txlock=immediate", "sqlite3-fk-wal")
	if err != nil {
		t.Fatal(err)
	}
	db.RawDB.SetMaxOpenConns(8)
	log := zerolog.New(lockedWriter{mu: mu, b: buf}).Level(zerolog.TraceLevel)
	db.Log = dbutil.ZeroLogger(log, dbutil.ZeroLogSettings{TraceLogAllQueries: true})
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func newDS(t *testing.T, cfg DeviceStoreConfig) *dsEnv {
	t.Helper()
	e := &dsEnv{t: t, clk: clock.NewFake(t0)}
	e.db = newDSDB(t, &e.logBuf, &e.logMu)
	e.st = e.newStore(dsBridge, cfg)
	return e
}

func (e *dsEnv) newStore(bridge networkid.BridgeID, cfg DeviceStoreConfig) *DeviceStore {
	e.t.Helper()
	log := zerolog.New(lockedWriter{mu: &e.logMu, b: &e.logBuf}).Level(zerolog.TraceLevel)
	st := NewDeviceStore(e.db, bridge, cfg, e.clk, log)
	if err := st.Upgrade(e.ctx()); err != nil {
		e.t.Fatal(err)
	}
	return st
}

func (e *dsEnv) ctx() context.Context {
	log := zerolog.New(lockedWriter{mu: &e.logMu, b: &e.logBuf}).Level(zerolog.TraceLevel)
	return log.WithContext(context.Background())
}

func (e *dsEnv) logs() string {
	e.logMu.Lock()
	defer e.logMu.Unlock()
	return e.logBuf.String()
}

func (e *dsEnv) issue(user id.UserID, login networkid.UserLoginID, name string) IssueResult {
	e.t.Helper()
	res, err := e.st.Issue(e.ctx(), IssueParams{User: user, Login: login, Room: roomMgmt, Name: name})
	if err != nil {
		e.t.Fatalf("Issue: %v", err)
	}
	return res
}

// enrolled issues a link for alice and redeems it, returning the device and its first token.
func (e *dsEnv) enrolled() (Device, string) {
	e.t.Helper()
	is := e.issue(alice, dsLogin, "Pixel")
	en, err := e.st.Enroll(e.ctx(), is.Code, ClientChromeAndroid)
	if err != nil {
		e.t.Fatalf("Enroll: %v", err)
	}
	return en.Device, en.Token
}

func (e *dsEnv) rotate(tok string) RotateResult {
	e.t.Helper()
	r, err := e.st.Rotate(e.ctx(), tok, ClientChromeAndroid)
	if err != nil {
		e.t.Fatalf("Rotate: %v", err)
	}
	if r.Reuse || !ValidDeviceToken(r.Next) {
		e.t.Fatalf("Rotate result %+v", r)
	}
	return r
}

func (e *dsEnv) confirm(tok string) ConfirmResult {
	e.t.Helper()
	r, err := e.st.Confirm(e.ctx(), tok)
	if err != nil {
		e.t.Fatalf("Confirm: %v", err)
	}
	return r
}

// renew is a full client renewal: exchange, then confirm the successor.
func (e *dsEnv) renew(tok string) string {
	e.t.Helper()
	next := e.rotate(tok).Next
	e.confirm(next)
	return next
}

func (e *dsEnv) count(query string, args ...any) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(e.ctx(), query, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *dsEnv) tokenRows(devID string) int {
	return e.count(`SELECT COUNT(*) FROM mrtcsip_dialpad_token WHERE bridge_id=$1 AND device_id=$2`, dsBridge, devID)
}

func wantErr(t *testing.T, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// ---- Schema ----

func TestDeviceStoreUpgradeIdempotent(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{})
	for _, tbl := range []string{"mrtcsip_dialpad_device", "mrtcsip_dialpad_token", DeviceVersionTable} {
		ok, err := e.st.db.TableExists(e.ctx(), tbl)
		if err != nil || !ok {
			t.Fatalf("table %s exists=%v err=%v", tbl, ok, err)
		}
	}
	_, tok := e.enrolled()
	// A second store (a restart) over the same DB upgrades again without error and sees the data.
	st2 := e.newStore(dsBridge, DeviceStoreConfig{})
	if err := st2.Upgrade(e.ctx()); err != nil {
		t.Fatal(err)
	}
	if _, err := st2.Rotate(e.ctx(), tok, ClientChromeAndroid); err != nil {
		t.Fatal(err)
	}
	if v := e.count(`SELECT version FROM ` + DeviceVersionTable); v != 1 {
		t.Fatalf("version = %d", v)
	}
}

// ---- Issue, peek, enroll ----

func TestDeviceIssuePeekEnroll(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{})
	is := e.issue(alice, dsLogin, "")
	if !ValidCode(is.Code) || len(is.Code) != len(DeviceCodePrefix)+43 {
		t.Fatalf("code format %q", is.Code)
	}
	d := is.Device
	if len(d.ID) != 6 || strings.Trim(d.ID, "abcdefghijklmnopqrstuvwxyz234567") != "" {
		t.Fatalf("id %q", d.ID)
	}
	if d.Name != "device-"+d.ID || d.State != DeviceStatePending || d.User != alice || d.Login != dsLogin ||
		d.Room != roomMgmt || !d.CodeExpires.Equal(t0.Add(15*time.Minute)) || !d.ExpiresAt.Equal(d.CodeExpires) {
		t.Fatalf("device %+v", d)
	}

	// Peek has no side effects.
	for range 3 {
		p, err := e.st.Peek(e.ctx(), is.Code)
		if err != nil || p.ID != d.ID || p.User != alice || p.State != DeviceStatePending {
			t.Fatalf("Peek %+v %v", p, err)
		}
	}
	e.clk.Advance(14 * time.Minute)
	en, err := e.st.Enroll(e.ctx(), is.Code, "netscape")
	if err != nil {
		t.Fatal(err)
	}
	if !ValidDeviceToken(en.Token) || en.Device.State != DeviceStateActive || en.Device.Client != ClientOther ||
		!en.Device.Enrolled.Equal(e.clk.Now()) || !en.Device.LastUsed.Equal(e.clk.Now()) ||
		!en.Device.HardExpiry.IsZero() || !en.Device.ExpiresAt.Equal(e.clk.Now().Add(90*day)) {
		t.Fatalf("enrolled %+v", en.Device)
	}
	// Single use.
	_, err = e.st.Enroll(e.ctx(), is.Code, ClientChromeAndroid)
	wantErr(t, err, ErrLinkUsed)
	_, err = e.st.Peek(e.ctx(), is.Code)
	wantErr(t, err, ErrLinkUsed)
	// The first token is confirmed: it rotates as "current".
	if r := e.rotate(en.Token); r.TokenState != TokenStateCurrent {
		t.Fatalf("state %s", r.TokenState)
	}
	if e.tokenRows(d.ID) != 2 {
		t.Fatal("token rows")
	}
}

func TestDeviceEnrollErrors(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{EnrollTTL: 10 * time.Minute})
	is := e.issue(alice, dsLogin, "a")
	e.clk.Advance(10 * time.Minute)
	_, err := e.st.Peek(e.ctx(), is.Code)
	wantErr(t, err, ErrLinkExpired)
	_, err = e.st.Enroll(e.ctx(), is.Code, ClientOther)
	wantErr(t, err, ErrLinkExpired)

	unknown, _ := newSecret(DeviceCodePrefix)
	for _, c := range []string{"", "garbage", unknown, strings.Replace(unknown, "dpc", "dpt", 1),
		unknown[:len(unknown)-1], unknown + "A", unknown[:len(unknown)-1] + "!"} {
		_, err = e.st.Peek(e.ctx(), c)
		wantErr(t, err, ErrLinkInvalid)
		_, err = e.st.Enroll(e.ctx(), c, ClientOther)
		wantErr(t, err, ErrLinkInvalid)
	}
	// Malformed codes never reach the database.
	before := e.logs()
	_, _ = e.st.Enroll(e.ctx(), "mrtcsip_dpc_short", ClientOther)
	_, _ = e.st.Rotate(e.ctx(), "mrtcsip_dpt_short", ClientOther)
	_, _ = e.st.Confirm(e.ctx(), "nope")
	if e.logs() != before {
		t.Fatal("malformed input hit the database")
	}

	// A revoked (deleted) link is invalid.
	is2 := e.issue(alice, dsLogin, "b")
	if _, err = e.st.Revoke(e.ctx(), alice, is2.Device.ID); err != nil {
		t.Fatal(err)
	}
	_, err = e.st.Enroll(e.ctx(), is2.Code, ClientOther)
	wantErr(t, err, ErrLinkInvalid)
}

func TestDeviceEnrollConcurrentSingleUse(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{})
	is := e.issue(alice, dsLogin, "race")
	const n = 8
	var wg sync.WaitGroup
	results := make([]error, n)
	tokens := make([]string, n)
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			r, err := e.st.Enroll(e.ctx(), is.Code, ClientChromeAndroid)
			results[i], tokens[i] = err, r.Token
		})
	}
	close(start)
	wg.Wait()
	wins := 0
	for i, err := range results {
		switch {
		case err == nil:
			wins++
			if !ValidDeviceToken(tokens[i]) {
				t.Fatal("winner without token")
			}
		case errors.Is(err, ErrLinkUsed):
		default:
			t.Fatalf("unexpected error %v", err)
		}
	}
	if wins != 1 || e.tokenRows(is.Device.ID) != 1 {
		t.Fatalf("wins = %d, tokens = %d", wins, e.tokenRows(is.Device.ID))
	}
}

func TestDeviceIssueLimitsAndNames(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{MaxLinksPerUser: 3, MaxDevicesTotal: 6})
	a := e.issue(alice, dsLogin, "Pixel 8")
	if a.Device.Name != "Pixel 8" {
		t.Fatal(a.Device.Name)
	}
	b := e.issue(alice, dsLogin2, "  Ünïcode_phone-2.0 ") // other login counts too; trimmed
	if b.Device.Name != "Ünïcode_phone-2.0" {
		t.Fatal(b.Device.Name)
	}
	if _, err := e.st.Enroll(e.ctx(), b.Code, ClientSafariIOS); err != nil {
		t.Fatal(err)
	}
	c := e.issue(alice, dsLogin, "")
	_, err := e.st.Issue(e.ctx(), IssueParams{User: alice, Login: dsLogin, Name: "x"})
	wantErr(t, err, ErrDeviceLimit)
	var le *DeviceLimitError
	want := []string{a.Device.ID, b.Device.ID, c.Device.ID}
	slices.Sort(want)
	if !errors.As(err, &le) || !slices.Equal(slices.Sorted(slices.Values(le.IDs)), want) {
		t.Fatalf("limit error %#v", err)
	}
	// Expired pending links don't count.
	e.clk.Advance(16 * time.Minute)
	e.issue(alice, dsLogin, "")
	e.issue(alice, dsLogin, "")
	// bob is limited separately; the global cap counts every device of the bridge.
	e.issue(bob, dsLoginB, "")
	_, err = e.st.Issue(e.ctx(), IssueParams{User: bob, Login: dsLoginB})
	wantErr(t, err, ErrDeviceCapacity)

	for _, bad := range []string{strings.Repeat("x", 33), "a<b", "emoji😀", "tab\tname", "new\nline"} {
		_, err = e.st.Issue(e.ctx(), IssueParams{User: bob, Login: dsLoginB, Name: bad})
		wantErr(t, err, ErrDeviceName)
	}
	if _, err = e.st.Issue(e.ctx(), IssueParams{User: alice}); err == nil {
		t.Fatal("Issue without login")
	}
}

func TestDeviceListRevoke(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{})
	d1, tok1 := e.enrolled()
	e.clk.Advance(time.Second) // List is oldest first
	p2 := e.issue(alice, dsLogin2, "two")
	if err := e.st.AttachNotice(e.ctx(), p2.Device.ID, roomMgmt, "$link2"); err != nil {
		t.Fatal(err)
	}
	b := e.issue(bob, dsLoginB, "bob")

	l, err := e.st.List(e.ctx(), alice)
	if err != nil || len(l) != 2 || l[0].ID != d1.ID || l[1].ID != p2.Device.ID {
		t.Fatalf("List %+v %v", l, err)
	}
	// Only the owner revokes.
	_, err = e.st.Revoke(e.ctx(), bob, d1.ID)
	wantErr(t, err, ErrDeviceNotFound)
	_, err = e.st.Revoke(e.ctx(), alice, "zzzzzz")
	wantErr(t, err, ErrDeviceNotFound)

	got, err := e.st.Revoke(e.ctx(), alice, d1.ID)
	if err != nil || len(got) != 1 || got[0].ID != d1.ID {
		t.Fatalf("Revoke %+v %v", got, err)
	}
	if e.tokenRows(d1.ID) != 0 {
		t.Fatal("tokens left behind")
	}
	_, err = e.st.Rotate(e.ctx(), tok1, ClientChromeAndroid)
	wantErr(t, err, ErrDeviceInvalid)

	got, err = e.st.Revoke(e.ctx(), alice, "all")
	if err != nil || len(got) != 1 || got[0].ID != p2.Device.ID || got[0].NoticeEvent != "$link2" {
		t.Fatalf("Revoke all %+v %v", got, err)
	}
	got, err = e.st.Revoke(e.ctx(), alice, "all")
	if err != nil || len(got) != 0 {
		t.Fatalf("Revoke all again %+v %v", got, err)
	}
	if l, _ = e.st.List(e.ctx(), bob); len(l) != 1 || l[0].ID != b.Device.ID {
		t.Fatal("bob's device affected")
	}
}

func TestDeviceBridgeIsolation(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{MaxLinksPerUser: 1})
	other := e.newStore("otherbridge", DeviceStoreConfig{MaxLinksPerUser: 1})
	d, tok := e.enrolled()
	is := e.issue(bob, dsLoginB, "")
	// The other bridge sees nothing and accepts none of this bridge's secrets.
	if l, _ := other.List(e.ctx(), alice); len(l) != 0 {
		t.Fatal("list leaks across bridges")
	}
	_, err := other.Rotate(e.ctx(), tok, ClientOther)
	wantErr(t, err, ErrDeviceInvalid)
	_, err = other.Confirm(e.ctx(), tok)
	wantErr(t, err, ErrDeviceInvalid)
	_, err = other.Peek(e.ctx(), is.Code)
	wantErr(t, err, ErrLinkInvalid)
	_, err = other.Enroll(e.ctx(), is.Code, ClientOther)
	wantErr(t, err, ErrLinkInvalid)
	_, err = other.Revoke(e.ctx(), alice, d.ID)
	wantErr(t, err, ErrDeviceNotFound)
	// Its own limit is separate.
	if _, err = other.Issue(e.ctx(), IssueParams{User: alice, Login: dsLogin}); err != nil {
		t.Fatal(err)
	}
	if _, err = other.PurgeOrphans(e.ctx(), func(context.Context, id.UserID, networkid.UserLoginID) (bool, error) {
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	if l, _ := e.st.List(e.ctx(), alice); len(l) != 1 {
		t.Fatal("other bridge's purge touched this bridge")
	}
}

// ---- Rotation ----

func TestDeviceRotationLifecycle(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{})
	d, t1 := e.enrolled()

	// Exchange: an unconfirmed successor; the presented token stays current.
	r := e.rotate(t1)
	if r.TokenState != TokenStateCurrent || r.Device.ID != d.ID || r.ClientChanged {
		t.Fatalf("rotate %+v", r)
	}
	t2 := r.Next
	e.clk.Advance(time.Hour)
	if r = e.rotate(t1); r.TokenState != TokenStateCurrent {
		t.Fatalf("old token not current before confirm: %s", r.TokenState)
	}
	t2b := r.Next

	// Confirm supersedes the parent and starts its grace.
	if c := e.confirm(t2b); c.Repeat || c.Reuse {
		t.Fatalf("confirm %+v", c)
	}
	if c := e.confirm(t2b); !c.Repeat {
		t.Fatal("second confirm must be a repeat")
	}
	e.clk.Advance(119 * time.Second)
	if r = e.rotate(t1); r.TokenState != TokenStateGrace {
		t.Fatalf("parent in grace: %s", r.TokenState)
	}
	if c := e.confirm(t1); !c.Repeat {
		t.Fatal("confirm of an in-grace token is a repeat")
	}
	_ = t2

	// After the grace window: reuse revokes the whole family, reported exactly once.
	e.clk.Advance(time.Second)
	r, err := e.st.Rotate(e.ctx(), t1, ClientChromeAndroid)
	wantErr(t, err, ErrDeviceRevoked)
	if !r.Reuse || r.Next != "" || r.Device.ID != d.ID || r.Device.State != DeviceStateRevoked ||
		r.Device.RevokedWhy != RevokedWhyReuse || r.Device.User != alice || r.Device.Room != roomMgmt {
		t.Fatalf("reuse result %+v", r)
	}
	for _, tok := range []string{t1, t2, t2b} {
		r, err = e.st.Rotate(e.ctx(), tok, ClientChromeAndroid)
		wantErr(t, err, ErrDeviceRevoked)
		if r.Reuse || r.Device.RevokedWhy != RevokedWhyReuse {
			t.Fatalf("after revoke %+v", r)
		}
		c, err := e.st.Confirm(e.ctx(), tok)
		wantErr(t, err, ErrDeviceRevoked)
		if c.Reuse || c.Device.RevokedWhy != RevokedWhyReuse {
			t.Fatalf("confirm after revoke %+v", c)
		}
	}
	// A revoked family never rotates, and it no longer counts against the user's limit.
	if n := e.count(`SELECT COUNT(*) FROM mrtcsip_dialpad_token WHERE device_id=$1 AND superseded_at IS NULL`, d.ID); n != 0 {
		t.Fatalf("%d live tokens after reuse revoke", n)
	}
	l, _ := e.st.List(e.ctx(), alice)
	if len(l) != 1 || l[0].State != DeviceStateRevoked {
		t.Fatalf("list %+v", l)
	}
}

func TestDeviceConfirmReuse(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{RotationGrace: 30 * time.Second})
	_, t1 := e.enrolled()
	t2 := e.renew(t1)
	e.clk.Advance(30 * time.Second)
	c, err := e.st.Confirm(e.ctx(), t1)
	wantErr(t, err, ErrDeviceRevoked)
	if !c.Reuse || c.Device.RevokedWhy != RevokedWhyReuse {
		t.Fatalf("confirm reuse %+v", c)
	}
	_, err = e.st.Rotate(e.ctx(), t2, ClientChromeAndroid)
	wantErr(t, err, ErrDeviceRevoked)
}

func TestDeviceUnconfirmedPresentedConfirms(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{})
	_, t1 := e.enrolled()
	t2 := e.rotate(t1).Next // stored by the client, confirm lost
	e.clk.Advance(3 * day)
	r := e.rotate(t2)
	if r.TokenState != TokenStateUnconfirmed {
		t.Fatalf("state %s", r.TokenState)
	}
	// t2 is now current and t1 superseded (grace from now).
	if c := e.confirm(t2); !c.Repeat {
		t.Fatal("t2 should be confirmed")
	}
	if r = e.rotate(t1); r.TokenState != TokenStateGrace {
		t.Fatalf("t1 state %s", r.TokenState)
	}
	e.clk.Advance(2 * time.Minute)
	_, err := e.st.Rotate(e.ctx(), t1, ClientChromeAndroid)
	wantErr(t, err, ErrDeviceRevoked)
}

func TestDeviceLostExchangeResponse(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{})
	_, t1 := e.enrolled()
	for range 5 { // every response lost
		_ = e.rotate(t1)
		e.clk.Advance(2 * day)
	}
	r := e.rotate(t1)
	if r.TokenState != TokenStateCurrent {
		t.Fatalf("state %s", r.TokenState)
	}
	e.confirm(r.Next)
	if c := e.confirm(r.Next); !c.Repeat {
		t.Fatal("repeat")
	}
}

// TestDeviceStaleUnconfirmedChildIsReuse is the fork PoC: a thief copies T and rotates once (C',
// never confirmed); the legit app renews and confirms; hours later the thief presents C'. Its parent T is
// dead, so this is reuse, for Rotate and for Confirm alike.
func TestDeviceStaleUnconfirmedChildIsReuse(t *testing.T) {
	for _, viaConfirm := range []bool{false, true} {
		e := newDS(t, DeviceStoreConfig{})
		d, tok := e.enrolled()
		thief := e.rotate(tok).Next // unconfirmed child of T
		legit := e.renew(tok)       // supersedes T
		e.clk.Advance(5 * time.Hour)
		var reuse bool
		var err error
		if viaConfirm {
			var c ConfirmResult
			c, err = e.st.Confirm(e.ctx(), thief)
			reuse = c.Reuse
		} else {
			var r RotateResult
			r, err = e.st.Rotate(e.ctx(), thief, ClientChromeAndroid)
			reuse = r.Reuse
		}
		wantErr(t, err, ErrDeviceRevoked)
		if !reuse {
			t.Fatalf("viaConfirm=%v: no reuse", viaConfirm)
		}
		l, _ := e.st.List(e.ctx(), alice)
		if len(l) != 1 || l[0].ID != d.ID || l[0].State != DeviceStateRevoked || l[0].RevokedWhy != RevokedWhyReuse {
			t.Fatalf("viaConfirm=%v: list %+v", viaConfirm, l)
		}
		_, err = e.st.Rotate(e.ctx(), legit, ClientChromeAndroid)
		wantErr(t, err, ErrDeviceRevoked)
	}
}

// An unconfirmed child still works while its parent is current or in grace.
func TestDeviceUnconfirmedChildParentGrace(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{})
	_, t1 := e.enrolled()
	a := e.rotate(t1).Next // stored, confirm lost
	b := e.renew(t1)       // another tab: t1 in grace now
	e.clk.Advance(time.Minute)
	if r := e.rotate(a); r.TokenState != TokenStateUnconfirmed {
		t.Fatalf("state %s", r.TokenState)
	}
	// a confirmed, b superseded (in grace); a sibling child of a still-current parent confirms too.
	c := e.rotate(a).Next
	if r := e.confirm(c); r.Repeat {
		t.Fatal("child of a current parent not confirmed")
	}
	if r := e.rotate(b); r.TokenState != TokenStateGrace {
		t.Fatalf("b state %s", r.TokenState)
	}
}

func TestDeviceUnconfirmedChildrenBounded(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{})
	d, t1 := e.enrolled()
	var kids []string
	for range 5 {
		kids = append(kids, e.rotate(t1).Next)
	}
	if n := e.count(`SELECT COUNT(*) FROM mrtcsip_dialpad_token WHERE device_id=$1 AND confirmed_at IS NULL`, d.ID); n != 2 {
		t.Fatalf("%d unconfirmed children", n)
	}
	for _, k := range kids[:3] {
		_, err := e.st.Confirm(e.ctx(), k)
		wantErr(t, err, ErrDeviceInvalid)
	}
	e.confirm(kids[3])
	e.confirm(kids[4])
}

// TestDeviceSiblingSuperseded: lock-less tabs fork the family inside the grace window. Confirming a
// sibling supersedes every other current token, so the device always has one current token and the
// losing branch goes to grace, then dead (reuse after the grace window).
func TestDeviceSiblingSuperseded(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{})
	d, t1 := e.enrolled()
	current := func() int {
		return e.count(`SELECT COUNT(*) FROM mrtcsip_dialpad_token WHERE device_id=$1
			AND confirmed_at IS NOT NULL AND superseded_at IS NULL`, d.ID)
	}
	var sib []string
	for range 2 {
		r := e.rotate(t1)
		e.confirm(r.Next)
		sib = append(sib, r.Next)
		if n := current(); n != 1 {
			t.Fatalf("%d current tokens", n)
		}
		e.clk.Advance(time.Second)
	}
	// Both siblings work inside the grace window; the older one is in grace.
	if r := e.rotate(sib[0]); r.TokenState != TokenStateGrace {
		t.Fatalf("older sibling %s", r.TokenState)
	}
	if r := e.rotate(sib[1]); r.TokenState != TokenStateCurrent {
		t.Fatalf("newer sibling %s", r.TokenState)
	}
	if c := e.confirm(sib[0]); !c.Repeat {
		t.Fatal("older sibling not a repeat")
	}
	e.clk.Advance(3 * time.Minute)
	r, err := e.st.Rotate(e.ctx(), sib[0], ClientChromeAndroid)
	wantErr(t, err, ErrDeviceRevoked)
	if !r.Reuse {
		t.Fatal("no reuse detection for the dead sibling")
	}
}

func TestDeviceConcurrentRotations(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{})
	d, t1 := e.enrolled()
	const n = 2
	var wg sync.WaitGroup
	res := make([]RotateResult, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := range n {
		wg.Go(func() {
			<-start
			res[i], errs[i] = e.st.Rotate(e.ctx(), t1, ClientChromeAndroid)
		})
	}
	close(start)
	wg.Wait()
	for i := range n {
		if errs[i] != nil || res[i].Reuse || res[i].Next == "" {
			t.Fatalf("rotation %d: %+v %v", i, res[i], errs[i])
		}
	}
	if res[0].Next == res[1].Next {
		t.Fatal("same successor twice")
	}
	// Both confirm (concurrently): the later confirm supersedes the other sibling, but both stay usable
	// inside the grace window, no reuse.
	for i := range n {
		wg.Go(func() { _, errs[i] = e.st.Confirm(e.ctx(), res[i].Next) })
	}
	wg.Wait()
	states := map[string]int{}
	for i := range n {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		r := e.rotate(res[i].Next)
		states[r.TokenState]++
	}
	if states[TokenStateCurrent] != 1 || states[TokenStateGrace] != 1 {
		t.Fatalf("sibling states %v", states)
	}
	l, _ := e.st.List(e.ctx(), alice)
	if len(l) != 1 || l[0].ID != d.ID || l[0].State != DeviceStateActive {
		t.Fatalf("list %+v", l)
	}
	// After the grace window the losing sibling is dead: presenting it is reuse.
	e.clk.Advance(3 * time.Minute)
	reused := 0
	for i := range n {
		r, err := e.st.Rotate(e.ctx(), res[i].Next, ClientChromeAndroid)
		if r.Reuse {
			reused++
			wantErr(t, err, ErrDeviceRevoked)
		}
	}
	if reused != 1 {
		t.Fatalf("%d reuse detections, want 1", reused)
	}
}

// TestDeviceRotationStress runs lock-less tabs that rotate, confirm and lose responses concurrently
// (all inside the grace window) against one shared "localStorage". Nothing may detect reuse, revoke or
// fail internally. ErrDeviceInvalid is tolerated: with more than two concurrent exchanges of one token,
// an older delivered sibling can be pruned (at most 2 unconfirmed children per token, §2.7 step 3).
func TestDeviceRotationStress(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{})
	d, t1 := e.enrolled()
	var mu sync.Mutex
	cur := t1
	var wg sync.WaitGroup
	errc := make(chan error, 64)
	check := func(tab, i int, what string, reuse bool, err error) bool {
		switch {
		case reuse:
			errc <- fmt.Errorf("tab %d %s %d: reuse", tab, what, i)
		case err == nil:
			return true
		case errors.Is(err, ErrDeviceInvalid):
		default:
			errc <- fmt.Errorf("tab %d %s %d: %w", tab, what, i, err)
		}
		return false
	}
	for tab := range 6 {
		wg.Go(func() {
			for i := range 8 {
				mu.Lock()
				tok := cur
				mu.Unlock()
				r, err := e.st.Rotate(e.ctx(), tok, ClientChromeAndroid)
				if !check(tab, i, "rotate", r.Reuse, err) || (tab+i)%3 == 0 { // response lost
					continue
				}
				mu.Lock()
				cur = r.Next
				mu.Unlock()
				if (tab+i)%4 == 0 {
					continue // confirm lost
				}
				c, err := e.st.Confirm(e.ctx(), r.Next)
				check(tab, i, "confirm", c.Reuse, err)
			}
		})
	}
	wg.Go(func() {
		for range 20 {
			if _, err := e.st.List(e.ctx(), alice); err != nil {
				errc <- err
				return
			}
		}
	})
	wg.Wait()
	close(errc)
	for err := range errc {
		t.Error(err)
	}
	l, _ := e.st.List(e.ctx(), alice)
	if len(l) != 1 || l[0].ID != d.ID || l[0].State != DeviceStateActive {
		t.Fatalf("device after stress %+v", l)
	}
	if n := e.count(`SELECT COUNT(*) FROM mrtcsip_dialpad_token WHERE device_id=$1
		AND confirmed_at IS NOT NULL AND superseded_at IS NULL`, d.ID); n != 1 {
		t.Fatalf("%d current tokens", n)
	}
}

func TestDeviceInactivityExpirySlides(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{})
	d, tok := e.enrolled()
	for range 4 { // used every 89 days: never expires
		e.clk.Advance(89 * day)
		r := e.rotate(tok)
		if !r.Device.ExpiresAt.Equal(e.clk.Now().Add(90 * day)) {
			t.Fatalf("expires %v", r.Device.ExpiresAt)
		}
		tok = r.Next
		e.confirm(tok)
	}
	e.clk.Advance(90 * day) // exactly at the boundary: still valid
	tok = e.renew(tok)
	e.clk.Advance(90*day + time.Millisecond)
	r, err := e.st.Rotate(e.ctx(), tok, ClientChromeAndroid)
	wantErr(t, err, ErrDeviceExpired)
	if r.Device.ID != d.ID || !r.Device.Expired {
		t.Fatalf("expired result %+v", r)
	}
	_, err = e.st.Confirm(e.ctx(), tok)
	wantErr(t, err, ErrDeviceExpired)
	l, _ := e.st.List(e.ctx(), alice)
	if len(l) != 1 || !l[0].Expired {
		t.Fatalf("list %+v", l)
	}
}

func TestDeviceHardExpiry(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{MaxLifetimeDays: 10, InactivityDays: 5})
	_, tok := e.enrolled()
	for i := range 3 {
		e.clk.Advance(3 * day)
		r := e.rotate(tok)
		want := t0.Add(10 * day)
		if i == 0 {
			want = e.clk.Now().Add(5 * day)
		}
		if !r.Device.HardExpiry.Equal(t0.Add(10*day)) || !r.Device.ExpiresAt.Equal(want) {
			t.Fatalf("round %d: hard %v expires %v", i, r.Device.HardExpiry, r.Device.ExpiresAt)
		}
		tok = r.Next
		e.confirm(tok)
	}
	e.clk.Advance(day + time.Millisecond) // day 10 + 1 ms
	_, err := e.st.Rotate(e.ctx(), tok, ClientChromeAndroid)
	wantErr(t, err, ErrDeviceExpired)

	// Off by default: no hard expiry is recorded or enforced.
	e2 := newDS(t, DeviceStoreConfig{InactivityDays: 5})
	d2, tok2 := e2.enrolled()
	if !d2.HardExpiry.IsZero() {
		t.Fatal("hard expiry set by default")
	}
	for range 100 {
		e2.clk.Advance(4 * day)
		tok2 = e2.renew(tok2)
	}
}

// Enabling max_lifetime_days later applies to devices enrolled before (computed from enrolled_at).
func TestDeviceHardExpiryEnabledLater(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{InactivityDays: 5})
	d, tok := e.enrolled()
	if !d.HardExpiry.IsZero() {
		t.Fatal("hard expiry recorded while off")
	}
	for range 2 {
		e.clk.Advance(4 * day)
		tok = e.renew(tok)
	}
	e.st = e.newStore(dsBridge, DeviceStoreConfig{InactivityDays: 5, MaxLifetimeDays: 10})
	l, _ := e.st.List(e.ctx(), alice)
	if len(l) != 1 || l[0].Expired || !l[0].ExpiresAt.Equal(t0.Add(10*day)) {
		t.Fatalf("list %+v", l)
	}
	e.clk.Advance(2*day + time.Millisecond) // day 10 + 1 ms: past enrolled + 10 days (inactivity: day 13)
	_, err := e.st.Rotate(e.ctx(), tok, ClientChromeAndroid)
	wantErr(t, err, ErrDeviceExpired)
}

// TestDeviceOfToken looks a device up by any of its tokens without side effects.
func TestDeviceOfToken(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{})
	d, t1 := e.enrolled()
	child := e.rotate(t1).Next
	t2 := e.renew(t1)
	e.clk.Advance(time.Hour) // t1 dead now
	before := e.count(`SELECT COUNT(*) FROM mrtcsip_dialpad_token WHERE device_id=$1 AND confirmed_at IS NOT NULL`, d.ID)
	for _, tok := range []string{t1, child, t2} {
		got, err := e.st.DeviceOfToken(e.ctx(), tok)
		if err != nil || got.ID != d.ID || got.User != alice {
			t.Fatalf("DeviceOfToken: %+v %v", got, err)
		}
	}
	if after := e.count(`SELECT COUNT(*) FROM mrtcsip_dialpad_token WHERE device_id=$1 AND confirmed_at IS NOT NULL`, d.ID); after != before {
		t.Fatal("DeviceOfToken confirmed a token")
	}
	l, _ := e.st.List(e.ctx(), alice)
	if l[0].State != DeviceStateActive || !l[0].LastUsed.Equal(t0) {
		t.Fatalf("DeviceOfToken changed the device: %+v", l[0])
	}
	_, err := e.st.DeviceOfToken(e.ctx(), "mrtcsip_dpt_"+strings.Repeat("A", 43))
	wantErr(t, err, ErrDeviceInvalid)
	_, err = e.st.DeviceOfToken(e.ctx(), "garbage")
	wantErr(t, err, ErrDeviceInvalid)
}

func TestDeviceDeadTokenOnExpiredDeviceIsExpired(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{InactivityDays: 1})
	_, t1 := e.enrolled()
	e.renew(t1)
	e.clk.Advance(2 * day)
	_, err := e.st.Rotate(e.ctx(), t1, ClientChromeAndroid)
	wantErr(t, err, ErrDeviceExpired) // expiry is checked before reuse: no revoke
	l, _ := e.st.List(e.ctx(), alice)
	if l[0].State != DeviceStateActive {
		t.Fatal("expired device was revoked")
	}
}

func TestDeviceClientChangeNotice(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{})
	_, tok := e.enrolled() // chrome_android
	r, err := e.st.Rotate(e.ctx(), tok, ClientFirefoxDesktop)
	if err != nil || !r.ClientChanged || r.Client != ClientFirefoxDesktop || r.Device.Client != ClientChromeAndroid {
		t.Fatalf("first change %+v %v", r, err)
	}
	e.confirm(r.Next)
	tok = r.Next
	e.clk.Advance(23 * time.Hour)
	r, _ = e.st.Rotate(e.ctx(), tok, ClientFirefoxDesktop)
	if r.ClientChanged {
		t.Fatal("notice twice within a day")
	}
	tok = r.Next
	e.confirm(tok)
	if r = e.rotate(tok); r.ClientChanged { // same client: never
		t.Fatal("notice for the enrolled client")
	}
	tok = r.Next
	e.confirm(tok)
	e.clk.Advance(time.Hour)
	r, _ = e.st.Rotate(e.ctx(), tok, "something odd")
	if !r.ClientChanged || r.Client != ClientOther {
		t.Fatalf("after a day %+v", r)
	}
}

// ---- Janitor ----

func TestDevicePurgeTokens(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{InactivityDays: 3})
	d, t1 := e.enrolled()
	t2 := e.renew(t1)          // t1 superseded at 0 h
	stale := e.rotate(t1).Next // unconfirmed child of a superseded parent (grace)
	keep := e.rotate(t2).Next  // unconfirmed child of the current t2: the client may hold it
	present := func(tok string) bool {
		return e.count(`SELECT COUNT(*) FROM mrtcsip_dialpad_token WHERE device_id=$1 AND token_hash=$2`,
			d.ID, hashSecret(tok)) == 1
	}
	e.clk.Advance(25 * time.Hour)
	st, err := e.st.PurgeExpired(e.ctx())
	if err != nil || st.Tokens != 1 || present(stale) || !present(keep) {
		t.Fatalf("purge %+v %v", st, err)
	}
	_, err = e.st.Confirm(e.ctx(), stale)
	wantErr(t, err, ErrDeviceInvalid)
	t3 := e.renew(keep) // the lost-confirm successor still works a day later

	// t1 stays recognizable (reuse) until inactivity_days after it was superseded.
	e.clk.Advance(46 * time.Hour) // 71 h
	if _, err = e.st.PurgeExpired(e.ctx()); err != nil || !present(t1) {
		t.Fatalf("superseded token pruned too early (%v)", err)
	}
	e.clk.Advance(2 * time.Hour) // 73 h
	if _, err = e.st.PurgeExpired(e.ctx()); err != nil || present(t1) {
		t.Fatalf("superseded token not pruned (%v)", err)
	}
	_, err = e.st.Rotate(e.ctx(), t1, ClientChromeAndroid)
	wantErr(t, err, ErrDeviceInvalid) // pruned: just invalid, no revoke
	e.renew(t3)
}

func TestDevicePurgeDevices(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{InactivityDays: 1})
	_, tokA := e.enrolled() // will expire
	// reuse-revoked device
	_, tokB := e.enrolled()
	e.renew(tokB)
	e.clk.Advance(3 * time.Minute)
	if _, err := e.st.Rotate(e.ctx(), tokB, ClientChromeAndroid); !errors.Is(err, ErrDeviceRevoked) {
		t.Fatal(err)
	}
	pend := e.issue(alice, dsLogin, "pending")              // expires, no notice
	pendN := e.issue(alice, dsLogin, "pending-with-notice") // expires, notice not yet redacted
	if err := e.st.AttachNotice(e.ctx(), pendN.Device.ID, roomMgmt, "$n"); err != nil {
		t.Fatal(err)
	}
	used := e.issue(alice, dsLogin, "used")
	en, err := e.st.Enroll(e.ctx(), used.Code, ClientOther)
	if err != nil {
		t.Fatal(err)
	}
	if err = e.st.RefreshCounts(e.ctx()); err != nil {
		t.Fatal(err)
	}
	if c := e.st.Counts(); c != (DeviceCounts{Pending: 2, Active: 2}) {
		t.Fatalf("counts %+v", c)
	}

	// 25 h: the pending device without a notice is gone, used code hashes are cleared.
	e.clk.Advance(23 * time.Hour)
	tokU := e.renew(en.Token)
	e.clk.Advance(2 * time.Hour)
	tokU = e.renew(tokU)
	st, err := e.st.PurgeExpired(e.ctx())
	if err != nil || st.Devices != 1 || st.Codes != 3 {
		t.Fatalf("purge %+v %v", st, err)
	}
	_, err = e.st.Peek(e.ctx(), pend.Code)
	wantErr(t, err, ErrLinkInvalid)
	_, err = e.st.Peek(e.ctx(), pendN.Code)
	wantErr(t, err, ErrLinkExpired)
	_, err = e.st.Peek(e.ctx(), used.Code)
	wantErr(t, err, ErrLinkInvalid) // after 24 h a used code is just invalid
	if c := e.st.Counts(); c != (DeviceCounts{Pending: 0, Active: 1}) {
		t.Fatalf("counts %+v", c)
	}

	// 31 days: the expired device and the reuse-revoked one are deleted; tokens with them.
	for range 31 {
		e.clk.Advance(day - time.Minute)
		tokU = e.renew(tokU)
	}
	if err = e.st.ClearNotice(e.ctx(), pendN.Device.ID, "$n"); err != nil {
		t.Fatal(err)
	}
	st, err = e.st.PurgeExpired(e.ctx())
	if err != nil || st.Devices != 3 {
		t.Fatalf("purge %+v %v", st, err)
	}
	_, err = e.st.Rotate(e.ctx(), tokA, ClientChromeAndroid)
	wantErr(t, err, ErrDeviceInvalid)
	_, err = e.st.Rotate(e.ctx(), tokB, ClientChromeAndroid)
	wantErr(t, err, ErrDeviceInvalid)
	l, _ := e.st.List(e.ctx(), alice)
	if len(l) != 1 || l[0].ID != en.Device.ID {
		t.Fatalf("left %+v", l)
	}
	if n := e.count(`SELECT COUNT(*) FROM mrtcsip_dialpad_token WHERE device_id<>$1`, en.Device.ID); n != 0 {
		t.Fatalf("%d orphan token rows", n)
	}
}

func TestDevicePurgeOrphans(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{})
	d1, tok1 := e.enrolled()
	p2 := e.issue(alice, dsLogin2, "")
	b := e.issue(bob, dsLoginB, "")
	calls := map[string]int{}
	var mu sync.Mutex
	got, err := e.st.PurgeOrphans(e.ctx(), func(_ context.Context, u id.UserID, l networkid.UserLoginID) (bool, error) {
		mu.Lock()
		calls[string(u)+"/"+string(l)]++
		mu.Unlock()
		switch l {
		case dsLogin:
			return false, nil // logged out
		case dsLoginB:
			return false, errors.New("db down") // kept
		default:
			return true, nil
		}
	})
	if err != nil || len(got) != 1 || got[0].ID != d1.ID {
		t.Fatalf("orphans %+v %v", got, err)
	}
	if len(calls) != 3 {
		t.Fatalf("calls %v", calls)
	}
	_, err = e.st.Rotate(e.ctx(), tok1, ClientChromeAndroid)
	wantErr(t, err, ErrDeviceInvalid)
	if e.tokenRows(d1.ID) != 0 {
		t.Fatal("orphan tokens left")
	}
	if l, _ := e.st.List(e.ctx(), alice); len(l) != 1 || l[0].ID != p2.Device.ID {
		t.Fatal("wrong device purged")
	}
	if l, _ := e.st.List(e.ctx(), bob); len(l) != 1 || l[0].ID != b.Device.ID {
		t.Fatal("device purged on lookup error")
	}
}

func TestDeviceNotices(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{})
	a := e.issue(alice, dsLogin, "a")
	b := e.issue(alice, dsLogin, "b")
	c := e.issue(alice, dsLogin, "c")
	for _, x := range []IssueResult{a, b, c} {
		if err := e.st.AttachNotice(e.ctx(), x.Device.ID, roomMgmt, id.EventID("$"+x.Device.ID)); err != nil {
			t.Fatal(err)
		}
	}
	wantErr(t, e.st.AttachNotice(e.ctx(), "zzzzzz", roomMgmt, "$x"), ErrDeviceNotFound)
	due, err := e.st.DueNotices(e.ctx())
	if err != nil || len(due) != 0 {
		t.Fatalf("due before use %+v %v", due, err)
	}
	en, err := e.st.Enroll(e.ctx(), a.Code, ClientOther)
	if err != nil || en.Device.NoticeEvent != id.EventID("$"+a.Device.ID) || en.Device.NoticeRoom != roomMgmt {
		t.Fatalf("enroll %+v %v", en.Device, err)
	}
	due, _ = e.st.DueNotices(e.ctx())
	if len(due) != 1 || due[0].DeviceID != a.Device.ID || due[0].Room != roomMgmt {
		t.Fatalf("due after enroll %+v", due)
	}
	if err = e.st.ClearNotice(e.ctx(), a.Device.ID, due[0].Event); err != nil {
		t.Fatal(err)
	}
	// Expiry makes b and c due; after a "restart" (new store) they are still due.
	e.clk.Advance(15 * time.Minute)
	st2 := e.newStore(dsBridge, DeviceStoreConfig{})
	due, _ = st2.DueNotices(e.ctx())
	if len(due) != 2 {
		t.Fatalf("due after expiry %+v", due)
	}
	// Failures: given up after 3 attempts.
	for i := 1; i <= 3; i++ {
		gave, err := st2.NoticeFailed(e.ctx(), b.Device.ID, id.EventID("$"+b.Device.ID))
		if err != nil || gave != (i == 3) {
			t.Fatalf("attempt %d gave=%v err=%v", i, gave, err)
		}
		if i < 3 {
			due, _ = st2.DueNotices(e.ctx())
			if len(due) != 2 || due[0].Attempts+due[1].Attempts != i {
				t.Fatalf("attempt %d due %+v", i, due)
			}
		}
	}
	due, _ = st2.DueNotices(e.ctx())
	if len(due) != 1 || due[0].DeviceID != c.Device.ID {
		t.Fatalf("due after give-up %+v", due)
	}
	// A stale event ID clears nothing.
	_ = st2.ClearNotice(e.ctx(), c.Device.ID, "$other")
	if due, _ = st2.DueNotices(e.ctx()); len(due) != 1 {
		t.Fatal("cleared with the wrong event")
	}
}

// ---- Privacy ----

// TestDeviceStoreHashesOnly checks that no secret (code, token or its random part) is stored in any
// column or appears in any log line, including dbutil's Trace query log with arguments.
func TestDeviceStoreHashesOnly(t *testing.T) {
	e := newDS(t, DeviceStoreConfig{})
	var secrets []string
	is := e.issue(alice, dsLogin, "Pixel")
	secrets = append(secrets, is.Code)
	if _, err := e.st.Peek(e.ctx(), is.Code); err != nil {
		t.Fatal(err)
	}
	en, err := e.st.Enroll(e.ctx(), is.Code, ClientChromeAndroid)
	if err != nil {
		t.Fatal(err)
	}
	secrets = append(secrets, en.Token)
	tok := en.Token
	for range 3 {
		r := e.rotate(tok)
		secrets = append(secrets, r.Next)
		e.confirm(r.Next)
		tok = r.Next
	}
	lost := e.rotate(tok).Next
	secrets = append(secrets, lost)
	e.clk.Advance(5 * time.Minute)
	_, _ = e.st.Rotate(e.ctx(), en.Token, ClientChromeAndroid) // reuse revoke
	is2 := e.issue(alice, dsLogin, "")
	secrets = append(secrets, is2.Code)

	var dump strings.Builder
	for _, tbl := range []string{"mrtcsip_dialpad_device", "mrtcsip_dialpad_token"} {
		rows, err := e.db.Query(e.ctx(), `SELECT * FROM `+tbl)
		if err != nil {
			t.Fatal(err)
		}
		cols, _ := rows.Columns()
		for rows.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err = rows.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			for _, v := range vals {
				if b, ok := v.([]byte); ok {
					v = string(b)
				}
				fmt.Fprintf(&dump, "%v\n", v)
			}
		}
		_ = rows.Close()
	}
	logs := e.logs()
	if !strings.Contains(logs, "query_args") {
		t.Fatal("query logging not captured")
	}
	if !strings.Contains(dump.String(), hashSecret(en.Token)) {
		t.Fatal("token hash not stored")
	}
	for _, s := range secrets {
		for _, part := range []string{s, s[12:], s[12:30]} {
			if strings.Contains(dump.String(), part) {
				t.Fatalf("secret stored in the database")
			}
			if strings.Contains(logs, part) {
				t.Fatalf("secret in the logs")
			}
		}
	}
}
