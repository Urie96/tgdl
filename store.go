package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure-Go sqlite driver, no cgo
)

const schema = `
CREATE TABLE IF NOT EXISTS channels (
	channel_id   INTEGER PRIMARY KEY,
	-- 'channel' (broadcast channel / supergroup) or 'chat' (basic group)
	kind         TEXT    NOT NULL DEFAULT 'channel',
	access_hash  INTEGER NOT NULL DEFAULT 0,
	username     TEXT    NOT NULL DEFAULT '',
	title        TEXT    NOT NULL DEFAULT '',
	folder         TEXT    NOT NULL DEFAULT '',
	head_id        INTEGER NOT NULL DEFAULT 0,
	backfill_off   INTEGER NOT NULL DEFAULT 0,
	backfill_done  INTEGER NOT NULL DEFAULT 0,
	updated_at     INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS videos (
	channel_id    INTEGER NOT NULL,
	message_id    INTEGER NOT NULL,
	file_name     TEXT    NOT NULL,
	file_path     TEXT    NOT NULL,
	file_size     INTEGER NOT NULL,
	duration      INTEGER NOT NULL DEFAULT 0,
	width         INTEGER NOT NULL DEFAULT 0,
	height        INTEGER NOT NULL DEFAULT 0,
	downloaded_at INTEGER NOT NULL,
	PRIMARY KEY (channel_id, message_id)
);

CREATE INDEX IF NOT EXISTS idx_videos_downloaded_at
	ON videos (downloaded_at DESC);

CREATE TABLE IF NOT EXISTS failures (
	channel_id INTEGER NOT NULL,
	message_id INTEGER NOT NULL,
	attempts   INTEGER NOT NULL DEFAULT 0,
	last_error TEXT    NOT NULL DEFAULT '',
	last_at    INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (channel_id, message_id)
);
`

// ChannelState is the persisted per-channel progress record.
//
// Two independent cursors are kept:
//
//   - Head is the highest message id ever processed. The "head pass" fetches
//     everything above it, so new posts are picked up within one poll tick.
//   - BackfillOffset walks the channel downwards for the historical archive.
//     BackfillDone marks that the beginning of the channel was reached.
type ChannelState struct {
	ChannelID int64
	// Kind is "channel" or "chat".
	Kind           string
	AccessHash     int64
	Username       string
	Title          string
	Folder         string
	Head           int
	BackfillOffset int
	BackfillDone   bool
}

// Store wraps the SQLite state database.
type Store struct {
	db *sql.DB
}

// OpenStore opens (and migrates) the state database.
func OpenStore(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create state dir: %w", err)
	}

	dsn := "file:" + path + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open state db: %w", err)
	}
	// modernc/sqlite is safe with a single writer; keep the pool small.
	db.SetMaxOpenConns(1)

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate state db: %w", err)
	}
	s := &Store{db: db}
	// `kind` was added after the first release: add it in place.
	if err := s.ensureColumn("channels", "kind", "TEXT NOT NULL DEFAULT 'channel'"); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// ensureColumn adds a column to an existing table when it is missing.
//
// SQLite has no "ADD COLUMN IF NOT EXISTS", and CREATE TABLE IF NOT EXISTS
// does not touch an existing table, so the schema check is explicit.
func (s *Store) ensureColumn(table, column, decl string) error {
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return fmt.Errorf("inspect %s: %w", table, err)
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return fmt.Errorf("inspect %s: %w", table, err)
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := s.db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + decl); err != nil {
		return fmt.Errorf("add %s.%s: %w", table, column, err)
	}
	return nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Channel returns the saved state for a channel id, or nil if unknown.
func (s *Store) Channel(id int64) (*ChannelState, error) {
	row := s.db.QueryRow(`
		SELECT channel_id, kind, access_hash, username, title, folder,
		       head_id, backfill_off, backfill_done
		FROM channels WHERE channel_id = ?`, id)
	st := &ChannelState{}
	err := row.Scan(&st.ChannelID, &st.Kind, &st.AccessHash, &st.Username, &st.Title,
		&st.Folder, &st.Head, &st.BackfillOffset, &st.BackfillDone)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load channel %d: %w", id, err)
	}
	return st, nil
}

// ChannelByUsername returns the saved state for a username, or nil.
func (s *Store) ChannelByUsername(username string) (*ChannelState, error) {
	row := s.db.QueryRow(`
		SELECT channel_id, kind, access_hash, username, title, folder,
		       head_id, backfill_off, backfill_done
		FROM channels WHERE username = ? COLLATE NOCASE`, username)
	st := &ChannelState{}
	err := row.Scan(&st.ChannelID, &st.Kind, &st.AccessHash, &st.Username, &st.Title,
		&st.Folder, &st.Head, &st.BackfillOffset, &st.BackfillDone)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load channel %q: %w", username, err)
	}
	return st, nil
}

