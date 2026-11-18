package main

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gotd/td/tg"
)

func TestParseChannelSpec(t *testing.T) {
	tests := []struct {
		in       string
		username string
		id       int64
		kind     peerKind
		wantErr  bool
	}{
		{in: "@durov", username: "durov", kind: kindChannel},
		{in: "durov", username: "durov", kind: kindChannel},
		{in: "https://t.me/durov", username: "durov", kind: kindChannel},
		{in: "t.me/durov/", username: "durov", kind: kindChannel},
		{in: "telegram.me/SomeChannel", username: "somechannel", kind: kindChannel},
		// -100<id> is a channel or supergroup.
		{in: "-1001234567890", id: 1234567890, kind: kindChannel},
		{in: "-1003331047977", id: 3331047977, kind: kindChannel},
		{in: "1234567890", id: 1234567890, kind: kindChannel},
		// Any other negative id is a basic group.
		{in: "-1234567890", id: 1234567890, kind: kindChat},
		{in: "-5075548374", id: 5075548374, kind: kindChat},
		{in: "   ", wantErr: true},
		{in: "@", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			username, id, kind, err := parseChannelSpec(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if username != tc.username || id != tc.id {
				t.Fatalf("got (%q, %d), want (%q, %d)", username, id, tc.username, tc.id)
			}
			if kind != tc.kind {
				t.Fatalf("kind = %q, want %q", kind, tc.kind)
			}
		})
	}
}

