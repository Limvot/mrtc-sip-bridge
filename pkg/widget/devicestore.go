package widget

// Dialpad PWA device store (docs/PWA-SPEC.md §2.3-2.7, §8.2). This file is the only place that touches
// the device tables.
//
// API for the device handlers and the DialpadLinks implementation (P3):
//
//	st := NewDeviceStore(br.DB.Database, br.ID, DeviceStoreConfig{...}, clk, log)
//	st.Upgrade(ctx)                                  // creates/upgrades the tables, fills Counts
//
//	// dialpad-link / dialpad-links / dialpad-revoke
//	st.Issue(ctx, IssueParams{User, Login, Room, Name}) -> IssueResult{Device, Code}
//	                                                 // ErrDeviceLimit (*DeviceLimitError has the IDs),
//	                                                 // ErrDeviceCapacity, ErrDeviceName
//	st.AttachNotice(ctx, id, room, evt)              // the bot's link message, for redaction
//	st.List(ctx, user) -> []Device
//	st.Revoke(ctx, user, id|"all") -> []Device       // the deleted devices: kill their sessions and
//	                                                 // redact their NoticeRoom/NoticeEvent; ErrDeviceNotFound
//
//	// POST device/enroll
//	st.Peek(ctx, code) -> Device                     // confirm:false, no state change
//	st.Enroll(ctx, code, client) -> EnrollResult{Device, Token}
//	                                                 // ErrLinkInvalid, ErrLinkUsed, ErrLinkExpired
//
//	// POST device/session and device/confirm (§2.7)
//	st.Rotate(ctx, token, client) -> RotateResult{Device, Next, TokenState, Reuse, ClientChanged}
//	st.Confirm(ctx, token) -> ConfirmResult{Device, Repeat, Reuse}
//	                                                 // ErrDeviceInvalid, ErrDeviceExpired, ErrDeviceRevoked.
//	                                                 // Reuse == true (always with ErrDeviceRevoked) means the
//	                                                 // family was revoked by THIS call: Warn log, kill the
//	                                                 // device's sessions, notify Device.Room. It is reported
//	                                                 // exactly once per family.
//	st.DeviceOfToken(ctx, token) -> Device           // ErrDeviceInvalid; lookup only, no side effects (enroll previous_token)
//
//	// janitor
//	st.DueNotices(ctx) -> []DeviceNotice             // then ClearNotice on success, NoticeFailed on failure
//	st.PurgeExpired(ctx) -> PurgeStats               // hourly; also refreshes Counts
//	st.PurgeOrphans(ctx, exists) -> []Device         // hourly; deleted devices (kill sessions, redact)
//	st.Counts()                                      // cached, no I/O (for the gauge)
//
// Malformed codes and tokens (wrong prefix or length) are rejected before any query; ValidCode and
// ValidDeviceToken let handlers do the same check first. No method logs or returns a secret; the DB only
// ever sees sha256 hashes of them.
//
// Concurrency: every multi-statement operation runs in one transaction (db.DoTxn). Writers are
// additionally serialized in-process by a mutex, and token operations lock their device row first, so
// two concurrent rotations of one token, a rotation racing a confirm, or two redemptions of one code all
// see each other's committed results. With SQLite the bridge's DSN must use _txlock=immediate (mautrix
// requires it for pooled SQLite connections).

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/bridgev2/networkid"
	"maunium.net/go/mautrix/id"

	"github.com/williamkray/mrtc-sip-bridge/pkg/clock"
)

//go:embed devicestore_upgrades/*.sql
var deviceUpgradesFS embed.FS

var deviceUpgrades = dbutil.BuildUpgradeTable().WithFSPath(deviceUpgradesFS, "devicestore_upgrades").Finish()

// DeviceVersionTable is the version table of the device store's child database.
const DeviceVersionTable = "mrtcsip_dialpad_version"

// Secret formats (§2.2).
const (
	DeviceCodePrefix  = "mrtcsip_dpc_" // one-time enrollment code
	DeviceTokenPrefix = "mrtcsip_dpt_" // rotating device token
	deviceSecretBytes = 32
	deviceSecretLen   = 43 // base64url chars of 32 bytes
	deviceIDLen       = 6
	maxDeviceNameLen  = 32
)

// Defaults of DeviceStoreConfig (PWA-SPEC §8.1).
const (
	DefaultInactivityDays  = 90
	DefaultRotationGrace   = 120 * time.Second
	DefaultEnrollTTL       = 15 * time.Minute
	DefaultMaxLinksPerUser = 5
	DefaultMaxDevicesTotal = 1000
)

const (
	deviceRetention        = 30 * 24 * time.Hour // expired and reuse-revoked devices are kept this long
	codeHashRetention      = 24 * time.Hour      // used codes stay recognizable (link_used) this long
	pendingRetention       = 24 * time.Hour      // expired pending devices stay (link_expired) this long
	unconfirmedRetention   = 24 * time.Hour
	maxUnconfirmedChildren = 2
	clientNoticeInterval   = 24 * time.Hour
	maxNoticeAttempts      = 3
	dayMS                  = int64(24 * time.Hour / time.Millisecond)
)

// DeviceState is the state column of a device.
type DeviceState string

const (
	DeviceStatePending DeviceState = "pending" // issued, code not redeemed yet
	DeviceStateActive  DeviceState = "active"  // enrolled
	DeviceStateRevoked DeviceState = "revoked" // reuse revoke (other revokes delete the row)
)

// RevokedWhyReuse is Device.RevokedWhy after a reuse revoke (§2.7).
const RevokedWhyReuse = "reuse"

// Client enum (§2.6). Unknown values are stored as ClientOther.
const (
	ClientChromeAndroid  = "chrome_android"
	ClientFirefoxAndroid = "firefox_android"
	ClientSafariIOS      = "safari_ios"
	ClientChromeDesktop  = "chrome_desktop"
	ClientFirefoxDesktop = "firefox_desktop"
	ClientOther          = "other"
)

// TokenState classifies a presented device token (span attribute widget.device.token_state).
const (
	TokenStateCurrent     = "current"
	TokenStateUnconfirmed = "unconfirmed"
	TokenStateGrace       = "grace"
	tokenStateDead        = "dead"
)

