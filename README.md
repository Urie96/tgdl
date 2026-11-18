# tgdl

A single-binary Go rewrite of the Telegram media downloader, scoped to one job:
**automatically download videos from a set of Telegram channels**, as a
long-running systemd service.

Everything from the Python version that was not needed for that job (NiceGUI
web UI, audio/photo/document/voice handling, `ids_to_retry` bookkeeping in YAML,
full-file MD5 de-duplication) is gone.

```
tgdl/
  main.go        service loop, sweep + catch-up passes
  config.go      YAML config
  source.go      history pagination, video detection
  download.go    download, resume, progress
  store.go       SQLite state (channels, videos, failures)
  telegram.go    gotd client, login, channel resolution
  naming.go      path / filename sanitising
```

## Build

```sh
cd tgdl
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o tgdl .
```

Static, no cgo, ~20 MB. Cross-compile with `GOOS=linux GOARCH=arm64`.

## Setup

```sh
sudo useradd --system --home /var/lib/tgdl --shell /usr/sbin/nologin tgdl
sudo install -d -o tgdl -g tgdl /var/lib/tgdl
sudo cp config.example.yaml /var/lib/tgdl/config.yaml
sudo -u tgdl $EDITOR /var/lib/tgdl/config.yaml      # api_id, api_hash, phone, channels

# one-time interactive login (writes session.json; must run as the same user)
sudo -u tgdl /usr/local/bin/tgdl -config /var/lib/tgdl/config.yaml -cmd login

sudo install -m755 tgdl /usr/local/bin/tgdl
sudo install -m644 tgdl.service /etc/systemd/system/tgdl.service
sudo systemctl enable --now tgdl
journalctl -u tgdl -f
```

Use `-cmd login` from a terminal: it wants the login code (and 2FA password)
on stdin. The service itself never prompts — if the session is missing it
exits with an explicit error instead of hanging.

## How it works

### Two cursors, two passes

State lives in SQLite, not in the YAML file. Rewriting YAML on every batch was
both fragile (comments lost) and racy. The config is read-only now.

Per channel the service keeps:

* `head_id` — highest message id ever processed.
* `backfill_off` / `backfill_done` — how far the historical sweep got.

Each poll tick does:

1. **Sweep** (only until `backfill_done`): walk the channel newest → oldest,
   processing each 100-message page oldest → newest, and persist the floor
   cursor after every page.
2. **Catch-up**: fetch everything above `head_id`, descending, again processing
   each page oldest → newest. One API call per idle tick.

Splitting the two avoids the classic bug of a single cursor: if you paginate
*descending* and store "highest id seen", you can never reach the older
messages again. If you paginate ascending you have to walk the whole channel to
find the end first. Two cursors give O(1) resume in both directions.

### Idempotency without hashing

The destination filename contains the message id, so it is unique by
construction:

```
{download_dir}/{source}/{YYYY}/{YYYYMMdd-HHmmss}_{message_id}_{name}.{ext}
```

**`state.db` is the source of truth.** A row in `videos` means the file was
written completely and its chunks passed hash verification, so the message is
never downloaded again — even if the file has since been moved to another disk
or deleted. That is what makes an external archive workflow possible, and it
is also what lets media downloaded by the old Python tool be recognised even
though its paths and filenames are completely different.

To make the file system authoritative instead, set:

```yaml
redownload_missing_files: true   # missing/wrong-sized file triggers a re-download
```

With the default (`false`), deleting a row from the `videos` table is the way
to force a re-download:

```sh
sqlite3 state.db 'DELETE FROM videos WHERE channel_id = 3331047977 AND message_id = 97'
```

### Resume

Downloads go to `{final}.part`, then a rename. On restart the prefix is verified
against Telegram's file hashes with `downloader.Builder.Verify`, and the
download resumes from the last verified hash window. This matters because gotd
writes parts with `WriteAt` from several goroutines, so a partial file's *size*
does not prove its prefix is complete — a naive `size`-based resume can produce
a video with a hole in the middle.

### Video detection

A message is downloaded when its media document carries a
`DocumentAttributeVideo`, and is **not**:

* `round_message` — selfie-style video notes,
* carrying `DocumentAttributeAnimated` — GIF-style mp4 animations.

The mime type is deliberately not checked: videos posted "as file" are often
tagged `application/octet-stream` while carrying the correct attribute,
which is what the official clients classify on. Everything else (photos,
audio, documents, voice, stickers) is ignored silently.

The `filters` block (`max_file_size`, `min_duration`, `max_duration`) narrows
the set further; it does not define what counts as a video.

## Why the Python version spiked the CPU

Two separate bugs, both gone here:

1. **`cryptg` was not installed.** Telethon falls back to `pyaes`, a pure-Python
   AES-IGE implementation, for *every* 512 KiB MTProto chunk. That is the main
   cause: the encryption path becomes pure interpreted Python. (Verified:
   `.venv` had `pyaes` and no `cryptg`.) Go uses AES-NI through `crypto/aes`.
2. **`manage_duplicate_file()` read whole files into memory** and MD5'd them —
   `md5(open(path, "rb").read())` — for the downloaded file *and* every glob
   match, on every duplicate name. For a 2 GB video that is 2 GB of RAM plus a
   full-file hash. The Go version has no duplicate handling at all, because the
   message id in the filename makes duplicates impossible.

Progress logging is also throttled to one line per 5 s (`progressEvery`),
whereas the Python version pushed every chunk into `tqdm` *and* into
`UI_PROGRESS_HOOK`.

