package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/telegram/downloader/fileref"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

// progressEvery throttles progress logging. gotd calls the progress callback
// once per 512 KiB part, i.e. thousands of times for a large video; logging
// each call is what made the Python version burn CPU and flood the journal.
const progressEvery = 5 * time.Second

// fileDCProvider hands out invokers bound to one datacenter.
//
// *telegram.Client implements it. It is needed because Telegram answers
// upload.getFileHashes only on the DC that stores the file: any other DC
// replies LOCATION_INVALID (upload.getFile, by contrast, replies FILE_MIGRATE,
// which gotd follows automatically). A video uploaded to a different DC than the
// account's home DC therefore failed hash verification — and with it the whole
// download — even though the file itself was reachable.
type fileDCProvider interface {
	FileDC(ctx context.Context, dc int) (tg.Invoker, error)
}

// dl downloads videos for one channel.
type dl struct {
	api *tg.Client
	// client is used to reach the DC that stores a document. May be nil, in
	// which case every request goes through api.
	client fileDCProvider
	// homeDC is the session's datacenter. Documents on other DCs need a
	// dedicated connection (see fileAPI).
	homeDC  int
	dl      *downloader.Downloader
	store   *Store
	cfg     *Config
	filters Filters
	log     *slog.Logger
	ref     channelRef
}

// fileAPI returns the client to use for one document's upload.* requests: one
// bound to the DC that stores the file, or the home-DC client when the DC is
// unknown, matches the session's DC, or cannot be reached.
func (d *dl) fileAPI(ctx context.Context, dc int) *tg.Client {
	if dc <= 0 || dc == d.homeDC || d.client == nil {
		return d.api
	}
	inv, err := d.client.FileDC(ctx, dc)
	if err != nil {
		d.log.Warn("cannot reach file DC, falling back to home DC", "dc", dc, "err", err)
		return d.api
	}
	d.log.Debug("using file DC", "dc", dc)
	return tg.NewClient(inv)
}

// targetPath builds the final destination path for a video.
//
// Layout:
//
//	{download_dir}/{channel}/{YYYY}/{YYYYMMDD-HHMMSS}_{message_id}_{name}.{ext}
//
// The message id makes every filename unique (no -copy suffixes, no hashing)
// and keeps the directory sortable by date.
func (d *dl) targetPath(v video) string {
	stem, ext := splitExt(v.FileName)
	if ext == "" {
		ext = extFromMime(v.Doc.MimeType)
	}
	if v.FileName == "" {
		stem = "video"
	}
	date := v.Date.In(time.Local)
	dir := filepath.Join(
		d.cfg.DownloadDir,
		d.ref.Folder,
		fmt.Sprintf("%04d", date.Year()),
	)
	name := fmt.Sprintf("%s_%d_%s%s",
		date.Format("20060102-150405"), v.MessageID, stem, ext)
	return filepath.Join(dir, name)
}

// skipReason returns why a video should not be downloaded, or "" to proceed.
func (d *dl) skipReason(v video) string {
	switch {
	case d.filters.MaxFileSize > 0 && v.Size() > d.filters.MaxFileSize:
		return fmt.Sprintf("larger than max_file_size (%d > %d)", v.Size(), d.filters.MaxFileSize)
	case d.filters.MinDuration > 0 && v.Duration < d.filters.MinDuration:
		return fmt.Sprintf("shorter than min_duration (%ds < %ds)", v.Duration, d.filters.MinDuration)
	case d.filters.MaxDuration > 0 && v.Duration > d.filters.MaxDuration:
		return fmt.Sprintf("longer than max_duration (%ds > %ds)", v.Duration, d.filters.MaxDuration)
	}
	return ""
}