// PutChannel inserts or updates a channel record, preserving the cursor.
func (s *Store) PutChannel(st *ChannelState) error {
	done := 0
	if st.BackfillDone {
		done = 1
	}
	kind := st.Kind
	if kind == "" {
		kind = "channel"
	}
	_, err := s.db.Exec(`
		INSERT INTO channels (channel_id, kind, access_hash, username, title, folder,
			head_id, backfill_off, backfill_done, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(channel_id) DO UPDATE SET
			kind          = excluded.kind,
			access_hash   = excluded.access_hash,
			username      = excluded.username,
			title         = excluded.title,
			folder        = excluded.folder,
			updated_at    = excluded.updated_at`,
		st.ChannelID, kind, st.AccessHash, st.Username, st.Title, st.Folder,
		st.Head, st.BackfillOffset, done, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("save channel %d: %w", st.ChannelID, err)
	}
	return nil
}

// SetHead persists the highest fully processed message id for a channel.
func (s *Store) SetHead(channelID int64, head int) error {
	_, err := s.db.Exec(
		`UPDATE channels SET head_id = ?, updated_at = ? WHERE channel_id = ?`,
		head, time.Now().Unix(), channelID)
	if err != nil {
		return fmt.Errorf("save head for %d: %w", channelID, err)
	}
	return nil
}

// SetBackfill persists the historical backfill cursor.
func (s *Store) SetBackfill(channelID int64, offset int, done bool) error {
	d := 0
	if done {
		d = 1
	}
	_, err := s.db.Exec(
		`UPDATE channels SET backfill_off = ?, backfill_done = ?, updated_at = ?
		 WHERE channel_id = ?`, offset, d, time.Now().Unix(), channelID)
	if err != nil {
		return fmt.Errorf("save backfill for %d: %w", channelID, err)
	}
	return nil
}

// Video is a recorded download.
type Video struct {
	ChannelID int64
	MessageID int
	FileName  string
	FilePath  string
	FileSize  int64
	Duration  int
	Width     int
	Height    int
}

// RecordVideo stores a successful download.
func (s *Store) RecordVideo(v Video) error {
	_, err := s.db.Exec(`
		INSERT INTO videos (channel_id, message_id, file_name, file_path, file_size,
			duration, width, height, downloaded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(channel_id, message_id) DO UPDATE SET
			file_name  = excluded.file_name,
			file_path  = excluded.file_path,
			file_size  = excluded.file_size`,
		v.ChannelID, v.MessageID, v.FileName, v.FilePath, v.FileSize,
		v.Duration, v.Width, v.Height, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("record video %d/%d: %w", v.ChannelID, v.MessageID, err)
	}
	// A successful download clears any previous failures.
	_, _ = s.db.Exec(`DELETE FROM failures WHERE channel_id = ? AND message_id = ?`,
		v.ChannelID, v.MessageID)
	return nil
}

// LookupVideo returns the recorded download for a message, or nil.
func (s *Store) LookupVideo(channelID int64, messageID int) (*Video, error) {
	row := s.db.QueryRow(`
		SELECT channel_id, message_id, file_name, file_path, file_size,
		       duration, width, height
		FROM videos WHERE channel_id = ? AND message_id = ?`, channelID, messageID)
	v := &Video{}
	err := row.Scan(&v.ChannelID, &v.MessageID, &v.FileName, &v.FilePath,
		&v.FileSize, &v.Duration, &v.Width, &v.Height)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup video: %w", err)
	}
	return v, nil
}

// RecordFailure bumps the attempt counter for a message and returns the total.
func (s *Store) RecordFailure(channelID int64, messageID int, cause error) (int, error) {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	_, err := s.db.Exec(`
		INSERT INTO failures (channel_id, message_id, attempts, last_error, last_at)
		VALUES (?, ?, 1, ?, ?)
		ON CONFLICT(channel_id, message_id) DO UPDATE SET
			attempts   = attempts + 1,
			last_error = excluded.last_error,
			last_at    = excluded.last_at`,
		channelID, messageID, msg, time.Now().Unix())
	if err != nil {
		return 0, fmt.Errorf("record failure: %w", err)
	}
	var n int
	if err := s.db.QueryRow(
		`SELECT attempts FROM failures WHERE channel_id = ? AND message_id = ?`,
		channelID, messageID).Scan(&n); err != nil {
		return 1, nil //nolint:nilerr // best effort counter
	}
	return n, nil
}

// Stats summarises the database.
type Stats struct {
	Channels int
	Videos   int
	Bytes    int64
	Failures int
}

// Stats returns aggregate counters for the summary log.
func (s *Store) Stats() (Stats, error) {
	var st Stats
	err := s.db.QueryRow(`SELECT
		(SELECT COUNT(*) FROM channels),
		(SELECT COUNT(*) FROM videos),
		(SELECT COALESCE(SUM(file_size), 0) FROM videos),
		(SELECT COUNT(*) FROM failures)`).
		Scan(&st.Channels, &st.Videos, &st.Bytes, &st.Failures)
	if err != nil {
		return st, fmt.Errorf("stats: %w", err)
	}
	return st, nil
}