var (
	// ErrDeviceLimit: the user has max_links_per_user pending or active devices. The error is a
	// *DeviceLimitError carrying their IDs.
	ErrDeviceLimit = errors.New("widget: dialpad link limit reached")
	// ErrDeviceCapacity: the bridge-wide device cap is reached.
	ErrDeviceCapacity = errors.New("widget: dialpad device capacity reached")
	// ErrDeviceName: the name is not 1-32 chars of letters, digits, space, '.', '_' or '-'.
	ErrDeviceName = errors.New("widget: invalid dialpad device name")

	ErrLinkInvalid = errors.New("widget: dialpad link invalid") // unknown, malformed or revoked code
	ErrLinkUsed    = errors.New("widget: dialpad link already used")
	ErrLinkExpired = errors.New("widget: dialpad link expired")

	ErrDeviceInvalid = errors.New("widget: dialpad device token invalid") // unknown/malformed token
	ErrDeviceExpired = errors.New("widget: dialpad device expired")
	ErrDeviceRevoked = errors.New("widget: dialpad device revoked")

	ErrDeviceNotFound = errors.New("widget: dialpad device not found") // Revoke/AttachNotice
)

// DeviceLimitError is the ErrDeviceLimit error; IDs are the user's pending and active devices.
type DeviceLimitError struct{ IDs []string }

func (e *DeviceLimitError) Error() string { return ErrDeviceLimit.Error() }
func (e *DeviceLimitError) Unwrap() error { return ErrDeviceLimit }

// DeviceStoreConfig is the store's part of the widget.pwa config. Zero values take the defaults.
type DeviceStoreConfig struct {
	InactivityDays  int           // device expires after this many days without use (default 90)
	MaxLifetimeDays int           // absolute lifetime after enrollment; 0 = none (default)
	RotationGrace   time.Duration // superseded tokens stay valid this long (default 120 s)
	EnrollTTL       time.Duration // enrollment code lifetime (default 15 min)
	MaxLinksPerUser int           // pending + active devices per user (default 5)
	MaxDevicesTotal int           // devices (any state) per bridge (default 1000)
}

func (c DeviceStoreConfig) withDefaults() DeviceStoreConfig {
	if c.InactivityDays <= 0 {
		c.InactivityDays = DefaultInactivityDays
	}
	if c.MaxLifetimeDays < 0 {
		c.MaxLifetimeDays = 0
	}
	if c.RotationGrace <= 0 {
		c.RotationGrace = DefaultRotationGrace
	}
	if c.EnrollTTL <= 0 {
		c.EnrollTTL = DefaultEnrollTTL
	}
	if c.MaxLinksPerUser <= 0 {
		c.MaxLinksPerUser = DefaultMaxLinksPerUser
	}
	if c.MaxDevicesTotal <= 0 {
		c.MaxDevicesTotal = DefaultMaxDevicesTotal
	}
	return c
}

// Device is one device (token family). Times are zero when unset.
type Device struct {
	ID          string
	State       DeviceState
	User        id.UserID
	Login       networkid.UserLoginID
	Room        id.RoomID // the management room the link was issued in (for notices)
	Name        string
	Client      string // client enum recorded at enrollment
	Created     time.Time
	CodeExpires time.Time // enrollment deadline
	Enrolled    time.Time
	LastUsed    time.Time
	HardExpiry  time.Time // absolute lifetime end stored at enrollment (informational; expiry uses Enrolled + MaxLifetimeDays)
	RevokedAt   time.Time
	RevokedWhy  string // RevokedWhyReuse for a reuse revoke
	NoticeRoom  id.RoomID
	NoticeEvent id.EventID // the bot's link message, until redacted

	// Computed at read time:
	Expired   bool      // pending: code expired; active: inactive too long or past the hard expiry
	ExpiresAt time.Time // pending: CodeExpires; active: when it expires if not used again
}

// IssueParams are the inputs of Issue. Name "" means "device-<id>".
type IssueParams struct {
	User  id.UserID
	Login networkid.UserLoginID
	Room  id.RoomID
	Name  string
}

// IssueResult is a new pending device and its one-time code (the only copy).
type IssueResult struct {
	Device Device
	Code   string
}

// EnrollResult is the enrolled device and its first token (already confirmed; the only copy).
type EnrollResult struct {
	Device Device
	Token  string
}

// RotateResult is the outcome of Rotate.
type RotateResult struct {
	Device     Device // set whenever the token's device was found (also with errors)
	Next       string // the unconfirmed successor token ("" on error)
	TokenState string // TokenStateCurrent, TokenStateUnconfirmed or TokenStateGrace on success
	// Reuse: this call detected reuse of a dead token and revoked the family (err is ErrDeviceRevoked).
	Reuse bool
	// ClientChanged: the client enum differs from the enrolled one and no client-change notice was due
	// for this device in the last 24 h; Client is the new value. The caller posts the notice.
	ClientChanged bool
	Client        string
}

// ConfirmResult is the outcome of Confirm.
type ConfirmResult struct {
	Device Device
	Repeat bool // the token was already confirmed (a retried confirm)
	Reuse  bool // as RotateResult.Reuse
}

// DeviceNotice is a link message due for redaction.
type DeviceNotice struct {
	DeviceID string
	Room     id.RoomID
	Event    id.EventID
	Attempts int // failed attempts so far
}

// PurgeStats counts what PurgeExpired removed.
type PurgeStats struct {
	Devices int // expired, reuse-revoked and stale pending devices deleted
	Tokens  int // token rows pruned (superseded past the inactivity window, stale unconfirmed)
	Codes   int // used code hashes cleared
}

// DeviceCounts are the cached gauge values (unexpired devices).
type DeviceCounts struct{ Pending, Active int }

// DeviceOwnerCheck reports whether a device's login still exists and belongs to user (and, if the
// host wants that, whether the user still has command permission). An error keeps the device.
type DeviceOwnerCheck func(ctx context.Context, user id.UserID, login networkid.UserLoginID) (bool, error)

// DeviceStore is the persistent store of Dialpad devices and their rotating tokens.
type DeviceStore struct {
	db       *dbutil.Database
	bridgeID networkid.BridgeID
	cfg      DeviceStoreConfig
	clk      clock.Clock
	log      zerolog.Logger

	mu sync.Mutex // serializes writers

	countsMu sync.Mutex
	counts   DeviceCounts
}

