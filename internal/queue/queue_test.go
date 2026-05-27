package queue

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
	"sync"
	"testing"

	"github.com/janbalmer/downloadclean/internal/dlc"
	"github.com/janbalmer/downloadclean/internal/hoster"
)

// fakeHoster is a test double letting each test program the Resolve behavior.
type fakeHoster struct {
	name    string
	resolve func(ctx context.Context, link string) (hoster.Resolved, error)
}

func (f *fakeHoster) Name() string                 { return f.name }
func (f *fakeHoster) Matches(link string) bool     { return true }
func (f *fakeHoster) Resolve(ctx context.Context, link string) (hoster.Resolved, error) {
	return f.resolve(ctx, link)
}

func eventKinds(events []Event) []EventKind {
	out := make([]EventKind, len(events))
	for i, e := range events {
		out[i] = e.Kind
	}
	return out
}

func containsInOrder(haystack, needle []EventKind) bool {
	i := 0
	for _, h := range haystack {
		if i < len(needle) && h == needle[i] {
			i++
		}
	}
	return i == len(needle)
}

func TestRun_HappyPath(t *testing.T) {
	payload := []byte("hello queue, this is the file")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	h := &fakeHoster{
		name: "fake",
		resolve: func(ctx context.Context, link string) (hoster.Resolved, error) {
			return hoster.Resolved{DirectURL: srv.URL, Filename: "out.bin", Size: int64(len(payload))}, nil
		},
	}
	jobs := []Job{{Link: dlc.Link{URL: "https://example/file1"}, Hoster: h, OutDir: dir}}

	var events []Event
	if err := Run(t.Context(), jobs, func(e Event) { events = append(events, e) }); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if !containsInOrder(eventKinds(events), []EventKind{EventStarted, EventResolved, EventDone}) {
		t.Errorf("event order = %v", eventKinds(events))
	}

	got, err := os.ReadFile(filepath.Join(dir, "out.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("contents = %q, want %q", got, payload)
	}
}

func TestRun_CollisionSkipped(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "out.bin")
	if err := os.WriteFile(dest, []byte("already here"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := &fakeHoster{
		name: "fake",
		resolve: func(ctx context.Context, link string) (hoster.Resolved, error) {
			return hoster.Resolved{DirectURL: "http://unused", Filename: "out.bin"}, nil
		},
	}
	jobs := []Job{{Link: dlc.Link{URL: "https://example/file1"}, Hoster: h, OutDir: dir}}

	var skipped Event
	if err := Run(t.Context(), jobs, func(e Event) {
		if e.Kind == EventSkipped {
			skipped = e
		}
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if skipped.Kind != EventSkipped {
		t.Fatal("expected EventSkipped")
	}
	var col *ErrCollision
	if !errors.As(skipped.Err, &col) {
		t.Fatalf("expected *ErrCollision, got %T: %v", skipped.Err, skipped.Err)
	}
	if col.Path != dest {
		t.Errorf("col.Path = %q, want %q", col.Path, dest)
	}
}

func TestRun_NoHosterSkipped(t *testing.T) {
	jobs := []Job{{Link: dlc.Link{URL: "https://example/file1"}, Hoster: nil, OutDir: t.TempDir()}}

	var ev Event
	if err := Run(t.Context(), jobs, func(e Event) { ev = e }); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if ev.Kind != EventSkipped {
		t.Errorf("Kind = %v, want EventSkipped", ev.Kind)
	}
	if !strings.Contains(ev.Description, "no hoster") {
		t.Errorf("Description = %q", ev.Description)
	}
	if ev.Err != nil {
		t.Errorf("Err should be nil for no-hoster skip: %v", ev.Err)
	}
}

func TestRun_ResolveError(t *testing.T) {
	h := &fakeHoster{
		name: "fake",
		resolve: func(ctx context.Context, link string) (hoster.Resolved, error) {
			return hoster.Resolved{}, fmt.Errorf("nope")
		},
	}
	jobs := []Job{{Link: dlc.Link{URL: "https://example/file1"}, Hoster: h, OutDir: t.TempDir()}}

	var failed Event
	if err := Run(t.Context(), jobs, func(e Event) {
		if e.Kind == EventFailed {
			failed = e
		}
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if failed.Kind != EventFailed {
		t.Fatal("expected EventFailed")
	}
	if failed.Err == nil || !strings.Contains(failed.Err.Error(), "nope") {
		t.Errorf("Err = %v", failed.Err)
	}
}

func TestRun_ContextCancelDuringResolve(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	h := &fakeHoster{
		name: "fake",
		resolve: func(ctx context.Context, link string) (hoster.Resolved, error) {
			cancel()
			<-ctx.Done()
			return hoster.Resolved{}, ctx.Err()
		},
	}
	jobs := []Job{
		{Link: dlc.Link{URL: "https://example/file1"}, Hoster: h, OutDir: t.TempDir()},
		{Link: dlc.Link{URL: "https://example/file2"}, Hoster: h, OutDir: t.TempDir()},
	}

	var failed int
	err := Run(ctx, jobs, func(e Event) {
		if e.Kind == EventFailed {
			failed++
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if failed != 0 {
		t.Errorf("expected 0 EventFailed (cancellation isn't a failure), got %d", failed)
	}
}

func TestRunner_AppendDuringRun(t *testing.T) {
	payload := []byte("xx")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()

	// Gate the first resolve until the test appends two more jobs. Subsequent
	// resolves run without blocking, so the runner drains the appended jobs.
	gate := make(chan struct{})
	var firstOnce bool
	h := &fakeHoster{
		name: "fake",
		resolve: func(ctx context.Context, link string) (hoster.Resolved, error) {
			if !firstOnce {
				firstOnce = true
				<-gate
			}
			return hoster.Resolved{DirectURL: srv.URL, Filename: link[strings.LastIndex(link, "/")+1:], Size: int64(len(payload))}, nil
		},
	}

	mkJob := func(name string, batch int) Job {
		return Job{
			Link:    dlc.Link{URL: "https://example/" + name},
			Hoster:  h,
			OutDir:  dir,
			BatchID: batch,
		}
	}

	runner := NewRunner([]Job{mkJob("a.bin", 1)})

	var (
		mu     sync.Mutex
		events []Event
		done   = make(chan error, 1)
	)
	go func() {
		done <- runner.Run(t.Context(), func(e Event) {
			mu.Lock()
			events = append(events, e)
			mu.Unlock()
		})
	}()

	// Append batch 2 while batch 1 is blocked in Resolve. Start index should
	// be 1 (one job already in the slice).
	if got := runner.Append(mkJob("b.bin", 2), mkJob("c.bin", 2)); got != 1 {
		t.Errorf("Append start index = %d, want 1", got)
	}
	close(gate)

	if err := <-done; err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	var doneEvents []Event
	for _, e := range events {
		if e.Kind == EventDone {
			doneEvents = append(doneEvents, e)
		}
	}
	if len(doneEvents) != 3 {
		t.Fatalf("EventDone count = %d, want 3", len(doneEvents))
	}

	// First job is batch 1; the appended pair is batch 2.
	wantBatches := []int{1, 2, 2}
	for i, ev := range doneEvents {
		if ev.BatchID != wantBatches[i] {
			t.Errorf("doneEvents[%d].BatchID = %d, want %d", i, ev.BatchID, wantBatches[i])
		}
	}
	// Final Total must reflect the post-append size.
	if got := doneEvents[len(doneEvents)-1].Total; got != 3 {
		t.Errorf("final Total = %d, want 3", got)
	}
}

func TestPickFilename(t *testing.T) {
	cases := []struct {
		name                                       string
		hosterName, dlcName, publicURL, directURL string
		want                                       string
	}{
		{"dlc-wins", "from_hoster.rar", "from_dlc", "https://x/foo", "https://y/bar", "from_dlc"},
		{"hoster-fallback", "from_hoster.rar", "", "https://x/foo", "https://y/bar", "from_hoster.rar"},
		{"dlc-set-only", "", "from_dlc.rar", "https://x/foo", "", "from_dlc.rar"},
		{"direct-url-fallback", "", "", "https://public/page", "https://cdn/real.bin", "real.bin"},
		{"public-url-fallback", "", "", "https://public/page.html", "", "page.html"},
		{"sanitize-strips-separators", "../../etc/passwd", "", "", "", ".._.._etc_passwd"},
		{"all-empty", "", "", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := pickFilename(c.hosterName, c.dlcName, c.publicURL, c.directURL)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}
