package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/janbalmer/downloadclean/internal/dlc"
	"github.com/janbalmer/downloadclean/internal/downloader"
	"github.com/janbalmer/downloadclean/internal/extractor"
	"github.com/janbalmer/downloadclean/internal/queue"
)

// rowsOffsetY is the vertical offset, inside the inner frame body, at
// which the parsed-screen table's first data row appears. It accounts for
// the outer frame title (1) + subtitle (1) + blank (1) + parsed-screen
// header line (1) + table header row (1) + table border line (1).
const rowsOffsetY = 6

// displayName returns the user-facing name for a link, matching what the
// parsed-list table shows so the downloading screen stays consistent. The
// queue tracks the hoster-rewritten filename on disk; the UI uses the dlc-
// declared name.
func displayName(link dlc.Link) string {
	if link.Name != "" {
		return link.Name
	}
	return link.URL
}

// batchByJobIndex returns the batch that owns the queue job at idx, or nil
// if no batch covers that index (shouldn't happen in practice — every
// running job was placed by startBatch or appendBatch).
func (m *model) batchByJobIndex(idx int) *batchInfo {
	for i := len(m.batches) - 1; i >= 0; i-- {
		if idx >= m.batches[i].startIdx {
			return &m.batches[i]
		}
	}
	return nil
}

// batchStatus categorises a batch for the status column on the downloading
// screen. A batch is "active" while it has at least one started-but-not-
// finished job, "done" when every job has reached a terminal kind, and
// "queued" before any of its jobs starts.
func (m *model) batchStatus(b *batchInfo) string {
	if b.finished() >= b.count && b.count > 0 {
		return "done"
	}
	if b.id == m.active.batchID {
		return "active"
	}
	if b.finished() > 0 {
		// Some jobs already finished but a different batch is active — this
		// can only happen if every job in this batch was skipped without an
		// EventStarted, which still counts as completed.
		return "done"
	}
	return "queued"
}

// updatePicker handles input on the file-picker screen. The textinput
// captures printable runes (paths can contain q/a/n/spaces) so we only
// intercept the structural keys: Enter to submit, Esc/Ctrl+C to exit. When
// addMode is set, Esc returns to the downloading screen instead of quitting
// and a successful parse will append to the running queue.
func updatePicker(m model, msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			if m.addMode {
				// Cancel both the add flow and the running queue, mirroring
				// the downloading screen's quit behavior.
				if m.cancel != nil {
					m.cancel()
					m.cancel = nil
				}
				m.addMode = false
				m.addSourcePath = ""
				m.parsing = false
				m.links = nil
				m.selected = nil
				m.pathInput.Reset()
				m.screen = screenDownloading
				return m, nil
			}
			return m, tea.Quit
		case "esc":
			if m.addMode {
				m.addMode = false
				m.addSourcePath = ""
				m.parsing = false
				m.links = nil
				m.selected = nil
				m.pathInput.Reset()
				m.screen = screenDownloading
				return m, nil
			}
			return m, tea.Quit
		case "enter":
			raw := m.pathInput.Value()
			path := normalizeDroppedPath(raw)
			if path == "" {
				m.err = errors.New("no path given")
				return m, nil
			}
			m.err = nil
			m.parsing = true
			m.addSourcePath = path
			return m, tea.Batch(parseDLCCmd(path), m.parseSpinner.Tick)
		}
		var cmd tea.Cmd
		m.pathInput, cmd = m.pathInput.Update(msg)
		return m, cmd

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.parseSpinner, cmd = m.parseSpinner.Update(msg)
		return m, cmd

	case dlcParsedMsg:
		m.parsing = false
		m.links = msg.Links
		m.selected = make([]bool, len(msg.Links))
		for i := range m.selected {
			m.selected[i] = true
		}
		if msg.Path != "" {
			m.addSourcePath = msg.Path
		}
		m.table = buildLinkTable(m, m.innerWidth()-4)
		m.screen = screenParsed
		return m, nil

	case dlcParseErrMsg:
		m.parsing = false
		m.err = msg.Err
		return m, nil
	}

	var cmd tea.Cmd
	m.pathInput, cmd = m.pathInput.Update(msg)
	return m, cmd
}

