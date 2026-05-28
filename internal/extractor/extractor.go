// Package extractor unpacks downloaded archives using the 7zz binary (with 7z
// fallback). It runs only as a follow-up step after a sequential download; the
// queue calls it once per archive set. Stdlib-only by convention.
//
// Supported single-file formats: .rar, .7z, .zip, .tar, .tar.gz, .tar.bz2,
// .tar.xz, .gz, .bz2, .xz. Supported multi-volume patterns: foo.partNN.rar
// (modern multi-RAR), foo.7z.NNN (split 7z), foo.rar + foo.r00..foo.rNN
// (legacy multi-RAR — the .rar file is the first volume).
//
// Limitations: tar-compound archives (.tar.gz etc.) extract to a .tar in one
// pass — the inner tar is not re-extracted. Archives inside archives are not
// recursively unpacked.
package extractor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Format identifies the on-disk shape of an archive set.
type Format int

// Format values cover every shape ArchiveSet recognises.
const (
	FormatUnknown Format = iota
	FormatRar
	FormatRarLegacy
	FormatSevenZip
	FormatZip
	FormatTar
	FormatTarGz
	FormatTarBz2
	FormatTarXz
	FormatGz
	FormatBz2
	FormatXz
)

// String returns a short human-readable label for the format.
func (f Format) String() string {
	switch f {
	case FormatRar:
		return "rar"
	case FormatRarLegacy:
		return "rar-legacy"
	case FormatSevenZip:
		return "7z"
	case FormatZip:
		return "zip"
	case FormatTar:
		return "tar"
	case FormatTarGz:
		return "tar.gz"
	case FormatTarBz2:
		return "tar.bz2"
	case FormatTarXz:
		return "tar.xz"
	case FormatGz:
		return "gz"
	case FormatBz2:
		return "bz2"
	case FormatXz:
		return "xz"
	default:
		return "unknown"
	}
}

// SetInfo describes the archive set a given filename belongs to.
//
// Key is the canonical set identifier (the basename stripped of any
// volume suffix). First is true iff the supplied filename is the volume
// 7zz should be invoked on (typically the lowest-numbered volume).
// Volumes is normally empty; populate it via [EnumerateVolumes] when a
// directory context is available.
type SetInfo struct {
	Key     string
	First   bool
	Volumes []string
	Format  Format

	// pattern matches sibling volumes belonging to this set inside a
	// directory. Nil for single-file formats — [EnumerateVolumes] falls
	// back to an os.Stat on Key in that case.
	pattern *regexp.Regexp
	// volumeWidth is the zero-padded volume number width seen in the
	// originating filename (e.g. 2 for .part01.rar, 3 for .7z.001). Zero
	// for single-file and legacy-rar sets, where the trigger filename
	// doesn't depend on a width.
	volumeWidth int
}

// Toggle owns mutable extraction settings shared between the TUI (which
// flips state) and the queue (which reads state per job). Mirrors
// [downloader.RateLimiter]'s internally-synchronized design.
type Toggle struct {
	mu        sync.Mutex
	enabled   bool
	passwords []string
}

// NewToggle returns a Toggle in the disabled state with no passwords set.
func NewToggle() *Toggle {
	return &Toggle{}
}

// Enabled reports whether extraction should run.
func (t *Toggle) Enabled() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.enabled
}

// SetEnabled toggles the extraction switch.
func (t *Toggle) SetEnabled(on bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.enabled = on
}

// Passwords returns a defensive copy of the configured password list, in the
// order [Extract] will try them.
func (t *Toggle) Passwords() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.passwords) == 0 {
		return nil
	}
	return append([]string(nil), t.passwords...)
}

// SetPasswords replaces the password list with a defensive copy of p so
// later caller mutations cannot leak into the stored state.
func (t *Toggle) SetPasswords(p []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(p) == 0 {
		t.passwords = nil
		return
	}
	t.passwords = append([]string(nil), p...)
}

// Compound suffixes (.tar.gz etc.) must be checked before the single-
// extension fallbacks so a .tar.gz isn't misclassified as FormatGz.
var compoundSuffixes = []struct {
	suffix string
	format Format
}{
	{".tar.gz", FormatTarGz},
	{".tar.bz2", FormatTarBz2},
	{".tar.xz", FormatTarXz},
}

var singleSuffixes = map[string]Format{
	".rar": FormatRar,
	".7z":  FormatSevenZip,
	".zip": FormatZip,
	".tar": FormatTar,
	".gz":  FormatGz,
	".bz2": FormatBz2,
	".xz":  FormatXz,
}

