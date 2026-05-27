// Command downloadclean is a JDownloader-style sequential link grabber.
// v1 is CLI-only; a Bubble Tea TUI will land in a later iteration.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/janbalmer/downloadclean/internal/config"
	"github.com/janbalmer/downloadclean/internal/dlc"
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
		dlcPath      = flag.String("dlc", "", "path to .dlc file (required)")
		outDir       = flag.String("output", config.DefaultOutputDir(), "directory to download into")
		insecure     = flag.Bool("insecure-config", false, "skip the file-permission check on accounts.json")
		list         = flag.Bool("list", false, "decrypt and print links, do not download")
		limit        = flag.Int("limit", 0, "download at most this many links (0 = all)")
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
		fmt.Fprintln(os.Stderr, "warning: --insecure-config: skipping file-permission checks on accounts.json")
	}
	accs, err := config.Load(*accountsPath, *insecure)
	if err != nil {
		return err
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

	printer := newPrinter()
	if err := queue.Run(ctx, jobs, printer.handle); err != nil {
		return err
	}
	printer.summary()
	if printer.failed > 0 {
		return fmt.Errorf("%d job(s) failed", printer.failed)
	}
	return nil
}

type printer struct {
	done    int
	failed  int
	skipped int
	lastLen int
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
	tag := fmt.Sprintf("[%d/%d]", e.Index+1, e.Total)
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
	}
}

func (p *printer) summary() {
	p.clearLine()
	fmt.Fprintf(os.Stdout, "summary: %d done, %d failed, %d skipped\n", p.done, p.failed, p.skipped)
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