// viewPicker renders the centred file path prompt and any banner.
func viewPicker(m model) string {
	hint := m.theme.Accent.Render("▶ ") +
		m.theme.Subtitle.Render("drop a .dlc file here or type a path…")

	inputBox := m.theme.Input.Render(m.pathInput.View())

	spin := ""
	if m.parsing {
		spin = m.parseSpinner.View() + " " +
			m.theme.Muted.Render("decrypting…")
	}

	banner := ""
	if m.err != nil {
		banner = m.theme.Banner.
			Foreground(lipgloss.Color(HexErrorRed)).
			Render(m.err.Error())
	}

	stack := lipgloss.JoinVertical(lipgloss.Left,
		hint,
		"",
		inputBox,
		spin,
	)

	body := lipgloss.Place(m.innerWidth(), 0, lipgloss.Center, lipgloss.Top, stack)
	if banner != "" {
		body = lipgloss.JoinVertical(lipgloss.Left, body, "", banner)
	}
	return body
}

// updateParsed handles input on the link-selection screen. When addMode is
// set, Back returns to the downloading screen (without disturbing the
// running queue) and Enter appends the selection as a new batch instead of
// starting a fresh download.
func updateParsed(m model, msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, m.keys.Quit):
			if m.addMode {
				// Ctrl+C / q during the add flow cancels both the add and
				// the queue itself, matching the downloading screen.
				if m.cancel != nil {
					m.cancel()
					m.cancel = nil
				}
				m.addMode = false
				m.addSourcePath = ""
				m.links = nil
				m.selected = nil
				m.pathInput.Reset()
				m.parseBanner = ""
				m.screen = screenDownloading
				return m, nil
			}
			return m, tea.Quit
		case key.Matches(msg, m.keys.Back):
			if m.addMode {
				m.addMode = false
				m.addSourcePath = ""
				m.links = nil
				m.selected = nil
				m.pathInput.Reset()
				m.parseBanner = ""
				m.screen = screenDownloading
				return m, nil
			}
			m.screen = screenPicker
			m.links = nil
			m.selected = nil
			m.parseBanner = ""
			m.pathInput.Reset()
			return m, nil
		case key.Matches(msg, m.keys.Toggle):
			c := m.table.Cursor()
			if c >= 0 && c < len(m.selected) {
				m.selected[c] = !m.selected[c]
				m.table.SetRows(linkTableRows(m))
			}
			return m, nil
		case key.Matches(msg, m.keys.SelectAll):
			for i := range m.selected {
				m.selected[i] = true
			}
			m.table.SetRows(linkTableRows(m))
			return m, nil
		case key.Matches(msg, m.keys.SelectNone):
			for i := range m.selected {
				m.selected[i] = false
			}
			m.table.SetRows(linkTableRows(m))
			return m, nil
		case key.Matches(msg, m.keys.Enter):
			if !anySelected(m.selected) {
				if m.addMode {
					m.parseBanner = "select at least one link to queue"
				} else {
					m.parseBanner = "select at least one link to start"
				}
				return m, nil
			}
			m.parseBanner = ""
			if m.addMode {
				// Registry/accounts were already loaded for the initial
				// batch; reuse them for the appended batch.
				return appendBatch(m)
			}
			return m, loadAccountsCmd(m.flags.accountsPath, m.flags.insecure)
		}
		var cmd tea.Cmd
		m.table, cmd = m.table.Update(msg)
		return m, cmd

	case tea.MouseMsg:
		if msg.Button == tea.MouseButtonLeft && msg.Action == tea.MouseActionPress {
			rowIdx := msg.Y - rowsOffsetY
			if rowIdx >= 0 && rowIdx < len(m.selected) {
				m.table.SetCursor(rowIdx)
				m.selected[rowIdx] = !m.selected[rowIdx]
				m.table.SetRows(linkTableRows(m))
			}
		}
		return m, nil

	case accountsLoadedMsg:
		m.accounts = msg.Accounts
		m.registry = msg.Registry
		return startBatch(m)

	case accountsErrMsg:
		m.parseBanner = msg.Err.Error()
		return m, nil
	}

	var cmd tea.Cmd
	m.table, cmd = m.table.Update(msg)
	return m, cmd
}

