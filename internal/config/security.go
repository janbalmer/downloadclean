package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// verifyCredentialFile runs the pre-open security checks on path: it must not
// be a symlink, it must be a regular file, and on Unix its parent directory
// must not be group- or world-writable. When allowInsecure is true all checks
// are skipped and nil is returned unconditionally (the caller is responsible
// for ensuring the path exists; this helper does not stat it in that case).
//
// The fstat-based re-check that closes the Lstat→Open TOCTOU window lives in
// [verifyOpenCredentialFile]; callers should invoke both — this one before
// os.Open and the fd-based one immediately after.
func verifyCredentialFile(path string, allowInsecure bool) error {
	if allowInsecure {
		return nil
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("credential file: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf(
			"credential file %s is a symlink; refusing to load credentials "+
				"through a symlink (pass --insecure-config to override)", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf(
			"credential file %s is not a regular file (mode %v); "+
				"pass --insecure-config to override", path, info.Mode())
	}
	if runtime.GOOS == "windows" {
		return nil
	}
	dirPath := filepath.Dir(path)
	dirInfo, err := os.Stat(dirPath)
	if err != nil {
		return fmt.Errorf("credential dir: %w", err)
	}
	if dmode := dirInfo.Mode().Perm(); dmode&0o022 != 0 {
		return fmt.Errorf(
			"credential file directory %s has permissions %#o (group/world writable); "+
				"`chmod go-w %s` or pass --insecure-config",
			dirPath, dmode, dirPath)
	}
	return nil
}

// verifyOpenCredentialFile re-checks permissions on an already-opened file
// descriptor. This closes the TOCTOU window between [verifyCredentialFile]
// (which Lstats the path) and os.Open (which follows whatever the path
// resolves to at that moment): fstat sees the file the caller actually holds
// a handle to, not whatever a concurrent rename or chmod may have substituted.
//
// On Windows or when allowInsecure is true the check is a no-op. path is
// used only for error messages.
func verifyOpenCredentialFile(f *os.File, path string, allowInsecure bool) error {
	if allowInsecure || runtime.GOOS == "windows" {
		return nil
	}
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("credential file: stat: %w", err)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		return fmt.Errorf(
			"credential file %s has permissions %#o (group/world readable); "+
				"run `chmod 600 %s` or pass --insecure-config",
			path, mode, path)
	}
	return nil
}
