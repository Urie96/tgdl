package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
)

func main() {
	if err := run(); err != nil {
		if errors.Is(err, context.Canceled) {
			return
		}
		fmt.Fprintf(os.Stderr, "\ntgdl: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		configPath = flag.String("config", "config.yaml", "path to config.yaml")
		cmd        = flag.String("cmd", "run", `command: "run", "login" or "stats"`)
		once       = flag.Bool("once", false, "process everything once and exit")
	)
	flag.Parse()

	cfg, err := LoadConfig(*configPath)
	if err != nil {
		return err
	}
	setupLogger(cfg.LogLevel)

	store, err := OpenStore(cfg.StateFile)
	if err != nil {
		return err
	}
	defer store.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := newClient(cfg)

	switch *cmd {
	case "login":
		return client.Run(ctx, func(ctx context.Context) error {
			return login(ctx, client, cfg)
		})
	case "stats":
		return printStats(store)
	case "run":
		err := client.Run(ctx, func(ctx context.Context) error {
			if err := ensureAuthorized(ctx, client); err != nil {
				return err
			}
			return serve(ctx, client.API(), store, cfg, *once)
		})
		if ctx.Err() != nil {
			// Interrupted via SIGTERM/SIGINT: that is a normal outcome.
			return nil
		}
		return err
	default:
		return fmt.Errorf("unknown -cmd %q", *cmd)
	}
}

func setupLogger(level string) {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	// Text handler to stderr: systemd/journald friendly, one event per line.
	handler := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: lv,
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				a.Value = slog.StringValue(time.Now().Format("15:04:05"))
			}
			return a
		},
	})
	slog.SetDefault(slog.New(handler))
}

func printStats(store *Store) error {
	st, err := store.Stats()
	if err != nil {
		return err
	}
	fmt.Printf("channels=%d videos=%d size=%s pending_failures=%d\n",
		st.Channels, st.Videos, humanBytes(st.Bytes), st.Failures)
	return nil
}

// serve resolves all channels and then polls them until ctx is cancelled.
func serve(ctx context.Context, api *tg.Client, store *Store, cfg *Config, once bool) error {
	log := slog.Default()

	resolved := make([]channelRef, 0, len(cfg.Channels))
	for _, spec := range cfg.Channels {
		if !spec.IsEnabled() {
			log.Info("channel disabled, skipping", "channel", spec.Channel)
			continue
		}
		ref, err := resolveChannel(ctx, api, store, spec)
		if err != nil {
			log.Error("cannot resolve channel", "channel", spec.Channel, "err", err)
			continue
		}
		log.Info("channel ready", "channel", ref.String(), "folder", ref.Folder)
		resolved = append(resolved, ref)
	}
	if len(resolved) == 0 {
		return errors.New("no usable channels configured")
	}

	sync := func() {
		for _, ref := range resolved {
			if err := syncChannel(ctx, api, store, cfg, ref); err != nil {
				if ctx.Err() != nil {
					return
				}
				log.Error("sync failed", "channel", ref.String(), "err", err)
			}
		}
		if st, err := store.Stats(); err == nil {
			log.Info("state", "videos", st.Videos, "size", humanBytes(st.Bytes))
		}
	}

	sync()
	if once {
		log.Info("single pass finished")
		return nil
	}

	log.Info("polling", "interval", cfg.PollInterval.D().String())
	ticker := time.NewTicker(cfg.PollInterval.D())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Info("shutting down")
			return nil
		case <-ticker.C:
			sync()
		}
	}
}

// syncChannel runs the historical sweep (once) and the incremental catch-up.
func syncChannel(ctx context.Context, api *tg.Client, store *Store, cfg *Config, ref channelRef) error {
	log := slog.Default()
	state, err := store.Channel(ref.ID)
	if err != nil {
		return err
	}
	if state == nil {
		return fmt.Errorf("channel %d not persisted", ref.ID)
	}

	d := &dl{
		api:     api,
		dl:      downloader.NewDownloader(),
		store:   store,
		cfg:     cfg,
		filters: cfg.FilterFor(ref.Spec),
		log:     log.With("channel", ref.Folder),
		ref:     ref,
	}

	if !state.BackfillDone {
		if cfg.BackfillFor(ref.Spec) {
			if err := d.sweep(ctx, state); err != nil {
				return err
			}
		} else if err := d.skipBackfill(ctx, state); err != nil {
			return err
		}
	}
	return d.catchUp(ctx, state)
}