// NewDeviceStore returns the store on a child database of db (own version table, DeviceVersionTable).
// Call Upgrade before use. bridgeID keys every row (several bridges may share one database).
func NewDeviceStore(db *dbutil.Database, bridgeID networkid.BridgeID, cfg DeviceStoreConfig, clk clock.Clock, log zerolog.Logger) *DeviceStore {
	if clk == nil {
		clk = clock.Real()
	}
	return &DeviceStore{
		db:       db.Child(DeviceVersionTable, deviceUpgrades, dbutil.ZeroLogger(log)),
		bridgeID: bridgeID,
		cfg:      cfg.withDefaults(),
		clk:      clk,
		log:      log,
	}
}

// Upgrade creates or upgrades the tables and fills the cached counts.
func (s *DeviceStore) Upgrade(ctx context.Context) error {
	if err := s.db.Upgrade(ctx); err != nil {
		return fmt.Errorf("upgrade dialpad device store: %w", err)
	}
	return s.RefreshCounts(ctx)
}

// ---- Secrets ----

// ValidCode reports whether s has the enrollment code format (prefix + 43 base64url chars).
func ValidCode(s string) bool { return validSecret(s, DeviceCodePrefix) }

// ValidDeviceToken reports whether s has the device token format.
func ValidDeviceToken(s string) bool { return validSecret(s, DeviceTokenPrefix) }

func validSecret(s, prefix string) bool {
	return len(s) == len(prefix)+deviceSecretLen && strings.HasPrefix(s, prefix) && isB64URL(s[len(prefix):])
}