// Volume-pattern regexes. Anchored on the full lowercased basename.
var (
	rarPartRE   = regexp.MustCompile(`^(.+)\.part(\d+)\.rar$`)
	sevenZipRE  = regexp.MustCompile(`^(.+)\.7z\.(\d+)$`)
	rarLegacyRE = regexp.MustCompile(`^(.+)\.r(\d+)$`)
)

// IsArchive reports whether the filename's extension matches a supported
// archive format. The check is case-insensitive and recognises multi-volume
// trigger and continuation files.
func IsArchive(name string) bool {
	return ArchiveSet(name).Format != FormatUnknown
}

// ArchiveSet inspects the filename's pattern and returns SetInfo describing
// the multi-volume set it belongs to (or a single-file set). For unknown
// formats returns SetInfo{Format: FormatUnknown}.
//
// The returned SetInfo does not populate Volumes — call [EnumerateVolumes]
// once a directory context is known.
func ArchiveSet(name string) SetInfo {
	base := filepath.Base(name)
	lower := strings.ToLower(base)

	if m := rarPartRE.FindStringSubmatch(lower); m != nil {
		num, err := strconv.Atoi(m[2])
		if err != nil {
			return SetInfo{Format: FormatUnknown}
		}
		key := m[1]
		width := len(m[2])
		pat := regexp.MustCompile(`^` + regexp.QuoteMeta(key) +
			`\.part(\d{` + strconv.Itoa(width) + `,})\.rar$`)
		return SetInfo{
			Key:         key,
			First:       num == 1,
			Format:      FormatRar,
			pattern:     pat,
			volumeWidth: width,
		}
	}

	if m := sevenZipRE.FindStringSubmatch(lower); m != nil {
		num, err := strconv.Atoi(m[2])
		if err != nil {
			return SetInfo{Format: FormatUnknown}
		}
		key := m[1]
		width := len(m[2])
		pat := regexp.MustCompile(`^` + regexp.QuoteMeta(key) +
			`\.7z\.(\d{` + strconv.Itoa(width) + `,})$`)
		return SetInfo{
			Key:         key,
			First:       num == 1,
			Format:      FormatSevenZip,
			pattern:     pat,
			volumeWidth: width,
		}
	}

	if m := rarLegacyRE.FindStringSubmatch(lower); m != nil {
		key := m[1]
		return SetInfo{
			Key:    key,
			First:  false,
			Format: FormatRarLegacy,
		}
	}

	for _, c := range compoundSuffixes {
		if strings.HasSuffix(lower, c.suffix) {
			key := strings.TrimSuffix(lower, c.suffix)
			return SetInfo{
				Key:    key,
				First:  true,
				Format: c.format,
			}
		}
	}

	ext := filepath.Ext(lower)
	if f, ok := singleSuffixes[ext]; ok {
		key := strings.TrimSuffix(lower, ext)
		return SetInfo{
			Key:    key,
			First:  true,
			Format: f,
		}
	}

	return SetInfo{Format: FormatUnknown}
}

// EnumerateVolumes returns the volume files present in dir for the given set.
//
// For single-file formats: returns [name] if a file matching the set's Key+
// extension is present, else nil. For multi-volume formats: returns the
// contiguous volume sequence starting from volume 1 (or volume 0 for legacy
// RAR continuations), sorted in 7zz invocation order. If the sequence has
// gaps the set is considered incomplete and nil is returned.
//
// Filenames are returned without dir prefix, matching the casing used on
// disk so the caller can pass them straight to os.Remove(filepath.Join(...)).
func EnumerateVolumes(dir string, set SetInfo) []string {
	if set.Format == FormatUnknown {
		return nil
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}

	// FormatRar and FormatSevenZip cover both multi-volume (set.pattern
	// populated by ArchiveSet) and single-file (no pattern) shapes — the
	// pattern's presence selects the right enumeration strategy.
	switch set.Format {
	case FormatRar, FormatSevenZip:
		if set.pattern != nil {
			return enumerateNumbered(entries, set, 1)
		}
		return enumerateSingle(entries, set)
	case FormatRarLegacy:
		return enumerateLegacyRar(entries, set)
	default:
		return enumerateSingle(entries, set)
	}
}

// SiblingsPresent reports whether the complete set is on disk. It is a thin
// convenience over [EnumerateVolumes].
func SiblingsPresent(dir string, set SetInfo) bool {
	return len(EnumerateVolumes(dir, set)) > 0
}