// viewParsed renders the link table and a stats header.
func viewParsed(m model) string {
	sel := 0
	var totalBytes int64
	for i, link := range m.links {
		if m.selected[i] {
			sel++
			if link.Size > 0 {
				totalBytes += link.Size
			}
		}
	}

	header := fmt.Sprintf("%d / %d selected · %s",
		sel, len(m.links), humanSize(totalBytes))
	headerStyled := m.theme.Subtitle.Render(header)

	banner := ""
	if m.parseBanner != "" {
		banner = "\n" + m.theme.Banner.
			Foreground(lipgloss.Color(HexErrorRed)).
			Render(m.parseBanner)
	}

	footer := m.theme.Muted.Render(
		"space toggle · a all · n none · enter download · esc back")

	return lipgloss.JoinVertical(lipgloss.Left,
		headerStyled,
		m.table.View(),
		footer,
		banner,
	)
}

// updateDownloading handles input and events while the queue runs.
func updateDownloading(m model, msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		if key.Matches(msg, m.keys.AddDLC) {
			// Drop into the picker screen in add mode. The queue keeps
			// running in its goroutine; the event channel keeps draining
			// because waitForEvent re-arms itself after every message.
			m.addMode = true
			m.addReturnTo = screenDownloading
			m.err = nil
			m.parseBanner = ""
			m.pathInput.Reset()
			m.pathInput.Focus()
			m.links = nil
			m.selected = nil
			m.screen = screenPicker
			return m, tea.Batch(textinput.Blink, m.parseSpinner.Tick)
		}
		if key.Matches(msg, m.keys.RateToggle) {
			m.rateLimiter.SetEnabled(!m.rateLimiter.Enabled())
			return m, nil
		}
		if key.Matches(msg, m.keys.ExtractToggle) {
			m.extractToggle.SetEnabled(!m.extractToggle.Enabled())
			return m, nil
		}
		if key.Matches(msg, m.keys.RateUp) {
			if !m.rateLimiter.Enabled() {
				m.rateLimiter.SetEnabled(true)
			}
			m.rateLimiter.SetBytesPerSec(m.rateLimiter.BytesPerSec() + 0.5*bytesPerMbit)
			return m, nil
		}
		if key.Matches(msg, m.keys.RateDown) {
			if !m.rateLimiter.Enabled() {
				m.rateLimiter.SetEnabled(true)
			}
			next := m.rateLimiter.BytesPerSec() - 0.5*bytesPerMbit
			if next < 0.5*bytesPerMbit {
				next = 0.5 * bytesPerMbit
			}
			m.rateLimiter.SetBytesPerSec(next)
			return m, nil
		}
		if msg.String() == "0" {
			m.rateLimiter.SetEnabled(false)
			return m, nil
		}
		if key.Matches(msg, m.keys.Quit) {
			if m.cancel != nil {
				m.cancel()
				m.cancel = nil
			}
			return m, nil
		}

	case spinner.TickMsg:
		var cmd tea.Cmd
		m.downSpinner, cmd = m.downSpinner.Update(msg)
		return m, cmd

	case progress.FrameMsg:
		pmFile, cmdF := m.progressFile.Update(msg)
		m.progressFile = pmFile.(progress.Model)
		pmBatch, cmdB := m.progressBatch.Update(msg)
		m.progressBatch = pmBatch.(progress.Model)
		return m, tea.Batch(cmdF, cmdB)
	}
	return m, nil
}

