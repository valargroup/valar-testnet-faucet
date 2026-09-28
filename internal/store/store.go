// Package store persists faucet claims in SQLite and enforces the payout limits.
//
// Every limit is checked and the claim inserted inside one transaction on a single
// connection, so concurrent requests can never both pass a check that only one of them
// should.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Status is a claim's position in its lifecycle.
type Status string

const (
	// StatusQueued: accepted, waiting for the payout worker.
	StatusQueued Status = "queued"
	// StatusSending: the worker has asked zecd to send; the outcome is not yet recorded.
	StatusSending Status = "sending"
	// StatusSent: zecd returned a txid.
	StatusSent Status = "sent"
	// StatusFailed: definitively not paid. Failed claims do not count against any limit.
	StatusFailed Status = "failed"
	// StatusReview: the send outcome is unknown (crash or ambiguous error mid-send). It
	// counts against limits and is never retried automatically.
	StatusReview Status = "review"
)

// Claim is one payout request.
type Claim struct {
	ID          string
	IPKey       string
	Address     string
	AmountZat   int64
	Status      Status
	TxID        string
	Error       string
	BroadcastOK int
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// Limits are the payout rules Reserve enforces.
type Limits struct {
	// PayoutZat is the amount paid per claim.
	PayoutZat int64
	// IPWindowLimitZat caps the total paid to one IP key per Window.
	IPWindowLimitZat int64
	// AddressCooldown is the minimum time between claims to one address.
	AddressCooldown time.Duration
	// GlobalWindowCapZat caps the total paid to everyone per Window.
	GlobalWindowCapZat int64
	// Window is the rolling window for IPWindowLimitZat and GlobalWindowCapZat.
	Window time.Duration
	// MaxQueue caps claims waiting to be paid.
	MaxQueue int
}

// Reason names the limit a rejected claim hit.
type Reason string

const (
	ReasonIPLimit         Reason = "ip_limit"
	ReasonAddressCooldown Reason = "address_cooldown"
	ReasonGlobalCap       Reason = "global_cap"
	ReasonQueueFull       Reason = "queue_full"
)

// LimitError is returned by Reserve when a claim is refused.
type LimitError struct {
	Reason     Reason
	RetryAfter time.Duration
}

func (e *LimitError) Error() string {
	return fmt.Sprintf("claim refused: %s (retry after %s)", e.Reason, e.RetryAfter.Round(time.Second))
}

// ErrNotFound is returned when a claim id does not exist.
var ErrNotFound = errors.New("claim not found")

// queueFullRetry is the Retry-After suggested when the queue is full.
const queueFullRetry = time.Minute

// Store is the claim database.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

const schema = `
CREATE TABLE IF NOT EXISTS claims (
	seq          INTEGER PRIMARY KEY AUTOINCREMENT,
	id           TEXT    NOT NULL UNIQUE,
	ip_key       TEXT    NOT NULL,
	address      TEXT    NOT NULL,
	amount_zat   INTEGER NOT NULL,
	status       TEXT    NOT NULL,
	txid         TEXT    NOT NULL DEFAULT '',
	error        TEXT    NOT NULL DEFAULT '',
	broadcast_ok INTEGER NOT NULL DEFAULT 0,
	created_at   INTEGER NOT NULL,
	updated_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS claims_ip      ON claims (ip_key, created_at);
CREATE INDEX IF NOT EXISTS claims_address ON claims (address, created_at);
CREATE INDEX IF NOT EXISTS claims_status  ON claims (status, seq);
CREATE INDEX IF NOT EXISTS claims_created ON claims (created_at);
CREATE TABLE IF NOT EXISTS meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
`

// Open opens (creating if needed) the database at path. Use ":memory:" in tests.
func Open(ctx context.Context, path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// One connection serializes every transaction in-process, which is all the
	// concurrency control Reserve needs, and keeps ":memory:" a single database.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return &Store{db: db, now: time.Now}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks the database is usable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Reserve checks every limit for a new claim by ipKey to address and, if all pass,
// records it as queued. A refusal is a *LimitError. Checks run in the order a user
// would find most useful to hear about: their own IP, their address, then global state.
func (s *Store) Reserve(ctx context.Context, ipKey, address string, lim Limits) (Claim, error) {
	id, err := newID()
	if err != nil {
		return Claim{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Claim{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit

	now := s.now()
	windowStart := now.Add(-lim.Window).Unix()

	// Per-IP amount over the rolling window.
	retry, over, err := windowOverflow(ctx, tx, now, lim.Window,
		`SELECT amount_zat, created_at FROM claims
		 WHERE ip_key = ? AND created_at > ? AND status != 'failed' ORDER BY created_at`,
		lim.PayoutZat, lim.IPWindowLimitZat, ipKey, windowStart)
	if err != nil {
		return Claim{}, err
	}
	if over {
		return Claim{}, &LimitError{Reason: ReasonIPLimit, RetryAfter: retry}
	}

	// Per-address cooldown.
	var last sql.NullInt64
	err = tx.QueryRowContext(ctx,
		`SELECT MAX(created_at) FROM claims WHERE address = ? AND created_at > ? AND status != 'failed'`,
		address, now.Add(-lim.AddressCooldown).Unix()).Scan(&last)
	if err != nil {
		return Claim{}, fmt.Errorf("address cooldown: %w", err)
	}
	if last.Valid {
		return Claim{}, &LimitError{Reason: ReasonAddressCooldown, RetryAfter: until(now, last.Int64, lim.AddressCooldown)}
	}

	// Global amount over the rolling window.
	retry, over, err = windowOverflow(ctx, tx, now, lim.Window,
		`SELECT amount_zat, created_at FROM claims
		 WHERE created_at > ? AND status != 'failed' ORDER BY created_at`,
		lim.PayoutZat, lim.GlobalWindowCapZat, windowStart)
	if err != nil {
		return Claim{}, err
	}
	if over {
		return Claim{}, &LimitError{Reason: ReasonGlobalCap, RetryAfter: retry}
	}

	// Queue depth.
	var pending int
	err = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM claims WHERE status IN ('queued', 'sending')`).Scan(&pending)
	if err != nil {
		return Claim{}, fmt.Errorf("queue depth: %w", err)
	}
	if pending >= lim.MaxQueue {
		return Claim{}, &LimitError{Reason: ReasonQueueFull, RetryAfter: queueFullRetry}
	}

	c := Claim{
		ID: id, IPKey: ipKey, Address: address, AmountZat: lim.PayoutZat,
		Status: StatusQueued, CreatedAt: time.Unix(now.Unix(), 0), UpdatedAt: time.Unix(now.Unix(), 0),
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO claims (id, ip_key, address, amount_zat, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.ID, c.IPKey, c.Address, c.AmountZat, c.Status, now.Unix(), now.Unix())
	if err != nil {
		return Claim{}, fmt.Errorf("insert claim: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Claim{}, fmt.Errorf("commit: %w", err)
	}
	return c, nil
}

// windowOverflow sums the (amount, created_at) rows query returns, oldest first. If
// adding payout would exceed limit, it reports how long until enough of those rows age
// out of the window for the payout to fit.
func windowOverflow(ctx context.Context, tx *sql.Tx, now time.Time, window time.Duration,
	query string, payout, limit int64, args ...any) (time.Duration, bool, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return 0, false, fmt.Errorf("window query: %w", err)
	}
	defer rows.Close()
	type row struct{ amount, created int64 }
	var all []row
	var sum int64
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.amount, &r.created); err != nil {
			return 0, false, fmt.Errorf("window scan: %w", err)
		}
		all = append(all, r)
		sum += r.amount
	}
	if err := rows.Err(); err != nil {
		return 0, false, fmt.Errorf("window rows: %w", err)
	}
	if sum+payout <= limit {
		return 0, false, nil
	}
	if payout > limit {
		// Can never fit; point at a full window rather than zero.
		return window, true, nil
	}
	for _, r := range all {
		sum -= r.amount
		if sum+payout <= limit {
			return until(now, r.created, window), true, nil
		}
	}
	return window, true, nil
}

// until returns how long from now until created+d, rounded up to whole seconds and
// never less than one second, so a client that waits exactly that long gets through.
func until(now time.Time, created int64, d time.Duration) time.Duration {
	left := time.Unix(created, 0).Add(d).Sub(now)
	if left < time.Second {
		return time.Second
	}
	if rem := left % time.Second; rem > 0 {
		left += time.Second - rem
	}
	return left
}

// Get returns the claim with id.
func (s *Store) Get(ctx context.Context, id string) (Claim, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+claimColumns+` FROM claims WHERE id = ?`, id)
	c, err := scanClaim(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Claim{}, ErrNotFound
	}
	return c, err
}

// TakeNext marks the oldest queued claim as sending and returns it. ok is false when
// the queue is empty.
func (s *Store) TakeNext(ctx context.Context) (c Claim, ok bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Claim{}, false, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	row := tx.QueryRowContext(ctx,
		`SELECT `+claimColumns+` FROM claims WHERE status = 'queued' ORDER BY seq LIMIT 1`)
	c, err = scanClaim(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Claim{}, false, nil
	}
	if err != nil {
		return Claim{}, false, err
	}
	now := s.now().Unix()
	if _, err := tx.ExecContext(ctx,
		`UPDATE claims SET status = 'sending', updated_at = ? WHERE id = ?`, now, c.ID); err != nil {
		return Claim{}, false, fmt.Errorf("mark sending: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Claim{}, false, fmt.Errorf("commit: %w", err)
	}
	c.Status = StatusSending
	c.UpdatedAt = time.Unix(now, 0)
	return c, true, nil
}

// MarkSent records the txid of a claim that was sending.
func (s *Store) MarkSent(ctx context.Context, id, txid string) error {
	return s.transition(ctx, id, StatusSending, StatusSent, "txid = ?", txid)
}

// Requeue returns a sending claim to the queue after a send that provably did not happen.
func (s *Store) Requeue(ctx context.Context, id string) error {
	return s.transition(ctx, id, StatusSending, StatusQueued, "")
}

// MarkFailed records a definitive non-payment.
func (s *Store) MarkFailed(ctx context.Context, id, reason string) error {
	return s.transition(ctx, id, StatusSending, StatusFailed, "error = ?", reason)
}

// MarkReview records a send with an unknown outcome.
func (s *Store) MarkReview(ctx context.Context, id, reason string) error {
	return s.transition(ctx, id, StatusSending, StatusReview, "error = ?", reason)
}

// SetBroadcastOK records how many nodes accepted a sent claim's transaction.
func (s *Store) SetBroadcastOK(ctx context.Context, id string, n int) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE claims SET broadcast_ok = ?, updated_at = ? WHERE id = ? AND status = 'sent'`,
		n, s.now().Unix(), id)
	if err != nil {
		return fmt.Errorf("set broadcast_ok: %w", err)
	}
	return nil
}

func (s *Store) transition(ctx context.Context, id string, from, to Status, set string, arg ...any) error {
	q := `UPDATE claims SET status = ?, updated_at = ?`
	args := []any{to, s.now().Unix()}
	if set != "" {
		q += ", " + set
		args = append(args, arg...)
	}
	q += ` WHERE id = ? AND status = ?`
	args = append(args, id, from)
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return fmt.Errorf("claim %s %s->%s: %w", id, from, to, err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("claim %s: not in status %s", id, from)
	}
	return nil
}

// RecoverInterrupted moves every sending claim to review. Call it once at startup,
// before the worker runs: a claim still sending then was interrupted mid-send, and
// whether it was paid is unknown.
func (s *Store) RecoverInterrupted(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE claims SET status = 'review', error = 'interrupted while sending', updated_at = ?
		 WHERE status = 'sending'`, s.now().Unix())
	if err != nil {
		return 0, fmt.Errorf("recover interrupted: %w", err)
	}
	return res.RowsAffected()
}

// ExpireQueued fails claims that have waited in the queue longer than maxAge.
func (s *Store) ExpireQueued(ctx context.Context, maxAge time.Duration) (int64, error) {
	now := s.now()
	res, err := s.db.ExecContext(ctx,
		`UPDATE claims SET status = 'failed', error = 'faucet busy, not paid in time', updated_at = ?
		 WHERE status = 'queued' AND created_at < ?`, now.Unix(), now.Add(-maxAge).Unix())
	if err != nil {
		return 0, fmt.Errorf("expire queued: %w", err)
	}
	return res.RowsAffected()
}

// Stats summarizes recent activity.
type Stats struct {
	// SentCount and SentZat cover claims paid within the window.
	SentCount int
	SentZat   int64
	// CommittedZat is everything counting against the global cap within the window.
	CommittedZat int64
	// Pending is the number of queued or sending claims.
	Pending int
}

// Stats returns activity over the trailing window.
func (s *Store) Stats(ctx context.Context, window time.Duration) (Stats, error) {
	var st Stats
	since := s.now().Add(-window).Unix()
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN status = 'sent' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status = 'sent' THEN amount_zat ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status != 'failed' THEN amount_zat ELSE 0 END), 0)
		FROM claims WHERE created_at > ?`, since).Scan(&st.SentCount, &st.SentZat, &st.CommittedZat)
	if err != nil {
		return Stats{}, fmt.Errorf("stats: %w", err)
	}
	err = s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM claims WHERE status IN ('queued', 'sending')`).Scan(&st.Pending)
	if err != nil {
		return Stats{}, fmt.Errorf("stats pending: %w", err)
	}
	return st, nil
}

// RecentSent returns up to n most recently paid claims, newest first.
func (s *Store) RecentSent(ctx context.Context, n int) ([]Claim, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+claimColumns+` FROM claims WHERE status = 'sent' ORDER BY seq DESC LIMIT ?`, n)
	if err != nil {
		return nil, fmt.Errorf("recent: %w", err)
	}
	defer rows.Close()
	var out []Claim
	for rows.Next() {
		c, err := scanClaim(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Meta returns a stored metadata value; ok is false when key is unset.
func (s *Store) Meta(ctx context.Context, key string) (value string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("meta %s: %w", key, err)
	}
	return value, true, nil
}

// SetMeta stores a metadata value.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value = excluded.value`,
		key, value)
	if err != nil {
		return fmt.Errorf("set meta %s: %w", key, err)
	}
	return nil
}

const claimColumns = `id, ip_key, address, amount_zat, status, txid, error, broadcast_ok, created_at, updated_at`

type scanner interface{ Scan(dest ...any) error }

func scanClaim(row scanner) (Claim, error) {
	var c Claim
	var created, updated int64
	err := row.Scan(&c.ID, &c.IPKey, &c.Address, &c.AmountZat, &c.Status, &c.TxID, &c.Error,
		&c.BroadcastOK, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Claim{}, err
		}
		return Claim{}, fmt.Errorf("scan claim: %w", err)
	}
	c.CreatedAt = time.Unix(created, 0)
	c.UpdatedAt = time.Unix(updated, 0)
	return c, nil
}

// newID returns a random 128-bit hex claim id; ids are unguessable so a claim's status
// is visible only to whoever submitted it.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("claim id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}
