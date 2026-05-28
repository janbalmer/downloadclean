package extractor

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"
)

func TestIsArchive(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want bool
	}{
		{"foo.zip", true},
		{"Foo.RAR", true},
		{"bar.tar.gz", true},
		{"bar.tar.bz2", true},
		{"bar.tar.xz", true},
		{"baz.7z", true},
		{"quux.part01.rar", true},
		{"quux.part10.rar", true},
		{"data.7z.001", true},
		{"data.7z.045", true},
		{"old.r00", true},
		{"old.r99", true},
		{"thing.gz", true},
		{"thing.bz2", true},
		{"thing.xz", true},
		{"thing.tar", true},

		{"foo.txt", false},
		{"bar.mp4", false},
		{"empty", false},
		{"no-ext", false},
		{"foo.r", false},
		{"foo.7z.", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsArchive(tc.name); got != tc.want {
				t.Errorf("IsArchive(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

func TestArchiveSet(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		wantKey   string
		wantFmt   Format
		wantFirst bool
	}{
		{"foo.part01.rar", "foo", FormatRar, true},
		{"foo.part1.rar", "foo", FormatRar, true},
		{"foo.part05.rar", "foo", FormatRar, false},
		{"foo.part10.rar", "foo", FormatRar, false},
		{"data.7z.001", "data", FormatSevenZip, true},
		{"data.7z.003", "data", FormatSevenZip, false},
		{"single.zip", "single", FormatZip, true},
		{"old.rar", "old", FormatRar, true},
		{"old.r00", "old", FormatRarLegacy, false},
		{"old.r05", "old", FormatRarLegacy, false},
		{"bar.tar.gz", "bar", FormatTarGz, true},
		{"bar.tar.bz2", "bar", FormatTarBz2, true},
		{"bar.tar.xz", "bar", FormatTarXz, true},
		{"thing.7z", "thing", FormatSevenZip, true},
		{"thing.tar", "thing", FormatTar, true},
		{"thing.gz", "thing", FormatGz, true},
		{"unknown.xyz", "", FormatUnknown, false},
		{"no_ext", "", FormatUnknown, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set := ArchiveSet(tc.name)
			if set.Format != tc.wantFmt {
				t.Errorf("Format = %v, want %v", set.Format, tc.wantFmt)
			}
			if set.Format == FormatUnknown {
				return
			}
			if set.Key != tc.wantKey {
				t.Errorf("Key = %q, want %q", set.Key, tc.wantKey)
			}
			if set.First != tc.wantFirst {
				t.Errorf("First = %v, want %v", set.First, tc.wantFirst)
			}
		})
	}
}

func TestEnumerateVolumes(t *testing.T) {
	t.Parallel()

	t.Run("single-file present", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		touch(t, filepath.Join(dir, "thing.zip"))

		set := ArchiveSet("thing.zip")
		got := EnumerateVolumes(dir, set)
		want := []string{"thing.zip"}
		if !slices.Equal(got, want) {
			t.Errorf("EnumerateVolumes = %v, want %v", got, want)
		}
		if !SiblingsPresent(dir, set) {
			t.Error("SiblingsPresent = false, want true")
		}
	})

	t.Run("single-file missing", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()

		set := ArchiveSet("thing.zip")
		if got := EnumerateVolumes(dir, set); got != nil {
			t.Errorf("EnumerateVolumes = %v, want nil", got)
		}
		if SiblingsPresent(dir, set) {
			t.Error("SiblingsPresent = true, want false")
		}
	})

	t.Run("multi-volume rar contiguous", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		touch(t, filepath.Join(dir, "movie.part01.rar"))
		touch(t, filepath.Join(dir, "movie.part02.rar"))
		touch(t, filepath.Join(dir, "movie.part03.rar"))

		set := ArchiveSet("movie.part01.rar")
		got := EnumerateVolumes(dir, set)
		want := []string{"movie.part01.rar", "movie.part02.rar", "movie.part03.rar"}
		if !slices.Equal(got, want) {
			t.Errorf("EnumerateVolumes = %v, want %v", got, want)
		}
	})

	t.Run("multi-volume rar with gap", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		touch(t, filepath.Join(dir, "movie.part01.rar"))
		touch(t, filepath.Join(dir, "movie.part02.rar"))
		touch(t, filepath.Join(dir, "movie.part04.rar"))
		touch(t, filepath.Join(dir, "movie.part05.rar"))

		set := ArchiveSet("movie.part01.rar")
		if got := EnumerateVolumes(dir, set); got != nil {
			t.Errorf("EnumerateVolumes = %v, want nil (gap detected)", got)
		}
	})

	t.Run("multi-volume 7z contiguous", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		touch(t, filepath.Join(dir, "data.7z.001"))
		touch(t, filepath.Join(dir, "data.7z.002"))

		set := ArchiveSet("data.7z.001")
		got := EnumerateVolumes(dir, set)
		want := []string{"data.7z.001", "data.7z.002"}
		if !slices.Equal(got, want) {
			t.Errorf("EnumerateVolumes = %v, want %v", got, want)
		}
	})

	t.Run("legacy rar with continuations", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		touch(t, filepath.Join(dir, "old.rar"))
		touch(t, filepath.Join(dir, "old.r00"))
		touch(t, filepath.Join(dir, "old.r01"))

		set := ArchiveSet("old.r00")
		got := EnumerateVolumes(dir, set)
		want := []string{"old.rar", "old.r00", "old.r01"}
		if !slices.Equal(got, want) {
			t.Errorf("EnumerateVolumes = %v, want %v", got, want)
		}
	})

	t.Run("legacy rar without continuations", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		touch(t, filepath.Join(dir, "old.rar"))

		set := ArchiveSet("old.rar")
		got := EnumerateVolumes(dir, set)
		want := []string{"old.rar"}
		if !slices.Equal(got, want) {
			t.Errorf("EnumerateVolumes = %v, want %v", got, want)
		}
	})

	t.Run("legacy rar gap in continuations", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		touch(t, filepath.Join(dir, "old.rar"))
		touch(t, filepath.Join(dir, "old.r00"))
		touch(t, filepath.Join(dir, "old.r02"))

		// Trigger via the .r00 continuation so we hit the legacy path.
		set := ArchiveSet("old.r00")
		if got := EnumerateVolumes(dir, set); got != nil {
			t.Errorf("EnumerateVolumes = %v, want nil (gap detected)", got)
		}
	})

	t.Run("case-insensitive matching", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		touch(t, filepath.Join(dir, "MOVIE.ZIP"))

		set := ArchiveSet("movie.zip")
		got := EnumerateVolumes(dir, set)
		want := []string{"MOVIE.ZIP"}
		if !slices.Equal(got, want) {
			t.Errorf("EnumerateVolumes = %v, want %v (case preserved from disk)", got, want)
		}
	})
}

