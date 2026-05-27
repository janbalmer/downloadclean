package downloader

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
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
	err := Download(context.Background(), srv.URL, dest, func(d, total int64) {
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
	if _, err := os.Stat(dest + ".part"); !os.IsNotExist(err) {
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
		offset, _ := strconv.Atoi(rest)
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

	if err := Download(context.Background(), srv.URL, dest, nil, Options{}); err != nil {
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

	if err := Download(context.Background(), srv.URL, dest, nil, Options{}); err != nil {
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
	err := Download(context.Background(), srv.URL, dest, nil, Options{})
	if err == nil || !strings.Contains(err.Error(), "HTTP 410") {
		t.Errorf("expected HTTP 410 error, got %v", err)
	}
}
