# Guidance for Claude sessions

## What this project is

A JDownloader-style sequential link downloader in Go. Reads a `.dlc`, resolves
each link via a hoster account, downloads files one at a time. v1 is CLI;
a Bubble Tea TUI is planned but not yet started.

## Architecture

```
cmd/downloadclean/        CLI entry point (v1). TUI will live in a sibling cmd/.
internal/config/          accounts.json loader + XDG path resolution.
internal/dlc/             .dlc decryption + XML parsing.
internal/hoster/          Hoster interface + Registry.
internal/hoster/rapidgator/  rapidgator.net premium client (v2 API).
internal/downloader/      Streaming HTTP GET with Range resume + progress.
internal/queue/           Sequential Job runner emitting events.
```

The split exists so that the TUI command can reuse every `internal/` package
without restructuring. New hosters are one file under `internal/hoster/<name>/`.

## Hard constraints

- **No third-party deps in v1.** Everything is `crypto/aes`, `encoding/*`,
  `net/http`, etc. The Charm Bracelet libs (`bubbletea`, `bubbles`, `lipgloss`)
  come in only when the TUI command is added.
- **Sequential downloads only.** The `queue` package is intentionally serial.
  Do not introduce goroutine fan-out without a design discussion.
- **Premium-only Rapidgator.** Free-tier (captcha, wait timers) is explicitly
  out of scope for now.
- **The repo is public on GitHub.** `accounts.json` is gitignored and the
  config loader (on Unix) refuses symlinked credential files, world/group-
  readable credential files, and world/group-writable parent directories.
  All three checks can be bypassed with `--insecure-config`. Preserve every
  one of these safeguards.

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

Bubble Tea TUI, parallel downloads, retry/backoff, bandwidth caps, free-tier
flows, RSDF/CCF, persistent queue, additional hosters. These are roadmap
items, not lurking work.