func TestSanitize(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"a/b\\c:d*e?f\"g<h>i|j", "a_b_c_d_e_f_g_h_i_j"},
		{"  spaced   out  ", "spaced out"},
		{"...", "unnamed"},
		{"", "unnamed"},
		{"ok name.mp4", "ok name.mp4"},
	}
	for _, tc := range tests {
		if got := sanitize(tc.in); got != tc.want {
			t.Errorf("sanitize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSanitizeLength(t *testing.T) {
	long := strings.Repeat("é", 300)
	got := sanitize(long)
	if len(got) > maxNameLen {
		t.Fatalf("sanitize produced %d bytes, want <= %d", len(got), maxNameLen)
	}
	if !strings.HasPrefix(got, "é") {
		t.Fatalf("sanitize split a rune: %q", got)
	}
}

func TestSplitExt(t *testing.T) {
	tests := []struct {
		in, stem, ext string
	}{
		{"movie.mp4", "movie", ".mp4"},
		{"Movie.MKV", "Movie", ".mkv"},
		{"no-ext", "no-ext", ""},
		{"", "unnamed", ""},
		{"a b/c.mp4", "a b_c", ".mp4"},
	}
	for _, tc := range tests {
		stem, ext := splitExt(tc.in)
		if stem != tc.stem || ext != tc.ext {
			t.Errorf("splitExt(%q) = (%q, %q), want (%q, %q)",
				tc.in, stem, ext, tc.stem, tc.ext)
		}
	}
}

func TestExtFromMime(t *testing.T) {
	tests := map[string]string{
		"video/mp4":                  ".mp4",
		"video/x-matroska":           ".mkv",
		"VIDEO/QUICKTIME":            ".mov",
		"video/webm; codecs=vp9":     ".webm",
		"application/octet-stream":   ".mp4",
		"video/x-some-unknown-thing": ".x-some-unknown-thing",
	}
	for mime, want := range tests {
		if got := extFromMime(mime); got != want {
			t.Errorf("extFromMime(%q) = %q, want %q", mime, got, want)
		}
	}
}

func TestTargetPath(t *testing.T) {
	d := &dl{
		cfg: &Config{DownloadDir: "/downloads"},
		ref: channelRef{Folder: "my_channel"},
	}
	v := video{
		MessageID: 42,
		Date:      time.Date(2024, 3, 5, 14, 30, 5, 0, time.UTC),
		Doc:       &tg.Document{MimeType: "video/mp4"},
		FileName:  "big clip.mp4",
	}
	got := d.targetPath(v)

	if !strings.HasPrefix(got, filepath.Join("/downloads", "my_channel", "2024")+string(filepath.Separator)) {
		t.Fatalf("unexpected path layout: %q", got)
	}
	if filepath.Base(got) != "20240305-143005_42_big clip.mp4" {
		// Local timezone may shift the timestamp; the suffix and id must not.
		if !strings.HasSuffix(filepath.Base(got), "_42_big clip.mp4") {
			t.Fatalf("unexpected file name: %q", filepath.Base(got))
		}
	}
	if strings.Contains(got, "_42_unnamed") {
		t.Fatalf("stem was not taken from the filename: %q", got)
	}
}

func TestTargetPathWithoutFilename(t *testing.T) {
	d := &dl{
		cfg: &Config{DownloadDir: "/downloads"},
		ref: channelRef{Folder: "ch"},
	}
	v := video{
		MessageID: 7,
		Date:      time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC),
		Doc:       &tg.Document{MimeType: "video/x-matroska"},
	}
	got := d.targetPath(v)
	if !strings.HasSuffix(filepath.Base(got), "_7_video.mkv") {
		t.Fatalf("unexpected name: %q", filepath.Base(got))
	}
}

func videoMessage(id int, attrs ...tg.DocumentAttributeClass) tg.MessageClass {
	return &tg.Message{
		ID:   id,
		Date: 1700000000,
		Media: &tg.MessageMediaDocument{
			Document: &tg.Document{
				ID:         999,
				AccessHash: 123,
				MimeType:   "video/mp4",
				Size:       1024,
				Attributes: attrs,
			},
		},
	}
}

func TestExtractVideo(t *testing.T) {
	t.Run("plain video", func(t *testing.T) {
		msg := videoMessage(5,
			&tg.DocumentAttributeVideo{Duration: 61.7, W: 1920, H: 1080},
			&tg.DocumentAttributeFilename{FileName: "clip.mp4"},
		)
		v, ok := extractVideo(msg)
		if !ok {
			t.Fatal("expected a video")
		}
		if v.MessageID != 5 || v.Duration != 61 || v.Width != 1920 || v.Height != 1080 {
			t.Fatalf("unexpected metadata: %+v", v)
		}
		if v.FileName != "clip.mp4" {
			t.Fatalf("unexpected filename %q", v.FileName)
		}
		if v.Size() != 1024 {
			t.Fatalf("unexpected size %d", v.Size())
		}
	})

	t.Run("video note is skipped", func(t *testing.T) {
		msg := videoMessage(6, &tg.DocumentAttributeVideo{RoundMessage: true})
		if _, ok := extractVideo(msg); ok {
			t.Fatal("video notes must be skipped")
		}
	})

	t.Run("document without video attribute", func(t *testing.T) {
		msg := videoMessage(7, &tg.DocumentAttributeFilename{FileName: "doc.pdf"})
		if _, ok := extractVideo(msg); ok {
			t.Fatal("plain documents must be skipped")
		}
	})

	t.Run("video posted as file", func(t *testing.T) {
		// Channels often post .mp4 as a document, which Telegram may tag
		// application/octet-stream. The attribute is authoritative.
		msg := &tg.Message{
			ID:   8,
			Date: 1700000000,
			Media: &tg.MessageMediaDocument{
				Document: &tg.Document{
					ID:       1,
					MimeType: "application/octet-stream",
					Size:     2048,
					Attributes: []tg.DocumentAttributeClass{
						&tg.DocumentAttributeVideo{Duration: 5, W: 640, H: 360},
						&tg.DocumentAttributeFilename{FileName: "clip.mp4"},
					},
				},
			},
		}
		v, ok := extractVideo(msg)
		if !ok {
			t.Fatal("a document carrying DocumentAttributeVideo must count")
		}
		if v.FileName != "clip.mp4" {
			t.Fatalf("unexpected filename %q", v.FileName)
		}
	})

	t.Run("animation is skipped", func(t *testing.T) {
		msg := &tg.Message{
			ID:   11,
			Date: 1700000000,
			Media: &tg.MessageMediaDocument{
				Document: &tg.Document{
					MimeType: "video/mp4",
					Attributes: []tg.DocumentAttributeClass{
						&tg.DocumentAttributeVideo{},
						&tg.DocumentAttributeAnimated{},
					},
				},
			},
		}
		if _, ok := extractVideo(msg); ok {
			t.Fatal("animations (GIF-style mp4) must be skipped")
		}
	})

	t.Run("photo message", func(t *testing.T) {
		msg := &tg.Message{
			ID:    9,
			Date:  1700000000,
			Media: &tg.MessageMediaPhoto{},
		}
		if _, ok := extractVideo(msg); ok {
			t.Fatal("photos must be skipped")
		}
	})

	t.Run("service message", func(t *testing.T) {
		if _, ok := extractVideo(&tg.MessageService{ID: 10}); ok {
			t.Fatal("service messages must be skipped")
		}
	})
}

func TestSkipReason(t *testing.T) {
	d := &dl{filters: Filters{MaxFileSize: 100, MinDuration: 10, MaxDuration: 60}}
	doc := &tg.Document{Size: 50}

	if got := d.skipReason(video{Doc: doc, Duration: 30}); got != "" {
		t.Fatalf("expected no skip, got %q", got)
	}
	if got := d.skipReason(video{Doc: &tg.Document{Size: 500}, Duration: 30}); !strings.Contains(got, "max_file_size") {
		t.Fatalf("expected size skip, got %q", got)
	}
	if got := d.skipReason(video{Doc: doc, Duration: 2}); !strings.Contains(got, "min_duration") {
		t.Fatalf("expected min duration skip, got %q", got)
	}
	if got := d.skipReason(video{Doc: doc, Duration: 600}); !strings.Contains(got, "max_duration") {
		t.Fatalf("expected max duration skip, got %q", got)
	}
}

func TestStoreRoundTrip(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if _, err := store.Channel(1); err != nil {
		t.Fatalf("missing channel should be nil: %v", err)
	}

	st := &ChannelState{ChannelID: 1, AccessHash: 77, Username: "ch", Title: "Channel", Folder: "ch"}
	if err := store.PutChannel(st); err != nil {
		t.Fatalf("put channel: %v", err)
	}
	if err := store.SetHead(1, 100); err != nil {
		t.Fatalf("set head: %v", err)
	}
	if err := store.SetBackfill(1, 55, false); err != nil {
		t.Fatalf("set backfill: %v", err)
	}

	got, err := store.Channel(1)
	if err != nil || got == nil {
		t.Fatalf("load channel: %v %v", got, err)
	}
	if got.Head != 100 || got.BackfillOffset != 55 || got.BackfillDone {
		t.Fatalf("unexpected state: %+v", got)
	}

	byName, err := store.ChannelByUsername("CH")
	if err != nil || byName == nil || byName.ChannelID != 1 {
		t.Fatalf("lookup by username: %v %v", byName, err)
	}

	// PutChannel must not clobber the progress cursors.
	if err := store.PutChannel(&ChannelState{ChannelID: 1, AccessHash: 88, Username: "ch2", Folder: "ch2"}); err != nil {
		t.Fatalf("re-put channel: %v", err)
	}
	got, _ = store.Channel(1)
	if got.Head != 100 || got.AccessHash != 88 || got.Username != "ch2" {
		t.Fatalf("PutChannel clobbered state: %+v", got)
	}

	if err := store.RecordVideo(Video{
		ChannelID: 1, MessageID: 100, FileName: "a.mp4", FilePath: "/x/a.mp4", FileSize: 2048,
	}); err != nil {
		t.Fatalf("record video: %v", err)
	}
	stats, err := store.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Channels != 1 || stats.Videos != 1 || stats.Bytes != 2048 {
		t.Fatalf("unexpected stats: %+v", stats)
	}
}

func TestRecordFailureCountsAndClears(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	for i := 1; i <= 3; i++ {
		n, err := store.RecordFailure(1, 42, errors.New("boom"))
		if err != nil {
			t.Fatalf("record failure: %v", err)
		}
		if n != i {
			t.Fatalf("attempt %d: got %d", i, n)
		}
	}
	if err := store.RecordVideo(Video{ChannelID: 1, MessageID: 42, FileName: "a.mp4"}); err != nil {
		t.Fatalf("record video: %v", err)
	}
	stats, err := store.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Failures != 0 {
		t.Fatalf("failures should be cleared after success, got %d", stats.Failures)
	}
}

func TestLoadConfigDefaultsAndPaths(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, `
api_id: 12345
api_hash: abcdef
channels:
  - channel: "@foo"
    dir: custom
    min_duration: 30
filters:
  max_file_size: 100
poll_interval: 2m
`)

	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.Threads != 4 || cfg.Retries != 5 {
		t.Fatalf("defaults not applied: %+v", cfg)
	}
	if cfg.PollInterval.D() != 2*time.Minute {
		t.Fatalf("poll interval: %v", cfg.PollInterval.D())
	}
	if !filepath.IsAbs(cfg.DownloadDir) || !strings.HasPrefix(cfg.DownloadDir, dir) {
		t.Fatalf("download dir not anchored to config: %q", cfg.DownloadDir)
	}

	f := cfg.FilterFor(cfg.Channels[0])
	if f.MaxFileSize != 100 || f.MinDuration != 30 {
		t.Fatalf("filter merge failed: %+v", f)
	}
}

