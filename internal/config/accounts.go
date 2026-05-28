package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	if err := verifyCredentialFile(path, allowInsecure); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("accounts file: %w", err)
	}
	defer f.Close()
	if err := verifyOpenCredentialFile(f, path, allowInsecure); err != nil {
		return nil, err
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
