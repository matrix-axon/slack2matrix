// Package store persists import progress so runs are resumable and idempotent.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	_ "github.com/mattn/go-sqlite3"
	"maunium.net/go/mautrix/id"
)

const schema = `
CREATE TABLE IF NOT EXISTS kv (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
-- Ghost profiles that have been set up.
CREATE TABLE IF NOT EXISTS ghosts (
	mxid        TEXT PRIMARY KEY,
	displayname TEXT NOT NULL,
	avatar_mxc  TEXT NOT NULL DEFAULT ''
);
-- The importer's device on each account it acts as.
CREATE TABLE IF NOT EXISTS devices (
	mxid        TEXT PRIMARY KEY,
	device_id   TEXT NOT NULL,
	cross_signed INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS rooms (
	channel_id TEXT PRIMARY KEY,
	room_id    TEXT NOT NULL,
	setup_done INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS members (
	room_id    TEXT NOT NULL,
	mxid       TEXT NOT NULL,
	membership TEXT NOT NULL,
	PRIMARY KEY (room_id, mxid)
);
-- One row per Matrix event sent for a Slack message part. status is
-- 'pending' while the request is in flight and 'sent' once the event ID is known.
CREATE TABLE IF NOT EXISTS events (
	channel_id TEXT NOT NULL,
	slack_ts   TEXT NOT NULL,
	part       INTEGER NOT NULL,
	kind       TEXT NOT NULL,
	sender     TEXT NOT NULL,
	txn_id     TEXT NOT NULL,
	status     TEXT NOT NULL,
	event_id   TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (channel_id, slack_ts, part)
);
-- Uploaded media, keyed by Slack file ID or remote URL. content holds the
-- JSON of the media fields (file/url/info) to merge into message content.
CREATE TABLE IF NOT EXISTS media (
	key     TEXT PRIMARY KEY,
	content TEXT NOT NULL
);
-- Media that could not be uploaded (missing, dead link), so it isn't retried.
CREATE TABLE IF NOT EXISTS media_failures (
	key   TEXT PRIMARY KEY,
	error TEXT NOT NULL
);
-- The event that first shared each Slack file, for file comment replies.
CREATE TABLE IF NOT EXISTS file_events (
	file_id  TEXT PRIMARY KEY,
	room_id  TEXT NOT NULL,
	event_id TEXT NOT NULL
);
-- Megolm sessions created by the importer. Each room and sender gets a new
-- session per calendar year of Slack history (and after any restart that
-- lost the outbound session).
CREATE TABLE IF NOT EXISTS megolm_sessions (
	session_id TEXT PRIMARY KEY,
	room_id    TEXT NOT NULL,
	sender     TEXT NOT NULL,
	year       INTEGER NOT NULL
);
-- Which sessions have reached which recipient by which method.
CREATE TABLE IF NOT EXISTS key_delivery (
	session_id TEXT NOT NULL,
	recipient  TEXT NOT NULL,
	method     TEXT NOT NULL,
	PRIMARY KEY (session_id, recipient, method)
);
`

type Store struct {
	DB *sql.DB
}

// Open opens (creating if needed) the app database in dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	db, err := OpenSQLite(filepath.Join(dir, "import.db"))
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("creating schema: %w", err)
	}
	return &Store{DB: db}, nil
}

// OpenSQLite opens a SQLite database in WAL mode with a busy timeout.
func OpenSQLite(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite3", "file:"+path+"?_journal_mode=WAL&_busy_timeout=10000&_foreign_keys=on&_txlock=immediate&_synchronous=NORMAL")
	if err != nil {
		return nil, err
	}
	return db, db.Ping()
}

