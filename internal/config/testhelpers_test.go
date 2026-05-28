package config

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// writeCredFile drops content into dir/name with the requested file and
// parent-directory permissions, defeating the process umask so the caller
// gets the exact modes asked for. Returned path is dir/name.
func writeCredFile(t *testing.T, dir, name, content string, fileMode, dirMode os.FileMode) string {
	t.Helper()
	if err := os.Chmod(dir, dirMode); err != nil {
		t.Fatalf("chmod dir: %v", err)
	}
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), fileMode); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := os.Chmod(path, fileMode); err != nil {
		t.Fatalf("chmod file: %v", err)
	}
	return path
}

// skipOnWindows is a no-op on Unix; on Windows it skips tests that depend
// on POSIX permission semantics that the package's security checks gate on.
func skipOnWindows(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix permission semantics not applicable on Windows")
	}
}
