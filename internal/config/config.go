// Package config loads accounts.json from XDG paths and verifies
// credential-file permissions on Unix.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

type Accounts struct {
	Rapidgator *RapidgatorAccount `json:"rapidgator,omitempty"`
}

type RapidgatorAccount struct {
	Login    string `json:"login"`
	Password string `json:"password"`
}

// Load reads and parses an accounts JSON file. If allowInsecure is false,
// the file must not be a symlink, and on Unix it must not be group- or
// world-readable and its parent directory must not be group- or
// world-writable.
func Load(path string, allowInsecure bool) (*Accounts, error) {
	// Lstat (not Stat) so a 0600 symlink pointing at a 0644 file doesn't
	// pass the permission check.
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("accounts file: %w", err)
	}
	if !allowInsecure {
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf(
				"accounts file %s is a symlink; refusing to load credentials "+
					"through a symlink (pass --insecure-config to override)", path)
		}
		if runtime.GOOS != "windows" {
			if mode := info.Mode().Perm(); mode&0o077 != 0 {
				return nil, fmt.Errorf(
					"accounts file %s has permissions %#o (group/world readable); "+
						"run `chmod 600 %s` or pass --insecure-config",
					path, mode, path)
			}
			dirPath := filepath.Dir(path)
			dirInfo, derr := os.Stat(dirPath)
			if derr != nil {
				return nil, fmt.Errorf("accounts dir: %w", derr)
			}
			if dmode := dirInfo.Mode().Perm(); dmode&0o022 != 0 {
				return nil, fmt.Errorf(
					"accounts file directory %s has permissions %#o (group/world writable); "+
						"`chmod go-w %s` or pass --insecure-config",
					dirPath, dmode, dirPath)
			}
		}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("accounts file: %w", err)
	}
	var a Accounts
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("accounts file: parse: %w", err)
	}
	return &a, nil
}

// DefaultPath returns $XDG_CONFIG_HOME/downloadclean/accounts.json,
// falling back to ~/.config/downloadclean/accounts.json.
func DefaultPath() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "downloadclean", "accounts.json")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".config", "downloadclean", "accounts.json")
	}
	return "accounts.json"
}

// DefaultOutputDir returns $XDG_DOWNLOAD_DIR if set, else ~/Downloads,
// else ./downloads.
func DefaultOutputDir() string {
	if x := os.Getenv("XDG_DOWNLOAD_DIR"); x != "" {
		return x
	}
	if home, err := os.UserHomeDir(); err == nil {
		d := filepath.Join(home, "Downloads")
		if st, err := os.Stat(d); err == nil && st.IsDir() {
			return d
		}
	}
	return "downloads"
}

// ErrMissingRapidgator is returned when no rapidgator account is configured
// but a rapidgator link is encountered.
var ErrMissingRapidgator = errors.New("no rapidgator account configured")