func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) Get(ctx context.Context, key string) (string, error) {
	var v string
	err := s.DB.QueryRowContext(ctx, "SELECT value FROM kv WHERE key=?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *Store) Set(ctx context.Context, key, value string) error {
	_, err := s.DB.ExecContext(ctx, "INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET value=excluded.value", key, value)
	return err
}

// Ghost returns the stored profile for a ghost, or ok=false.
func (s *Store) Ghost(ctx context.Context, mxid id.UserID) (displayname, avatar string, ok bool, err error) {
	err = s.DB.QueryRowContext(ctx, "SELECT displayname, avatar_mxc FROM ghosts WHERE mxid=?", mxid).Scan(&displayname, &avatar)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	return displayname, avatar, err == nil, err
}

func (s *Store) SetGhost(ctx context.Context, mxid id.UserID, displayname, avatar string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO ghosts (mxid, displayname, avatar_mxc) VALUES (?, ?, ?)
		ON CONFLICT (mxid) DO UPDATE SET displayname=excluded.displayname, avatar_mxc=excluded.avatar_mxc`, mxid, displayname, avatar)
	return err
}

// Device returns the importer's device for an account, or "" if none.
func (s *Store) Device(ctx context.Context, mxid id.UserID) (id.DeviceID, bool, error) {
	var dev string
	var xs bool
	err := s.DB.QueryRowContext(ctx, "SELECT device_id, cross_signed FROM devices WHERE mxid=?", mxid).Scan(&dev, &xs)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id.DeviceID(dev), xs, err
}

func (s *Store) SetDevice(ctx context.Context, mxid id.UserID, dev id.DeviceID) error {
	_, err := s.DB.ExecContext(ctx, "INSERT INTO devices (mxid, device_id) VALUES (?, ?) ON CONFLICT (mxid) DO UPDATE SET device_id=excluded.device_id", mxid, dev)
	return err
}

func (s *Store) SetCrossSigned(ctx context.Context, mxid id.UserID) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE devices SET cross_signed=1 WHERE mxid=?", mxid)
	return err
}

// Devices lists every account with an importer device.
func (s *Store) Devices(ctx context.Context) (map[id.UserID]id.DeviceID, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT mxid, device_id FROM devices")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[id.UserID]id.DeviceID{}
	for rows.Next() {
		var u, d string
		if err := rows.Scan(&u, &d); err != nil {
			return nil, err
		}
		out[id.UserID(u)] = id.DeviceID(d)
	}
	return out, rows.Err()
}

// Room returns the Matrix room for a Slack channel.
func (s *Store) Room(ctx context.Context, channelID string) (room id.RoomID, setupDone bool, err error) {
	var r string
	err = s.DB.QueryRowContext(ctx, "SELECT room_id, setup_done FROM rooms WHERE channel_id=?", channelID).Scan(&r, &setupDone)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return id.RoomID(r), setupDone, err
}

func (s *Store) SetRoom(ctx context.Context, channelID string, room id.RoomID) error {
	_, err := s.DB.ExecContext(ctx, "INSERT INTO rooms (channel_id, room_id) VALUES (?, ?)", channelID, room)
	return err
}

func (s *Store) SetRoomSetupDone(ctx context.Context, channelID string) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE rooms SET setup_done=1 WHERE channel_id=?", channelID)
	return err
}

// ForgetChannel removes a channel's room and everything sent to it, so the
// next import starts the channel over. Uploaded media is kept for reuse.
func (s *Store) ForgetChannel(ctx context.Context, channelID string, room id.RoomID) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []struct {
		sql string
		arg any
	}{
		{"DELETE FROM events WHERE channel_id=?", channelID},
		{"DELETE FROM rooms WHERE channel_id=?", channelID},
		{"DELETE FROM members WHERE room_id=?", room},
		{"DELETE FROM file_events WHERE room_id=?", room},
	} {
		if _, err := tx.ExecContext(ctx, q.sql, q.arg); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Rooms returns every channel→room mapping.
func (s *Store) Rooms(ctx context.Context) (map[string]id.RoomID, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT channel_id, room_id FROM rooms")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]id.RoomID{}
	for rows.Next() {
		var c, r string
		if err := rows.Scan(&c, &r); err != nil {
			return nil, err
		}
		out[c] = id.RoomID(r)
	}
	return out, rows.Err()
}

func (s *Store) Membership(ctx context.Context, room id.RoomID, mxid id.UserID) (string, error) {
	var m string
	err := s.DB.QueryRowContext(ctx, "SELECT membership FROM members WHERE room_id=? AND mxid=?", room, mxid).Scan(&m)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return m, err
}

func (s *Store) SetMembership(ctx context.Context, room id.RoomID, mxid id.UserID, membership string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO members (room_id, mxid, membership) VALUES (?, ?, ?)
		ON CONFLICT (room_id, mxid) DO UPDATE SET membership=excluded.membership`, room, mxid, membership)
	return err
}

