package main

import (
	"context"
	"fmt"
	"os"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/janbalmer/downloadclean/internal/config"
	"github.com/janbalmer/downloadclean/internal/dlc"
	"github.com/janbalmer/downloadclean/internal/downloader"
	"github.com/janbalmer/downloadclean/internal/extractor"
	"github.com/janbalmer/downloadclean/internal/hoster"
	"github.com/janbalmer/downloadclean/internal/hoster/rapidgator"
	"github.com/janbalmer/downloadclean/internal/queue"
)

// parseDLCCmd opens the file at path and decrypts it through dlc.Parser.
// The parser hits a remote dlcrypt service, but the round-trip is short
// enough to share the program's background context.
func parseDLCCmd(path string) tea.Cmd {
	return func() tea.Msg {
		f, err := os.Open(path)
		if err != nil {
			return dlcParseErrMsg{Err: fmt.Errorf("open dlc: %w", err)}
		}
		defer f.Close()

		links, err := (&dlc.Parser{}).Parse(context.Background(), f)
		if err != nil {
			return dlcParseErrMsg{Err: fmt.Errorf("parse dlc: %w", err)}
		}
		return dlcParsedMsg{Path: path, Links: links}
	}
}

// newDisplayRegistry returns a registry seeded with credential-less hoster
// clients, suitable for URL→name lookups (e.g. the parsed-screen hoster
// column) before the real accounts.json has been read. Resolve calls on
// these clients will fail; only Name and Matches are usable here.
func newDisplayRegistry() *hoster.Registry {
	return hoster.NewRegistry(rapidgator.New("", ""))
}

// loadAccountsCmd reads accounts.json and builds a hoster registry seeded
// with whatever hosters have credentials configured.
func loadAccountsCmd(accountsPath string, insecure bool) tea.Cmd {
	return func() tea.Msg {
		accs, err := config.Load(accountsPath, insecure)
		if err != nil {
			return accountsErrMsg{Err: err}
		}
		reg := hoster.NewRegistry()
		if accs.Rapidgator != nil && accs.Rapidgator.Login != "" {
			reg.Register(rapidgator.New(accs.Rapidgator.Login, accs.Rapidgator.Password))
		}
		return accountsLoadedMsg{Accounts: accs, Registry: reg}
	}
}

// runQueue launches Runner.Run on its own goroutine and returns the runner
// (for mid-run Append) and the channels the model needs to drain. The
// events channel is closed before the error is sent on done, so a
// waitForEvent that observes a closed channel knows the terminal event
// will arrive via waitForDone. A non-nil limiter throttles every download
// in the run; pass nil to disable throttling entirely. A non-nil extract
// toggle wires post-download extraction (only runs when the toggle is
// Enabled at the moment each download completes).
func runQueue(parent context.Context, jobs []queue.Job, limiter *downloader.RateLimiter, extract *extractor.Toggle) (
	*queue.Runner, context.CancelFunc, <-chan queue.Event, <-chan error,
) {
	ctx, cancel := context.WithCancel(parent)
	events := make(chan queue.Event, 64)
	done := make(chan error, 1)
	runner := queue.NewRunner(jobs)
	runner.RateLimiter = limiter
	runner.Extractor = extract

	go func() {
		err := runner.Run(ctx, func(e queue.Event) {
			events <- e
		})
		close(events)
		done <- err
		close(done)
	}()

	return runner, cancel, events, done
}

// waitForEvent yields one event from the bridge. Returning nil on a closed
// channel lets the runtime drop the message — waitForDone carries the
// terminal signal so the model still transitions to the summary screen.
func waitForEvent(events <-chan queue.Event) tea.Cmd {
	return func() tea.Msg {
		ev, ok := <-events
		if !ok {
			return nil
		}
		return queueEventMsg{Ev: ev}
	}
}

// waitForDone blocks until the queue goroutine reports its terminal error.
func waitForDone(done <-chan error) tea.Cmd {
	return func() tea.Msg {
		return queueDoneMsg{Err: <-done}
	}
}

// tickCmd schedules the next speed/ETA refresh on the downloading screen.
func tickCmd() tea.Cmd {
	return tea.Tick(500*time.Millisecond, func(time.Time) tea.Msg {
		return tickMsg{}
	})
}