// viewDownloading renders the active job panel on top and the batches
// table below. The active panel shows the currently downloading file with
// progress / speed / ETA; the batches table tracks every queued .dlc with
// its source, first file, file count, and per-batch status.
func viewDownloading(m model) string {
	th := m.theme

	header := fmt.Sprintf("%s [%d/%d]  %s  %s",
		m.downSpinner.View(),
		m.active.index+1, m.active.total,
		th.Accent.Render(m.active.hosterName),
		m.active.filename,
	)

	partLine := ""
	if m.active.destPath != "" {
		partLine = th.Muted.Render("→ " + m.active.destPath + ".part")
	}

	pct := 0.0
	if m.active.sizeBytes > 0 {
		pct = float64(m.active.downloaded) / float64(m.active.sizeBytes)
		if pct > 1 {
			pct = 1
		}
	}
	pctLabel := th.ProgressLabel.Render(fmt.Sprintf("%5.1f%%", pct*100))

	speedText := "--"
	etaText := "--"
	if m.active.smoothedSpeed > 0 {
		speedText = humanSize(int64(m.active.smoothedSpeed)) + "/s"
		if m.active.sizeBytes > 0 && m.active.downloaded < m.active.sizeBytes {
			remaining := float64(m.active.sizeBytes-m.active.downloaded) / m.active.smoothedSpeed
			etaText = formatDuration(time.Duration(remaining * float64(time.Second)))
		}
	}

	sizeText := ""
	if m.active.sizeBytes > 0 {
		sizeText = fmt.Sprintf("%s / %s",
			humanSize(m.active.downloaded), humanSize(m.active.sizeBytes))
	} else if m.active.downloaded > 0 {
		sizeText = humanSize(m.active.downloaded)
	} else {
		sizeText = th.Muted.Render("waiting…")
	}

	stats := fmt.Sprintf("%s  •  %s  •  ETA %s",
		sizeText,
		th.Accent.Render(speedText),
		th.KeyHint.Render(etaText),
	)

	finished := m.done + m.failed + m.skipped
	batchLabel := th.ProgressLabel.Render(
		fmt.Sprintf("[%d/%d completed]", finished, m.active.total))

	activeRows := []string{header}
	if partLine != "" {
		activeRows = append(activeRows, partLine)
	}
	activeRows = append(activeRows,
		"",
		m.progressFile.View()+"  "+pctLabel,
		stats,
		renderExtract(th, m.extractToggle, m.active),
		renderRateLimit(th, m.rateLimiter),
		"",
		m.progressBatch.View()+"  "+batchLabel,
	)
	activePanel := th.Panel.Render(lipgloss.JoinVertical(lipgloss.Left, activeRows...))

	batchesHeader := th.Subtitle.Render("batches:")
	batchesTable := renderBatchesTable(&m)

	// Two lines so the hint fits inside the 80-column minimum without
	// wrapping mid-token: rate controls on top, extract + lifecycle below.
	hintLine1 := th.Muted.Render("l: limit · +/-: rate · 0: off")
	hintLine2 := th.Muted.Render("e: auto-extract · a: add dlc · q / ctrl+c: cancel")

	parts := []string{activePanel, "", batchesHeader, batchesTable, "", hintLine1, hintLine2}
	return lipgloss.JoinVertical(lipgloss.Left, parts...)
}

// renderRateLimit returns the single-line "rate limit: …" indicator shown
// inside the active panel. When the limiter is off the whole line is muted;
// when it's on, the numeric cap is rendered in the accent color so a glance
// at the panel reveals the current throttle.
func renderRateLimit(th Theme, lim *downloader.RateLimiter) string {
	if !lim.Enabled() {
		return th.Muted.Render("rate limit: off")
	}
	mbit := lim.BytesPerSec() / bytesPerMbit
	return th.Muted.Render("rate limit: ") + th.Accent.Render(fmt.Sprintf("%.1f Mbit/s", mbit))
}

