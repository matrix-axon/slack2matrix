# slack2matrix

Imports a Slack workspace export into Matrix as an end-to-end encrypted space.
Each Slack channel becomes an encrypted room inside the space. Every message,
thread, reaction and file is imported with its original author and timestamp,
and all media is encrypted on the client before upload, so the homeserver
never stores a plaintext copy of the history.

## How it works

- **Appservice.** The tool registers with Synapse as an application service,
  which lets it act as other users and set `origin_server_ts` (`?ts=`) on
  messages, reactions and membership events.
- **Who sends what.**
  - Slack users you map to existing Matrix accounts send as those accounts,
    via a non-exclusive namespace.
  - Everyone else, and every bot, becomes a ghost account (`@slackimport_<name>`)
    with the Slack display name and avatar.
- **Encryption.**
  - The importer creates one device, `SLACKIMPORT`, on each account it sends
    as, and encrypts with its own Megolm sessions: one per room, sender and
    year of history.
  - Room keys reach the people who will read the history in three ways, each
    sufficient on its own:
    1. Olm to-device messages to every device they have during the import.
    2. Their server-side key backup, so any future device can restore the
       history.
    3. An Element-compatible key export file.
  - With a user's recovery key, the importer cross-signs its device for that
    user, so Element shows the history as verified. Ghost accounts get their
    own cross-signing identity.
- **Media.** Files are AES-CTR encrypted before upload.
  - Synapse can't thumbnail encrypted media, so the tool makes thumbnails
    itself: ImageMagick for images, ffmpeg for video poster frames.
  - HEIC and TIFF images are converted to JPEG so every client can display
    them.
- **Resumable.** Progress is stored in SQLite (`state_dir/import.db`), so you
  can stop and re-run at any time.
  - Each event's transaction ID is derived from its Slack message.
  - After a crash, events that were in flight are reconciled by decrypting the
    room's latest events and matching the Slack source embedded in each one.

### How Slack content is mapped