func TestLoadConfigMissingSecrets(t *testing.T) {
	clearCredEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, "channels:\n  - channel: '@foo'\n")
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected validation error for missing api_id")
	}
}

// clearCredEnv isolates the config tests from credentials in the host
// environment.
func clearCredEnv(t *testing.T) {
	t.Helper()
	t.Setenv(EnvAPIID, "")
	t.Setenv(EnvAPIHash, "")
}

func TestLoadConfigEnvCredentials(t *testing.T) {
	clearCredEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, "channels:\n  - channel: '@foo'\n")

	t.Setenv(EnvAPIID, "424242")
	t.Setenv(EnvAPIHash, "deadbeef")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load config from env: %v", err)
	}
	if cfg.APIID != 424242 || cfg.APIHash != "deadbeef" {
		t.Fatalf("env credentials not applied: %+v", cfg)
	}
}

func TestLoadConfigEnvOverridesFile(t *testing.T) {
	clearCredEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, "api_id: 1\napi_hash: filehash\nchannels:\n  - channel: '@foo'\n")

	t.Setenv(EnvAPIID, "7")
	t.Setenv(EnvAPIHash, "envhash")
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.APIID != 7 || cfg.APIHash != "envhash" {
		t.Fatalf("env should win over yaml: %+v", cfg)
	}

	// A partial override keeps the other value from the file.
	t.Setenv(EnvAPIHash, "")
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.APIID != 7 || cfg.APIHash != "filehash" {
		t.Fatalf("partial override: %+v", cfg)
	}
}