## Commands

```sh
tgdl -config cfg.yaml                 # run as a service (poll forever)
tgdl -config cfg.yaml -once           # one pass, then exit (systemd timer)
tgdl -config cfg.yaml -cmd login      # interactive login
tgdl -config cfg.yaml -cmd stats      # print counters
```

`backfill: true` (default) downloads the channel's existing archive on the
first run; `backfill: false` starts from the newest message and only picks up
new videos. It is global by default and can be overridden per source. The
choice is applied once, when the source's `backfill_done` flag is first set.

## Channels vs basic groups

Two different MTProto peer types are supported:

| | basic group | channel / supergroup |
|---|---|---|
| TL type | `tg.Chat` | `tg.Channel` |
| marked id | `-<id>` | `-100<id>` |
| input peer | `InputPeerChat` | `InputPeerChannel` |
| access hash | none | required |
| member limit | 200 | unlimited |
| `@username` | — | yes |

Broadcast channels and supergroups are the *same* TL type, distinguished only
by the `broadcast` / `megagroup` flags. Basic groups are a separate type, which
is why a group works without an access hash and cannot be addressed by
username.

`tgdl` picks the type from the id prefix: `-100…` is a channel/supergroup,
anything else negative is a basic group. A basic group that was upgraded to a
supergroup is followed automatically through `chat.migrated_to`.

Messages are listed with `messages.getHistory` and file references are
refreshed with `fileref.Message`, both of which already branch on the peer type
internally.

## Storage layout

```
/var/lib/tgdl/
  config.yaml
  session.json      # MTProto auth key — treat as a password, 0600
  state.db          # SQLite (WAL): channels, videos, failures
  downloads/
    <channel>/
      2024/
        20240305-143005_1234_big clip.mp4
```

## Failure handling

* A failed message increments a counter in `failures` and the pass stops, so it
  is retried on the next tick.
* After `retries` attempts the message is skipped permanently with an error
  log, so one broken video cannot block a channel forever.
* `FLOOD_WAIT` is handled by gotd internally (it sleeps and retries); the retry
  handler only logs it.
* Fatal channel errors (`CHANNEL_INVALID`, `CHANNEL_PRIVATE`, `SESSION_REVOKED`,
  …) abort the pass immediately and are logged; the next tick retries, which
  covers transient cases like a reconnect.

## Notes and limitations

* Sources are processed sequentially. Downloads for one block the next; this
  keeps the flood-wait budget predictable. Give each source its own service
  instance with a separate `state.db` if you need parallelism.
* Only `*tg.Message` is inspected; stories, photos and albums-only messages are
  skipped.
* `channels[].channel` must be a channel/supergroup username, a `-100…` id or a
  `-…` basic-group id. Usernames do not exist for basic groups — MTProto's
  `Chat` has no `username` field.
* No proxy support yet. If you need one, put the service behind WireGuard or
  add a `dcs.Plain` dialer with a SOCKS5 `DialFunc`.
* The session format is gotd's JSON, **not** Telethon's `.session` SQLite file.
  You must log in once with `-cmd login`.

## Migrating an existing Telethon install

Three things are worth carrying over.

**1. Credentials.** Go's YAML decoding wants `api_id` as an *unquoted* integer;
Python configs usually have `api_id: '10688019'`, which fails here.

**2. The channel's `access_hash`.** A numeric `channel: -100...` entry cannot be
resolved without one. It is stored in the Telethon session:

```sh
sqlite3 media_downloader.session 'select id, hash from entities'
```

Create the schema first (`tgdl -cmd stats`), then seed it:

```sh
sqlite3 state.db "INSERT INTO channels
  (channel_id, access_hash, username, title, folder,
   head_id, backfill_off, backfill_done, updated_at)
  VALUES (3331047977, -4964861933056236318, '', 'mychannel', 'mychannel',
          196, 0, 0, strftime('%s','now'))
  ON CONFLICT(channel_id) DO UPDATE SET access_hash = excluded.access_hash;"
```

`-1003331047977` → `channel_id = 3331047977` (strip the `-100` prefix). Seeding
`head_id` from the old `last_read_message_id` preserves the download position:
with `backfill: true` the sweep covers the archive anyway, with `backfill:
false` the run continues from that message.

**3. Download history.** `downloads.sqlite3` is worth importing: it is the only
record that a message was already downloaded, and with the default
`redownload_missing_files: false` an imported row permanently suppresses that
download. Import every video row, including ones whose file has since been
moved:

```sh
sqlite3 state.db "ATTACH 'downloads.sqlite3' AS old;
  INSERT INTO videos
    (channel_id, message_id, file_name, file_path, file_size,
     duration, width, height, downloaded_at)
  SELECT
    CASE WHEN old.download_history.chat_id LIKE '-100%'
         THEN CAST(substr(old.download_history.chat_id, 5) AS INTEGER)
         ELSE -CAST(old.download_history.chat_id AS INTEGER) END,
    message_id, file_name, COALESCE(file_path, ''), file_size,
    0, 0, 0, strftime('%s','now')
  FROM old.download_history
  WHERE media_type = 'video'
  ON CONFLICT(channel_id, message_id) DO NOTHING;"
```

`-100<id>` becomes `<id>` and `-<id>` becomes `<id>`. Rows whose files are long
gone are still useful: they encode "already downloaded", which is exactly the
question the downloader asks. Delete rows from `videos` if you want any of
them re-fetched.