func TestTriggerVolume(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, input, want string
	}{
		{"rar-part-trigger", "movie.part01.rar", "movie.part01.rar"},
		{"rar-part-non-trigger-2digit", "movie.part05.rar", "movie.part01.rar"},
		{"rar-part-non-trigger-3digit", "movie.part123.rar", "movie.part001.rar"},
		{"sevenzip-multi-trigger", "data.7z.001", "data.7z.001"},
		{"sevenzip-multi-non-trigger", "data.7z.045", "data.7z.001"},
		{"sevenzip-single", "thing.7z", "thing.7z"},
		{"rar-single", "old.rar", "old.rar"},
		{"rar-legacy-continuation", "old.r00", "old.rar"},
		{"zip", "thing.zip", "thing.zip"},
		{"tar-gz", "bar.tar.gz", "bar.tar.gz"},
		{"tar-bz2", "bar.tar.bz2", "bar.tar.bz2"},
		{"tar-xz", "bar.tar.xz", "bar.tar.xz"},
		{"gz", "thing.gz", "thing.gz"},
		{"unknown", "foo.txt", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set := ArchiveSet(tc.input)
			if got := TriggerVolume(set); got != tc.want {
				t.Errorf("TriggerVolume(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestToggle(t *testing.T) {
	t.Parallel()

	t.Run("enabled round-trip", func(t *testing.T) {
		t.Parallel()
		tg := NewToggle()
		if tg.Enabled() {
			t.Error("new Toggle should be disabled")
		}
		tg.SetEnabled(true)
		if !tg.Enabled() {
			t.Error("SetEnabled(true) did not stick")
		}
		tg.SetEnabled(false)
		if tg.Enabled() {
			t.Error("SetEnabled(false) did not stick")
		}
	})

	t.Run("passwords round-trip", func(t *testing.T) {
		t.Parallel()
		tg := NewToggle()
		in := []string{"alpha", "beta", "gamma"}
		tg.SetPasswords(in)

		got := tg.Passwords()
		if !slices.Equal(got, in) {
			t.Errorf("Passwords = %v, want %v", got, in)
		}
	})

	t.Run("defensive copy on get", func(t *testing.T) {
		t.Parallel()
		tg := NewToggle()
		tg.SetPasswords([]string{"alpha", "beta"})

		got := tg.Passwords()
		got[0] = "tampered"

		again := tg.Passwords()
		if again[0] != "alpha" {
			t.Errorf("internal state mutated via returned slice: %v", again)
		}
	})

	t.Run("defensive copy on set", func(t *testing.T) {
		t.Parallel()
		tg := NewToggle()
		in := []string{"alpha", "beta"}
		tg.SetPasswords(in)

		in[0] = "tampered"

		got := tg.Passwords()
		if got[0] != "alpha" {
			t.Errorf("internal state mutated via input slice: %v", got)
		}
	})

	t.Run("empty passwords slice", func(t *testing.T) {
		t.Parallel()
		tg := NewToggle()
		tg.SetPasswords([]string{"x"})
		tg.SetPasswords(nil)
		if got := tg.Passwords(); got != nil {
			t.Errorf("Passwords after SetPasswords(nil) = %v, want nil", got)
		}
	})

	t.Run("concurrent toggle", func(t *testing.T) {
		t.Parallel()
		tg := NewToggle()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
		defer cancel()

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				tg.SetEnabled(true)
				tg.SetEnabled(false)
			}
		}()
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_ = tg.Enabled()
			}
		}()
		wg.Wait()
	})
}