| Slack | Matrix |
|---|---|
| channel | encrypted room in the space; topic and purpose become the room topic |
| rich text (bold, italic, strike, code, quotes, lists, code blocks) | `formatted_body` HTML, plus a plain-text `body` |
| `@user`, `#channel` | pills with `m.mentions`; links to the imported room |
| links to Slack messages, channels and profiles | `matrix.to` links to the imported event, room or user (a link to a channel that wasn't imported is left as is) |
| emoji | Unicode, honouring skin tones; custom emoji stay as `:name:` |
| threads, including "also send to channel" | `m.thread` relations with reply fallback |
| reactions | `m.reaction` annotations from each reacting user (Slack has no reaction timestamps, so they use the message's) |
| files | `m.image`, `m.video`, `m.audio` or `m.file` with encrypted thumbnails; a single file plus text becomes one captioned event |
| snippets up to 16 KB | inline code block with a language hint |
| deleted or missing files | `[file deleted]` / `[file unavailable: name]` |
| bot and app cards (giphy, xkcd, Twitter, Reacji) | quoted HTML card; images re-hosted, falling back to a link if gone |
| link unfurls | skipped by default; `import.link_previews: true` embeds title/description as MSC4095 previews (no images), `import.unfurls: card` renders them as quoted cards |
| `/me` | `m.emote` |
| bot messages, Slackbot, system messages | `m.notice` from a ghost |
| legacy file comments | reply to the file's event |
| join / leave | timestamped membership changes |

## Requirements

- Go 1.26 or newer to build. The Makefile sets `GOEXPERIMENT=nojsonv2`; the
  binary refuses to run without it, because under json/v2 mautrix signs Olm
  one-time keys incorrectly.
- ImageMagick (`magick`) and ffmpeg (`ffmpeg`, `ffprobe`) on `PATH`.
- A Synapse homeserver you administer.
- An unpacked Slack export, with file downloads mirrored under
  `<export>/media/files.slack.com/...` (see below).

```sh
make build        # -> bin/slack2matrix
make test
```

### Downloading the files

A Slack export contains only JSON. Every file object in it points to the
content with a `url_private_download` URL, and that URL carries a token from
the export (`?t=xoxe-…`), so it can be fetched without signing in. Fetch the
files before importing.

slack2matrix finds each file at `media_dir` (default `<export>/media`)
followed by the URL without `https://`, kept verbatim. Percent escapes stay
as they are, and the query string is part of the file name:

```
https://files.slack.com/files-pri/T0123-F0456/download/my%20photo.jpg?t=xoxe-…
→ <export>/media/files.slack.com/files-pri/T0123-F0456/download/my%20photo.jpg?t=xoxe-…
```

This needs `jq` and `curl`. Run it from the export directory; it skips files
already downloaded, so it can be re-run.

```sh
cd /path/to/slack-export
# Every Slack-hosted file referenced by a message. Skip macOS "._" files,
# deleted files (tombstones) and links to external services.
find . -mindepth 2 -name '*.json' ! -name '.*' -not -path './media/*' -print0 |
  xargs -0 jq -r '.[] | (.files[]?, .attachments[]?.files[]?)
    | select(.mode != "tombstone" and .mode != "external")
    | .url_private_download // .url_private // empty
    | select(startswith("https://files.slack.com/"))' |
  sort -u > file-urls.txt

while IFS= read -r url; do
  out="media/${url#https://}"
  [ -s "$out" ] && continue
  # -g: URLs can contain [ ] { }, which curl would otherwise treat as globs.
  if ct=$(curl -fsSLg --create-dirs -o "$out" -w '%{content_type}' "$url"); then
    case $ct in text/html*) echo "check (got a web page): $out" >&2 ;; esac
  else
    echo "failed: $url" >&2
    rm -f "$out"
  fi
done < file-urls.txt
```

Then check what's still missing:

```sh
bin/slack2matrix plan -missing
```

The export's token may stop working after a while, or once the export is
deleted. If downloads fail, or the "check" lines show Slack's sign-in page
instead of the file, make a fresh export and run the script against it.
Files that can't be fetched are imported as a "[file could not be imported:
name]" notice.

## Runbook

1. **Create a config.**
   ```sh
   bin/slack2matrix init                       # writes config.yaml with fresh tokens
   ```
   Then edit `config.yaml`:
   - `homeserver.url` and `homeserver.server_name` (the part after `:` in user IDs).
   - `users`: Slack user ID (or username) → existing Matrix account, for each
     person. Unmapped users become ghosts.
   - `admins`: who gets admin rights in the space and rooms.
   - `crypto.recovery_keys`: for each mapped person, a file containing their
     Matrix recovery key. This is optional but recommended; see below.
   - Keep `state_dir` on a disk with room for the databases. Back it up, and
     keep it private: it contains every room key.

2. **Inspect the export.**
   ```sh
   bin/slack2matrix plan
   bin/slack2matrix render -channel general -sample 20   # preview conversions
   ```

3. **Install the appservice.**
   ```sh
   bin/slack2matrix registration -out slack-import-registration.yaml
   ```
   Add it to `homeserver.yaml`, then restart Synapse:
   ```yaml
   app_service_config_files:
     - /path/to/slack-import-registration.yaml
   max_upload_size: 1G        # the largest file in this export is 488 MiB; raise any reverse-proxy body limit too
   ```

4. **Set up the people.** Each mapped person should have recovery (key
   backup) set up in Element *before* the import. If they also want keys
   pushed to their existing devices, those devices should be logged in.

5. **Import.**
   ```sh
   bin/slack2matrix import -channels general -limit 200     # try a little first
   bin/slack2matrix import                                   # everything (resumable)
   ```
   Re-run the same command after any interruption. During the import, real
   users' rooms are muted, so their phones won't light up with ten years of
   history.

   All channels run at once; `-concurrency` (default `import.room_concurrency`)
   caps how many messages are being sent at a time, and the largest channels
   get first turn. A message that links to an earlier message in another
   channel waits until that channel has imported it, so the link can point
   at the Matrix event. Media is uploaded ahead of the messages that need
   it by `-media-concurrency` background workers (default
   `import.media_concurrency`; `0` uploads each file only when its message
   is sent). On a busy production server, start low, e.g.
   `-concurrency 2 -media-concurrency 2`. Progress is logged every 1,000
   messages (and at least once a minute):
   ```
   INF Processed 12,000 of 690,907 messages (1.7%) elapsed=4m10s eta=3h55m rate=48/s
   ```
   Links into channels left out of a run (`-channels …`) point at the room,
   or stay Slack links if that room doesn't exist yet, so import the whole
   export in one run for the best links.

6. **Finalize.**
   ```sh
   bin/slack2matrix finalize
   ```
   This uploads all room keys to each person's key backup, writes
   `crypto.key_export_file` (passphrase in `crypto.key_export_passphrase_file`),
   unmutes the rooms, and marks them read.

7. **Clean up.** Remove the registration from `homeserver.yaml` (or narrow
   the non-exclusive user regexes) and restart Synapse. Store `state_dir` and
   the key export somewhere safe.

To redo one channel (for example after changing import options), delete its
room and import it again:
```sh
bin/slack2matrix reset-channel -channel general -admin-token-file admin-token.txt
bin/slack2matrix import -channels general
```
`reset-channel` purges the room with Synapse's admin API, removes it from the
space and forgets its events; uploaded media is reused. It acts as the first
`admins` entry unless that user isn't a server admin, in which case pass a
server admin's access token (`-admin-token-file -` prompts for it).
`send-test -channel <name> -ts <ts>` posts a single message to its room now,
marked as a test, to try options without recording anything.

### Recovery keys and trust

With a person's recovery key, the importer does two things:
- Signs its `SLACKIMPORT` device with that person's self-signing key, so the
  imported history shows as verified.
- Reads the backup key from secret storage, which proves the server-side key
  backup is genuine before keys are uploaded to it.

Without a recovery key:
- The device stays unverified, and Element marks those messages "Encrypted by
  a device not verified by its owner".
- The backup is trusted only if it is signed by the person's master
  cross-signing key. Use `finalize -force` to skip that check.

The importer never creates or replaces a real person's cross-signing keys.
Synapse would allow an appservice to do this silently, which would reset the
person's identity.

## Testing

`scripts/synapse-up.sh` starts a fresh Synapse in Docker. It installs the
appservice registration and creates test accounts `admin`, `steve` and
`jamie`. Set `POSTGRES=1` to back it with PostgreSQL.

```sh
bin/slack2matrix -config dev/test.yaml registration -out dev/test-registration.yaml
POSTGRES=1 DATA=$PWD/dev/synapse-test scripts/synapse-up.sh dev/test-registration.yaml

# A "pre-existing" device per person, with recovery set up the way Element does it:
bin/slack2matrix -config dev/test.yaml testclient login -user @steve:test.local \
    -password test-steve-pw -setup-recovery dev/test-state/rk-steve.txt

bin/slack2matrix -config dev/test.yaml import -channels zzz,lynx
bin/slack2matrix -config dev/test.yaml finalize

# (a) the existing device decrypts with keys received over Olm
bin/slack2matrix -config dev/test.yaml testclient check -user @steve:test.local -password test-steve-pw
# (b) a brand-new device restores the key backup
bin/slack2matrix -config dev/test.yaml testclient check -user @steve:test.local -password test-steve-pw \
    -db dev/test-state/new.db -restore-backup dev/test-state/rk-steve.txt
# (c) a brand-new device imports the key export file
bin/slack2matrix -config dev/test.yaml testclient check -user @steve:test.local -password test-steve-pw \
    -db dev/test-state/new2.db -import-keys dev/test-state/megolm-keys.txt \
    -import-passphrase-file dev/test-state/megolm-keys.passphrase
```

For every room, `check` reports how many events decrypted. It fails if any
event can't be decrypted, if any message was sent in plaintext, or if any
`origin_server_ts` differs from the Slack timestamp.

## Limitations

- Synapse ignores `?ts=` on room creation, so each room's creation event
  carries the import date. The history itself is timestamped correctly.
- Room names, topics, membership and relation metadata (threads, reactions)
  are not end-to-end encrypted. This is inherent to Matrix.
- Element Web can't search encrypted history; Element Desktop can, using its
  local index. Another option is [Axon](https://github.com/matrix-axon/matrix.com)
  which provides a full-text searchable index across multiple accounts, spaces,
  and encrypted rooms.
- Slack exports contain only the final text of edited messages and no reaction
  timestamps. `import.mark_edited` adds an "(edited)" marker.
- The `SLACKIMPORT` devices must stay on the accounts. Deleting them makes
  Element flag the history as sent from a deleted device.

## License

Apache License 2.0; see `LICENSE`. Third-party components compiled into the
binary and their licenses are listed in `THIRD_PARTY_NOTICES.md`, which is
regenerated with `make notices`.