func TestLoadConfigInvalidEnvAPIID(t *testing.T) {
	clearCredEnv(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, "api_hash: x\nchannels:\n  - channel: '@foo'\n")

	t.Setenv(EnvAPIID, "not-a-number")
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("expected error for non-numeric " + EnvAPIID)
	}
}

func TestInputPeerKind(t *testing.T) {
	ch := channelRef{ID: 3331047977, AccessHash: -4964861933056236318, Kind: kindChannel}
	if _, ok := ch.InputPeer().(*tg.InputPeerChannel); !ok {
		t.Fatalf("channel must use InputPeerChannel, got %T", ch.InputPeer())
	}

	grp := channelRef{ID: 5075548374, Kind: kindChat}
	peer, ok := grp.InputPeer().(*tg.InputPeerChat)
	if !ok {
		t.Fatalf("group must use InputPeerChat, got %T", grp.InputPeer())
	}
	if peer.ChatID != 5075548374 {
		t.Fatalf("unexpected chat id %d", peer.ChatID)
	}
}

func TestChannelRefFormatting(t *testing.T) {
	if got := (channelRef{ID: 1234567890, Kind: kindChannel}).String(); got != "1234567890 (1234567890)" {
		t.Fatalf("unexpected channel formatting: %q", got)
	}
	if got := (channelRef{ID: 1, Username: "abc", Kind: kindChannel}).String(); got != "@abc (1)" {
		t.Fatalf("unexpected username formatting: %q", got)
	}
	got := (channelRef{ID: 5075548374, Title: "群", Kind: kindChat}).String()
	if got != "群 (group 5075548374)" {
		t.Fatalf("unexpected group formatting: %q", got)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestBackfillDefaultAndOverride(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, `
api_id: 1
api_hash: x
channels:
  - channel: "@foo"
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if !cfg.BackfillEnabled() {
		t.Fatal("backfill should default to true")
	}

	writeFile(t, path, `
api_id: 1
api_hash: x
backfill: false
channels:
  - channel: "@foo"
`)
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.BackfillEnabled() {
		t.Fatal("backfill: false was ignored")
	}
}

func TestShutdownIsNotAFailure(t *testing.T) {
	// A cancelled context aborts the page without touching the failure table.
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d := &dl{
		store: store,
		cfg:   &Config{Retries: 5},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		ref:   channelRef{ID: 1},
	}
	complete, err := d.processPage(ctx, historyPage{
		Messages: []tg.MessageClass{videoMessage(99, &tg.DocumentAttributeVideo{})},
	})
	if complete {
		t.Fatal("page should not report completion on shutdown")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	stats, err := store.Stats()
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Failures != 0 {
		t.Fatalf("shutdown must not record a failure, got %d", stats.Failures)
	}
}

func TestStoreKindAndMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")

	// Simulate a database created before `kind` existed.
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	if _, err := raw.Exec(`
		CREATE TABLE channels (
			channel_id   INTEGER PRIMARY KEY,
			access_hash  INTEGER NOT NULL DEFAULT 0,
			username     TEXT    NOT NULL DEFAULT '',
			title        TEXT    NOT NULL DEFAULT '',
			folder       TEXT    NOT NULL DEFAULT '',
			head_id      INTEGER NOT NULL DEFAULT 0,
			backfill_off INTEGER NOT NULL DEFAULT 0,
			backfill_done  INTEGER NOT NULL DEFAULT 0,
			updated_at     INTEGER NOT NULL DEFAULT 0
		);
		INSERT INTO channels (channel_id, access_hash, title, folder, head_id)
		VALUES (3331047977, 42, 'old', 'old', 196);`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}
	raw.Close()

	store, err := OpenStore(path)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	defer store.Close()

	got, err := store.Channel(3331047977)
	if err != nil || got == nil {
		t.Fatalf("load migrated channel: %v %v", got, err)
	}
	if got.Kind != "channel" {
		t.Fatalf("kind should default to channel, got %q", got.Kind)
	}
	if got.Head != 196 || got.AccessHash != 42 {
		t.Fatalf("migration lost data: %+v", got)
	}

	if err := store.PutChannel(&ChannelState{
		ChannelID: 5075548374, Kind: "chat", Title: "群", Folder: "群",
	}); err != nil {
		t.Fatalf("put chat: %v", err)
	}
	got, err = store.Channel(5075548374)
	if err != nil || got == nil {
		t.Fatalf("load chat: %v %v", got, err)
	}
	if got.Kind != "chat" {
		t.Fatalf("kind not persisted, got %q", got.Kind)
	}
}

func TestLookupVideo(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if v, err := store.LookupVideo(1, 2); err != nil || v != nil {
		t.Fatalf("expected (nil, nil), got (%v, %v)", v, err)
	}

	want := Video{
		ChannelID: 1, MessageID: 2, FileName: "a.mp4",
		FilePath: "/x/a.mp4", FileSize: 10, Duration: 30,
	}
	if err := store.RecordVideo(want); err != nil {
		t.Fatalf("record: %v", err)
	}
	got, err := store.LookupVideo(1, 2)
	if err != nil || got == nil {
		t.Fatalf("lookup: %v %v", got, err)
	}
	if got.FilePath != want.FilePath || got.FileSize != 10 || got.Duration != 30 {
		t.Fatalf("unexpected record: %+v", got)
	}
}

func TestAlreadyHave(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	present := filepath.Join(dir, "present.mp4")
	if err := os.WriteFile(present, make([]byte, 100), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	for msg, path := range map[int]string{
		1: present,                              // file still there
		2: filepath.Join(dir, "moved-away.mp4"), // file archived elsewhere
	} {
		if err := store.RecordVideo(Video{
			ChannelID: 7, MessageID: msg, FileName: filepath.Base(path),
			FilePath: path, FileSize: 100,
		}); err != nil {
			t.Fatalf("record: %v", err)
		}
	}

	mk := func(redownload bool) *dl {
		return &dl{
			store: store,
			cfg:   &Config{RedownloadMissing: redownload},
			log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
			ref:   channelRef{ID: 7, Folder: "ch", Kind: kindChannel},
		}
	}
	msg := func(id int, size int64) video {
		return video{MessageID: id, Doc: &tg.Document{Size: size}}
	}

	t.Run("record alone is enough", func(t *testing.T) {
		d := mk(false)
		// Missing file: still a hit, because state.db is authoritative.
		if ok, rec := d.alreadyHave(msg(2, 100)); !ok || rec == nil {
			t.Fatal("a recorded message must not be downloaded again")
		}
		if ok, _ := d.alreadyHave(msg(1, 100)); !ok {
			t.Fatal("present file must be skipped")
		}
		if ok, _ := d.alreadyHave(msg(99, 100)); ok {
			t.Fatal("unknown message must be downloaded")
		}
	})

	t.Run("redownload_missing_files", func(t *testing.T) {
		d := mk(true)
		if ok, _ := d.alreadyHave(msg(1, 100)); !ok {
			t.Fatal("present file with matching size must be skipped")
		}
		if ok, _ := d.alreadyHave(msg(2, 100)); ok {
			t.Fatal("missing file must be re-downloaded in this mode")
		}
		if ok, _ := d.alreadyHave(msg(1, 999)); ok {
			t.Fatal("size mismatch must trigger a re-download in this mode")
		}
	})
}

func TestBackfillPerChannel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	writeFile(t, path, `
api_id: 1
api_hash: x
backfill: true
channels:
  - channel: "@archive"
  - channel: "-5075548374"
    backfill: false
`)
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if !cfg.BackfillFor(cfg.Channels[0]) {
		t.Fatal("channel 0 should inherit backfill: true")
	}
	if cfg.BackfillFor(cfg.Channels[1]) {
		t.Fatal("channel 1 should override backfill: false")
	}
}

func TestBackfillToggleIsReversible(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if err := store.PutChannel(&ChannelState{ChannelID: 1, Kind: "channel", Head: 500}); err != nil {
		t.Fatalf("put channel: %v", err)
	}
	d := &dl{
		store: store,
		cfg:   &Config{},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		ref:   channelRef{ID: 1, Kind: kindChannel},
	}
	// head is already known, so no API call is made.
	if err := d.skipBackfill(context.Background(), &ChannelState{ChannelID: 1, Head: 500}); err != nil {
		t.Fatalf("skipBackfill: %v", err)
	}

	got, err := store.Channel(1)
	if err != nil || got == nil {
		t.Fatalf("load channel: %v %v", got, err)
	}
	if got.BackfillDone {
		t.Fatal("backfill: false must not latch backfill_done; it would make the option irreversible")
	}
	if got.Head != 500 {
		t.Fatalf("head must be preserved, got %d", got.Head)
	}
	if got.BackfillOffset != 0 {
		t.Fatalf("backfill offset must stay untouched, got %d", got.BackfillOffset)
	}
}
