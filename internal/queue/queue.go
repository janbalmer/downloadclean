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
	"github.com/janbalmer/downloadclean/internal/extractor"
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
	// EventExtractStarted is emitted just before 7zz is invoked on a
	// completed archive (or multi-volume set). Event.Filename is the
	// trigger volume; Event.DestPath is the directory extracted into.
	EventExtractStarted
	// EventExtractProgress is emitted as 7zz reports progress; the
	// percentage is in Event.ExtractPercent.
	EventExtractProgress
	// EventExtractDone is emitted after a successful extraction. The
	// original archive volume(s) have been removed from disk.
	EventExtractDone
	// EventExtractFailed is emitted when extraction fails. Event.Err is
	// set; consumers can use errors.As to detect *extractor.ErrEncrypted
	// (password required / wrong) or *extractor.ErrExtract (other 7zz
	// failure).
	EventExtractFailed
	// EventExtractSkipped is emitted at end of Run for any multi-volume
	// archive set whose trigger or siblings never arrived. Event.Filename
	// is the canonical trigger volume name (which may not exist on disk);
	// Event.Description spells out what's missing.
	EventExtractSkipped
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
	case EventExtractStarted:
		return "EventExtractStarted"
	case EventExtractProgress:
		return "EventExtractProgress"
	case EventExtractDone:
		return "EventExtractDone"
	case EventExtractFailed:
		return "EventExtractFailed"
	case EventExtractSkipped:
		return "EventExtractSkipped"
	default:
		return fmt.Sprintf("EventKind(%d)", int(k))
	}
}

// Event reports queue progress. Total may be -1 if unknown.
type Event struct {
	Kind           EventKind
	Index          int // 0-based index within the supplied job slice
	Total          int // total number of jobs (grows if more are appended)
	BatchID        int // mirrors Job.BatchID for the emitting job
	Link           dlc.Link
	Hoster         string // hoster name, e.g. "rapidgator" (empty for EventSkipped due to no hoster)
	Filename       string // populated once known
	Downloaded     int64
	SizeBytes      int64 // -1 if unknown
	DestPath       string
	Err            error
	Description    string // human-readable note (skip reason, etc.)
	ExtractPercent int    // 0-100, populated only for EventExtractProgress
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
	// RateLimiter, if non-nil, throttles every Download in this run.
	// Safe to mutate concurrently via its Set* methods.
	RateLimiter *downloader.RateLimiter
	// Extractor, if non-nil and Extractor.Enabled(), triggers archive
	// extraction after each successful download. Multi-volume archive
	// sets are extracted once their final volume completes; incomplete
	// sets at end of Run produce EventExtractSkipped. Safe to mutate
	// concurrently via Extractor's Set* methods.
	Extractor *extractor.Toggle

	// pending tracks multi-volume archive sets discovered during Run
	// whose trigger or sibling volumes have not all arrived. Keyed by
	// SetInfo.Key. Touched only on the Run goroutine, hence no mutex.
	pending map[string]pendingExtract
}

// pendingExtract records a multi-volume set that cannot yet be extracted.
// trigger is the canonical first-volume filename so end-of-run reporting
// names something recognizable even when the trigger itself never arrived.
type pendingExtract struct {
	set     extractor.SetInfo
	outDir  string
	trigger string
}

// NewRunner returns a Runner pre-seeded with the given initial jobs.
func NewRunner(initial []Job) *Runner {
	r := &Runner{
		pending: make(map[string]pendingExtract),
	}
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
			r.sweepPending(emit)
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
		}, downloader.Options{RateLimiter: r.RateLimiter})
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

		r.maybeExtract(ctx, base, i, job.OutDir, filename, emit)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// Run is a one-shot helper that executes a static job slice. It exists for
// callers (like the CLI) that don't need to append jobs mid-run.
func Run(ctx context.Context, jobs []Job, on EventFn) error {
	return NewRunner(jobs).Run(ctx, on)
}

// maybeExtract inspects the filename just produced by a successful download
// and either extracts immediately, stashes the volume in r.pending until
// siblings arrive, or returns silently (extractor off, non-archive file).
// base carries Index/Total/BatchID/Link/Hoster pre-populated for the current
// job so emitted extract events line up with the originating job; doneIndex
// is the index of that job within r.jobs, used to look at the remaining
// queue for sibling volumes.
func (r *Runner) maybeExtract(ctx context.Context, base Event, doneIndex int, outDir, filename string, emit EventFn) {
	if r.Extractor == nil || !r.Extractor.Enabled() {
		return
	}
	if !extractor.IsArchive(filename) {
		return
	}
	set := extractor.ArchiveSet(filename)
	if set.Format == extractor.FormatUnknown {
		return
	}

	trigger := extractor.TriggerVolume(set)

	// Multi-volume sets defer extraction until no more siblings remain in
	// the queue — otherwise enumerating the partial run on disk yields a
	// contiguous-but-incomplete sequence that 7zz reports as truncated.
	if extractor.IsMultiVolume(set) && r.hasPendingSiblings(doneIndex, set.Key) {
		if _, ok := r.pending[set.Key]; !ok {
			r.pending[set.Key] = pendingExtract{set: set, outDir: outDir, trigger: trigger}
		}
		return
	}

	volumes := extractor.EnumerateVolumes(outDir, set)
	if len(volumes) == 0 {
		// Single-file trigger missing (race / hoster mis-named it) or
		// multi-volume non-trigger without trigger on disk. Either way,
		// stash and let sweepPending report at end of run.
		r.pending[set.Key] = pendingExtract{set: set, outDir: outDir, trigger: trigger}
		return
	}

	r.runExtraction(ctx, base, outDir, volumes, emit)
	delete(r.pending, set.Key)

	// If this completion satisfied a previously-stashed sibling set, flush.
	r.flushPending(ctx, doneIndex, base, emit)
}