// IsMultiVolume reports whether the set spans multiple files on disk.
// Returns true for .partNN.rar, .7z.NNN, and legacy-RAR continuation sets;
// false for single-file archives and unknown formats.
func IsMultiVolume(set SetInfo) bool {
	switch set.Format {
	case FormatRar, FormatSevenZip:
		return set.volumeWidth > 0
	case FormatRarLegacy:
		return true
	default:
		return false
	}
}

// TriggerVolume returns the canonical filename 7zz must be invoked on to
// unpack the set: the lowest-numbered volume for multi-volume formats, the
// .rar for legacy RAR sets, or the sole filename for single-file sets. The
// result is lowercase because Key was lowercased during parsing — callers
// that need on-disk casing should pass it to [EnumerateVolumes] and use the
// first returned entry.
//
// Returns "" when Format is FormatUnknown.
func TriggerVolume(set SetInfo) string {
	switch set.Format {
	case FormatUnknown:
		return ""
	case FormatRar:
		if set.volumeWidth > 0 {
			return fmt.Sprintf("%s.part%0*d.rar", set.Key, set.volumeWidth, 1)
		}
		return set.Key + ".rar"
	case FormatSevenZip:
		if set.volumeWidth > 0 {
			return fmt.Sprintf("%s.7z.%0*d", set.Key, set.volumeWidth, 1)
		}
		return set.Key + ".7z"
	case FormatRarLegacy:
		return set.Key + ".rar"
	default:
		if ext := extensionFor(set.Format); ext != "" {
			return set.Key + ext
		}
		return ""
	}
}

// enumerateNumbered collects contiguous numbered volumes matching set.pattern
// starting at startAt. Returns nil when the run isn't contiguous from startAt.
func enumerateNumbered(entries []os.DirEntry, set SetInfo, startAt int) []string {
	if set.pattern == nil {
		return nil
	}

	type vol struct {
		name string
		num  int
	}
	var vols []vol

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := set.pattern.FindStringSubmatch(strings.ToLower(e.Name()))
		if m == nil {
			continue
		}
		num, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		vols = append(vols, vol{name: e.Name(), num: num})
	}
	if len(vols) == 0 {
		return nil
	}

	sort.Slice(vols, func(i, j int) bool { return vols[i].num < vols[j].num })

	if vols[0].num != startAt {
		return nil
	}
	for i := 1; i < len(vols); i++ {
		if vols[i].num != vols[i-1].num+1 {
			return nil
		}
	}

	out := make([]string, len(vols))
	for i, v := range vols {
		out[i] = v.name
	}
	return out
}

// enumerateLegacyRar gathers the .rar plus contiguous .r00..rNN continuations
// for the given set. The .rar is required (it's the trigger volume); the
// continuations must form a contiguous run starting at 0.
func enumerateLegacyRar(entries []os.DirEntry, set SetInfo) []string {
	keyLower := strings.ToLower(set.Key)
	rarRE := regexp.MustCompile(`^` + regexp.QuoteMeta(keyLower) + `\.rar$`)
	contRE := regexp.MustCompile(`^` + regexp.QuoteMeta(keyLower) + `\.r(\d+)$`)

	var rarName string
	type cont struct {
		name string
		num  int
	}
	var conts []cont

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		lower := strings.ToLower(e.Name())
		if rarRE.MatchString(lower) {
			rarName = e.Name()
			continue
		}
		if m := contRE.FindStringSubmatch(lower); m != nil {
			num, err := strconv.Atoi(m[1])
			if err != nil {
				continue
			}
			conts = append(conts, cont{name: e.Name(), num: num})
		}
	}

	if rarName == "" {
		return nil
	}

	sort.Slice(conts, func(i, j int) bool { return conts[i].num < conts[j].num })
	if len(conts) > 0 {
		if conts[0].num != 0 {
			return nil
		}
		for i := 1; i < len(conts); i++ {
			if conts[i].num != conts[i-1].num+1 {
				return nil
			}
		}
	}

	out := make([]string, 0, 1+len(conts))
	out = append(out, rarName)
	for _, c := range conts {
		out = append(out, c.name)
	}
	return out
}

// enumerateSingle returns [filename] if a file matching the set's Key plus
// expected extension is present in entries. Casing is preserved from disk.
func enumerateSingle(entries []os.DirEntry, set SetInfo) []string {
	ext := extensionFor(set.Format)
	if ext == "" {
		return nil
	}
	want := strings.ToLower(set.Key + ext)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.ToLower(e.Name()) == want {
			return []string{e.Name()}
		}
	}
	return nil
}

