package queue

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/janbalmer/downloadclean/internal/dlc"
	"github.com/janbalmer/downloadclean/internal/extractor"
	"github.com/janbalmer/downloadclean/internal/hoster"
)

// fakeHoster is a test double letting each test program the Resolve behavior.
type fakeHoster struct {
	name    string
	resolve func(ctx context.Context, link string) (hoster.Resolved, error)
}

func (f *fakeHoster) Name() string             { return f.name }
func (f *fakeHoster) Matches(link string) bool { return true }
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

// requireExtractor skips the test when no 7zz/7z binary is on PATH.
func requireExtractor(t *testing.T) {
	t.Helper()
	if _, err := extractor.Binary(); err != nil {
		t.Skipf("no 7zz/7z on PATH: %v", err)
	}
}

// serveBytes returns an httptest.Server that responds to every request with
// payload. The Cleanup is registered on t.
func serveBytes(t *testing.T, payload []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", strconv.Itoa(len(payload)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// makeZipBytes builds a zip archive in memory with the given members.
func makeZipBytes(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
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
	return buf.Bytes()
}

// makeSplit7z shells out to the 7zz/7z binary to produce a split 7z. It
// returns the volume contents in numeric order. Skips the test when the
// binary cannot create the requested split (older 7-Zip builds with no
// split support, etc.).
func makeSplit7z(t *testing.T, payload []byte) [][]byte {
	t.Helper()
	bin, err := extractor.Binary()
	if err != nil {
		t.Skipf("no 7zz/7z on PATH: %v", err)
	}

	work := t.TempDir()
	src := filepath.Join(work, "src.bin")
	if err := os.WriteFile(src, payload, 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}

	out := filepath.Join(work, "split.7z")
	cmd := exec.Command(bin, "a", "-v1k", out, src)
	cmd.Dir = work
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Skipf("create split 7z: %v\n%s", err, output)
	}

	var volumes [][]byte
	for i := 1; ; i++ {
		path := filepath.Join(work, fmt.Sprintf("split.7z.%03d", i))
		data, err := os.ReadFile(path)
		if err != nil {
			break
		}
		volumes = append(volumes, data)
	}
	if len(volumes) < 2 {
		t.Skipf("split 7z produced only %d volume(s); need >= 2 for this test", len(volumes))
	}
	return volumes
}

// makeEncryptedZipBytes uses the 7zz/7z binary to produce an AES-encrypted
// zip with the given member. Returns the raw archive bytes.
func makeEncryptedZipBytes(t *testing.T, password, member, contents string) []byte {
	t.Helper()
	bin, err := extractor.Binary()
	if err != nil {
		t.Skipf("no 7zz/7z on PATH: %v", err)
	}

	work := t.TempDir()
	src := filepath.Join(work, member)
	if err := os.WriteFile(src, []byte(contents), 0o644); err != nil {
		t.Fatalf("write src: %v", err)
	}
	out := filepath.Join(work, "enc.zip")
	cmd := exec.Command(bin, "a", "-tzip", "-p"+password, "-mem=AES256", out, src)
	cmd.Dir = work
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("create encrypted zip: %v\n%s", err, output)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read encrypted zip: %v", err)
	}
	return data
}

func TestRun_NoExtractWhenToggleOff(t *testing.T) {
	requireExtractor(t)

	payload := makeZipBytes(t, map[string][]byte{"hello.txt": []byte("hi")})
	url := serveBytes(t, payload)
	dir := t.TempDir()

	h := &fakeHoster{
		name: "fake",
		resolve: func(ctx context.Context, link string) (hoster.Resolved, error) {
			return hoster.Resolved{DirectURL: url, Filename: "thing.zip", Size: int64(len(payload))}, nil
		},
	}
	jobs := []Job{{Link: dlc.Link{URL: "https://example/thing.zip"}, Hoster: h, OutDir: dir}}

	runner := NewRunner(jobs)
	runner.Extractor = extractor.NewToggle() // disabled by default

	var events []Event
	if err := runner.Run(t.Context(), func(e Event) { events = append(events, e) }); err != nil {
		t.Fatalf("Run: %v", err)
	}

	for _, e := range events {
		switch e.Kind {
		case EventExtractStarted, EventExtractProgress, EventExtractDone, EventExtractFailed, EventExtractSkipped:
			t.Errorf("unexpected extract event %v emitted with toggle off", e.Kind)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "thing.zip")); err != nil {
		t.Errorf("archive missing on disk: %v", err)
	}
}

