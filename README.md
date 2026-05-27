# DownloadClean

A small, JDownloader-style sequential link downloader written in Go.

The goal is a focused subset of what JDownloader 2 does — read a `.dlc`
container, resolve each link with a hoster account, download files one after
another. A Bubble Tea TUI is planned; v1 is CLI-only.

## Status

Early prototype. v1 supports:

- `.dlc` parsing (two-stage AES-CBC, via the AppWork dlcrypt service).
- **rapidgator.net premium** via the official v2 API.
- Sequential downloads with HTTP `Range` resume.
- XDG-aware default paths, overridable on the command line.

Not yet supported: TUI, parallel downloads, free-tier handling (captcha/wait),
RSDF/CCF containers, other hosters.

## Install

```sh
go install github.com/janbalmer/downloadclean/cmd/downloadclean@latest
```

Or from a clone:

```sh
go build -o downloadclean ./cmd/downloadclean
```

## Configure

Create `~/.config/downloadclean/accounts.json` (see `accounts.example.json`):

```json
{ "rapidgator": { "login": "you@example.com", "password": "secret" } }
```

On Unix, the file must be `chmod 600`, must not be a symlink, and its parent
directory must not be group- or world-writable. The tool refuses to load
credentials otherwise (override with `--insecure-config`).

## Use

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