// renderExtract returns the single-line "auto-extract: …" indicator shown
// in the active panel. While extraction is running for the current job the
// line switches to "extracting: <filename>  NN%" so the user sees real-time
// progress; otherwise it just reflects the toggle state.
func renderExtract(th Theme, tog *extractor.Toggle, a activeJob) string {
	if a.extracting {
		label := a.extractFile
		if label == "" {
			label = "archive"
		}
		return th.Muted.Render("extracting: ") +
			th.Accent.Render(fmt.Sprintf("%s  %d%%", label, a.extractPct))
	}
	if tog.Enabled() {
		return th.Muted.Render("auto-extract: ") + th.Accent.Render("on")
	}
	return th.Muted.Render("auto-extract: off")
}

// renderBatchesTable lays out the per-batch status table on the downloading
// screen. Columns: #, source (.dlc filename), first file, files, status.
func renderBatchesTable(m *model) string {
	th := m.theme
	if len(m.batches) == 0 {
		return th.Muted.Render("(no batches)")
	}

	const (
		idW     = 3
		filesW  = 6
		statusW = 9
	)
	avail := m.innerWidth() - idW - filesW - statusW - 8 // spacing between columns
	if avail < 20 {
		avail = 20
	}
	sourceW := avail / 2
	if sourceW < 12 {
		sourceW = 12
	}
	firstW := avail - sourceW
	if firstW < 12 {
		firstW = 12
	}

	headerCells := []string{
		padRight("#", idW),
		padRight("source", sourceW),
		padRight("first file", firstW),
		padRight("files", filesW),
		padRight("status", statusW),
	}
	headerLine := th.TableHeader.Render(strings.Join(headerCells, "  "))

	rows := []string{headerLine}
	for i := range m.batches {
		b := &m.batches[i]
		status := m.batchStatus(b)
		statusStyle := th.Muted
		switch status {
		case "active":
			statusStyle = th.Accent
		case "done":
			statusStyle = th.Success
		}
		filesText := fmt.Sprintf("%d/%d", b.finished(), b.count)
		cells := []string{
			padRight(fmt.Sprintf("%d", b.id), idW),
			padRight(truncate(b.source, sourceW), sourceW),
			padRight(truncate(b.firstFile, firstW), firstW),
			padRight(filesText, filesW),
			padRight(statusStyle.Render(status), statusW),
		}
		rows = append(rows, strings.Join(cells, "  "))
	}
	return lipgloss.JoinVertical(lipgloss.Left, rows...)
}

// padRight pads s with trailing spaces until its visible width is w. ANSI
// escapes from lipgloss styling are ignored — lipgloss.Width strips them.
func padRight(s string, w int) string {
	visible := lipgloss.Width(s)
	if visible >= w {
		return s
	}
	return s + strings.Repeat(" ", w-visible)
}

// updateSummary handles input on the summary screen.
func updateSummary(m model, msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch {
		case key.Matches(msg, m.keys.Quit):
			return m, tea.Quit
		case key.Matches(msg, m.keys.Back):
			m.screen = screenPicker
			m.links = nil
			m.selected = nil
			m.registry = nil
			m.accounts = nil
			m.parseBanner = ""
			m.pathInput.Reset()
			m.resetBatchState()
			return m, nil
		case key.Matches(msg, m.keys.Rerun):
			if len(m.failedJobs) == 0 {
				return m, nil
			}
			retry := m.failedJobs
			m.resetBatchState()
			m.jobs = retry
			return startBatch(m)
		}
	}
	return m, nil
}