func TestRun_ExtractSingleZip(t *testing.T) {
	requireExtractor(t)

	payload := makeZipBytes(t, map[string][]byte{"hello.txt": []byte("hi")})
	url := serveBytes(t, payload)
	dir := t.TempDir()

	h := &fakeHoster{
		name: "fake",
		resolve: func(ctx context.Context, link string) (hoster.Resolved, error) {
			return hoster.Resolved{DirectURL: url, Filename: "thing.zip", Size: int64(len(payload))}, nil
		},
	}
	jobs := []Job{{Link: dlc.Link{URL: "https://example/thing.zip"}, Hoster: h, OutDir: dir}}

	runner := NewRunner(jobs)
	runner.Extractor = extractor.NewToggle()
	runner.Extractor.SetEnabled(true)

	var events []Event
	if err := runner.Run(t.Context(), func(e Event) { events = append(events, e) }); err != nil {
		t.Fatalf("Run: %v", err)
	}

	want := []EventKind{EventStarted, EventResolved, EventDone, EventExtractStarted, EventExtractDone}
	if !containsInOrder(eventKinds(events), want) {
		t.Errorf("event order = %v, want subsequence %v", eventKinds(events), want)
	}

	got, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(got) != "hi" {
		t.Errorf("hello.txt = %q, want %q", got, "hi")
	}
	if _, err := os.Stat(filepath.Join(dir, "thing.zip")); !os.IsNotExist(err) {
		t.Errorf("zip should be removed after extract; stat err = %v", err)
	}
}

