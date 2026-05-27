package main

import (
	"github.com/janbalmer/downloadclean/internal/config"
	"github.com/janbalmer/downloadclean/internal/dlc"
	"github.com/janbalmer/downloadclean/internal/hoster"
	"github.com/janbalmer/downloadclean/internal/queue"
)

// dlcParsedMsg carries the result of a successful .dlc decryption.
type dlcParsedMsg struct {
	Path  string
	Links []dlc.Link
}

// dlcParseErrMsg carries a .dlc decryption or read failure.
type dlcParseErrMsg struct{ Err error }

// accountsLoadedMsg carries a successfully loaded accounts file and the
// hoster registry built from it.
type accountsLoadedMsg struct {
	Accounts *config.Accounts
	Registry *hoster.Registry
}

// accountsErrMsg carries an accounts.json load failure.
type accountsErrMsg struct{ Err error }

// queueEventMsg wraps a single event drained from the bridge channel.
type queueEventMsg struct{ Ev queue.Event }

// queueDoneMsg signals that the queue goroutine returned. Err is nil on
// success, context.Canceled when the user cancelled, or some other error.
type queueDoneMsg struct{ Err error }

// tickMsg drives speed/ETA refreshes on the downloading screen so the
// view re-renders even when no new progress event has arrived.
type tickMsg struct{}