// EventRow is the recorded state of one Slack message part.
type EventRow struct {
	Kind    string
	Sender  id.UserID
	TxnID   string
	Status  string
	EventID id.EventID
}

// Event looks up a recorded event part.
func (s *Store) Event(ctx context.Context, channelID, slackTS string, part int) (*EventRow, error) {
	var r EventRow
	err := s.DB.QueryRowContext(ctx, "SELECT kind, sender, txn_id, status, event_id FROM events WHERE channel_id=? AND slack_ts=? AND part=?",
		channelID, slackTS, part).Scan(&r.Kind, &r.Sender, &r.TxnID, &r.Status, &r.EventID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &r, err
}

// MarkPending records that a send is about to be attempted.
func (s *Store) MarkPending(ctx context.Context, channelID, slackTS string, part int, kind string, sender id.UserID, txnID string) error {
	_, err := s.DB.ExecContext(ctx, `INSERT INTO events (channel_id, slack_ts, part, kind, sender, txn_id, status) VALUES (?, ?, ?, ?, ?, ?, 'pending')
		ON CONFLICT (channel_id, slack_ts, part) DO UPDATE SET kind=excluded.kind, sender=excluded.sender, txn_id=excluded.txn_id`,
		channelID, slackTS, part, kind, sender, txnID)
	return err
}

// MarkSent records the event ID of a completed send.
func (s *Store) MarkSent(ctx context.Context, channelID, slackTS string, part int, eventID id.EventID) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE events SET status='sent', event_id=? WHERE channel_id=? AND slack_ts=? AND part=?",
		eventID, channelID, slackTS, part)
	return err
}

