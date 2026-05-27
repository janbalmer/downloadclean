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
	"strings"
	"time"
)

// ProgressFn receives the number of bytes downloaded and the total expected
// (-1 if unknown). It may be called frequently — implementations should be
// cheap or rate-limit themselves.
type ProgressFn func(downloaded, total int64)

// Options configures Download. Zero values use sensible defaults.
type Options struct {
	// HTTPClient overrides the HTTP client used for the download. Nil means
	// a default client with no overall timeout (suitable for large files).
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
		// Server honored the Range request. Validate Content-Range so a server
		// that returns 206 but with the wrong offset can't silently corrupt the
		// resume.
		if cr := resp.Header.Get("Content-Range"); cr != "" {
			if start, ok := parseContentRangeStart(cr); !ok || start != startOffset {
				return fmt.Errorf("download: server returned 206 with Content-Range %q, want start=%d", cr, startOffset)
			}
		}
	default:
		return fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}

	total := int64(-1)
	if resp.ContentLength >= 0 {
		total = startOffset + resp.ContentLength
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
	// One final progress tick at 100%. If total was unknown, it equals count now.
	if p != nil {
		finalTotal := pr.total
		if finalTotal < 0 {
			finalTotal = pr.count
		}
		p(pr.count, finalTotal)
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

// parseContentRangeStart returns the start byte from a Content-Range header
// of the form "bytes <start>-<end>/<total>". Returns (0, false) on parse error.
func parseContentRangeStart(h string) (int64, bool) {
	rest, ok := strings.CutPrefix(h, "bytes ")
	if !ok {
		return 0, false
	}
	startStr, _, ok := strings.Cut(rest, "-")
	if !ok {
		return 0, false
	}
	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil {
		return 0, false
	}
	return start, true
}