// sweep walks the channel from newest to oldest once, downloading the archive.
//
// The floor cursor is persisted after every page, so an interrupted sweep
// resumes exactly where it stopped. Already-downloaded files are detected on
// disk and adopted, so re-scanning is cheap.
func (d *dl) sweep(ctx context.Context, state *ChannelState) error {
	offset := state.BackfillOffset
	maxID := state.Head

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := nextPage(ctx, d.api, d.ref.InputPeer(), 0, offset)
		if err != nil {
			return fmt.Errorf("fetch history: %w", err)
		}
		if len(page.Messages) == 0 {
			break
		}
		if page.Highest > maxID {
			maxID = page.Highest
			// Persist the head marker as we go: if the sweep is interrupted,
			// the catch-up pass must not restart from a stale value.
			if err := d.store.SetHead(d.ref.ID, maxID); err != nil {
				return err
			}
		}
		complete, err := d.processPage(ctx, page)
		if err != nil {
			return err
		}
		if !complete {
			// A message failed: keep the floor where it is and retry this
			// page on the next poll tick. Files already downloaded are
			// adopted, so re-scanning the page is cheap.
			return nil
		}

		offset = page.Lowest
		if err := d.store.SetBackfill(d.ref.ID, offset, page.Short); err != nil {
			return err
		}
		if page.Short {
			state.BackfillDone = true
			break
		}
	}

	// Persist sweep completion together with the final head marker.
	if err := d.store.SetHead(d.ref.ID, maxID); err != nil {
		return err
	}
	state.Head = maxID
	if !state.BackfillDone {
		if err := d.store.SetBackfill(d.ref.ID, offset, true); err != nil {
			return err
		}
		state.BackfillDone = true
	}
	d.log.Info("historical sweep finished", "videos", d.storeVideoCount())
	return nil
}

// skipBackfill honours `backfill: false` for one run.
//
// On a fresh source the head marker is first moved to the newest message, so
// catch-up does not replay the whole history; an existing head (e.g. a migrated
// one) is kept so the downloader continues from where it was.
//
// backfill_done is deliberately NOT written here: that flag means "a real sweep
// finished". Writing it on a config toggle made the option a one-way latch —
// turning backfill back on would silently do nothing. Leaving it alone means
// `backfill: false` → `backfill: true` resumes the archive scan as expected.
func (d *dl) skipBackfill(ctx context.Context, state *ChannelState) error {
	if state.Head == 0 {
		page, err := nextPage(ctx, d.api, d.ref.InputPeer(), 0, 0)
		if err != nil {
			return fmt.Errorf("fetch newest message: %w", err)
		}
		if page.Highest > 0 {
			if err := d.store.SetHead(d.ref.ID, page.Highest); err != nil {
				return err
			}
			state.Head = page.Highest
			d.log.Info("backfill disabled, starting from the newest message",
				"head", state.Head)
		}
	}
	return nil
}

// catchUp downloads messages newer than the last seen message id.
//
// The head marker is only advanced after the whole pass, and old messages that
// are already on disk are adopted instead of downloaded again, so a crash
// mid-pass is harmless.
func (d *dl) catchUp(ctx context.Context, state *ChannelState) error {
	start := state.Head
	offset := 0
	maxID := start
	found := 0

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		page, err := nextPage(ctx, d.api, d.ref.InputPeer(), start, offset)
		if err != nil {
			return fmt.Errorf("fetch new messages: %w", err)
		}
		if len(page.Messages) == 0 {
			break
		}
		if page.Highest > maxID {
			maxID = page.Highest
		}
		found += len(page.Messages)
		complete, err := d.processPage(ctx, page)
		if err != nil {
			return err
		}
		if !complete {
			// Leave head untouched so this page is retried next tick.
			return nil
		}
		if page.Short {
			break
		}
		offset = page.Lowest
	}

	if maxID != start {
		if err := d.store.SetHead(d.ref.ID, maxID); err != nil {
			return err
		}
		state.Head = maxID
	}
	if found > 0 {
		d.log.Info("catch-up finished", "messages", found)
	}
	return nil
}

// processPage downloads every video of an ascending page.
//
// It reports whether the whole page was handled. A false result with a nil
// error means a download failed and the caller should retry the page on the
// next tick without advancing its cursor.
func (d *dl) processPage(ctx context.Context, page historyPage) (bool, error) {
	for _, mc := range page.Messages {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		msg, ok := mc.(*tg.Message)
		if !ok {
			continue
		}
		if err := d.processMessage(ctx, mc); err != nil {
			// A shutdown (SIGTERM / Ctrl-C) is not a download failure: it must
			// not consume a retry, otherwise repeated restarts permanently
			// skip the video that happened to be in flight.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return false, ctxErr
			}
			if isFatalChannelError(err) {
				return false, fmt.Errorf("message %d: %w", msg.ID, err)
			}
			attempts, rerr := d.store.RecordFailure(d.ref.ID, msg.ID, err)
			if rerr != nil {
				d.log.Warn("cannot record failure", "err", rerr)
			}
			if attempts >= d.cfg.Retries {
				d.log.Error("giving up on message",
					"message", msg.ID, "attempts", attempts, "err", err)
				continue
			}
			d.log.Warn("download failed, will retry",
				"message", msg.ID, "attempt", attempts, "err", err)
			return false, nil
		}
	}
	return true, nil
}

// processMessage downloads one message if it carries an acceptable video.
func (d *dl) processMessage(ctx context.Context, mc tg.MessageClass) error {
	v, ok := extractVideo(mc)
	if !ok {
		return nil
	}
	if reason := d.skipReason(v); reason != "" {
		d.log.Debug("skipping video", "message", v.MessageID, "reason", reason)
		return nil
	}
	return d.run(ctx, v)
}

func (d *dl) storeVideoCount() int {
	st, err := d.store.Stats()
	if err != nil {
		return 0
	}
	return st.Videos
}