func TestRun_ExtractMultiVolume7z(t *testing.T) {
	requireExtractor(t)

	payload := make([]byte, 2048)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	volumes := makeSplit7z(t, payload)
	dir := t.TempDir()

	// Each volume becomes its own job with a dedicated URL on the test server.
	type vol struct {
		name string
		data []byte
	}
	vols := make([]vol, len(volumes))
	for i, v := range volumes {
		vols[i] = vol{name: fmt.Sprintf("split.7z.%03d", i+1), data: v}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for _, v := range vols {
			if strings.HasSuffix(r.URL.Path, v.name) {
				w.Header().Set("Content-Length", strconv.Itoa(len(v.data)))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(v.data)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	h := &fakeHoster{
		name: "fake",
		resolve: func(ctx context.Context, link string) (hoster.Resolved, error) {
			name := link[strings.LastIndex(link, "/")+1:]
			for _, v := range vols {
				if v.name == name {
					return hoster.Resolved{
						DirectURL: srv.URL + "/" + name,
						Filename:  name,
						Size:      int64(len(v.data)),
					}, nil
				}
			}
			return hoster.Resolved{}, fmt.Errorf("unknown link %q", link)
		},
	}

	jobs := make([]Job, len(vols))
	for i, v := range vols {
		jobs[i] = Job{
			Link:   dlc.Link{URL: "https://example/" + v.name},
			Hoster: h,
			OutDir: dir,
		}
	}

	runner := NewRunner(jobs)
	runner.Extractor = extractor.NewToggle()
	runner.Extractor.SetEnabled(true)

	var (
		mu     sync.Mutex
		events []Event
	)
	if err := runner.Run(t.Context(), func(e Event) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	var starts, dones, fails int
	for _, e := range events {
		switch e.Kind {
		case EventExtractStarted:
			starts++
		case EventExtractDone:
			dones++
		case EventExtractFailed:
			fails++
			t.Errorf("unexpected EventExtractFailed: %v", e.Err)
		}
	}
	if starts != 1 || dones != 1 {
		t.Errorf("extract events: started=%d done=%d failed=%d, want 1/1/0", starts, dones, fails)
	}

	for _, v := range vols {
		if _, err := os.Stat(filepath.Join(dir, v.name)); !os.IsNotExist(err) {
			t.Errorf("volume %s should be removed; stat err = %v", v.name, err)
		}
	}
	got, err := os.ReadFile(filepath.Join(dir, "src.bin"))
	if err != nil {
		t.Fatalf("read extracted payload: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("extracted payload mismatch (len got=%d want=%d)", len(got), len(payload))
	}
}

func TestRun_ExtractMultiVolumeIncomplete(t *testing.T) {
	requireExtractor(t)

	payload := make([]byte, 2048)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	volumes := makeSplit7z(t, payload)
	dir := t.TempDir()

	// Queue only a non-trigger volume. The trigger (.001) never arrives, so
	// the set can never be extracted — sweepPending must report it skipped.
	siblingName := "split.7z.002"
	url := serveBytes(t, volumes[1])

	h := &fakeHoster{
		name: "fake",
		resolve: func(ctx context.Context, link string) (hoster.Resolved, error) {
			return hoster.Resolved{
				DirectURL: url,
				Filename:  siblingName,
				Size:      int64(len(volumes[1])),
			}, nil
		},
	}
	jobs := []Job{{Link: dlc.Link{URL: "https://example/" + siblingName}, Hoster: h, OutDir: dir}}

	runner := NewRunner(jobs)
	runner.Extractor = extractor.NewToggle()
	runner.Extractor.SetEnabled(true)

	var events []Event
	if err := runner.Run(t.Context(), func(e Event) { events = append(events, e) }); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var started, skipped int
	var skippedEv Event
	for _, e := range events {
		switch e.Kind {
		case EventExtractStarted:
			started++
		case EventExtractSkipped:
			skipped++
			skippedEv = e
		}
	}
	if started != 0 {
		t.Errorf("EventExtractStarted count = %d, want 0", started)
	}
	if skipped != 1 {
		t.Errorf("EventExtractSkipped count = %d, want 1", skipped)
	}
	if skippedEv.Filename != "split.7z.001" {
		t.Errorf("skipped Filename = %q, want %q", skippedEv.Filename, "split.7z.001")
	}
	if _, err := os.Stat(filepath.Join(dir, siblingName)); err != nil {
		t.Errorf("sibling volume should still be on disk: %v", err)
	}
}

// TestRun_ExtractLegacyRarAllVolumesRemoved guards the legacy-RAR deletion
// bug: when name.rar is the last file downloaded in a name.rar+name.rNN set,
// the queue used to delete only name.rar because EnumerateVolumes saw
// ArchiveSet("name.rar") as FormatRar single. With the extractor patched to
// probe for legacy continuations when the trigger is a bare .rar, all
// volumes must be removed after a successful extraction.
//
// 7zz identifies archive format by content, so we serve a valid zip as
// "old.rar" — that lets the extraction step succeed without needing the
// rar codec or a separate split-archive tool. The .r00/.r01 files are
// dummy bytes; they exist purely so EnumerateVolumes can see them on disk.
func TestRun_ExtractLegacyRarAllVolumesRemoved(t *testing.T) {
	requireExtractor(t)

	rarPayload := makeZipBytes(t, map[string][]byte{"hello.txt": []byte("hi")})
	contPayload := []byte("filler")

	files := map[string][]byte{
		"old.r00": contPayload,
		"old.r01": contPayload,
		"old.rar": rarPayload,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		for name, data := range files {
			if strings.HasSuffix(r.URL.Path, name) {
				w.Header().Set("Content-Length", strconv.Itoa(len(data)))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(data)
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	h := &fakeHoster{
		name: "fake",
		resolve: func(ctx context.Context, link string) (hoster.Resolved, error) {
			name := link[strings.LastIndex(link, "/")+1:]
			if data, ok := files[name]; ok {
				return hoster.Resolved{
					DirectURL: srv.URL + "/" + name,
					Filename:  name,
					Size:      int64(len(data)),
				}, nil
			}
			return hoster.Resolved{}, fmt.Errorf("unknown link %q", link)
		},
	}

	// Order matters: old.rar last so the bug used to fire — the prior
	// jobs stash in pending, then the .rar arrives, EnumerateVolumes runs
	// in the FormatRar single branch, and runExtraction sweeps the
	// volume list. Before the fix only old.rar was removed.
	order := []string{"old.r00", "old.r01", "old.rar"}
	jobs := make([]Job, len(order))
	for i, name := range order {
		jobs[i] = Job{
			Link:   dlc.Link{URL: "https://example/" + name},
			Hoster: h,
			OutDir: dir,
		}
	}

	runner := NewRunner(jobs)
	runner.Extractor = extractor.NewToggle()
	runner.Extractor.SetEnabled(true)

	var (
		mu     sync.Mutex
		events []Event
	)
	if err := runner.Run(t.Context(), func(e Event) {
		mu.Lock()
		events = append(events, e)
		mu.Unlock()
	}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()

	var started, done, failed int
	for _, e := range events {
		switch e.Kind {
		case EventExtractStarted:
			started++
		case EventExtractDone:
			done++
		case EventExtractFailed:
			failed++
			t.Errorf("unexpected EventExtractFailed: %v", e.Err)
		}
	}
	if started != 1 || done != 1 {
		t.Errorf("extract events: started=%d done=%d failed=%d, want 1/1/0", started, done, failed)
	}

	for name := range files {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("volume %s should be removed; stat err = %v", name, err)
		}
	}
}

func TestRun_ExtractToggledMidRun(t *testing.T) {
	requireExtractor(t)

	payload := makeZipBytes(t, map[string][]byte{"hello.txt": []byte("hi")})
	url := serveBytes(t, payload)
	dir1, dir2 := t.TempDir(), t.TempDir()

	// Gate the second resolve until the toggle flips so we deterministically
	// observe the off→on transition between the two jobs.
	gate := make(chan struct{})
	calls := 0
	var mu sync.Mutex
	h := &fakeHoster{
		name: "fake",
		resolve: func(ctx context.Context, link string) (hoster.Resolved, error) {
			mu.Lock()
			calls++
			n := calls
			mu.Unlock()
			if n == 2 {
				<-gate
			}
			name := link[strings.LastIndex(link, "/")+1:]
			return hoster.Resolved{DirectURL: url, Filename: name, Size: int64(len(payload))}, nil
		},
	}
	jobs := []Job{
		{Link: dlc.Link{URL: "https://example/first.zip"}, Hoster: h, OutDir: dir1},
		{Link: dlc.Link{URL: "https://example/second.zip"}, Hoster: h, OutDir: dir2},
	}

	runner := NewRunner(jobs)
	runner.Extractor = extractor.NewToggle()

	var (
		emu    sync.Mutex
		events []Event
		flip   sync.Once
		done   = make(chan error, 1)
	)
	go func() {
		done <- runner.Run(t.Context(), func(e Event) {
			emu.Lock()
			events = append(events, e)
			emu.Unlock()

			// Flip when the second job starts: at this moment the first
			// job's maybeExtract has already run (with toggle off) and
			// the second hoster Resolve is blocked on `gate`, so we can
			// safely enable extraction and unblock it.
			if e.Kind == EventStarted && strings.Contains(e.Link.URL, "second.zip") {
				flip.Do(func() {
					runner.Extractor.SetEnabled(true)
					close(gate)
				})
			}
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not finish within 10s")
	}

	emu.Lock()
	defer emu.Unlock()

	var firstExtract, secondExtract bool
	for _, e := range events {
		if e.Kind != EventExtractDone {
			continue
		}
		if e.DestPath == dir1 {
			firstExtract = true
		}
		if e.DestPath == dir2 {
			secondExtract = true
		}
	}
	if firstExtract {
		t.Error("first download should not have been extracted (toggle was off)")
	}
	if !secondExtract {
		t.Error("second download should have been extracted (toggle flipped on)")
	}
	if _, err := os.Stat(filepath.Join(dir1, "first.zip")); err != nil {
		t.Errorf("first zip should remain on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir2, "second.zip")); !os.IsNotExist(err) {
		t.Errorf("second zip should be removed after extract; stat err = %v", err)
	}
}

func TestRun_ExtractEncryptedWithPassword(t *testing.T) {
	requireExtractor(t)

	payload := makeEncryptedZipBytes(t, "swordfish", "secret.txt", "shhh")
	url := serveBytes(t, payload)
	dir := t.TempDir()

	h := &fakeHoster{
		name: "fake",
		resolve: func(ctx context.Context, link string) (hoster.Resolved, error) {
			return hoster.Resolved{DirectURL: url, Filename: "enc.zip", Size: int64(len(payload))}, nil
		},
	}
	jobs := []Job{{Link: dlc.Link{URL: "https://example/enc.zip"}, Hoster: h, OutDir: dir}}

	runner := NewRunner(jobs)
	runner.Extractor = extractor.NewToggle()
	runner.Extractor.SetEnabled(true)
	runner.Extractor.SetPasswords([]string{"swordfish"})

	var events []Event
	if err := runner.Run(t.Context(), func(e Event) { events = append(events, e) }); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var done bool
	for _, e := range events {
		if e.Kind == EventExtractFailed {
			t.Errorf("unexpected EventExtractFailed: %v", e.Err)
		}
		if e.Kind == EventExtractDone {
			done = true
		}
	}
	if !done {
		t.Fatal("expected EventExtractDone")
	}
	if got, err := os.ReadFile(filepath.Join(dir, "secret.txt")); err != nil {
		t.Errorf("read extracted: %v", err)
	} else if string(got) != "shhh" {
		t.Errorf("secret.txt = %q, want %q", got, "shhh")
	}
	if _, err := os.Stat(filepath.Join(dir, "enc.zip")); !os.IsNotExist(err) {
		t.Errorf("encrypted zip should be removed; stat err = %v", err)
	}
}

func TestRun_ExtractEncryptedWrongPassword(t *testing.T) {
	requireExtractor(t)

	payload := makeEncryptedZipBytes(t, "swordfish", "secret.txt", "shhh")
	url := serveBytes(t, payload)
	dir := t.TempDir()

	h := &fakeHoster{
		name: "fake",
		resolve: func(ctx context.Context, link string) (hoster.Resolved, error) {
			return hoster.Resolved{DirectURL: url, Filename: "enc.zip", Size: int64(len(payload))}, nil
		},
	}
	jobs := []Job{{Link: dlc.Link{URL: "https://example/enc.zip"}, Hoster: h, OutDir: dir}}

	runner := NewRunner(jobs)
	runner.Extractor = extractor.NewToggle()
	runner.Extractor.SetEnabled(true)
	runner.Extractor.SetPasswords([]string{"wrong"})

	var events []Event
	if err := runner.Run(t.Context(), func(e Event) { events = append(events, e) }); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var failedEv Event
	var failed int
	for _, e := range events {
		if e.Kind == EventExtractDone {
			t.Error("unexpected EventExtractDone with wrong password")
		}
		if e.Kind == EventExtractFailed {
			failed++
			failedEv = e
		}
	}
	if failed != 1 {
		t.Fatalf("EventExtractFailed count = %d, want 1", failed)
	}
	var ee *extractor.ErrEncrypted
	if !errors.As(failedEv.Err, &ee) {
		t.Fatalf("Err = %T (%v), want *extractor.ErrEncrypted", failedEv.Err, failedEv.Err)
	}
	if _, err := os.Stat(filepath.Join(dir, "enc.zip")); err != nil {
		t.Errorf("encrypted zip should remain on disk: %v", err)
	}
}

func TestPickFilename(t *testing.T) {
	cases := []struct {
		name                                      string
		hosterName, dlcName, publicURL, directURL string
		want                                      string
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
