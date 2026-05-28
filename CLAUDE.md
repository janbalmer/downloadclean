# Guidance for Claude sessions

## What this project is

A JDownloader-style sequential link downloader in Go. Reads a `.dlc`, resolves
each link via a hoster account, downloads files one at a time. Two front-ends:
a plain CLI and a Bubble Tea TUI; both wrap the same `internal/` packages.

## Architecture

```
cmd/downloadclean/        CLI entry point. Prints to stdout, exit 1 on failure.
cmd/downloadclean-tui/    Bubble Tea TUI. Cyberpunk palette + drag-drop picker.
internal/config/          accounts.json + config.json loaders, XDG paths,
                          shared credential-file security helpers.
internal/dlc/             .dlc decryption + XML parsing.
internal/hoster/          Hoster interface + Registry.
internal/hoster/rapidgator/  rapidgator.net premium client (v2 API).
internal/downloader/      Streaming HTTP GET with Range resume + progress.
internal/queue/           Sequential Job runner emitting events.
internal/extractor/       Post-download 7zz invocation with multi-volume
                          detection (single archive set extracted per call).
```

The split exists so both front-ends reuse every `internal/` package without
restructuring. New hosters are one file under `internal/hoster/<name>/`.

The TUI bridges the synchronous `queue.Run` to Bubble Tea by spawning a
goroutine that publishes `queue.Event` values onto a buffered channel; a
self-rearming `tea.Cmd` (`waitForEvent`) reads one event per cycle and
delivers it as `queueEventMsg`. Cancellation flows through the context
stored on the model — `Ctrl+C` / `q` on the downloading screen calls
`m.cancel()` and waits for `queueDoneMsg` rather than quitting Bubble Tea
directly, so `.part` files remain on disk for resume.

`internal/extractor/` shells out to `7zz` (with `7z` fallback) to unpack
archives after each successful download. Multi-volume detection
(modern `.partNN.rar`, split `.7z.NNN`, legacy `.rar`+`.r00..rNN`) lives
here as pure helpers — `ArchiveSet` returns metadata, `EnumerateVolumes`
scans the destination dir for the actual files, and `TriggerVolume`
names the canonical first volume of a set. The queue orchestrates:
after each `EventDone` for an archive, it stashes non-trigger volumes in
`Runner.pending`, fires `extractor.Extract` only when all siblings are
present, and at end-of-`Run` emits `EventExtractSkipped` for sets whose
siblings never arrived. Like `RateLimiter`, `Runner.Extractor` is a
pointer to an internally-synchronized `extractor.Toggle` — the TUI
flips state mid-run without races. Encrypted archives use `-p-` to fail
fast, then retry with each password from
`~/.config/downloadclean/config.json`'s `archive_passwords` list.
Successful extraction deletes every volume in the set; cancellation or
failure leaves them on disk (same philosophy as `.part`).

## Hard constraints

- **`internal/` stays dependency-free.** Only `cmd/downloadclean-tui/`
  imports the Charm libs (`bubbletea`, `bubbles`, `lipgloss`). The CLI and
  every `internal/` package use stdlib only. New code in `internal/` must
  not pull in third-party deps.
- **Sequential downloads only.** The `queue` package is intentionally serial.
  Do not introduce goroutine fan-out without a design discussion.
- **Premium-only Rapidgator.** Free-tier (captcha, wait timers) is explicitly
  out of scope for now.
- **The repo is public on GitHub.** `accounts.json` and `config.json` (which
  holds archive passwords) are gitignored and both go through the same
  loader-side security checks: refuse symlinked credential files,
  world/group-readable credential files, and world/group-writable parent
  directories. All three can be bypassed with `--insecure-config`. The
  shared helper lives in `internal/config/security.go` — use it for any
  new credential file. Preserve every one of these safeguards.

## DLC decryption notes

The algorithm is a direct port of pyLoad's plugin: base64-strip-88,
AES-CBC-decrypt the service's `<rc>` payload with the hardcoded key/IV
(`cb99b5cbc24db398` / `9bc24cb995cb8db3`), use the first 16 bytes of the
result as both key *and* IV for a second AES-CBC pass over the body, then
parse the XML. Constants are documented in `internal/dlc/dlc.go`.

The dlcrypt URL is overridable via `DOWNLOADCLEAN_DLC_SERVICE` (a printf
format string with one `%s` for the key blob) — tests use `httptest` through
this hook, and end users can swap in a mirror if AppWork's service is down.

## Testing

- `internal/dlc` round-trips an XML through real AES-CBC against a fake
  dlcrypt server (`httptest`). When changing the algorithm, this test is the
  canary.
- `internal/hoster/rapidgator` and `internal/downloader` also use `httptest`.
- Run `go test ./...` — no network or credentials required.
- Real end-to-end testing needs a live `.dlc` and a premium Rapidgator
  account; the user runs that manually.

## Conventions

- Library packages return errors with package-prefixed context
  (`fmt.Errorf("rapidgator: download: %w", err)`).
- The downloader writes to `<dest>.part` and renames on success; never leave
  a half-written file at the final path. A stale `.part` is the intended
  resume mechanism — do not auto-delete it on cancellation.
- Filename sanitization in `queue` strips path separators — keep that, a
  hostile hoster response should not escape `--output`. Cross-platform
  hardening (Windows reserved names, control chars, length cap) is a
  Phase 2 deliverable; until then, `sanitize` is intentionally minimal.
- The queue refuses to overwrite an existing destination file: a colliding
  job emits `EventSkipped` with `Err = *queue.ErrCollision{Path: dest}`.
  The TUI uses `errors.As` to detect this and offer overwrite/rename.
- Hoster-specific URL quirks (e.g. Rapidgator's `.html` page suffix) live
  inside the hoster package, not in `queue`. `queue.basenameFromURL` is
  hoster-agnostic; each hoster's `Resolve` cleans its own filename.

## Deferred (don't build unprompted)

Parallel downloads, retry/backoff, free-tier flows, RSDF/CCF, persistent
queue, additional hosters, recursive extraction (archive-inside-archive),
tar-compound second-pass (`.tar.gz` extracts to `.tar` only), interactive
password prompts (passwords come from `config.json` only), persisted
extract-toggle state (session-only, matching rate-limit). These are
roadmap items, not lurking work.