// PendingEvents lists parts whose send outcome is unknown.
func (s *Store) PendingEvents(ctx context.Context, channelID string) ([]string, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT slack_ts || '/' || part FROM events WHERE channel_id=? AND status='pending'", channelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// FirstEvent returns the event ID of a Slack message's first sent part, used
// as the target of threads, replies and reactions.
func (s *Store) FirstEvent(ctx context.Context, channelID, slackTS string) (id.EventID, error) {
	var e string
	err := s.DB.QueryRowContext(ctx, "SELECT event_id FROM events WHERE channel_id=? AND slack_ts=? AND status='sent' AND kind IN ('message','media') ORDER BY part LIMIT 1",
		channelID, slackTS).Scan(&e)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id.EventID(e), err
}

// LastEvent returns the most recent sent event in a channel.
func (s *Store) LastEvent(ctx context.Context, channelID string) (id.EventID, error) {
	var e string
	err := s.DB.QueryRowContext(ctx, "SELECT event_id FROM events WHERE channel_id=? AND status='sent' AND kind IN ('message','media') ORDER BY CAST(slack_ts AS REAL) DESC, part DESC LIMIT 1",
		channelID).Scan(&e)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id.EventID(e), err
}

// SentCounts returns, per room, how many events the importer recorded as sent.
func (s *Store) SentCounts(ctx context.Context) (map[id.RoomID]int, error) {
	rows, err := s.DB.QueryContext(ctx, `SELECT r.room_id, COUNT(*) FROM events e JOIN rooms r ON r.channel_id=e.channel_id
		WHERE e.status='sent' AND e.event_id!='' GROUP BY r.room_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[id.RoomID]int{}
	for rows.Next() {
		var r string
		var n int
		if err := rows.Scan(&r, &n); err != nil {
			return nil, err
		}
		out[id.RoomID(r)] = n
	}
	return out, rows.Err()
}

// Media returns stored upload content for a media key.
func (s *Store) Media(ctx context.Context, key string) (string, error) {
	var c string
	err := s.DB.QueryRowContext(ctx, "SELECT content FROM media WHERE key=?", key).Scan(&c)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return c, err
}

func (s *Store) SetMedia(ctx context.Context, key, content string) error {
	_, err := s.DB.ExecContext(ctx, "INSERT INTO media (key, content) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET content=excluded.content", key, content)
	return err
}

func (s *Store) MediaFailure(ctx context.Context, key string) (string, error) {
	var e string
	err := s.DB.QueryRowContext(ctx, "SELECT error FROM media_failures WHERE key=?", key).Scan(&e)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return e, err
}

func (s *Store) SetMediaFailure(ctx context.Context, key, msg string) error {
	_, err := s.DB.ExecContext(ctx, "INSERT INTO media_failures (key, error) VALUES (?, ?) ON CONFLICT (key) DO UPDATE SET error=excluded.error", key, msg)
	return err
}

func (s *Store) FileEvent(ctx context.Context, fileID string) (id.RoomID, id.EventID, error) {
	var r, e string
	err := s.DB.QueryRowContext(ctx, "SELECT room_id, event_id FROM file_events WHERE file_id=?", fileID).Scan(&r, &e)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", nil
	}
	return id.RoomID(r), id.EventID(e), err
}

// SetFileEvent records the first event sharing a file; later shares are ignored.
func (s *Store) SetFileEvent(ctx context.Context, fileID string, room id.RoomID, evt id.EventID) error {
	_, err := s.DB.ExecContext(ctx, "INSERT INTO file_events (file_id, room_id, event_id) VALUES (?, ?, ?) ON CONFLICT DO NOTHING", fileID, room, evt)
	return err
}

// LatestMegolmSession returns the newest session for a room and sender.
func (s *Store) LatestMegolmSession(ctx context.Context, room id.RoomID, sender id.UserID) (id.SessionID, int, error) {
	var sid string
	var year int
	err := s.DB.QueryRowContext(ctx, "SELECT session_id, year FROM megolm_sessions WHERE room_id=? AND sender=? ORDER BY rowid DESC LIMIT 1", room, sender).Scan(&sid, &year)
	if errors.Is(err, sql.ErrNoRows) {
		return "", 0, nil
	}
	return id.SessionID(sid), year, err
}

func (s *Store) AddMegolmSession(ctx context.Context, room id.RoomID, sender id.UserID, year int, sid id.SessionID) error {
	_, err := s.DB.ExecContext(ctx, "INSERT INTO megolm_sessions (session_id, room_id, sender, year) VALUES (?, ?, ?, ?)", sid, room, sender, year)
	return err
}

// SessionRef identifies a Megolm session created by the importer.
type SessionRef struct {
	RoomID    id.RoomID
	Sender    id.UserID
	SessionID id.SessionID
}

// MegolmSessions lists every session the importer created.
func (s *Store) MegolmSessions(ctx context.Context) ([]SessionRef, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT room_id, sender, session_id FROM megolm_sessions ORDER BY rowid")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionRef
	for rows.Next() {
		var r SessionRef
		if err := rows.Scan(&r.RoomID, &r.Sender, &r.SessionID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) KeyDelivered(ctx context.Context, sid id.SessionID, recipient, method string) (bool, error) {
	var n int
	err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM key_delivery WHERE session_id=? AND recipient=? AND method=?", sid, recipient, method).Scan(&n)
	return n > 0, err
}

func (s *Store) SetKeyDelivered(ctx context.Context, sid id.SessionID, recipient, method string) error {
	_, err := s.DB.ExecContext(ctx, "INSERT INTO key_delivery (session_id, recipient, method) VALUES (?, ?, ?) ON CONFLICT DO NOTHING", sid, recipient, method)
	return err
}