// extensionFor returns the on-disk extension for a single-file format.
// Multi-volume formats return "" because their extension depends on the
// volume index.
func extensionFor(f Format) string {
	switch f {
	case FormatTarGz:
		return ".tar.gz"
	case FormatTarBz2:
		return ".tar.bz2"
	case FormatTarXz:
		return ".tar.xz"
	case FormatTar:
		return ".tar"
	case FormatGz:
		return ".gz"
	case FormatBz2:
		return ".bz2"
	case FormatXz:
		return ".xz"
	case FormatZip:
		return ".zip"
	case FormatRar:
		return ".rar"
	case FormatSevenZip:
		return ".7z"
	default:
		return ""
	}
}

// Result captures non-error metadata from a successful Extract.
type Result struct {
	// PasswordUsed is the entry from passwords that unlocked the archive,
	// or empty when no password was needed.
	PasswordUsed string
}

// ErrEncrypted indicates the archive is password-protected and no supplied
// password unlocked it.
type ErrEncrypted struct {
	Path string
}

// Error implements [error].
func (e *ErrEncrypted) Error() string {
	return fmt.Sprintf("extractor: %s is encrypted and no password unlocked it", e.Path)
}

// ErrExtract is returned for 7zz failures other than encryption. ExitCode is
// the binary's exit status; Stderr captures the (possibly truncated) tail of
// its stderr output for diagnostics.
type ErrExtract struct {
	Path     string
	ExitCode int
	Stderr   string
}

// Error implements [error].
func (e *ErrExtract) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("extractor: extract %s: 7zz exit %d", e.Path, e.ExitCode)
	}
	return fmt.Sprintf("extractor: extract %s: 7zz exit %d: %s", e.Path, e.ExitCode, strings.TrimSpace(e.Stderr))
}

// Binary resolves the 7zz/7z binary on PATH, preferring 7zz. Returns an
// error if neither is available.
func Binary() (string, error) {
	if p, err := exec.LookPath("7zz"); err == nil {
		return p, nil
	}
	if p, err := exec.LookPath("7z"); err == nil {
		return p, nil
	}
	return "", errors.New("extractor: 7zz or 7z binary not found on PATH")
}

// progressInterval debounces onProgress invocations from Extract. 200ms is
// fast enough to feel responsive in a TUI without bottlenecking on the
// callback.
const progressInterval = 200 * time.Millisecond

// stderrTailCap bounds how much stderr is preserved for ErrExtract. The
// keepalive cost is one allocation per extraction; the tail is enough to
// see 7zz's "Errors:" block on any realistic failure.
const stderrTailCap = 4 * 1024

// encryptionMarkers are case-insensitive substrings 7zz prints when it
// fails because the archive is encrypted (or the supplied password is
// wrong).
var encryptionMarkers = []string{
	"wrong password",
	"cannot open encrypted archive",
	"can not open encrypted archive",
	"data error in encrypted file",
}

// Extract invokes 7zz to unpack archive into destDir. If the archive is
// encrypted, it tries each entry of passwords in order until one succeeds.
// onProgress, if non-nil, is invoked with extraction percentage (0-100),
// debounced to roughly 200ms and deduplicated against the last value.
//
// On success returns a Result and nil. On encryption failure with no
// working password returns *[ErrEncrypted]. On other extraction errors
// returns *[ErrExtract]. Context cancellation terminates 7zz and returns
// ctx.Err(); the archive is not removed.
func Extract(ctx context.Context, archive, destDir string, passwords []string, onProgress func(percent int)) (Result, error) {
	bin, err := Binary()
	if err != nil {
		return Result{}, err
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return Result{}, fmt.Errorf("extractor: mkdir %s: %w", destDir, err)
	}

	// First attempt with the explicit "no password" flag so 7zz fails fast
	// on encrypted archives instead of waiting on stdin.
	err = runOnce(ctx, bin, archive, destDir, "-p-", onProgress)
	if err == nil {
		return Result{}, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return Result{}, ctxErr
	}

	var ee *ErrExtract
	if !errors.As(err, &ee) {
		return Result{}, err
	}
	if !looksEncrypted(ee.Stderr) {
		return Result{}, err
	}

	if len(passwords) == 0 {
		return Result{}, &ErrEncrypted{Path: archive}
	}

	for _, pwd := range passwords {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Result{}, ctxErr
		}
		perr := runOnce(ctx, bin, archive, destDir, "-p"+pwd, onProgress)
		if perr == nil {
			return Result{PasswordUsed: pwd}, nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return Result{}, ctxErr
		}
		var pee *ErrExtract
		if !errors.As(perr, &pee) {
			return Result{}, perr
		}
		if !looksEncrypted(pee.Stderr) {
			return Result{}, perr
		}
	}
	return Result{}, &ErrEncrypted{Path: archive}
}

