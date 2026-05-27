package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const validJSON = `{"rapidgator":{"login":"alice","password":"hunter2"}}`

func writeAccountsFile(t *testing.T, dir, content string, fileMode, dirMode os.FileMode) string {
	t.Helper()
	if err := os.Chmod(dir, dirMode); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	path := filepath.Join(dir, "accounts.json")
	if err := os.WriteFile(path, []byte(content), fileMode); err != nil {
		t.Fatalf("write file: %v", err)
	}
	// WriteFile honors umask; force the requested mode.
	if err := os.Chmod(path, fileMode); err != nil {
		t.Fatalf("chmod file: %v", err)
	}
	return path
}

func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission semantics not applicable on Windows")
	}
}

func TestLoad_HappyPath(t *testing.T) {
	skipOnWindows(t)
	path := writeAccountsFile(t, t.TempDir(), validJSON, 0o600, 0o700)
	a, err := Load(path, false)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if a.Rapidgator == nil || a.Rapidgator.Login != "alice" || a.Rapidgator.Password != "hunter2" {
		t.Errorf("got %+v", a.Rapidgator)
	}
}

func TestLoad_SymlinkRefused(t *testing.T) {
	skipOnWindows(t)
	realDir := t.TempDir()
	linkDir := t.TempDir()
	realPath := writeAccountsFile(t, realDir, validJSON, 0o600, 0o700)
	linkPath := filepath.Join(linkDir, "accounts.json")
	if err := os.Symlink(realPath, linkPath); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if _, err := Load(linkPath, false); err == nil {
		t.Fatal("expected error, got nil")
	} else if !strings.Contains(err.Error(), "symlink") {
		t.Errorf("error should mention symlink: %v", err)
	}

	// --insecure-config bypasses the check.
	if _, err := Load(linkPath, true); err != nil {
		t.Errorf("insecure bypass failed: %v", err)
	}
}

func TestLoad_WorldReadableRefused(t *testing.T) {
	skipOnWindows(t)
	for _, mode := range []os.FileMode{0o644, 0o640, 0o604} {
		path := writeAccountsFile(t, t.TempDir(), validJSON, mode, 0o700)
		if _, err := Load(path, false); err == nil {
			t.Errorf("mode %#o: expected error", mode)
		} else if !strings.Contains(err.Error(), "chmod") {
			t.Errorf("mode %#o: error should suggest chmod: %v", mode, err)
		}
	}
}

func TestLoad_WorldWritableParentRefused(t *testing.T) {
	skipOnWindows(t)
	path := writeAccountsFile(t, t.TempDir(), validJSON, 0o600, 0o777)
	if _, err := Load(path, false); err == nil {
		t.Fatal("expected error, got nil")
	} else if !strings.Contains(err.Error(), "group/world writable") {
		t.Errorf("error should mention writable directory: %v", err)
	}
}

func TestLoad_InsecureBypassesAll(t *testing.T) {
	skipOnWindows(t)
	path := writeAccountsFile(t, t.TempDir(), validJSON, 0o644, 0o777)
	if _, err := Load(path, true); err != nil {
		t.Errorf("--insecure-config should bypass perm checks: %v", err)
	}
}

func TestLoad_NonRegularFileRefused(t *testing.T) {
	skipOnWindows(t)
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Point Load at the directory itself, not a file inside it.
	if _, err := Load(dir, false); err == nil {
		t.Fatal("expected error loading a directory")
	} else if !strings.Contains(err.Error(), "not a regular file") {
		t.Errorf("error should mention regular file: %v", err)
	}
}

func TestLoad_MalformedJSON(t *testing.T) {
	skipOnWindows(t)
	path := writeAccountsFile(t, t.TempDir(), `{"rapidgator": {`, 0o600, 0o700)
	if _, err := Load(path, false); err == nil {
		t.Fatal("expected parse error")
	} else if !strings.Contains(err.Error(), "parse") {
		t.Errorf("error should mention parse: %v", err)
	}
}

func TestLoad_UnknownFieldRefused(t *testing.T) {
	skipOnWindows(t)
	// A "rapidgater" typo (note the misspelling) used to silently disable the
	// hoster — strict decoding makes it a parse error so the user can fix it.
	path := writeAccountsFile(t, t.TempDir(), `{"rapidgater": {"login":"alice"}}`, 0o600, 0o700)
	if _, err := Load(path, false); err == nil {
		t.Fatal("expected error for unknown field")
	} else if !strings.Contains(err.Error(), "rapidgater") {
		t.Errorf("error should name the unknown field: %v", err)
	}
}

func TestDefaultPath(t *testing.T) {
	got := DefaultPath()
	if got == "" {
		t.Fatal("DefaultPath returned empty string")
	}
	if !strings.HasSuffix(filepath.ToSlash(got), "downloadclean/accounts.json") {
		t.Errorf("DefaultPath = %q, want suffix downloadclean/accounts.json", got)
	}
}