// run downloads one video, resuming from a partial file when present.
func (d *dl) run(ctx context.Context, v video) error {
	path := d.targetPath(v)
	partPath := path + ".part"

	// state.db is the source of truth: a recorded message was downloaded
	// completely, so it is skipped even if the file has since been moved or
	// deleted. Set redownload_missing_files to also require the file on disk.
	if done, rec := d.alreadyHave(v); done {
		d.log.Debug("already downloaded, skipping",
			"message", v.MessageID, "recorded_at", rec.FilePath)
		return nil
	}

	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		// Already on disk from an earlier run: adopt it instead of
		// re-downloading (the Python version re-hashed entire files here,
		// which is what caused the memory/CPU spikes).
		if v.Size() == 0 || info.Size() == v.Size() {
			d.log.Info("already downloaded, adopting", "file", path)
			return d.store.RecordVideo(d.record(v, path))
		}
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove stale file: %w", err)
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create directory: %w", err)
	}

	loc := &tg.InputDocumentFileLocation{
		ID:            v.Doc.ID,
		AccessHash:    v.Doc.AccessHash,
		FileReference: v.Doc.FileReference,
	}

	// download and verify on the DC that stores the file; refresh (re-reading
	// the message for a fresh file_reference) stays on the primary client.
	b := d.dl.Download(d.fileAPI(ctx, v.Doc.DCID), loc).
		WithThreads(d.cfg.Threads).
		WithSize(v.Size()).
		WithVerify(true).
		WithRefresh(fileref.Message(d.api, d.ref.InputPeer(), v.MessageID, loc)).
		WithRetryHandler(d.onRetry(v)).
		WithProgress(d.onProgress(v))

	// A partial file may contain holes: gotd writes parts with WriteAt from
	// several goroutines, so its size alone does not prove the prefix is
	// complete. Verify the prefix against server hashes and resume from the
	// last verified hash window instead of guessing.
	resume, err := d.verifiedPrefix(ctx, b, partPath)
	if err != nil {
		d.log.Warn("partial file is not resumable, starting over", "err", err)
		resume = 0
	}
	if v.Size() > 0 && resume >= v.Size() {
		// Fully verified: a previous run finished but died before the rename.
		if err := os.Rename(partPath, path); err != nil {
			return fmt.Errorf("finalize partial: %w", err)
		}
		v.Doc.Size = resume
		d.log.Info("completed from partial file", "file", path)
		return d.store.RecordVideo(d.record(v, path))
	}
	switch {
	case resume > 0:
		d.log.Info("resuming partial download",
			"file", path,
			"done", humanBytes(resume),
			"total", humanBytes(v.Size()))
		b = b.WithOffset(resume)
	case fileExists(partPath):
		if err := os.Remove(partPath); err != nil {
			return fmt.Errorf("remove unusable partial: %w", err)
		}
	}

	start := time.Now()
	if _, err := b.ToPath(ctx, partPath); err != nil {
		return fmt.Errorf("download: %w", err)
	}
	if err := os.Rename(partPath, path); err != nil {
		return fmt.Errorf("finalize download: %w", err)
	}

	size := v.Size()
	if info, err := os.Stat(path); err == nil {
		size = info.Size()
		v.Doc.Size = size
	}
	d.log.Info("downloaded",
		"file", path,
		"size", humanBytes(size),
		"took", time.Since(start).Round(time.Second).String(),
		"speed", humanRate(size-resume, time.Since(start)),
	)
	return d.store.RecordVideo(d.record(v, path))
}

// partSize must match downloader.Downloader's part size (512 KiB by default).
const partSize = 512 << 10