// viewSummary renders the centred completion banner.
func viewSummary(m model) string {
	th := m.theme

	header := "COMPLETE"
	headerStyle := th.Success
	if m.cancelled {
		header = "CANCELLED"
		headerStyle = th.Skip
	} else if m.failed > 0 {
		headerStyle = th.Error
	}

	doneText := th.Success.Render(fmt.Sprintf("✓ %d done", m.done))
	skipText := th.Skip.Render(fmt.Sprintf("⊘ %d skipped", m.skipped))
	failText := th.Error.Render(fmt.Sprintf("✗ %d failed", m.failed))

	counters := lipgloss.JoinHorizontal(lipgloss.Top,
		doneText, "   ", skipText, "   ", failText)

	hints := th.Muted.Render("q quit · r rerun failed · esc new dlc")

	banner := th.Banner.Render(lipgloss.JoinVertical(lipgloss.Center,
		headerStyle.Render(header),
		"",
		counters,
		"",
		hints,
	))

	if m.summaryErr != nil && !errors.Is(m.summaryErr, context.Canceled) {
		banner = lipgloss.JoinVertical(lipgloss.Center,
			banner,
			"",
			th.Muted.Render(m.summaryErr.Error()),
		)
	}

	return lipgloss.Place(m.innerWidth(), 0, lipgloss.Center, lipgloss.Top, banner)
}

// startBatch builds the job slice from the current selection, spins up the
// queue goroutine via runQueue, registers it as batch 1, and transitions to
// the downloading screen. When called with m.jobs already populated (rerun
// of failed), it reuses those jobs and labels them batch 1 too.
func startBatch(m model) (tea.Model, tea.Cmd) {
	m.nextBatchID = 1
	source := batchSourceLabel(m.flags.initialDLC)
	var firstFile string

	if len(m.jobs) == 0 {
		jobs := make([]queue.Job, 0, len(m.links))
		for i, link := range m.links {
			if !m.selected[i] {
				continue
			}
			h := m.registry.Find(link.URL)
			jobs = append(jobs, queue.Job{
				Link:    link,
				Hoster:  h,
				OutDir:  m.flags.outputDir,
				BatchID: m.nextBatchID,
			})
			if firstFile == "" {
				firstFile = displayName(link)
			}
		}
		m.jobs = jobs
	} else {
		for i := range m.jobs {
			m.jobs[i].BatchID = m.nextBatchID
		}
		firstFile = displayName(m.jobs[0].Link)
		source = "rerun"
	}
	if len(m.jobs) == 0 {
		m.parseBanner = "nothing to download"
		return m, nil
	}

	m.batches = []batchInfo{{
		id:        m.nextBatchID,
		source:    source,
		firstFile: firstFile,
		count:     len(m.jobs),
		startIdx:  0,
	}}
	m.nextBatchID++

	runner, cancel, events, errs := runQueue(context.Background(), m.jobs, m.rateLimiter, m.extractToggle)
	m.runner = runner
	m.cancel = cancel
	m.events = events
	m.errs = errs
	m.active = activeJob{total: len(m.jobs), batchID: m.batches[0].id}
	m.screen = screenDownloading

	resetFile := m.progressFile.SetPercent(0)
	resetBatch := m.progressBatch.SetPercent(0)
	return m, tea.Batch(
		waitForEvent(events),
		waitForDone(errs),
		m.downSpinner.Tick,
		tickCmd(),
		resetFile,
		resetBatch,
	)
}

// appendBatch turns the current selection into queue.Jobs, appends them to
// the running Runner as a new batch, and returns to the downloading screen.
// Called when the user confirms link selection during the add-DLC flow.
func appendBatch(m model) (tea.Model, tea.Cmd) {
	jobs := make([]queue.Job, 0, len(m.links))
	var firstFile string
	for i, link := range m.links {
		if !m.selected[i] {
			continue
		}
		h := m.registry.Find(link.URL)
		jobs = append(jobs, queue.Job{
			Link:    link,
			Hoster:  h,
			OutDir:  m.flags.outputDir,
			BatchID: m.nextBatchID,
		})
		if firstFile == "" {
			firstFile = displayName(link)
		}
	}
	if len(jobs) == 0 {
		m.parseBanner = "select at least one link to queue"
		return m, nil
	}
	if m.runner == nil {
		// Defensive: should never happen because the add flow is only
		// reachable from screenDownloading.
		m.parseBanner = "no active queue to append to"
		return m, nil
	}

	startIdx := m.runner.Append(jobs...)
	m.jobs = append(m.jobs, jobs...)
	m.batches = append(m.batches, batchInfo{
		id:        m.nextBatchID,
		source:    batchSourceLabel(m.addSourcePath),
		firstFile: firstFile,
		count:     len(jobs),
		startIdx:  startIdx,
	})
	m.nextBatchID++

	m.addMode = false
	m.addSourcePath = ""
	m.parseBanner = ""
	m.links = nil
	m.selected = nil
	m.pathInput.Reset()
	m.screen = screenDownloading
	return m, nil
}

