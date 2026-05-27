package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

type Accounts struct {
	Rapidgator *RapidgatorAccount `json:"rapidgator,omitempty"`
}

type RapidgatorAccount struct {
	Login    string `json:"login"`
	Password string `json:"password"`
}

// Load reads and parses an accounts JSON file. If allowInsecure is false,
// the file must not be group- or world-readable on Unix.
func Load(path string, allowInsecure bool) (*Accounts, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("accounts file: %w", err)
	}
	if !allowInsecure {
		if mode := info.Mode().Perm(); mode&0o077 != 0 {
			return nil, fmt.Errorf(
				"accounts file %s has permissions %#o (group/world readable); "+
					"run `chmod 600 %s` or pass --insecure-config",
				path, mode, path)
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