// verifiedPrefix returns how many leading bytes of the partial file are
// proven correct by server hashes, rounded down to a part boundary.
func (d *dl) verifiedPrefix(ctx context.Context, b *downloader.Builder, partPath string) (int64, error) {
	f, err := os.Open(partPath)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size() <= 0 {
		return 0, nil
	}

	verified, err := b.Verify(ctx, f, info.Size())
	if err != nil {
		return 0, err
	}
	return verified - verified%partSize, nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// alreadyHave reports whether the message is already recorded as downloaded.
//
// By default the presence of a record is enough: the file was fully written and
// verified before RecordVideo was called, so moving the media elsewhere (or
// deleting it) does not make it a candidate for another download.
func (d *dl) alreadyHave(v video) (bool, *Video) {
	rec, err := d.store.LookupVideo(d.ref.ID, v.MessageID)
	if err != nil {
		d.log.Warn("cannot read download history", "err", err)
		return false, nil
	}
	if rec == nil {
		return false, nil
	}
	if !d.cfg.RedownloadMissing {
		return true, rec
	}

	info, statErr := os.Stat(rec.FilePath)
	switch {
	case statErr != nil:
		d.log.Info("recorded file is gone, re-downloading",
			"message", v.MessageID, "path", rec.FilePath)
		return false, rec
	case info.IsDir():
		return false, rec
	case v.Size() > 0 && info.Size() != v.Size():
		d.log.Info("recorded file has the wrong size, re-downloading",
			"message", v.MessageID, "path", rec.FilePath,
			"size", info.Size(), "expected", v.Size())
		return false, rec
	}
	return true, rec
}

func (d *dl) record(v video, path string) Video {
	return Video{
		ChannelID: d.ref.ID,
		MessageID: v.MessageID,
		FileName:  filepath.Base(path),
		FilePath:  path,
		FileSize:  v.Size(),
		Duration:  v.Duration,
		Width:     v.Width,
		Height:    v.Height,
	}
}

// onProgress returns a throttled progress reporter. gotd guarantees these
// calls are not concurrent.
func (d *dl) onProgress(v video) downloader.ProgressFunc {
	var lastLog atomic.Int64
	lastLog.Store(time.Now().Add(-progressEvery).UnixNano())
	total := v.Size()

	return func(done, size int64) {
		if size > 0 {
			total = size
		}
		now := time.Now()
		if now.UnixNano()-lastLog.Load() < int64(progressEvery) {
			return
		}
		lastLog.Store(now.UnixNano())

		if total <= 0 {
			d.log.Info("progress", "message", v.MessageID, "done", humanBytes(done))
			return
		}
		pct := float64(done) / float64(total) * 100
		d.log.Info("progress",
			"message", v.MessageID,
			"percent", fmt.Sprintf("%.1f%%", pct),
			"done", humanBytes(done),
			"total", humanBytes(total),
		)
	}
}

// onRetry logs transient downloader retries, notably FLOOD_WAIT, which gotd
// already handles by waiting internally.
func (d *dl) onRetry(v video) downloader.RetryHandler {
	return func(e downloader.RetryEvent) {
		attrs := []any{
			"message", v.MessageID,
			"op", e.Operation,
			"attempt", e.Attempt,
			"err", e.Err,
		}
		if wait, ok := tgerr.AsFloodWait(e.Err); ok {
			attrs = append(attrs, "flood_wait", wait.String())
		}
		d.log.Warn("retrying download", attrs...)
	}
}

// isAuthError reports errors that mean the session itself is unusable: the pass
// cannot succeed on the next poll tick either, somebody has to log in again.
//
// Telegram answers 401 for every flavour of "this auth key is not valid", so the
// code is checked as well as the exact types.
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrNotAuthorized) {
		return true
	}
	if tgerr.IsCode(err, 401) {
		return true
	}
	return tgerr.Is(err,
		"AUTH_KEY_UNREGISTERED",
		"AUTH_KEY_INVALID",
		"AUTH_KEY_DUPLICATED",
		"SESSION_REVOKED",
		"SESSION_EXPIRED",
		"USER_DEACTIVATED",
		"USER_DEACTIVATED_BAN",
	)
}

// isFatalChannelError reports errors that mean the whole channel is unusable
// (bad access hash, kicked, no access), so the pass should stop immediately.
func isFatalChannelError(err error) bool {
	if err == nil {
		return false
	}
	if isAuthError(err) {
		return true
	}
	for _, typ := range []string{
		"CHANNEL_INVALID",
		"CHANNEL_PRIVATE",
		"CHANNEL_PUBLIC_GROUP_NA",
		"CHAT_FORBIDDEN",
		"USER_BANNED_IN_CHANNEL",
	} {
		if tgerr.Is(err, typ) {
			return true
		}
	}
	return false
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func humanRate(bytes int64, d time.Duration) string {
	if d <= 0 {
		return "n/a"
	}
	return humanBytes(int64(float64(bytes)/d.Seconds())) + "/s"
}
