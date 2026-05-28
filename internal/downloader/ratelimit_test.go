package downloader

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestRateLimiter_DisabledFastPath(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter()
	start := time.Now()
	if err := rl.Wait(t.Context(), 1<<30); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Millisecond {
		t.Errorf("disabled Wait took %v, want < 5ms", elapsed)
	}
}

func TestRateLimiter_ZeroBytesFastPath(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter()
	rl.SetEnabled(true)
	rl.SetBytesPerSec(1) // would otherwise block effectively forever
	if err := rl.Wait(t.Context(), 0); err != nil {
		t.Errorf("Wait(0): %v", err)
	}
	if err := rl.Wait(t.Context(), -42); err != nil {
		t.Errorf("Wait(-42): %v", err)
	}
}

func TestRateLimiter_Throughput(t *testing.T) {
	t.Parallel()
	const rate = 1 << 20 // 1 MiB/s
	const chunk = rate / 2
	rl := NewRateLimiter()
	rl.SetBytesPerSec(rate)
	rl.SetEnabled(true)

	start := time.Now()
	if err := rl.Wait(t.Context(), chunk); err != nil {
		t.Fatalf("Wait 1: %v", err)
	}
	if err := rl.Wait(t.Context(), chunk); err != nil {
		t.Fatalf("Wait 2: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 400*time.Millisecond {
		t.Errorf("two 512KiB waits at 1MiB/s took %v, want >= 400ms", elapsed)
	}
	if elapsed > 1200*time.Millisecond {
		t.Errorf("two 512KiB waits at 1MiB/s took %v, want <= 1.2s", elapsed)
	}
}

func TestRateLimiter_ToggleOffWakesWaiter(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter()
	rl.SetBytesPerSec(1024) // 1 KiB/s — a 1 MiB wait would take ~17 minutes
	rl.SetEnabled(true)

	done := make(chan error, 1)
	go func() {
		done <- rl.Wait(t.Context(), 1<<20)
	}()

	time.Sleep(100 * time.Millisecond)
	rl.SetEnabled(false)

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Wait returned %v, want nil", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Wait did not return within 500ms of SetEnabled(false)")
	}
}

func TestRateLimiter_ContextCancelWakesWaiter(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter()
	rl.SetBytesPerSec(1024)
	rl.SetEnabled(true)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- rl.Wait(ctx, 1<<20)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Wait returned %v, want context.Canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Wait did not return within 500ms of cancel")
	}
}

func TestRateLimiter_SetBytesPerSecClamps(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter()
	rl.SetBytesPerSec(0)
	if got := rl.BytesPerSec(); got != 1 {
		t.Errorf("after SetBytesPerSec(0), BytesPerSec = %v, want 1", got)
	}
	rl.SetBytesPerSec(-1000)
	if got := rl.BytesPerSec(); got != 1 {
		t.Errorf("after SetBytesPerSec(-1000), BytesPerSec = %v, want 1", got)
	}
	rl.SetBytesPerSec(2048)
	if got := rl.BytesPerSec(); got != 2048 {
		t.Errorf("after SetBytesPerSec(2048), BytesPerSec = %v, want 2048", got)
	}
}

func TestRateLimiter_EnabledRoundTrip(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter()
	if rl.Enabled() {
		t.Error("new limiter should be disabled")
	}
	rl.SetEnabled(true)
	if !rl.Enabled() {
		t.Error("SetEnabled(true) did not stick")
	}
	rl.SetEnabled(false)
	if rl.Enabled() {
		t.Error("SetEnabled(false) did not stick")
	}
}

func TestLimitedReader_PropagatesCtxError(t *testing.T) {
	t.Parallel()
	rl := NewRateLimiter()
	rl.SetBytesPerSec(64)
	rl.SetEnabled(true)

	ctx, cancel := context.WithCancel(t.Context())
	src := bytes.NewReader(bytes.Repeat([]byte("x"), 1<<16))
	lr := &limitedReader{r: src, lim: rl, ctx: ctx}

	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := io.Copy(io.Discard, lr)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("io.Copy returned %v, want context.Canceled", err)
	}
}

func TestDownload_RateLimited(t *testing.T) {
	t.Parallel()
	payload := bytes.Repeat([]byte("a"), 64*1024) // 64 KiB
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	rl := NewRateLimiter()
	// Cap at ~5x the payload-per-second so the download finishes in roughly
	// 200ms; tight enough to detect throttling, loose enough to keep CI sane.
	rl.SetBytesPerSec(float64(len(payload)) * 5)
	rl.SetEnabled(true)

	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	start := time.Now()
	err := Download(t.Context(), srv.URL, dest, nil, Options{RateLimiter: rl})
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	elapsed := time.Since(start)

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("file mismatch: got %d bytes, want %d", len(got), len(payload))
	}
	if elapsed > 2*time.Second {
		t.Errorf("rate-limited download took %v, want < 2s", elapsed)
	}
}