func newSecret(prefix string) (string, error) {
	var raw [deviceSecretBytes]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// hashSecret is the stored form of a code or token: hex sha256 of the whole string.
func hashSecret(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// hashEqual compares a stored hash with a computed one in constant time (defense in depth: rows are
// found by indexed equality on the hash already).
func hashEqual(stored, computed string) bool {
	return subtle.ConstantTimeCompare([]byte(stored), []byte(computed)) == 1
}

var deviceIDEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func newDeviceID() (string, error) {
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return deviceIDEncoding.EncodeToString(raw[:])[:deviceIDLen], nil
}

var deviceNameRE = regexp.MustCompile(`^[\p{L}\p{N} ._-]{1,32}$`)

// normalizeDeviceName trims name and validates it; "" is allowed (the caller substitutes a default).
func normalizeDeviceName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	if !deviceNameRE.MatchString(name) {
		return "", ErrDeviceName
	}
	return name, nil
}

func normalizeClient(c string) string {
	switch c {
	case ClientChromeAndroid, ClientFirefoxAndroid, ClientSafariIOS, ClientChromeDesktop, ClientFirefoxDesktop, ClientOther:
		return c
	default:
		return ClientOther
	}
}

// ---- Rows ----

const deviceCols = `id, state, code_hash, code_expires, user_mxid, login_id, room_id, name, client,
	client_notice_at, created_at, enrolled_at, last_used_at, hard_expiry, revoked_at, revoked_why,
	notice_room, notice_event, notice_tries`

// deviceColsD is deviceCols qualified with the table alias d (for joins).
var deviceColsD = func() string {
	cols := strings.Split(deviceCols, ",")
	for i, c := range cols {
		cols[i] = "d." + strings.TrimSpace(c)
	}
	return strings.Join(cols, ", ")
}()

type deviceRow struct {
	Device
	codeHash       sql.NullString
	clientNoticeAt sql.NullInt64
	noticeTries    int
	lastUsedMS     int64
}

func msToTime(v sql.NullInt64) time.Time {
	if !v.Valid {
		return time.Time{}
	}
	return time.UnixMilli(v.Int64)
}

type scanner interface{ Scan(...any) error }

func scanDevice(sc scanner) (*deviceRow, error) {
	var r deviceRow
	var state, user, login, room string
	var codeExp, created, enrolled, lastUsed, hard, revokedAt sql.NullInt64
	var revokedWhy, noticeRoom, noticeEvent sql.NullString
	err := sc.Scan(&r.ID, &state, &r.codeHash, &codeExp, &user, &login, &room, &r.Name, &r.Client,
		&r.clientNoticeAt, &created, &enrolled, &lastUsed, &hard, &revokedAt, &revokedWhy,
		&noticeRoom, &noticeEvent, &r.noticeTries)
	if err != nil {
		return nil, err
	}
	r.State = DeviceState(state)
	r.User = id.UserID(user)
	r.Login = networkid.UserLoginID(login)
	r.Room = id.RoomID(room)
	r.CodeExpires = msToTime(codeExp)
	r.Created = msToTime(created)
	r.Enrolled = msToTime(enrolled)
	r.LastUsed = msToTime(lastUsed)
	r.lastUsedMS = lastUsed.Int64
	r.HardExpiry = msToTime(hard)
	r.RevokedAt = msToTime(revokedAt)
	r.RevokedWhy = revokedWhy.String
	r.NoticeRoom = id.RoomID(noticeRoom.String)
	r.NoticeEvent = id.EventID(noticeEvent.String)
	return &r, nil
}

// expiry returns the unix ms at which the device expires (0 = never) and whether it has expired at now.
func (s *DeviceStore) expiry(r *deviceRow, now int64) (int64, bool) {
	switch r.State {
	case DeviceStatePending:
		exp := r.CodeExpires.UnixMilli()
		return exp, now >= exp
	case DeviceStateActive:
		exp := r.lastUsedMS + int64(s.cfg.InactivityDays)*dayMS
		expired := now > exp
		// The absolute lifetime is computed from the enrollment time and the current config, so
		// enabling or changing max_lifetime_days applies to devices enrolled before; the hard_expiry
		// column is informational only.
		if s.cfg.MaxLifetimeDays > 0 && !r.Enrolled.IsZero() {
			h := r.Enrolled.UnixMilli() + int64(s.cfg.MaxLifetimeDays)*dayMS
			if h < exp {
				exp = h
			}
			expired = expired || now > h
		}
		return exp, expired
	default:
		return 0, false
	}
}

// finish fills the computed fields and returns the public Device.
func (s *DeviceStore) finish(r *deviceRow, now int64) Device {
	d := r.Device
	exp, expired := s.expiry(r, now)
	d.Expired = expired
	if exp != 0 {
		d.ExpiresAt = time.UnixMilli(exp)
	}
	return d
}

type tokenRow struct {
	hash       string
	device     string
	parent     sql.NullString
	seq        int64
	issued     int64
	confirmed  sql.NullInt64
	superseded sql.NullInt64
}

const tokenCols = `token_hash, device_id, parent_hash, seq, issued_at, confirmed_at, superseded_at`

func scanToken(sc scanner) (*tokenRow, error) {
	var t tokenRow
	err := sc.Scan(&t.hash, &t.device, &t.parent, &t.seq, &t.issued, &t.confirmed, &t.superseded)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// classify returns the token's state at now (§2.7).
func (s *DeviceStore) classify(t *tokenRow, now int64) string {
	switch {
	case !t.confirmed.Valid:
		return TokenStateUnconfirmed
	case !t.superseded.Valid:
		return TokenStateCurrent
	case now-t.superseded.Int64 < s.cfg.RotationGrace.Milliseconds():
		return TokenStateGrace
	default:
		return tokenStateDead
	}
}

func (s *DeviceStore) now() int64 { return s.clk.Now().UnixMilli() }

func nullMS(v int64) sql.NullInt64 { return sql.NullInt64{Int64: v, Valid: v != 0} }

func (s *DeviceStore) getDevice(ctx context.Context, devID string) (*deviceRow, error) {
	r, err := scanDevice(s.db.QueryRow(ctx,
		`SELECT `+deviceCols+` FROM mrtcsip_dialpad_device WHERE bridge_id=$1 AND id=$2`, s.bridgeID, devID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func (s *DeviceStore) getToken(ctx context.Context, hash string) (*tokenRow, error) {
	t, err := scanToken(s.db.QueryRow(ctx,
		`SELECT `+tokenCols+` FROM mrtcsip_dialpad_token WHERE bridge_id=$1 AND token_hash=$2`, s.bridgeID, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

func (s *DeviceStore) getDeviceByCode(ctx context.Context, codeHash string) (*deviceRow, error) {
	r, err := scanDevice(s.db.QueryRow(ctx,
		`SELECT `+deviceCols+` FROM mrtcsip_dialpad_device WHERE bridge_id=$1 AND code_hash=$2`, s.bridgeID, codeHash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err == nil && !hashEqual(r.codeHash.String, codeHash) {
		return nil, nil
	}
	return r, err
}

// ---- Issue, list, revoke ----

// Issue creates a pending device bound to (user, login) and returns its one-time code. The per-user
// limit counts the user's unexpired pending and active devices (all logins).
func (s *DeviceStore) Issue(ctx context.Context, p IssueParams) (IssueResult, error) {
	if p.User == "" || p.Login == "" {
		return IssueResult{}, errors.New("widget: Issue needs a user and a login")
	}
	name, err := normalizeDeviceName(p.Name)
	if err != nil {
		return IssueResult{}, err
	}
	code, err := newSecret(DeviceCodePrefix)
	if err != nil {
		return IssueResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var res IssueResult
	err = s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		now := s.now()
		var total int
		if err := s.db.QueryRow(ctx, `SELECT COUNT(*) FROM mrtcsip_dialpad_device WHERE bridge_id=$1`,
			s.bridgeID).Scan(&total); err != nil {
			return err
		}
		if total >= s.cfg.MaxDevicesTotal {
			return ErrDeviceCapacity
		}
		mine, err := s.listUser(ctx, p.User, now)
		if err != nil {
			return err
		}
		var live []string
		for _, d := range mine {
			if (d.State == DeviceStatePending || d.State == DeviceStateActive) && !d.Expired {
				live = append(live, d.ID)
			}
		}
		if len(live) >= s.cfg.MaxLinksPerUser {
			return &DeviceLimitError{IDs: live}
		}
		devID, err := s.uniqueID(ctx)
		if err != nil {
			return err
		}
		n := name
		if n == "" {
			n = "device-" + devID
		}
		exp := now + s.cfg.EnrollTTL.Milliseconds()
		_, err = s.db.Exec(ctx, `INSERT INTO mrtcsip_dialpad_device
			(bridge_id, id, state, code_hash, code_expires, user_mxid, login_id, room_id, name, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			s.bridgeID, devID, string(DeviceStatePending), hashSecret(code), exp, string(p.User),
			string(p.Login), string(p.Room), n, now)
		if err != nil {
			return err
		}
		r, err := s.getDevice(ctx, devID)
		if err != nil || r == nil {
			return errors.Join(errors.New("widget: issued device vanished"), err)
		}
		res = IssueResult{Device: s.finish(r, now), Code: code}
		return nil
	})
	if err != nil {
		return IssueResult{}, err
	}
	return res, nil
}

func (s *DeviceStore) uniqueID(ctx context.Context) (string, error) {
	for range 16 {
		devID, err := newDeviceID()
		if err != nil {
			return "", err
		}
		var n int
		if err = s.db.QueryRow(ctx, `SELECT COUNT(*) FROM mrtcsip_dialpad_device WHERE bridge_id=$1 AND id=$2`,
			s.bridgeID, devID).Scan(&n); err != nil {
			return "", err
		}
		if n == 0 {
			return devID, nil
		}
	}
	return "", errors.New("widget: no free dialpad device id")
}

func (s *DeviceStore) listUser(ctx context.Context, user id.UserID, now int64) ([]Device, error) {
	rows, err := s.db.Query(ctx, `SELECT `+deviceCols+` FROM mrtcsip_dialpad_device
		WHERE bridge_id=$1 AND user_mxid=$2 ORDER BY created_at, id`, s.bridgeID, string(user))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		r, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s.finish(r, now))
	}
	return out, rows.Err()
}

// List returns all of user's devices (every login and state, including expired and reuse-revoked
// ones), oldest first.
func (s *DeviceStore) List(ctx context.Context, user id.UserID) ([]Device, error) {
	return s.listUser(ctx, user, s.now())
}

// Revoke deletes user's device devID (or all of user's devices for devID "all") with all its tokens
// and returns the deleted devices; the caller kills their sessions and redacts pending notices. A
// device of another user is ErrDeviceNotFound; "all" with no devices returns none and no error.
func (s *DeviceStore) Revoke(ctx context.Context, user id.UserID, devID string) ([]Device, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Device
	err := s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		now := s.now()
		mine, err := s.listUser(ctx, user, now)
		if err != nil {
			return err
		}
		for _, d := range mine {
			if devID == "all" || d.ID == devID {
				out = append(out, d)
			}
		}
		if len(out) == 0 && devID != "all" {
			return ErrDeviceNotFound
		}
		for _, d := range out {
			if err := s.deleteDevice(ctx, d.ID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// deleteDevice deletes a device and its tokens (explicitly: SQLite only cascades with foreign keys on).
func (s *DeviceStore) deleteDevice(ctx context.Context, devID string) error {
	if _, err := s.db.Exec(ctx, `DELETE FROM mrtcsip_dialpad_token WHERE bridge_id=$1 AND device_id=$2`,
		s.bridgeID, devID); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `DELETE FROM mrtcsip_dialpad_device WHERE bridge_id=$1 AND id=$2`, s.bridgeID, devID)
	return err
}

// ---- Enrollment ----

func (s *DeviceStore) codeError(r *deviceRow, now int64) error {
	switch {
	case r == nil || r.State == DeviceStateRevoked:
		return ErrLinkInvalid
	case r.State == DeviceStateActive:
		return ErrLinkUsed
	case now >= r.CodeExpires.UnixMilli():
		return ErrLinkExpired
	default:
		return nil
	}
}

// Peek returns the pending device of code without changing anything (enroll with confirm:false).
func (s *DeviceStore) Peek(ctx context.Context, code string) (Device, error) {
	if !ValidCode(code) {
		return Device{}, ErrLinkInvalid
	}
	now := s.now()
	r, err := s.getDeviceByCode(ctx, hashSecret(code))
	if err != nil {
		return Device{}, err
	}
	if err = s.codeError(r, now); err != nil {
		return Device{}, err
	}
	return s.finish(r, now), nil
}

// Enroll redeems code (single use, race-free) and returns the device with its first token, which is
// already confirmed. client is the §2.6 enum (unknown values become ClientOther).
func (s *DeviceStore) Enroll(ctx context.Context, code, client string) (EnrollResult, error) {
	if !ValidCode(code) {
		return EnrollResult{}, ErrLinkInvalid
	}
	tok, err := newSecret(DeviceTokenPrefix)
	if err != nil {
		return EnrollResult{}, err
	}
	codeHash := hashSecret(code)
	client = normalizeClient(client)
	s.mu.Lock()
	defer s.mu.Unlock()
	var res EnrollResult
	err = s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		now := s.now()
		var hard int64
		if s.cfg.MaxLifetimeDays > 0 {
			hard = now + int64(s.cfg.MaxLifetimeDays)*dayMS
		}
		r, err := s.db.Exec(ctx, `UPDATE mrtcsip_dialpad_device
			SET state=$1, enrolled_at=$2, last_used_at=$2, hard_expiry=$3, client=$4
			WHERE bridge_id=$5 AND code_hash=$6 AND state=$7 AND code_expires>$2`,
			string(DeviceStateActive), now, nullMS(hard), client, s.bridgeID, codeHash, string(DeviceStatePending))
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return err
		}
		dev, err := s.getDeviceByCode(ctx, codeHash)
		if err != nil {
			return err
		}
		if n != 1 || dev == nil {
			if cerr := s.codeError(dev, now); cerr != nil {
				return cerr
			}
			return ErrLinkInvalid
		}
		_, err = s.db.Exec(ctx, `INSERT INTO mrtcsip_dialpad_token
			(bridge_id, token_hash, device_id, parent_hash, seq, issued_at, confirmed_at)
			VALUES ($1, $2, $3, NULL, 1, $4, $4)`, s.bridgeID, hashSecret(tok), dev.ID, now)
		if err != nil {
			return err
		}
		res = EnrollResult{Device: s.finish(dev, now), Token: tok}
		return nil
	})
	if err != nil {
		return EnrollResult{}, err
	}
	return res, nil
}

// ---- Rotation (§2.7) ----

// lockToken locks the device of token hash h and returns the token and device rows; ErrDeviceInvalid
// when either is missing. Later statements of the transaction see every committed change.
func (s *DeviceStore) lockToken(ctx context.Context, h string) (*tokenRow, *deviceRow, error) {
	// The no-op UPDATE takes the device's row lock (Postgres) or the write lock (SQLite) first, so
	// concurrent operations on one family are serialized even across processes.
	if _, err := s.db.Exec(ctx, `UPDATE mrtcsip_dialpad_device SET state=state WHERE bridge_id=$1 AND id=
		(SELECT device_id FROM mrtcsip_dialpad_token WHERE bridge_id=$1 AND token_hash=$2)`, s.bridgeID, h); err != nil {
		return nil, nil, err
	}
	t, err := s.getToken(ctx, h)
	if err != nil {
		return nil, nil, err
	}
	if t == nil || !hashEqual(t.hash, h) {
		return nil, nil, ErrDeviceInvalid
	}
	d, err := s.getDevice(ctx, t.device)
	if err != nil {
		return nil, nil, err
	}
	if d == nil {
		return nil, nil, ErrDeviceInvalid
	}
	return t, d, nil
}

// usable checks the device state in §2.7 step 1 order: revoked, then expired.
func (s *DeviceStore) usable(d *deviceRow, now int64) error {
	switch d.State {
	case DeviceStateRevoked:
		return ErrDeviceRevoked
	case DeviceStateActive:
		if _, expired := s.expiry(d, now); expired {
			return ErrDeviceExpired
		}
		return nil
	default:
		return ErrDeviceInvalid
	}
}

// revokeReuse revokes the family after reuse of a dead token: the device row stays (state revoked,
// reason reuse) with all token hashes, every token marked superseded, so any later presentation of a
// family token answers device_revoked; PurgeExpired deletes it after 30 days.
func (s *DeviceStore) revokeReuse(ctx context.Context, d *deviceRow, now int64) error {
	if _, err := s.db.Exec(ctx, `UPDATE mrtcsip_dialpad_device SET state=$1, revoked_at=$2, revoked_why=$3
		WHERE bridge_id=$4 AND id=$5`, string(DeviceStateRevoked), now, RevokedWhyReuse, s.bridgeID, d.ID); err != nil {
		return err
	}
	if _, err := s.db.Exec(ctx, `UPDATE mrtcsip_dialpad_token SET superseded_at=$1
		WHERE bridge_id=$2 AND device_id=$3 AND superseded_at IS NULL`, now, s.bridgeID, d.ID); err != nil {
		return err
	}
	d.State = DeviceStateRevoked
	d.RevokedAt = time.UnixMilli(now)
	d.RevokedWhy = RevokedWhyReuse
	return nil
}

// confirmToken marks an unconfirmed token confirmed and supersedes every other current token of the
// device (its parent and any sibling branch; their grace starts now), so the device has exactly one
// current token afterwards. A fork created inside a grace window therefore cannot persist: the losing
// branch goes to grace, then dead, and presenting it after that is reuse.
func (s *DeviceStore) confirmToken(ctx context.Context, t *tokenRow, now int64) error {
	if _, err := s.db.Exec(ctx, `UPDATE mrtcsip_dialpad_token SET confirmed_at=$1
		WHERE bridge_id=$2 AND token_hash=$3 AND confirmed_at IS NULL`, now, s.bridgeID, t.hash); err != nil {
		return err
	}
	t.confirmed = sql.NullInt64{Int64: now, Valid: true}
	_, err := s.db.Exec(ctx, `UPDATE mrtcsip_dialpad_token SET superseded_at=$1
		WHERE bridge_id=$2 AND device_id=$3 AND confirmed_at IS NOT NULL AND superseded_at IS NULL
		AND token_hash<>$4`, now, s.bridgeID, t.device, t.hash)
	return err
}

// unconfirmedParentDead reports whether the parent of the unconfirmed token t is dead (§2.7): an
// unconfirmed child is only usable while its parent is current or in grace, otherwise a thief who
// rotated once could confirm the child later and fork the family undetected. A missing (pruned) parent
// is ErrDeviceInvalid.
func (s *DeviceStore) unconfirmedParentDead(ctx context.Context, t *tokenRow, now int64) (bool, error) {
	if !t.parent.Valid {
		return false, nil
	}
	p, err := s.getToken(ctx, t.parent.String)
	if err != nil {
		return false, err
	}
	if p == nil {
		return false, ErrDeviceInvalid
	}
	return s.classify(p, now) == tokenStateDead, nil
}

// Rotate exchanges token for an unconfirmed successor (POST device/session, §2.7). The presented token
// stays valid; only Confirm of the successor (or presenting it) supersedes it. Errors:
// ErrDeviceInvalid, ErrDeviceExpired, ErrDeviceRevoked (with Reuse set when this call detected the
// reuse). client is the enum of this request (unknown values become ClientOther).
func (s *DeviceStore) Rotate(ctx context.Context, token, client string) (RotateResult, error) {
	if !ValidDeviceToken(token) {
		return RotateResult{}, ErrDeviceInvalid
	}
	h := hashSecret(token)
	next, err := newSecret(DeviceTokenPrefix)
	if err != nil {
		return RotateResult{}, err
	}
	client = normalizeClient(client)
	s.mu.Lock()
	defer s.mu.Unlock()
	var res RotateResult
	err = s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		res = RotateResult{}
		now := s.now()
		t, d, err := s.lockToken(ctx, h)
		if err != nil {
			return err
		}
		res.Device = s.finish(d, now)
		if err = s.usable(d, now); err != nil {
			return err
		}
		state := s.classify(t, now)
		switch state {
		case tokenStateDead:
			if err = s.revokeReuse(ctx, d, now); err != nil {
				return err
			}
			res.Device = s.finish(d, now)
			res.Reuse = true
			return nil // commit the revoke; reported as ErrDeviceRevoked below
		case TokenStateUnconfirmed:
			// The client stored this successor but its confirm was lost: presenting it confirms it,
			// unless its parent is dead (another branch won long ago: reuse).
			dead, err := s.unconfirmedParentDead(ctx, t, now)
			if err != nil {
				return err
			}
			if dead {
				if err = s.revokeReuse(ctx, d, now); err != nil {
					return err
				}
				res.Device = s.finish(d, now)
				res.Reuse = true
				return nil
			}
			if err = s.confirmToken(ctx, t, now); err != nil {
				return err
			}
		}
		var seq int64
		if err = s.db.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM mrtcsip_dialpad_token
			WHERE bridge_id=$1 AND device_id=$2`, s.bridgeID, d.ID).Scan(&seq); err != nil {
			return err
		}
		if _, err = s.db.Exec(ctx, `INSERT INTO mrtcsip_dialpad_token
			(bridge_id, token_hash, device_id, parent_hash, seq, issued_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, s.bridgeID, hashSecret(next), d.ID, t.hash, seq, now); err != nil {
			return err
		}
		// Keep only the newest maxUnconfirmedChildren unconfirmed children of t (lost responses).
		if _, err = s.db.Exec(ctx, `DELETE FROM mrtcsip_dialpad_token
			WHERE bridge_id=$1 AND parent_hash=$2 AND confirmed_at IS NULL AND seq NOT IN
			(SELECT seq FROM mrtcsip_dialpad_token WHERE bridge_id=$1 AND parent_hash=$2 AND confirmed_at IS NULL
			 ORDER BY seq DESC LIMIT $3)`, s.bridgeID, t.hash, maxUnconfirmedChildren); err != nil {
			return err
		}
		if client != d.Client && d.Client != "" &&
			(!d.clientNoticeAt.Valid || now-d.clientNoticeAt.Int64 >= clientNoticeInterval.Milliseconds()) {
			res.ClientChanged = true
			d.clientNoticeAt = sql.NullInt64{Int64: now, Valid: true}
		}
		res.Client = client
		if _, err = s.db.Exec(ctx, `UPDATE mrtcsip_dialpad_device SET last_used_at=$1, client_notice_at=$2
			WHERE bridge_id=$3 AND id=$4`, now, d.clientNoticeAt, s.bridgeID, d.ID); err != nil {
			return err
		}
		d.LastUsed, d.lastUsedMS = time.UnixMilli(now), now
		res.Device = s.finish(d, now)
		res.Next = next
		res.TokenState = state
		return nil
	})
	switch {
	case err != nil:
		res.Next, res.TokenState, res.ClientChanged, res.Client = "", "", false, ""
		return res, err
	case res.Reuse:
		return res, ErrDeviceRevoked
	default:
		return res, nil
	}
}

// Confirm confirms a successor token (POST device/confirm, §2.7); idempotent. An unconfirmed token is
// confirmed and supersedes its parent; an already confirmed token (current or in grace) is a Repeat.
// A dead token is reuse; errors as Rotate.
func (s *DeviceStore) Confirm(ctx context.Context, token string) (ConfirmResult, error) {
	if !ValidDeviceToken(token) {
		return ConfirmResult{}, ErrDeviceInvalid
	}
	h := hashSecret(token)
	s.mu.Lock()
	defer s.mu.Unlock()
	var res ConfirmResult
	err := s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		res = ConfirmResult{}
		now := s.now()
		t, d, err := s.lockToken(ctx, h)
		if err != nil {
			return err
		}
		res.Device = s.finish(d, now)
		if err = s.usable(d, now); err != nil {
			return err
		}
		switch s.classify(t, now) {
		case tokenStateDead:
			if err = s.revokeReuse(ctx, d, now); err != nil {
				return err
			}
			res.Device = s.finish(d, now)
			res.Reuse = true
			return nil
		case TokenStateUnconfirmed:
			dead, err := s.unconfirmedParentDead(ctx, t, now)
			if err != nil {
				return err
			}
			if dead {
				if err = s.revokeReuse(ctx, d, now); err != nil {
					return err
				}
				res.Device = s.finish(d, now)
				res.Reuse = true
				return nil
			}
			if err = s.confirmToken(ctx, t, now); err != nil {
				return err
			}
		default:
			res.Repeat = true
		}
		if _, err = s.db.Exec(ctx, `UPDATE mrtcsip_dialpad_device SET last_used_at=$1 WHERE bridge_id=$2 AND id=$3`,
			now, s.bridgeID, d.ID); err != nil {
			return err
		}
		d.LastUsed, d.lastUsedMS = time.UnixMilli(now), now
		res.Device = s.finish(d, now)
		return nil
	})
	switch {
	case err != nil:
		res.Repeat = false
		return res, err
	case res.Reuse:
		return res, ErrDeviceRevoked
	default:
		return res, nil
	}
}

// DeviceOfToken returns the device of token whatever the token's state or the device's state, without
// changing anything (no confirm, no reuse detection, no last-use update). Malformed or unknown tokens
// are ErrDeviceInvalid.
func (s *DeviceStore) DeviceOfToken(ctx context.Context, token string) (Device, error) {
	if !ValidDeviceToken(token) {
		return Device{}, ErrDeviceInvalid
	}
	h := hashSecret(token)
	r, err := scanDevice(s.db.QueryRow(ctx, `SELECT `+deviceColsD+`
		FROM mrtcsip_dialpad_token t JOIN mrtcsip_dialpad_device d ON d.bridge_id=t.bridge_id AND d.id=t.device_id
		WHERE t.bridge_id=$1 AND t.token_hash=$2`, s.bridgeID, h))
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrDeviceInvalid
	}
	if err != nil {
		return Device{}, err
	}
	return s.finish(r, s.now()), nil
}

// ---- Link notices (§4.2) ----

// AttachNotice records the bot's link message of device devID for redaction.
func (s *DeviceStore) AttachNotice(ctx context.Context, devID string, room id.RoomID, evt id.EventID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, err := s.db.Exec(ctx, `UPDATE mrtcsip_dialpad_device SET notice_room=$1, notice_event=$2, notice_tries=0
		WHERE bridge_id=$3 AND id=$4`, string(room), string(evt), s.bridgeID, devID)
	if err != nil {
		return err
	}
	if n, err := r.RowsAffected(); err != nil {
		return err
	} else if n == 0 {
		return ErrDeviceNotFound
	}
	return nil
}

// DueNotices returns the link messages to redact now: their code was used (or the device revoked
// for reuse) or has expired.
func (s *DeviceStore) DueNotices(ctx context.Context) ([]DeviceNotice, error) {
	rows, err := s.db.Query(ctx, `SELECT id, notice_room, notice_event, notice_tries FROM mrtcsip_dialpad_device
		WHERE bridge_id=$1 AND notice_event IS NOT NULL AND (state<>$2 OR code_expires<=$3) ORDER BY id`,
		s.bridgeID, string(DeviceStatePending), s.now())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceNotice
	for rows.Next() {
		var n DeviceNotice
		var room, evt string
		if err = rows.Scan(&n.DeviceID, &room, &evt, &n.Attempts); err != nil {
			return nil, err
		}
		n.Room, n.Event = id.RoomID(room), id.EventID(evt)
		out = append(out, n)
	}
	return out, rows.Err()
}

// ClearNotice forgets the link message evt of devID (after it was redacted).
func (s *DeviceStore) ClearNotice(ctx context.Context, devID string, evt id.EventID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(ctx, `UPDATE mrtcsip_dialpad_device SET notice_room=NULL, notice_event=NULL, notice_tries=0
		WHERE bridge_id=$1 AND id=$2 AND notice_event=$3`, s.bridgeID, devID, string(evt))
	return err
}

// NoticeFailed records a failed redaction of evt. After maxNoticeAttempts (3) failures the notice is
// forgotten and gaveUp is true.
func (s *DeviceStore) NoticeFailed(ctx context.Context, devID string, evt id.EventID) (gaveUp bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	err = s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		if _, err := s.db.Exec(ctx, `UPDATE mrtcsip_dialpad_device SET notice_tries=notice_tries+1
			WHERE bridge_id=$1 AND id=$2 AND notice_event=$3`, s.bridgeID, devID, string(evt)); err != nil {
			return err
		}
		r, err := s.db.Exec(ctx, `UPDATE mrtcsip_dialpad_device SET notice_room=NULL, notice_event=NULL, notice_tries=0
			WHERE bridge_id=$1 AND id=$2 AND notice_event=$3 AND notice_tries>=$4`,
			s.bridgeID, devID, string(evt), maxNoticeAttempts)
		if err != nil {
			return err
		}
		n, err := r.RowsAffected()
		gaveUp = n > 0
		return err
	})
	return gaveUp, err
}

// ---- Janitor ----

// PurgeExpired deletes expired devices 30 days after expiry, reuse-revoked devices 30 days after the
// revoke, and expired pending devices 24 h after their code expired once their link message is gone;
// clears used code hashes after 24 h; prunes superseded tokens inactivity_days after they were
// superseded and unconfirmed tokens older than 24 h whose parent is no longer current (a sibling won).
// Unconfirmed children of a still-current parent are kept: they may be the client's stored token whose
// confirm was lost, and there are at most 2 per parent. Refreshes Counts.
func (s *DeviceStore) PurgeExpired(ctx context.Context) (PurgeStats, error) {
	s.mu.Lock()
	var st PurgeStats
	err := s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		st = PurgeStats{}
		now := s.now()
		retain := deviceRetention.Milliseconds()
		inactive := int64(s.cfg.InactivityDays) * dayMS
		var dead []string
		rows, err := s.db.Query(ctx, `SELECT `+deviceCols+` FROM mrtcsip_dialpad_device WHERE bridge_id=$1`, s.bridgeID)
		if err != nil {
			return err
		}
		for rows.Next() {
			r, err := scanDevice(rows)
			if err != nil {
				_ = rows.Close()
				return err
			}
			exp, expired := s.expiry(r, now)
			switch r.State {
			case DeviceStateActive:
				if expired && now-exp > retain {
					dead = append(dead, r.ID)
				}
			case DeviceStateRevoked:
				if now-r.RevokedAt.UnixMilli() > retain {
					dead = append(dead, r.ID)
				}
			case DeviceStatePending:
				if r.NoticeEvent == "" && now-exp > pendingRetention.Milliseconds() {
					dead = append(dead, r.ID)
				}
			}
		}
		if err = errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for _, devID := range dead {
			if err = s.deleteDevice(ctx, devID); err != nil {
				return err
			}
		}
		st.Devices = len(dead)
		res, err := s.db.Exec(ctx, `UPDATE mrtcsip_dialpad_device SET code_hash=NULL
			WHERE bridge_id=$1 AND state<>$2 AND code_hash IS NOT NULL AND enrolled_at<$3`,
			s.bridgeID, string(DeviceStatePending), now-codeHashRetention.Milliseconds())
		if err != nil {
			return err
		}
		st.Codes = rowsAffected(res)
		res, err = s.db.Exec(ctx, `DELETE FROM mrtcsip_dialpad_token
			WHERE bridge_id=$1 AND superseded_at IS NOT NULL AND superseded_at<$2 AND device_id IN
			(SELECT id FROM mrtcsip_dialpad_device WHERE bridge_id=$1 AND state=$3)`,
			s.bridgeID, now-inactive, string(DeviceStateActive))
		if err != nil {
			return err
		}
		st.Tokens = rowsAffected(res)
		res, err = s.db.Exec(ctx, `DELETE FROM mrtcsip_dialpad_token
			WHERE bridge_id=$1 AND confirmed_at IS NULL AND issued_at<$2
			AND device_id IN (SELECT id FROM mrtcsip_dialpad_device WHERE bridge_id=$1 AND state=$3)
			AND NOT EXISTS (SELECT 1 FROM mrtcsip_dialpad_token p WHERE p.bridge_id=$1
				AND p.token_hash=mrtcsip_dialpad_token.parent_hash AND p.superseded_at IS NULL)`,
			s.bridgeID, now-unconfirmedRetention.Milliseconds(), string(DeviceStateActive))
		if err != nil {
			return err
		}
		st.Tokens += rowsAffected(res)
		return nil
	})
	s.mu.Unlock()
	if err != nil {
		return PurgeStats{}, err
	}
	return st, s.RefreshCounts(ctx)
}

func rowsAffected(r sql.Result) int {
	n, err := r.RowsAffected()
	if err != nil {
		return 0
	}
	return int(n)
}

// PurgeOrphans deletes the devices whose (user, login) exists reports false for and returns them (the
// caller kills their sessions and redacts pending notices). exists is called outside any transaction,
// once per distinct (user, login); an error keeps that pair's devices.
func (s *DeviceStore) PurgeOrphans(ctx context.Context, exists DeviceOwnerCheck) ([]Device, error) {
	type pair struct {
		user  id.UserID
		login networkid.UserLoginID
	}
	rows, err := s.db.Query(ctx, `SELECT DISTINCT user_mxid, login_id FROM mrtcsip_dialpad_device WHERE bridge_id=$1`,
		s.bridgeID)
	if err != nil {
		return nil, err
	}
	var pairs []pair
	for rows.Next() {
		var u, l string
		if err = rows.Scan(&u, &l); err != nil {
			_ = rows.Close()
			return nil, err
		}
		pairs = append(pairs, pair{id.UserID(u), networkid.UserLoginID(l)})
	}
	if err = errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	var gone []pair
	for _, p := range pairs {
		ok, err := exists(ctx, p.user, p.login)
		if err != nil {
			s.log.Debug().Err(err).Str("user", p.user.String()).Msg("Dialpad orphan check failed, keeping devices")
			continue
		}
		if !ok {
			gone = append(gone, p)
		}
	}
	if len(gone) == 0 {
		return nil, nil
	}
	s.mu.Lock()
	var out []Device
	err = s.db.DoTxn(ctx, nil, func(ctx context.Context) error {
		out = nil
		now := s.now()
		for _, p := range gone {
			rows, err := s.db.Query(ctx, `SELECT `+deviceCols+` FROM mrtcsip_dialpad_device
				WHERE bridge_id=$1 AND user_mxid=$2 AND login_id=$3`, s.bridgeID, string(p.user), string(p.login))
			if err != nil {
				return err
			}
			for rows.Next() {
				r, err := scanDevice(rows)
				if err != nil {
					_ = rows.Close()
					return err
				}
				out = append(out, s.finish(r, now))
			}
			if err = errors.Join(rows.Err(), rows.Close()); err != nil {
				return err
			}
		}
		for _, d := range out {
			if err := s.deleteDevice(ctx, d.ID); err != nil {
				return err
			}
		}
		return nil
	})
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return out, s.RefreshCounts(ctx)
}

// RefreshCounts recomputes the cached Counts.
func (s *DeviceStore) RefreshCounts(ctx context.Context) error {
	now := s.now()
	rows, err := s.db.Query(ctx, `SELECT `+deviceCols+` FROM mrtcsip_dialpad_device WHERE bridge_id=$1 AND state<>$2`,
		s.bridgeID, string(DeviceStateRevoked))
	if err != nil {
		return err
	}
	defer rows.Close()
	var c DeviceCounts
	for rows.Next() {
		r, err := scanDevice(rows)
		if err != nil {
			return err
		}
		if _, expired := s.expiry(r, now); expired {
			continue
		}
		switch r.State {
		case DeviceStatePending:
			c.Pending++
		case DeviceStateActive:
			c.Active++
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	s.countsMu.Lock()
	s.counts = c
	s.countsMu.Unlock()
	return nil
}

// Counts returns the cached unexpired device counts (refreshed by Upgrade, PurgeExpired, PurgeOrphans
// and RefreshCounts). No I/O.
func (s *DeviceStore) Counts() DeviceCounts {
	s.countsMu.Lock()
	defer s.countsMu.Unlock()
	return s.counts
}
