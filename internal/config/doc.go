// Package config loads downloadclean's on-disk configuration from XDG
// locations and verifies credential-file permissions on Unix. It exposes
// two file loaders: [Load] for the per-hoster credentials in accounts.json
// (required for any hoster that needs login) and [LoadConfig] for the
// optional application-wide settings in config.json. Both files are
// subject to the same permission model and both checks can be bypassed
// with allowInsecure=true (the CLI's --insecure-config flag).
package config