// looksEncrypted reports whether stderr contains any known 7zz encryption-
// failure marker.
func looksEncrypted(stderr string) bool {
	lower := strings.ToLower(stderr)
	for _, m := range encryptionMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// runOnce invokes 7zz a single time with the given password flag (which may
// be "-p-" for "no password"). Returns nil on success (exit code 0 or 1 —
// 1 is "non-fatal warning" per 7zz convention) and *ErrExtract or ctx.Err()
// otherwise.
func runOnce(ctx context.Context, bin, archive, destDir, passFlag string, onProgress func(percent int)) error {
	// -aoa overwrites existing files. We need it because a -p- first
	// pass on an encrypted archive leaves 0-byte placeholder files in
	// destDir; the password retry must clobber those rather than skip.
	cmd := exec.CommandContext(ctx, bin,
		"x", archive,
		"-o"+destDir,
		"-y",
		"-aoa",
		"-bsp1", // progress to stdout
		"-bse2", // errors to stderr
		"-bb0",  // suppress per-file log noise
		passFlag,
	)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("extractor: extract %s: stdout pipe: %w", archive, err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("extractor: extract %s: stderr pipe: %w", archive, err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("extractor: extract %s: start: %w", archive, err)
	}

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		scanProgress(stdout, onProgress)
	}()

	tail := newTailBuffer(stderrTailCap)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(tail, stderr)
	}()

	waitErr := cmd.Wait()
	wg.Wait()

	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	exitCode := 0
	if waitErr != nil {
		var xerr *exec.ExitError
		if errors.As(waitErr, &xerr) {
			exitCode = xerr.ExitCode()
		} else {
			return fmt.Errorf("extractor: extract %s: wait: %w", archive, waitErr)
		}
	}

	// Per 7zz/7z convention, exit code 1 is a non-fatal warning (e.g.
	// recoverable CRC mismatch on a non-critical file). Anything >= 2 is
	// a hard failure.
	if exitCode >= 2 {
		return &ErrExtract{
			Path:     archive,
			ExitCode: exitCode,
			Stderr:   tail.String(),
		}
	}

	return nil
}

// scanProgress reads 7zz's stdout line by line and translates "NN%" tokens
// into debounced, deduplicated onProgress calls.
func scanProgress(r io.Reader, onProgress func(percent int)) {
	if onProgress == nil {
		// Drain stdout so the child doesn't block on a full pipe.
		_, _ = io.Copy(io.Discard, r)
		return
	}

	// 7zz writes progress as "\b\b\b\b\b 47% ..." with carriage-return /
	// backspace, never newlines, so a plain bufio.Scanner won't tick. Read
	// in modest chunks and scan for the percentage on each chunk instead.
	pctRE := regexp.MustCompile(`\b(\d{1,3})%`)
	buf := make([]byte, 4096)

	lastPct := -1
	lastEmit := time.Time{}

	for {
		n, err := r.Read(buf)
		if n > 0 {
			matches := pctRE.FindAllSubmatch(buf[:n], -1)
			for _, m := range matches {
				pct, perr := strconv.Atoi(string(m[1]))
				if perr != nil || pct < 0 || pct > 100 {
					continue
				}
				if pct == lastPct {
					continue
				}
				now := time.Now()
				if pct < 100 && now.Sub(lastEmit) < progressInterval {
					continue
				}
				lastPct = pct
				lastEmit = now
				onProgress(pct)
			}
		}
		if err != nil {
			return
		}
	}
}

// tailBuffer keeps the last limit bytes written to it; older bytes are
// dropped. It is an io.Writer suitable for io.Copy.
type tailBuffer struct {
	limit int
	buf   []byte
}

// newTailBuffer constructs a tailBuffer retaining at most limit bytes.
// limit <= 0 is treated as 1 so the buffer always has a positive cap.
func newTailBuffer(limit int) *tailBuffer {
	if limit <= 0 {
		limit = 1
	}
	return &tailBuffer{limit: limit}
}

// Write appends p to the buffer, discarding the oldest bytes when the
// total would exceed the limit.
func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if n >= t.limit {
		t.buf = append(t.buf[:0], p[n-t.limit:]...)
		return n, nil
	}
	t.buf = append(t.buf, p...)
	if overflow := len(t.buf) - t.limit; overflow > 0 {
		t.buf = t.buf[overflow:]
	}
	return n, nil
}

// String returns the tail as a string. The result aliases the buffer; do
// not call concurrently with Write.
func (t *tailBuffer) String() string {
	return string(t.buf)
}
