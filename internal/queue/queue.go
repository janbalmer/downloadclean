// Package queue runs download Jobs sequentially.
package queue

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/janbalmer/downloadclean/internal/dlc"
	"github.com/janbalmer/downloadclean/internal/downloader"
	"github.com/janbalmer/downloadclean/internal/hoster"
)

// Job is a single download to perform.
type Job struct {
	Link   dlc.Link
	Hoster hoster.Hoster
	OutDir string
}

// EventKind describes the type of a JobEvent.
type EventKind int

const (
	EventStarted EventKind = iota
	EventResolved
	EventProgress
	EventDone
	EventFailed
	EventSkipped
)

// Event reports queue progress. Total may be -1 if unknown.
type Event struct {
	Kind        EventKind
	Index       int    // 0-based index within the supplied job slice
	Total       int    // total number of jobs
	Link        dlc.Link
	Hoster      string // hoster name, e.g. "rapidgator" (empty for EventSkipped due to no hoster)
	Filename    string // populated once known
	Downloaded  int64
	SizeBytes   int64 // -1 if unknown
	DestPath    string
	Err         error
	Description string // human-readable note (skip reason, etc.)
}

// ErrCollision is set on Event.Err when a job is skipped because the
// destination file already exists. Callers can use errors.As to detect
// this case and offer the user an overwrite/rename prompt.
type ErrCollision struct {
	Path string
}

func (e *ErrCollision) Error() string {
	return fmt.Sprintf("queue: destination exists: %s", e.Path)
}

// EventFn receives queue events. Implementations should be cheap.
type EventFn func(Event)

// Run executes jobs in order. A failure in one job is reported as EventFailed
// and execution continues with the next job, unless ctx is canceled.
func Run(ctx context.Context, jobs []Job, on EventFn) error {
	emit := func(e Event) {
		if on != nil {
			on(e)
		}
	}
	for i, job := range jobs {
		if err := ctx.Err(); err != nil {
			return err
		}
		base := Event{Index: i, Total: len(jobs), Link: job.Link, SizeBytes: -1}

		if job.Hoster == nil {
			ev := base
			ev.Kind = EventSkipped
			ev.Description = "no hoster registered for this link"
			emit(ev)
			continue
		}
		base.Hoster = job.Hoster.Name()
		emit(eventWith(base, EventStarted, ""))

		resolved, err := job.Hoster.Resolve(ctx, job.Link.URL)
		if err != nil {
			ev := base
			ev.Kind = EventFailed
			ev.Err = fmt.Errorf("resolve: %w", err)
			emit(ev)
			continue
		}

		filename := pickFilename(resolved.Filename, job.Link.Name, job.Link.URL, resolved.DirectURL)
		if filename == "" {
			ev := base
			ev.Kind = EventFailed
			ev.Err = fmt.Errorf("queue: pick filename: no candidate found")
			emit(ev)
			continue
		}

		size := resolved.Size
		if size == 0 {
			size = -1
		}

		dest := filepath.Join(job.OutDir, filename)
		evResolved := base
		evResolved.Kind = EventResolved
		evResolved.Filename = filename
		evResolved.DestPath = dest
		evResolved.SizeBytes = size
		emit(evResolved)

		// Refuse to overwrite an existing file at dest. Stale .part files are
		// fine (the downloader will resume them); a complete dest means a
		// previous run, or an earlier job in this batch, already produced it.
		if _, statErr := os.Stat(dest); statErr == nil {
			ev := base
			ev.Kind = EventSkipped
			ev.Filename = filename
			ev.DestPath = dest
			ev.Description = "destination file already exists"
			ev.Err = &ErrCollision{Path: dest}
			emit(ev)
			continue
		}

		if err := os.MkdirAll(job.OutDir, 0o755); err != nil {
			ev := base
			ev.Kind = EventFailed
			ev.Filename = filename
			ev.DestPath = dest
			ev.Err = fmt.Errorf("mkdir: %w", err)
			emit(ev)
			continue
		}

		err = downloader.Download(ctx, resolved.DirectURL, dest, func(done, total int64) {
			ev := base
			ev.Kind = EventProgress
			ev.Filename = filename
			ev.DestPath = dest
			ev.Downloaded = done
			ev.SizeBytes = total
			emit(ev)
		}, downloader.Options{})
		if err != nil {
			ev := base
			ev.Kind = EventFailed
			ev.Filename = filename
			ev.DestPath = dest
			ev.Err = err
			emit(ev)
			continue
		}

		evDone := base
		evDone.Kind = EventDone
		evDone.Filename = filename
		evDone.DestPath = dest
		evDone.SizeBytes = size
		emit(evDone)
	}
	return nil
}

func eventWith(base Event, kind EventKind, desc string) Event {
	base.Kind = kind
	base.Description = desc
	return base
}

// pickFilename picks the best filename available, in priority order:
// hoster-provided, dlc-provided, direct URL basename, then public URL basename.
func pickFilename(hosterName, dlcName, publicURL, directURL string) string {
	for _, c := range []string{hosterName, dlcName} {
		c = strings.TrimSpace(c)
		if c != "" {
			return sanitize(c)
		}
	}
	for _, u := range []string{directURL, publicURL} {
		if name := basenameFromURL(u); name != "" {
			return sanitize(name)
		}
	}
	return ""
}

func basenameFromURL(s string) string {
	if s == "" {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil {
		return ""
	}
	b := path.Base(u.Path)
	if b == "." || b == "/" || b == "" {
		return ""
	}
	return b
}

func sanitize(name string) string {
	// Strip path separators so a hoster cannot redirect us outside OutDir.
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return ""
	}
	return name
}
