// Command downloadclean is a JDownloader-style sequential link grabber.
// v1 is CLI-only; a Bubble Tea TUI will land in a later iteration.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/janbalmer/downloadclean/internal/config"
	"github.com/janbalmer/downloadclean/internal/dlc"
	"github.com/janbalmer/downloadclean/internal/extractor"
	"github.com/janbalmer/downloadclean/internal/hoster"
	"github.com/janbalmer/downloadclean/internal/hoster/rapidgator"
	"github.com/janbalmer/downloadclean/internal/queue"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		accountsPath = flag.String("accounts", config.DefaultPath(), "path to accounts.json")
		configPath   = flag.String("config", config.DefaultConfigPath(), "path to config.json (archive passwords, etc.); missing file is ignored")
		dlcPath      = flag.String("dlc", "", "path to .dlc file (required)")
		outDir       = flag.String("output", config.DefaultOutputDir(), "directory to download into")
		insecure     = flag.Bool("insecure-config", false, "skip the file-permission checks on accounts.json and config.json")
		list         = flag.Bool("list", false, "decrypt and print links, do not download")
		limit        = flag.Int("limit", 0, "download at most this many links (0 = all)")
		extract      = flag.Bool("extract", false, "auto-extract downloaded archives and delete the archive on success")
	)
	flag.Parse()

	if *dlcPath == "" {
		flag.Usage()
		return fmt.Errorf("--dlc is required")
	}
	if *limit < 0 {
		return fmt.Errorf("--limit must be >= 0")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	dlcFile, err := os.Open(*dlcPath)
	if err != nil {
		return fmt.Errorf("open dlc: %w", err)
	}
	defer dlcFile.Close()

	parser := &dlc.Parser{}
	links, err := parser.Parse(ctx, dlcFile)
	if err != nil {
		return fmt.Errorf("parse dlc: %w", err)
	}
	if len(links) == 0 {
		return fmt.Errorf("dlc contained no links")
	}
	fmt.Fprintf(os.Stderr, "%d link(s) parsed from %s\n", len(links), *dlcPath)

	if *list {
		for i, l := range links {
			fmt.Printf("%d\t%s\t%s\t%d\t%s\n", i+1, l.URL, l.Name, l.Size, l.Package)
		}
		return nil
	}

	if *insecure {
		fmt.Fprintln(os.Stderr, "warning: --insecure-config: skipping file-permission checks on accounts.json and config.json")
	}
	accs, err := config.Load(*accountsPath, *insecure)
	if err != nil {
		return err
	}
	cfg, err := config.LoadConfig(*configPath, *insecure)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}

	reg := hoster.NewRegistry()
	if accs.Rapidgator != nil && accs.Rapidgator.Login != "" {
		reg.Register(rapidgator.New(accs.Rapidgator.Login, accs.Rapidgator.Password))
	}

	if *limit > 0 {
		n := min(*limit, len(links))
		if n < len(links) {
			fmt.Fprintf(os.Stderr, "limit: taking first %d of %d link(s)\n", n, len(links))
			links = links[:n]
		}
	}

	jobs := make([]queue.Job, 0, len(links))
	for _, l := range links {
		h := reg.Find(l.URL)
		jobs = append(jobs, queue.Job{Link: l, Hoster: h, OutDir: *outDir})
	}

	runner := queue.NewRunner(jobs)
	if *extract {
		tog := extractor.NewToggle()
		tog.SetEnabled(true)
		tog.SetPasswords(cfg.ArchivePasswords)
		runner.Extractor = tog
	}

	printer := newPrinter()
	if err := runner.Run(ctx, printer.handle); err != nil {
		return err
	}
	printer.summary()
	if printer.failed > 0 {
		return fmt.Errorf("%d job(s) failed", printer.failed)
	}
	return nil
}

type printer struct {
	done           int
	failed         int
	skipped        int
	extracted      int
	extractFailed  int
	extractSkipped int
	lastLen        int
}

func newPrinter() *printer {
	return &printer{}
}

func (p *printer) clearLine() {
	if p.lastLen > 0 {
		fmt.Fprintf(os.Stdout, "\r%s\r", strings.Repeat(" ", p.lastLen))
		p.lastLen = 0
	}
}

func (p *printer) write(line string) {
	fmt.Fprint(os.Stdout, line)
	p.lastLen = len(line)
}