func TestExtractZip(t *testing.T) {
	t.Parallel()
	requireBinary(t)

	src := t.TempDir()
	archive := filepath.Join(src, "hello.zip")
	makeZip(t, archive, map[string]string{"hello.txt": "hi"})

	dest := t.TempDir()
	res, err := Extract(t.Context(), archive, dest, nil, nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.PasswordUsed != "" {
		t.Errorf("PasswordUsed = %q, want empty", res.PasswordUsed)
	}

	got, err := os.ReadFile(filepath.Join(dest, "hello.txt"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(got) != "hi" {
		t.Errorf("hello.txt = %q, want %q", got, "hi")
	}
}

func TestExtractProgress(t *testing.T) {
	t.Parallel()
	requireBinary(t)

	src := t.TempDir()
	archive := filepath.Join(src, "blob.zip")
	// A 1 MiB-ish payload gives 7zz enough work to emit at least one
	// progress tick reliably.
	payload := make([]byte, 1<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	makeZipBytes(t, archive, map[string][]byte{"blob.bin": payload})

	dest := t.TempDir()
	var mu sync.Mutex
	var pcts []int
	onProgress := func(p int) {
		mu.Lock()
		pcts = append(pcts, p)
		mu.Unlock()
	}

	if _, err := Extract(t.Context(), archive, dest, nil, onProgress); err != nil {
		t.Fatalf("Extract: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	for i, p := range pcts {
		if p < 0 || p > 100 {
			t.Errorf("pcts[%d] = %d, want 0..100", i, p)
		}
	}
	for i := 1; i < len(pcts); i++ {
		if pcts[i] == pcts[i-1] {
			t.Errorf("duplicate adjacent progress reports at index %d: %v", i, pcts)
		}
	}
}

func TestExtractCancellation(t *testing.T) {
	t.Parallel()
	requireBinary(t)

	src := t.TempDir()
	archive := filepath.Join(src, "big.zip")
	// Cancel beats extraction by pre-cancelling the context, so even a
	// small archive triggers ctx.Err() before runOnce can succeed. The
	// payload size only needs to be enough that the os.Rename at the end
	// doesn't race the cancel check.
	payload := make([]byte, 1<<20)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	makeZipBytes(t, archive, map[string][]byte{"big.bin": payload})

	dest := t.TempDir()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := Extract(ctx, archive, dest, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Extract returned %v, want errors.Is(err, context.Canceled)", err)
	}
	if _, err := os.Stat(archive); err != nil {
		t.Errorf("archive removed on cancel: %v", err)
	}
}

func TestExtractEncryptedNoPassword(t *testing.T) {
	t.Parallel()
	requireBinary(t)

	archive := makeEncryptedZip(t, "wordpass", "secret.txt", "shhh")
	dest := t.TempDir()

	_, err := Extract(t.Context(), archive, dest, nil, nil)
	var ee *ErrEncrypted
	if !errors.As(err, &ee) {
		t.Fatalf("Extract returned %v, want *ErrEncrypted", err)
	}
	if ee.Path != archive {
		t.Errorf("ErrEncrypted.Path = %q, want %q", ee.Path, archive)
	}
}

func TestExtractEncryptedCorrectPassword(t *testing.T) {
	t.Parallel()
	requireBinary(t)

	archive := makeEncryptedZip(t, "wordpass", "secret.txt", "shhh")
	dest := t.TempDir()

	res, err := Extract(t.Context(), archive, dest, []string{"wordpass"}, nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.PasswordUsed != "wordpass" {
		t.Errorf("PasswordUsed = %q, want %q", res.PasswordUsed, "wordpass")
	}

	got, err := os.ReadFile(filepath.Join(dest, "secret.txt"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(got) != "shhh" {
		t.Errorf("secret.txt = %q, want %q", got, "shhh")
	}
}

func TestExtractEncryptedPasswordTriedSecond(t *testing.T) {
	t.Parallel()
	requireBinary(t)

	archive := makeEncryptedZip(t, "wordpass", "secret.txt", "shhh")
	dest := t.TempDir()

	res, err := Extract(t.Context(), archive, dest, []string{"bad", "wordpass", "ignored"}, nil)
	if err != nil {
		t.Fatalf("Extract: %v", err)
	}
	if res.PasswordUsed != "wordpass" {
		t.Errorf("PasswordUsed = %q, want %q", res.PasswordUsed, "wordpass")
	}
}

func TestExtractEncryptedWrongPasswords(t *testing.T) {
	t.Parallel()
	requireBinary(t)

	archive := makeEncryptedZip(t, "wordpass", "secret.txt", "shhh")
	dest := t.TempDir()

	_, err := Extract(t.Context(), archive, dest, []string{"bad1", "bad2"}, nil)
	var ee *ErrEncrypted
	if !errors.As(err, &ee) {
		t.Fatalf("Extract returned %v, want *ErrEncrypted", err)
	}
}

func TestBinaryNotFound(t *testing.T) {
	t.Setenv("PATH", "")
	_, err := Binary()
	if err == nil {
		t.Fatal("Binary() returned nil error with empty PATH")
	}
}

// touch creates an empty file at path. Parent dirs must already exist.
func touch(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", path, err)
	}
}

// makeZip writes a zip archive at path containing the given filename → string
// payloads. Uses the stdlib archive/zip writer.
func makeZip(t *testing.T, path string, files map[string]string) {
	t.Helper()
	bytes := make(map[string][]byte, len(files))
	for k, v := range files {
		bytes[k] = []byte(v)
	}
	makeZipBytes(t, path, bytes)
}

// makeZipBytes is the []byte variant of [makeZip] for binary payloads.
func makeZipBytes(t *testing.T, path string, files map[string][]byte) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer f.Close()

	zw := zip.NewWriter(f)
	for name, data := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("zip create %s: %v", name, err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatalf("zip write %s: %v", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
}

// makeEncryptedZip uses 7zz itself to produce an AES-encrypted zip. Returns
// the absolute archive path. archive/zip can't produce encrypted output, so
// we shell out — the test already requires the binary anyway.
func makeEncryptedZip(t *testing.T, password, filename, contents string) string {
	t.Helper()
	bin, err := Binary()
	if err != nil {
		t.Fatalf("Binary: %v", err)
	}

	work := t.TempDir()
	src := filepath.Join(work, filename)
	if err := os.WriteFile(src, []byte(contents), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	out := filepath.Join(work, "encrypted.zip")
	cmd := exec.Command(bin, "a", "-tzip", "-p"+password, "-mem=AES256", out, src)
	cmd.Dir = work
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create encrypted zip: %v\n%s", err, output)
	}
	return out
}

// requireBinary skips the test if neither 7zz nor 7z is available on PATH.
func requireBinary(t *testing.T) {
	t.Helper()
	if _, err := Binary(); err != nil {
		t.Skipf("no 7zz/7z on PATH: %v", err)
	}
}
