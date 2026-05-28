package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Config holds non-credential application settings loaded from
// $XDG_CONFIG_HOME/downloadclean/config.json. A missing file is not an
// error: callers receive a zero-valued Config from [LoadConfig].
type Config struct {
	// ArchivePasswords are tried in order when the extractor encounters an
	// encrypted archive. Empty (nil) means no passwords are configured and
	// encrypted archives will be left untouched.
	ArchivePasswords []string `json:"archive_passwords,omitempty"`
}

// LoadConfig reads and parses the optional application config file at path.
// When the file does not exist it returns a zero-valued *Config and a nil
// error. When the file is present it is subject to the same permission
// checks as the accounts file (see [Load]) because the archive-password
// list is itself credential material; pass allowInsecure=true to bypass.
func LoadConfig(path string, allowInsecure bool) (*Config, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Config{}, nil
		}
		return nil, fmt.Errorf("config file: %w", err)
	}
	if err := verifyCredentialFile(path, allowInsecure); err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("config file: %w", err)
	}
	defer f.Close()
	if err := verifyOpenCredentialFile(f, path, allowInsecure); err != nil {
		return nil, err
	}

	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("config file: parse %s: %w", path, err)
	}
	return &c, nil
}

// DefaultConfigPath returns the platform-appropriate path for config.json,
// living next to accounts.json under the downloadclean config directory.
func DefaultConfigPath() string {
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "downloadclean", "config.json")
	}
	return "config.json"
}