// batchSourceLabel returns a short label for a .dlc file path, used as the
// "source" column in the batches table. Empty paths fall back to a generic
// label so the table still has something to show.
func batchSourceLabel(p string) string {
	if p == "" {
		return "selection"
	}
	// Trim any directory portion so the table stays narrow.
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[i+1:]
		}
	}
	return p
}

// applyQueueEvent folds one queue event into the model. It returns any
// progress command the caller should batch into the next tea.Cmd.
func applyQueueEvent(m *model, ev queue.Event) tea.Cmd {
	switch ev.Kind {
	case queue.EventStarted:
		m.active.index = ev.Index
		m.active.total = ev.Total
		m.active.batchID = ev.BatchID
		m.active.hosterName = ev.Hoster
		m.active.filename = displayName(ev.Link)
		m.active.destPath = ""
		m.active.downloaded = 0
		m.active.sizeBytes = -1
		m.active.lastSampleAt = time.Time{}
		m.active.lastSampleDown = 0
		m.active.smoothedSpeed = 0
		m.active.extracting = false
		m.active.extractFile = ""
		m.active.extractPct = 0
		return m.progressFile.SetPercent(0)

	case queue.EventResolved:
		m.active.sizeBytes = ev.SizeBytes
		m.active.destPath = ev.DestPath
		return nil

	case queue.EventProgress:
		now := time.Now()
		if !m.active.lastSampleAt.IsZero() {
			dt := now.Sub(m.active.lastSampleAt).Seconds()
			if dt > 0 {
				inst := float64(ev.Downloaded-m.active.lastSampleDown) / dt
				if inst < 0 {
					inst = 0
				}
				if m.active.smoothedSpeed == 0 {
					m.active.smoothedSpeed = inst
				} else {
					m.active.smoothedSpeed = 0.3*inst + 0.7*m.active.smoothedSpeed
				}
			}
		}
		m.active.lastSampleAt = now
		m.active.lastSampleDown = ev.Downloaded
		m.active.downloaded = ev.Downloaded
		if ev.SizeBytes > 0 {
			m.active.sizeBytes = ev.SizeBytes
		}
		if m.active.sizeBytes > 0 {
			pct := float64(m.active.downloaded) / float64(m.active.sizeBytes)
			return m.progressFile.SetPercent(pct)
		}
		return nil

	case queue.EventDone:
		if b := m.batchByJobIndex(ev.Index); b != nil {
			b.done++
		}
		m.done++
		// Total may have grown if a new batch was appended; keep the active
		// figure in sync so the bottom progress bar reflects current totals.
		m.active.total = ev.Total
		return m.progressBatch.SetPercent(m.batchPct())

	case queue.EventFailed:
		if b := m.batchByJobIndex(ev.Index); b != nil {
			b.failed++
		}
		m.failed++
		m.failedJobs = append(m.failedJobs, queue.Job{
			Link:   ev.Link,
			Hoster: m.registry.Find(ev.Link.URL),
			OutDir: m.flags.outputDir,
		})
		m.active.total = ev.Total
		return m.progressBatch.SetPercent(m.batchPct())

	case queue.EventSkipped:
		if b := m.batchByJobIndex(ev.Index); b != nil {
			b.skipped++
		}
		m.skipped++
		m.active.total = ev.Total
		return m.progressBatch.SetPercent(m.batchPct())

	case queue.EventExtractStarted:
		m.active.extracting = true
		m.active.extractFile = ev.Filename
		m.active.extractPct = 0
		return nil

	case queue.EventExtractProgress:
		// A late progress tick can arrive after Done/Failed cleared the
		// indicator; ignore it so the panel doesn't flash an extra row.
		if !m.active.extracting {
			return nil
		}
		m.active.extractPct = ev.ExtractPercent
		return nil

	case queue.EventExtractDone:
		m.active.extracting = false
		m.active.extractFile = ""
		m.active.extractPct = 0
		return nil

	case queue.EventExtractFailed:
		// Extraction failure is non-fatal for the queue — the downloaded
		// archive stays on disk. Clear the indicator; the underlying error
		// is preserved in ev.Err (encrypted, missing 7zz, CRC, …) and could
		// surface in a future banner without changing the active-panel layout.
		m.active.extracting = false
		m.active.extractFile = ""
		m.active.extractPct = 0
		return nil

	case queue.EventExtractSkipped:
		// Emitted only at end of Run for incomplete multi-volume sets; it has
		// no bearing on the currently-active job. No download counter changes.
		return nil
	}
	return nil
}