// hasPendingSiblings reports whether any job in r.jobs[afterIndex+1:] is
// likely to deliver another volume of the set identified by key. The check
// is best-effort: it inspects Job.Link.Name and the URL basename — these
// match in the common case (DLCs use real filenames; URLs end in them) —
// and may miss hoster-rewritten names, in which case we extract early and
// 7zz fails noisily. The user can re-trigger by re-running.
func (r *Runner) hasPendingSiblings(afterIndex int, key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := afterIndex + 1; i < len(r.jobs); i++ {
		j := r.jobs[i]
		for _, name := range []string{j.Link.Name, basenameFromURL(j.Link.URL)} {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			s := extractor.ArchiveSet(name)
			if s.Format != extractor.FormatUnknown && s.Key == key {
				return true
			}
		}
	}
	return false
}

// flushPending re-checks every stashed set for completeness and extracts
// any that became whole and no longer have pending siblings in the queue.
// Called after a successful extraction in case a sibling completion also
// rounded out a different pending set, and at end of Run.
func (r *Runner) flushPending(ctx context.Context, doneIndex int, base Event, emit EventFn) {
	for key, entry := range r.pending {
		if ctx.Err() != nil {
			return
		}
		if extractor.IsMultiVolume(entry.set) && r.hasPendingSiblings(doneIndex, entry.set.Key) {
			continue
		}
		volumes := extractor.EnumerateVolumes(entry.outDir, entry.set)
		if len(volumes) == 0 {
			continue
		}
		r.runExtraction(ctx, base, entry.outDir, volumes, emit)
		delete(r.pending, key)
	}
}

// sweepPending fires EventExtractSkipped for every entry still pending at
// the end of Run. A final flushPending attempt runs first so any set that
// completed during the tail of the job list still extracts. The doneIndex
// passed to flushPending is past the end of the job list so the sibling
// check sees no remaining queue.
func (r *Runner) sweepPending(emit EventFn) {
	r.mu.Lock()
	endIdx := len(r.jobs)
	r.mu.Unlock()
	r.flushPending(context.Background(), endIdx, Event{SizeBytes: -1}, emit)
	for key, entry := range r.pending {
		ev := Event{
			Kind:        EventExtractSkipped,
			Filename:    entry.trigger,
			DestPath:    entry.outDir,
			SizeBytes:   -1,
			Description: describePending(entry),
		}
		emit(ev)
		delete(r.pending, key)
	}
}

// describePending summarises why entry can't be extracted. It distinguishes
// the "trigger never arrived" case from the "trigger present but siblings
// missing" case — the latter we can't enumerate precisely without knowing
// the total volume count.
func describePending(entry pendingExtract) string {
	if entry.trigger == "" {
		return fmt.Sprintf("multi-volume set %s incomplete", entry.set.Key)
	}
	triggerPath := filepath.Join(entry.outDir, entry.trigger)
	if _, err := os.Stat(triggerPath); err != nil {
		return fmt.Sprintf("incomplete archive set; trigger volume %s missing", entry.trigger)
	}
	return fmt.Sprintf("incomplete archive set; siblings of %s missing", entry.trigger)
}

// runExtraction invokes the extractor on the supplied volume list and
// emits EventExtractStarted/Progress/Done/Failed accordingly. On success
// the volume files are removed; a removal error becomes a note on the
// Done event rather than a failure (the archive contents are already
// safely extracted).
func (r *Runner) runExtraction(ctx context.Context, base Event, outDir string, volumes []string, emit EventFn) {
	archiveName := volumes[0]
	archivePath := filepath.Join(outDir, archiveName)

	started := base
	started.Kind = EventExtractStarted
	started.Filename = archiveName
	started.DestPath = outDir
	emit(started)

	passwords := r.Extractor.Passwords()
	lastPct := -1
	onProgress := func(pct int) {
		if pct == lastPct {
			return
		}
		lastPct = pct
		ev := base
		ev.Kind = EventExtractProgress
		ev.Filename = archiveName
		ev.DestPath = outDir
		ev.ExtractPercent = pct
		emit(ev)
	}

	if _, err := extractor.Extract(ctx, archivePath, outDir, passwords, onProgress); err != nil {
		if ctx.Err() != nil {
			return
		}
		ev := base
		ev.Kind = EventExtractFailed
		ev.Filename = archiveName
		ev.DestPath = outDir
		ev.Err = err
		emit(ev)
		return
	}

	var removeErr error
	for _, v := range volumes {
		if err := os.Remove(filepath.Join(outDir, v)); err != nil && removeErr == nil {
			removeErr = err
		}
	}

	done := base
	done.Kind = EventExtractDone
	done.Filename = archiveName
	done.DestPath = outDir
	if removeErr != nil {
		done.Description = fmt.Sprintf("archive(s) removed with errors: %v", removeErr)
	}
	emit(done)
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
