# DownloadClean

A small, JDownloader-style sequential link downloader written in Go.

The goal is a focused subset of what JDownloader 2 does — read a `.dlc`
container, resolve each link with a hoster account, download files one after
another. Ships a plain CLI and a Bubble Tea TUI with a neon palette.

## Features

Both front-ends share the same `internal/` packages and offer:

- **`.dlc` containers.** Two-stage AES-CBC decryption against the AppWork
  dlcrypt service. The service endpoint is overridable via
  `DOWNLOADCLEAN_DLC_SERVICE` if AppWork's default is unreachable.
- **Rapidgator premium.** Authenticates against the official v2 API,
  resolves direct download URLs, and surfaces server-side errors verbatim.
  Free-tier (captcha / wait timers) is out of scope.
- **Sequential downloads with HTTP `Range` resume.** Each transfer streams
  to `<dest>.part` and is atomically renamed on success. Cancelling a run
  leaves the `.part` on disk so the next run picks up where it stopped.
- **Collision-safe writes.** The queue refuses to overwrite an existing
  destination file and reports the conflict so the TUI can offer
  overwrite/rename. Path separators in hoster-returned filenames are
  stripped to keep writes inside `--output`.
- **Hardened config loader.** On Unix, `accounts.json` must be `chmod 600`,
  not a symlink, with a parent directory that is not group- or
  world-writable. All three checks can be bypassed with
  `--insecure-config` for development.
- **XDG paths.** Accounts default to `$XDG_CONFIG_HOME/downloadclean/`
  and downloads to `$XDG_DOWNLOAD_DIR` (or `~/Downloads`), both
  overridable on the command line.

The TUI adds on top:

- **Four screens** — drop-zone picker, parsed-link selection table, live
  download view, and a summary banner with rerun-failed and start-new
  shortcuts.
- **Drag-and-drop picker** that normalises `file://` URLs, percent-escapes,
  surrounding quotes, and shell-escaped spaces. The fallback Tab-driven
  file browser handles terminals that don't paste on drop.
- **Live progress** with neon gradient bars for the current file and the
  whole batch, smoothed transfer speed, ETA, and the destination
  `<path>.part` shown beside the progress bar.
- **Mid-download queueing.** Press `a` on the downloading screen to drop
  another `.dlc` onto the running queue; each container shows up as its
  own row in a batches table with per-batch counters.
- **Rerun failed.** The summary screen retains the failed job list; one
  keypress re-queues just those jobs.

Not yet supported: parallel downloads, retry/backoff, bandwidth caps,
free-tier flows, RSDF/CCF containers, persistent queue, additional hosters.

## Install

CLI:

```sh
go install github.com/janbalmer/downloadclean/cmd/downloadclean@latest
```

TUI:

```sh
go install github.com/janbalmer/downloadclean/cmd/downloadclean-tui@latest
```

Or from a clone:

```sh
go build -o downloadclean ./cmd/downloadclean
go build -o downloadclean-tui ./cmd/downloadclean-tui
```

## Configure

Create `~/.config/downloadclean/accounts.json` (see `accounts.example.json`):

```json
{ "rapidgator": { "login": "you@example.com", "password": "secret" } }
```

On Unix, the file must be `chmod 600`, must not be a symlink, and its parent
directory must not be group- or world-writable. The tool refuses to load
credentials otherwise (override with `--insecure-config`).

## Use (CLI)

```sh
downloadclean --dlc path/to/links.dlc --output ~/Downloads
```

Flags:

| flag                 | default                                        |
| -------------------- | ---------------------------------------------- |
| `--dlc`              | (required) path to a `.dlc` file               |
| `--accounts`         | `$XDG_CONFIG_HOME/downloadclean/accounts.json` |
| `--output`           | `$XDG_DOWNLOAD_DIR` or `~/Downloads`           |
| `--list`             | decrypt and print links, do not download       |
| `--limit`            | download at most N links (0 = all)             |
| `--insecure-config`  | skip the accounts-file permission checks       |

Set `DOWNLOADCLEAN_DLC_SERVICE` to override the dlcrypt endpoint if AppWork's
default service is unreachable. The format string takes one `%s` placeholder
for the URL-encoded key blob.

## Use (TUI)

```sh
downloadclean-tui                       # opens the picker
downloadclean-tui path/to/links.dlc     # skips the picker
```

Four screens: a drop-zone picker, a checkbox table of parsed links, a live
download view with neon progress bars + speed + ETA + a per-batch progress
bar and a batches table, and a summary screen with a one-key rerun-failed
shortcut. Mouse left click toggles a link on the parsed screen; `a` on the
downloading screen queues another `.dlc` as a new batch; `q` cancels the
running queue (the `.part` file is kept so the next run resumes from where
it stopped).

Drag-and-drop: drop a `.dlc` from your file manager onto the terminal window
while the picker is focused, then press Enter. The TUI normalises common
encodings (`file://` URLs, percent-escapes, surrounding quotes, shell-escaped
spaces). Empirically this works on Kitty, WezTerm, GNOME Terminal, foot, and
iTerm2; on Konsole and urxvt the terminal does not paste anything when a
file is dropped, so use Tab for the fallback file browser instead.

Flags mirror the CLI's:

| flag                 | default                                        |
| -------------------- | ---------------------------------------------- |
| `[dlc-path]`         | optional positional; skips the picker          |
| `--accounts`         | `$XDG_CONFIG_HOME/downloadclean/accounts.json` |
| `--output`           | `$XDG_DOWNLOAD_DIR` or `~/Downloads`           |
| `--insecure-config`  | skip the accounts-file permission checks       |

The neon palette assumes a dark, truecolor-capable terminal. On
256-colour terminals the look degrades gracefully but the magenta/violet
gradient may collapse.

## Caveats

- `.dlc` decryption requires a working dlcrypt service. There is no offline
  decryption path; the per-file key is held by the service operator.
- Rapidgator integration uses the official premium API. Free accounts are not
  supported.

## Build & test

```sh
go build ./...
go test ./...
```
