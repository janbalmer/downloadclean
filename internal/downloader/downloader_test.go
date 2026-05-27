package downloader

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDownload_Fresh(t *testing.T) {
	payload := []byte("hello world, this is a small file payload")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			t.Errorf("unexpected Range header on fresh download: %q", r.Header.Get("Range"))
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusOK)
		w.Write(payload)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")

	var lastDone, lastTotal int64
	err := Download(t.Context(), srv.URL, dest, func(d, total int64) {
		lastDone, lastTotal = d, total
	}, Options{})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("file mismatch: got %q want %q", got, payload)
	}
	if _, err := os.Stat(dest + ".part"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".part should be gone after success, stat err = %v", err)
	}
	if lastDone != int64(len(payload)) || lastTotal != int64(len(payload)) {
		t.Errorf("final progress = (%d, %d), want (%d, %d)", lastDone, lastTotal, len(payload), len(payload))
	}
}

func TestDownload_Resume(t *testing.T) {
	full := []byte(strings.Repeat("abcdefghij", 100)) // 1000 bytes
	const have = 400
	var sawRange string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRange = r.Header.Get("Range")
		if !strings.HasPrefix(sawRange, "bytes=") {
			http.Error(w, "want range", http.StatusBadRequest)
			return
		}
		// Honor the range: bytes=N- form.
		rest := strings.TrimPrefix(sawRange, "bytes=")
		rest = strings.TrimSuffix(rest, "-")
		offset, err := strconv.Atoi(rest)
		if err != nil {
			t.Errorf("bad Range value %q: %v", sawRange, err)
			http.Error(w, "bad range", http.StatusBadRequest)
			return
		}
		body := full[offset:]
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, len(full)-1, len(full)))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(body)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(dest+".part", full[:have], 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Download(t.Context(), srv.URL, dest, nil, Options{}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	if sawRange != fmt.Sprintf("bytes=%d-", have) {
		t.Errorf("Range header = %q, want bytes=%d-", sawRange, have)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(full) {
		t.Errorf("resumed file mismatch: len got=%d want=%d", len(got), len(full))
	}
}

func TestDownload_ServerIgnoresRange(t *testing.T) {
	full := []byte("the whole file fresh from byte zero")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Pretend we don't support Range — return 200 with the whole file.
		w.Header().Set("Content-Length", strconv.Itoa(len(full)))
		w.WriteHeader(http.StatusOK)
		w.Write(full)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	// Pre-populate .part with garbage that should be overwritten.
	if err := os.WriteFile(dest+".part", []byte("STALE"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := Download(t.Context(), srv.URL, dest, nil, Options{}); err != nil {
		t.Fatalf("Download: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(full) {
		t.Errorf("got %q, want %q", got, full)
	}
}

func TestDownload_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusGone)
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	err := Download(t.Context(), srv.URL, dest, nil, Options{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 410") {
		t.Errorf("expected HTTP 410 error, got %v", err)
	}
}

// TestDownload_ContextCancelPreservesPart verifies the load-bearing invariant
// from CLAUDE.md: a cancellation mid-stream leaves the .part file with the
// bytes received so far, and never produces a partial file at the final path.
func TestDownload_ContextCancelPreservesPart(t *testing.T) {
	const sent = 64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("a", sent)))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	err := Download(ctx, srv.URL, dest, nil, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled in error chain, got %v", err)
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dest should not exist after cancel, stat err = %v", err)
	}
	st, err := os.Stat(dest + ".part")
	if err != nil {
		t.Fatalf(".part should survive cancel: %v", err)
	}
	if st.Size() == 0 {
		t.Errorf(".part is empty; expected some received bytes")
	}
	if st.Size() > sent {
		t.Errorf(".part size %d exceeds bytes sent (%d)", st.Size(), sent)
	}
}

// TestDownload_ConnectionDropPreservesPart verifies that a server-side
// connection drop mid-stream leaves .part with whatever bytes were received.
func TestDownload_ConnectionDropPreservesPart(t *testing.T) {
	const sent = 100
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("a", sent)))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter is not a Hijacker")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatalf("hijack: %v", err)
		}
		_ = conn.Close()
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	err := Download(t.Context(), srv.URL, dest, nil, Options{})
	if err == nil {
		t.Fatal("expected error from dropped connection")
	}
	if _, err := os.Stat(dest); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dest should not exist after drop, stat err = %v", err)
	}
	st, err := os.Stat(dest + ".part")
	if err != nil {
		t.Fatalf(".part should survive: %v", err)
	}
	if st.Size() == 0 {
		t.Errorf(".part is empty")
	}
}

// TestDownload_BadContentRange asserts that a 206 response whose Content-Range
// disagrees with our requested start offset is rejected, preventing silent
// resume-corruption from a misbehaving proxy.
func TestDownload_BadContentRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Server returns 206 but with a Content-Range starting at byte 0 even
		// though we requested bytes=400-.
		w.Header().Set("Content-Length", "1000")
		w.Header().Set("Content-Range", "bytes 0-999/1000")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte(strings.Repeat("x", 1000)))
	}))
	defer srv.Close()

	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(dest+".part", []byte(strings.Repeat("y", 400)), 0o644); err != nil {
		t.Fatal(err)
	}

	err := Download(t.Context(), srv.URL, dest, nil, Options{})
	if err == nil || !strings.Contains(err.Error(), "Content-Range") {
		t.Errorf("expected Content-Range error, got %v", err)
	}
}

func TestParseContentRangeStart(t *testing.T) {
	cases := []struct {
		in    string
		want  int64
		ok    bool
	}{
		{"bytes 200-1000/67589", 200, true},
		{"bytes 0-999/*", 0, true},
		{"bytes 12345-99999/100000", 12345, true},
		{"items 0-10/20", 0, false},
		{"bytes -10/20", 0, false},
		{"bytes abc-10/20", 0, false},
		{"", 0, false},
	}
	for _, c := range cases {
		t.Run(c.in, func(t *testing.T) {
			got, ok := parseContentRangeStart(c.in)
			if ok != c.ok {
				t.Errorf("ok = %v, want %v", ok, c.ok)
			}
			if got != c.want {
				t.Errorf("got %d, want %d", got, c.want)
			}
		})
	}
}