// batchPct returns the fraction of finished (any terminal kind) jobs.
func (m *model) batchPct() float64 {
	if m.active.total == 0 {
		return 0
	}
	finished := m.done + m.failed + m.skipped
	return float64(finished) / float64(m.active.total)
}

// buildLinkTable constructs the parsed-screen table with the configured
// column widths sized to the available terminal width.
func buildLinkTable(m model, width int) table.Model {
	width = max(width, 60)
	checkW := 3
	idxW := 4
	sizeW := 10
	hosterW := 12
	pkgW := 14
	nameW := width - checkW - idxW - sizeW - hosterW - pkgW - 6
	if nameW < 10 {
		nameW = 10
	}

	cols := []table.Column{
		{Title: " ", Width: checkW},
		{Title: "#", Width: idxW},
		{Title: "filename", Width: nameW},
		{Title: "size", Width: sizeW},
		{Title: "hoster", Width: hosterW},
		{Title: "package", Width: pkgW},
	}

	t := table.New(
		table.WithColumns(cols),
		table.WithFocused(true),
		table.WithHeight(maxInt(m.height-12, 6)),
	)
	st := table.DefaultStyles()
	st.Header = m.theme.TableHeader
	st.Cell = m.theme.TableRow
	st.Selected = m.theme.TableSelected
	t.SetStyles(st)
	t.SetRows(linkTableRows(m))
	return t
}

// linkTableRows builds the table rows from the current links and selection
// mask, picking a checkbox glyph and a truncated filename per row.
func linkTableRows(m model) []table.Row {
	rows := make([]table.Row, len(m.links))
	for i, link := range m.links {
		glyph := "◻"
		if m.selected[i] {
			glyph = "◼"
		}
		name := displayName(link)
		size := ""
		if link.Size > 0 {
			size = humanSize(link.Size)
		}
		hosterName := ""
		if m.registry != nil {
			if h := m.registry.Find(link.URL); h != nil {
				hosterName = h.Name()
			}
		}
		if hosterName == "" {
			hosterName = m.theme.Muted.Render("—")
		}
		rows[i] = table.Row{
			glyph,
			fmt.Sprintf("%d", i+1),
			truncate(name, 60),
			size,
			hosterName,
			truncate(link.Package, 14),
		}
	}
	return rows
}

// anySelected reports whether the selection mask has at least one true entry.
func anySelected(sel []bool) bool {
	for _, s := range sel {
		if s {
			return true
		}
	}
	return false
}

// truncate clips s to n runes, appending an ellipsis if it was shortened.
func truncate(s string, n int) string {
	if n <= 0 || len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n < 2 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
