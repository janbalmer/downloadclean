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
	"sync"

	"github.com/janbalmer/downloadclean/internal/dlc"
	"github.com/janbalmer/downloadclean/internal/downloader"
	"github.com/janbalmer/downloadclean/internal/hoster"
)

// Job is a single download to perform.
type Job struct {
	// Link is the DLC entry to download.
	Link dlc.Link
	// Hoster resolves Link.URL to a direct download URL. A nil Hoster
	// causes the queue to emit [EventSkipped] for this job.
	Hoster hoster.Hoster
	// OutDir is the directory the final file is written into.
	OutDir string
	// BatchID identifies the group this job belongs to, letting front-ends
	// render multiple .dlc files queued in one session as distinct batches.
	// Zero is a valid value for callers that don't care.
	BatchID int
}

// EventKind describes the type of an [Event].
type EventKind int

const (
	// EventStarted is emitted once a job begins, before resolution.
	EventStarted EventKind = iota
	// EventResolved is emitted after the hoster returns a direct download URL.
	EventResolved
	// EventProgress is emitted periodically while bytes are being received.
	EventProgress
	// EventDone is emitted after the file has been written to its destination.
	EventDone
	// EventFailed is emitted when a job errors at any stage; Err is set.
	EventFailed
	// EventSkipped is emitted when a job is intentionally not run
	// (no hoster, or the destination file already exists).
	EventSkipped
)

// String returns the constant's identifier (e.g. "EventStarted").
func (k EventKind) String() string {
	switch k {
	case EventStarted:
		return "EventStarted"
	case EventResolved:
		return "EventResolved"
	case EventProgress:
		return "EventProgress"
	case EventDone:
		return "EventDone"
	case EventFailed:
		return "EventFailed"
	case EventSkipped:
		return "EventSkipped"
	default:
		return fmt.Sprintf("EventKind(%d)", int(k))
	}
}

// Event reports queue progress. Total may be -1 if unknown.
type Event struct {
	Kind        EventKind
	Index       int // 0-based index within the supplied job slice
	Total       int // total number of jobs (grows if more are appended)
	BatchID     int // mirrors Job.BatchID for the emitting job
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
// destination file already exists. Callers can use [errors.As] to detect
// this case and offer the user an overwrite/rename prompt.
type ErrCollision struct {
	// Path is the destination file that already exists.
	Path string
}

func (e *ErrCollision) Error() string {
	return fmt.Sprintf("queue: destination exists: %s", e.Path)
}

// EventFn receives queue events. Implementations should be cheap.
type EventFn func(Event)

// Runner executes a growable job list sequentially. Append may be called
// from another goroutine while Run is executing; appended jobs are picked
// up by subsequent loop iterations. Sequential execution is intentional —
// do not introduce fan-out here.
type Runner struct {
	mu   sync.Mutex
	jobs []Job
}

// NewRunner returns a Runner pre-seeded with the given initial jobs.
func NewRunner(initial []Job) *Runner {
	r := &Runner{}
	if len(initial) > 0 {
		r.jobs = append(r.jobs, initial...)
	}
	return r
}

// Append adds jobs to the queue and returns the index at which the first
// new job was placed. Safe to call concurrently with Run; if the loop has
// already drained the slice and returned, the appended jobs are simply
// left behind (the caller is responsible for noticing).
func (r *Runner) Append(jobs ...Job) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	start := len(r.jobs)
	r.jobs = append(r.jobs, jobs...)
	return start
}

// Run executes jobs in order on the caller's goroutine. A failure in one
// job is reported as EventFailed and execution continues with the next
// job, unless ctx is canceled (in which case Run returns ctx.Err() without
// emitting a failure event for the cancelled job).
func (r *Runner) Run(ctx context.Context, on EventFn) error {
	emit := func(e Event) {
		if on != nil {
			on(e)
		}
	}
	for i := 0; ; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		r.mu.Lock()
		if i >= len(r.jobs) {
			r.mu.Unlock()
			return nil
		}
		job := r.jobs[i]
		total := len(r.jobs)
		r.mu.Unlock()

		base := Event{
			Index:     i,
			Total:     total,
			BatchID:   job.BatchID,
			Link:      job.Link,
			SizeBytes: -1,
		}

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
			if ctx.Err() != nil {
				return ctx.Err()
			}
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
			if ctx.Err() != nil {
				return ctx.Err()
			}
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
}

// Run is a one-shot helper that executes a static job slice. It exists for
// callers (like the CLI) that don't need to append jobs mid-run.
func Run(ctx context.Context, jobs []Job, on EventFn) error {
	return NewRunner(jobs).Run(ctx, on)
}

func eventWith(base Event, kind EventKind, desc string) Event {
	base.Kind = kind
	base.Description = desc
	return base
}

// pickFilename picks the best filename available, in priority order:
// dlc-provided, hoster-provided, direct URL basename, then public URL basename.
// The dlc name wins so the on-disk file matches what the user saw in their
// container and the TUI's parsed-list view; hosters often rewrite the name
// (Rapidgator returns an opaque CDN filename) which is less recognisable.
func pickFilename(hosterName, dlcName, publicURL, directURL string) string {
	for _, c := range []string{dlcName, hosterName} {
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
