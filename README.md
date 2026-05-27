# DownloadClean

A small, JDownloader-style sequential link downloader written in Go.

The goal is a focused subset of what JDownloader 2 does — read a `.dlc`
container, resolve each link with a hoster account, download files one after
another. Ships a plain CLI and a Bubble Tea TUI with a Japan-cyberpunk palette.

## Status

Early prototype. Both front-ends support:

- `.dlc` parsing (two-stage AES-CBC, via the AppWork dlcrypt service).
- **rapidgator.net premium** via the official v2 API.
- Sequential downloads with HTTP `Range` resume.
- XDG-aware default paths, overridable on the command line.

Not yet supported: parallel downloads, free-tier handling (captcha/wait),
RSDF/CCF containers, other hosters.

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
bar, and a summary screen with a one-key rerun-failed shortcut. Mouse left
click toggles a link on the parsed screen; `q` cancels a running batch (the
`.part` file is kept so the next run resumes from where it stopped).

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

The cyberpunk palette assumes a dark, truecolor-capable terminal. On
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
