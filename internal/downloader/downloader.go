// Package downloader streams an HTTP URL to a file with progress reporting
// and Range-based resume.
package downloader

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"
)

// ProgressFn receives the number of bytes downloaded and the total expected
// (-1 if unknown). It may be called frequently — implementations should be
// cheap or rate-limit themselves.
type ProgressFn func(downloaded, total int64)

// Options configures Download. Zero values use sensible defaults.
type Options struct {
	HTTPClient *http.Client
	// ProgressInterval throttles progress callbacks. Default: 100ms.
	ProgressInterval time.Duration
}

// Download streams url into dest using a .part suffix, renaming on success.
// If a .part file already exists, it resumes via the Range header.
func Download(ctx context.Context, url, dest string, p ProgressFn, opts Options) error {
	if opts.HTTPClient == nil {
		opts.HTTPClient = &http.Client{Timeout: 0} // no overall timeout; large files
	}
	if opts.ProgressInterval == 0 {
		opts.ProgressInterval = 100 * time.Millisecond
	}

	part := dest + ".part"
	var startOffset int64
	if st, err := os.Stat(part); err == nil {
		startOffset = st.Size()
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	if startOffset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", startOffset))
	}

	resp, err := opts.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("download: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		// Server ignored our Range request (or we had no offset). Start fresh.
		startOffset = 0
	case http.StatusPartialContent:
		// Server honored the Range request; we'll append.
	default:
		return fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}

	total := startOffset + resp.ContentLength
	if resp.ContentLength < 0 {
		total = -1
	}

	flag := os.O_CREATE | os.O_WRONLY
	if resp.StatusCode == http.StatusPartialContent {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	f, err := os.OpenFile(part, flag, 0o644)
	if err != nil {
		return fmt.Errorf("download: open %s: %w", part, err)
	}
	defer f.Close()

	pr := &progressReader{
		r:        resp.Body,
		count:    startOffset,
		total:    total,
		cb:       p,
		interval: opts.ProgressInterval,
		last:     time.Now(),
	}
	if _, err := io.Copy(f, pr); err != nil {
		return fmt.Errorf("download: copy: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("download: close: %w", err)
	}
	// One final progress tick at 100%.
	if p != nil {
		p(pr.count, pr.total)
	}
	if err := os.Rename(part, dest); err != nil {
		return fmt.Errorf("download: rename: %w", err)
	}
	return nil
}

type progressReader struct {
	r        io.Reader
	count    int64
	total    int64
	cb       ProgressFn
	interval time.Duration
	last     time.Time
}

func (pr *progressReader) Read(b []byte) (int, error) {
	n, err := pr.r.Read(b)
	if n > 0 {
		pr.count += int64(n)
		if pr.cb != nil && time.Since(pr.last) >= pr.interval {
			pr.cb(pr.count, pr.total)
			pr.last = time.Now()
		}
	}
	return n, err
}

// ParseContentLength is exposed for callers (and tests) that want to parse
// the Content-Length header into an int64. Returns -1 when missing or invalid.
func ParseContentLength(h http.Header) int64 {
	s := h.Get("Content-Length")
	if s == "" {
		return -1
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return -1
	}
	return n
}
