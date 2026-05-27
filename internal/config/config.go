// Package config loads accounts.json from XDG paths and verifies
// credential-file permissions on Unix.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Accounts is the on-disk shape of accounts.json: per-hoster credential
// blocks keyed by hoster name. A nil block means that hoster is not
// configured and links for it will be skipped.
type Accounts struct {
	Rapidgator *RapidgatorAccount `json:"rapidgator,omitempty"`
}

// RapidgatorAccount holds rapidgator.net premium credentials.
type RapidgatorAccount struct {
	Login    string `json:"login"`
	Password string `json:"password"`
}

// Load reads and parses an accounts JSON file. If allowInsecure is false,
// the file must be a regular file (not a symlink, directory, FIFO, etc.),
// and on Unix it must not be group- or world-readable and its parent
// directory must not be group- or world-writable.
//
// Permission checks are performed on the open file descriptor (not the
// path) to close the Lstat→Read TOCTOU window.
func Load(path string, allowInsecure bool) (*Accounts, error) {
	// Lstat (not Stat) catches a symlinked credential file before we open it.
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
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf(
				"accounts file %s is not a regular file (mode %v); "+
					"pass --insecure-config to override", path, info.Mode())
		}
		if runtime.GOOS != "windows" {
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

	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("accounts file: %w", err)
	}
	defer f.Close()

	// Re-check the mode on the open fd to close the TOCTOU between Lstat and
	// Open. fstat sees the file we actually have a handle to, not whatever a
	// concurrent rename/chmod might have put in place.
	if !allowInsecure && runtime.GOOS != "windows" {
		fi, err := f.Stat()
		if err != nil {
			return nil, fmt.Errorf("accounts file: stat: %w", err)
		}
		if mode := fi.Mode().Perm(); mode&0o077 != 0 {
			return nil, fmt.Errorf(
				"accounts file %s has permissions %#o (group/world readable); "+
					"run `chmod 600 %s` or pass --insecure-config",
				path, mode, path)
		}
	}

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var a Accounts
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("accounts file: parse: %w", err)
	}
	return &a, nil
}

// DefaultPath returns the platform-appropriate config path for accounts.json
// (XDG on Unix, %AppData% on Windows, ~/Library/Application Support on macOS).
func DefaultPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "downloadclean", "accounts.json")
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
