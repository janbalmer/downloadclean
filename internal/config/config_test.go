package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validConfigJSON = `{"archive_passwords":["alpha","bravo","charlie"]}`

func writeConfigFile(t *testing.T, dir, content string, fileMode, dirMode os.FileMode) string {
	t.Helper()
	return writeCredFile(t, dir, "config.json", content, fileMode, dirMode)
}

func TestLoadConfig_MissingFileReturnsEmpty(t *testing.T) {
	skipOnWindows(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	path := filepath.Join(dir, "config.json")

	cfg, err := LoadConfig(path, false)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("LoadConfig returned nil *Config for missing file; want zero value")
	}
	if len(cfg.ArchivePasswords) != 0 {
		t.Errorf("ArchivePasswords = %v, want empty", cfg.ArchivePasswords)
	}
}

func TestLoadConfig_HappyPath(t *testing.T) {
	skipOnWindows(t)
	path := writeConfigFile(t, t.TempDir(), validConfigJSON, 0o600, 0o700)

	cfg, err := LoadConfig(path, false)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	want := []string{"alpha", "bravo", "charlie"}
	if len(cfg.ArchivePasswords) != len(want) {
		t.Fatalf("ArchivePasswords = %v, want %v", cfg.ArchivePasswords, want)
	}
	for i, p := range want {
		if cfg.ArchivePasswords[i] != p {
			t.Errorf("ArchivePasswords[%d] = %q, want %q", i, cfg.ArchivePasswords[i], p)
		}
	}
}

func TestLoadConfig_EmptyJSONObject(t *testing.T) {
	skipOnWindows(t)
	path := writeConfigFile(t, t.TempDir(), `{}`, 0o600, 0o700)

	cfg, err := LoadConfig(path, false)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(cfg.ArchivePasswords) != 0 {
		t.Errorf("ArchivePasswords = %v, want empty", cfg.ArchivePasswords)
	}
}

func TestLoadConfig_SymlinkRefused(t *testing.T) {
	skipOnWindows(t)
	realDir := t.TempDir()
	linkDir := t.TempDir()
	realPath := writeConfigFile(t, realDir, validConfigJSON, 0o600, 0o700)
	if err := os.Chmod(linkDir, 0o700); err != nil {
		t.Fatalf("chmod linkDir: %v", err)
	}
	linkPath := filepath.Join(linkDir, "config.json")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := LoadConfig(linkPath, false); err == nil {
		t.Fatal("expected error, got nil")
	} else if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error should mention symlink: %v", err)
	}

	cfg, err := LoadConfig(linkPath, true)
	if err != nil {
		t.Fatalf("insecure bypass failed: %v", err)
	}
	if len(cfg.ArchivePasswords) != 3 {
		t.Errorf("insecure bypass: ArchivePasswords = %v, want 3 entries", cfg.ArchivePasswords)
	}
}

func TestLoadConfig_WorldReadableRefused(t *testing.T) {
	skipOnWindows(t)
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604} {
		dir := t.TempDir()
		path := writeConfigFile(t, dir, validConfigJSON, mode, 0o700)
		if _, err := LoadConfig(path, false); err == nil {
			t.Errorf("mode %#o: expected error", mode)
		} else if !strings.Contains(err.Error(), "chmod") {
			t.Errorf("mode %#o: error should suggest chmod: %v", mode, err)
		}
		if _, err := LoadConfig(path, true); err != nil {
			t.Errorf("mode %#o: insecure bypass failed: %v", mode, err)
		}
	}
}

func TestLoadConfig_WorldWritableParentRefused(t *testing.T) {
	skipOnWindows(t)
	path := writeConfigFile(t, t.TempDir(), validConfigJSON, 0o600, 0o777)

	if _, err := LoadConfig(path, false); err == nil {
		t.Fatal("expected error, got nil")
	} else if !strings.Contains(err.Error(), "group/world writable") {
		t.Errorf("error should mention writable directory: %v", err)
	}
	if _, err := LoadConfig(path, true); err != nil {
		t.Errorf("insecure bypass failed: %v", err)
	}
}

func TestLoadConfig_MalformedJSON(t *testing.T) {
	skipOnWindows(t)
	path := writeConfigFile(t, t.TempDir(), `not json`, 0o600, 0o700)

	if _, err := LoadConfig(path, false); err == nil {
		t.Fatal("expected parse error")
	} else if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error should mention parse: %v", err)
	}
}

func TestLoadConfig_UnknownFieldRefused(t *testing.T) {
	skipOnWindows(t)
	// Strict decoding catches typos (here: "archive_passwords" misspelled)
	// rather than silently swallowing them and leaving the user wondering
	// why their passwords aren't being tried.
	path := writeConfigFile(t, t.TempDir(), `{"archive_password":["x"]}`, 0o600, 0o700)
	if _, err := LoadConfig(path, false); err == nil {
		t.Fatal("expected error for unknown field")
	} else if !strings.Contains(err.Error(), "archive_password") {
		t.Errorf("error should name the unknown field: %v", err)
	}
}

func TestDefaultConfigPath(t *testing.T) {
	got := DefaultConfigPath()
	if got == "" {
		t.Fatal("DefaultConfigPath returned empty string")
	}
	if !strings.HasSuffix(filepath.ToSlash(got), "downloadclean/config.json") {
		t.Errorf("DefaultConfigPath = %q, want suffix downloadclean/config.json", got)
	}
}