func (p *printer) handle(e queue.Event) {
	tag := eventTag(e)
	switch e.Kind {
	case queue.EventStarted:
		p.clearLine()
		fmt.Fprintf(os.Stdout, "%s starting: %s\n", tag, e.Link.URL)
	case queue.EventResolved:
		fmt.Fprintf(os.Stdout, "%s resolved: %s (%s)\n", tag, e.Filename, humanSize(e.SizeBytes))
	case queue.EventProgress:
		p.clearLine()
		p.write(fmt.Sprintf("%s %s  %s", tag, e.Filename, progressBar(e.Downloaded, e.SizeBytes)))
	case queue.EventDone:
		p.clearLine()
		fmt.Fprintf(os.Stdout, "%s done: %s\n", tag, e.DestPath)
		p.done++
	case queue.EventFailed:
		p.clearLine()
		fmt.Fprintf(os.Stdout, "%s FAILED: %s — %v\n", tag, e.Link.URL, e.Err)
		p.failed++
	case queue.EventSkipped:
		fmt.Fprintf(os.Stdout, "%s skipped: %s — %s\n", tag, e.Link.URL, e.Description)
		p.skipped++
	case queue.EventExtractStarted:
		p.clearLine()
		fmt.Fprintf(os.Stdout, "%s extract: %s\n", tag, e.Filename)
	case queue.EventExtractProgress:
		p.clearLine()
		// %3d keeps the percent column fixed so the overwriting line doesn't
		// jitter as the value crosses 10 and 100.
		p.write(fmt.Sprintf("%s extracting: %s  %3d%%", tag, e.Filename, clampPct(e.ExtractPercent)))
	case queue.EventExtractDone:
		p.clearLine()
		if e.Description != "" {
			fmt.Fprintf(os.Stdout, "%s extracted: %s — %s\n", tag, e.Filename, e.Description)
		} else {
			fmt.Fprintf(os.Stdout, "%s extracted: %s\n", tag, e.Filename)
		}
		p.extracted++
	case queue.EventExtractFailed:
		p.clearLine()
		reason := extractFailureReason(e.Err)
		fmt.Fprintf(os.Stdout, "%s extract failed: %s — %s\n", tag, e.Filename, reason)
		p.extractFailed++
	case queue.EventExtractSkipped:
		p.clearLine()
		fmt.Fprintf(os.Stdout, "%s extract skipped: %s — %s\n", tag, e.Filename, e.Description)
		p.extractSkipped++
	}
}

// eventTag formats the per-event "[i/n]" prefix. Sweep-time extract events
// (emitted by queue.sweepPending at end of Run) have no originating job and
// arrive with Total == 0; for those a positional tag would be nonsensical,
// so it is omitted.
func eventTag(e queue.Event) string {
	if e.Total <= 0 {
		return "[--]"
	}
	return fmt.Sprintf("[%d/%d]", e.Index+1, e.Total)
}

// clampPct constrains an extractor progress value into the 0-100 display
// range so a malformed 7zz line never produces a "120%" or "-5%" frame.
func clampPct(p int) int {
	return min(100, max(0, p))
}

// extractFailureReason maps an extraction error to a short human-readable
// description, collapsing the encrypted-archive case to a stable string the
// user can search for instead of the raw "extractor: ... encrypted ..."
// wrapping.
func extractFailureReason(err error) string {
	if err == nil {
		return "unknown error"
	}
	var enc *extractor.ErrEncrypted
	if errors.As(err, &enc) {
		return "encrypted, no working password"
	}
	return err.Error()
}

func (p *printer) summary() {
	p.clearLine()
	fmt.Fprintf(os.Stdout, "summary: %d done, %d failed, %d skipped\n", p.done, p.failed, p.skipped)
	if p.extracted > 0 || p.extractFailed > 0 || p.extractSkipped > 0 {
		fmt.Fprintf(os.Stdout, "extract: %d extracted, %d failed, %d skipped\n", p.extracted, p.extractFailed, p.extractSkipped)
	}
}

func progressBar(done, total int64) string {
	if total <= 0 {
		return humanSize(done)
	}
	pct := float64(done) / float64(total) * 100
	return fmt.Sprintf("%.1f%% (%s / %s)", pct, humanSize(done), humanSize(total))
}

func humanSize(n int64) string {
	if n < 0 {
		return "?"
	}
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for x := n / unit; x >= unit; x /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
