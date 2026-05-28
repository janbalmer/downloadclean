package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/charmbracelet/bubbles/help"
	"github.com/charmbracelet/bubbles/key"
	"github.com/charmbracelet/bubbles/progress"
	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/janbalmer/downloadclean/internal/config"
	"github.com/janbalmer/downloadclean/internal/dlc"
	"github.com/janbalmer/downloadclean/internal/downloader"
	"github.com/janbalmer/downloadclean/internal/hoster"
	"github.com/janbalmer/downloadclean/internal/queue"
)

// bytesPerMbit is the byte-per-second equivalent of one decimal megabit per
// second. Used to translate the user-facing Mbit/s cap into the bytes-per-
// second value the rate limiter expects.
const bytesPerMbit = 125_000.0

// screen identifies which page the model is showing.
type screen int

const (
	screenPicker screen = iota
	screenParsed
	screenDownloading
	screenSummary
)

// flags mirrors the CLI's flag surface. Stored on the model so screens can
// pass it back into commands without going through globals.
type flags struct {
	accountsPath string
	outputDir    string
	insecure     bool
	initialDLC   string
}

// activeJob tracks the in-flight download for the downloading screen's
// header, progress bar, and speed/ETA readout.
type activeJob struct {
	index          int
	total          int
	batchID        int
	hosterName     string
	filename       string
	destPath       string
	downloaded     int64
	sizeBytes      int64
	lastSampleAt   time.Time
	lastSampleDown int64
	smoothedSpeed  float64
}

// batchInfo is one row in the batches table on the downloading screen. The
// batches slice on the model is kept in BatchID order (id == index+1) so the
// table renders newest at the bottom.
type batchInfo struct {
	id        int    // 1-based, matches queue.Job.BatchID
	source    string // basename of the .dlc file (or "selection" for pasted paths)
	firstFile string // displayName(links[0]) — the first file in this batch
	count     int    // jobs added from this batch
	done      int
	failed    int
	skipped   int
	startIdx  int // queue index of the batch's first job (from Runner.Append)
}

// finished reports whether every job in the batch has reached a terminal kind.
func (b batchInfo) finished() int { return b.done + b.failed + b.skipped }

// keyMap is the source of truth for every key binding the TUI reacts to,
// plus their help text. Methods on it satisfy the help.KeyMap interface;
// the active screen is consulted to filter which bindings the help footer
// advertises at any given moment.
type keyMap struct {
	Quit       key.Binding
	Help       key.Binding
	Up         key.Binding
	Down       key.Binding
	Enter      key.Binding
	Toggle     key.Binding
	SelectAll  key.Binding
	SelectNone key.Binding
	Back       key.Binding
	Rerun      key.Binding
	AddDLC     key.Binding
	RateToggle key.Binding
	RateUp     key.Binding
	RateDown   key.Binding

	activeScreen screen
}

// newKeyMap returns a keyMap with every binding pre-populated.
func newKeyMap() keyMap {
	return keyMap{
		Quit:       key.NewBinding(key.WithKeys("ctrl+c", "q"), key.WithHelp("q", "quit")),
		Help:       key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Up:         key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:       key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		Enter:      key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "confirm")),
		Toggle:     key.NewBinding(key.WithKeys(" "), key.WithHelp("space", "toggle")),
		SelectAll:  key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "select all")),
		SelectNone: key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "select none")),
		Back:       key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "back")),
		Rerun:      key.NewBinding(key.WithKeys("r"), key.WithHelp("r", "rerun failed")),
		AddDLC:     key.NewBinding(key.WithKeys("a"), key.WithHelp("a", "add dlc")),
		RateToggle: key.NewBinding(key.WithKeys("l"), key.WithHelp("l", "rate limit")),
		RateUp:     key.NewBinding(key.WithKeys("+", "="), key.WithHelp("+", "+0.5 Mbit/s")),
		RateDown:   key.NewBinding(key.WithKeys("-", "_"), key.WithHelp("-", "-0.5 Mbit/s")),
	}
}

// ShortHelp implements help.KeyMap with a per-screen subset.
func (k keyMap) ShortHelp() []key.Binding {
	switch k.activeScreen {
	case screenPicker:
		return []key.Binding{k.Enter, k.Back, k.Help, k.Quit}
	case screenParsed:
		return []key.Binding{k.Up, k.Down, k.Toggle, k.SelectAll, k.SelectNone, k.Enter, k.Back, k.Help, k.Quit}
	case screenDownloading:
		return []key.Binding{k.AddDLC, k.RateToggle, k.RateUp, k.RateDown, k.Quit, k.Help}
	case screenSummary:
		return []key.Binding{k.Rerun, k.Back, k.Quit, k.Help}
	}
	return []key.Binding{k.Quit, k.Help}
}

// FullHelp implements help.KeyMap with all binding groups for the `?` view.
func (k keyMap) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{k.Up, k.Down, k.Enter, k.Back},
		{k.Toggle, k.SelectAll, k.SelectNone},
		{k.AddDLC, k.Rerun, k.Help, k.Quit},
		{k.RateToggle, k.RateUp, k.RateDown},
	}
}

// model is the root Bubble Tea model. It owns every sub-component and the
// channels into the queue goroutine for the lifetime of one download batch.
type model struct {
	theme  Theme
	flags  flags
	width  int
	height int

	screen screen
	err    error

	pathInput    textinput.Model
	parseSpinner spinner.Model
	parsing      bool

	links       []dlc.Link
	selected    []bool
	table       table.Model
	accounts    *config.Accounts
	registry    *hoster.Registry
	parseBanner string

	jobs          []queue.Job
	failedJobs    []queue.Job
	runner        *queue.Runner
	events        <-chan queue.Event
	errs          <-chan error
	cancel        context.CancelFunc
	progressFile  progress.Model
	progressBatch progress.Model
	downSpinner   spinner.Model
	active        activeJob
	batches       []batchInfo
	nextBatchID   int
	addMode       bool   // user is adding a .dlc on top of an in-flight queue
	addReturnTo   screen // screen to return to after add flow completes/cancels
	addSourcePath string // path of the .dlc being parsed in add mode
	done          int
	failed        int
	skipped       int
	cancelled     bool

	summaryErr error

	rateLimiter *downloader.RateLimiter

	keys keyMap
	help help.Model
}

// newModel constructs the initial model and pre-configures every bubble
// with theme-tinted styles.
func newModel(f flags) model {
	th := NewTheme()

	ti := textinput.New()
	ti.Placeholder = "drop a .dlc file here or type a path…"
	ti.Prompt = th.Accent.Render("▶ ") + " "
	ti.CharLimit = 4096
	ti.ShowSuggestions = false
	ti.Width = 60
	ti.Focus()
	ti.PromptStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(HexHotPink))
	ti.TextStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(HexNeonCyan))
	ti.PlaceholderStyle = lipgloss.NewStyle().Foreground(lipgloss.Color(HexMuted)).Italic(true)
	ti.Cursor.Style = lipgloss.NewStyle().Foreground(lipgloss.Color(HexNeonMag))

	ps := spinner.New()
	ps.Spinner = spinner.MiniDot
	ps.Style = lipgloss.NewStyle().Foreground(lipgloss.Color(HexNeonMag))

	ds := spinner.New()
	ds.Spinner = spinner.Dot
	ds.Style = lipgloss.NewStyle().Foreground(lipgloss.Color(HexHotPink))

	pf := progress.New(
		progress.WithGradient(HexNeonMag, HexNeonCyan),
		progress.WithoutPercentage(),
		progress.WithWidth(50),
	)

	pb := progress.New(
		progress.WithGradient(HexNeonMag, HexNeonCyan),
		progress.WithoutPercentage(),
		progress.WithWidth(50),
	)

	rl := downloader.NewRateLimiter()
	rl.SetBytesPerSec(10.0 * bytesPerMbit)

	h := help.New()
	h.Styles.ShortKey = th.KeyHint
	h.Styles.ShortDesc = th.Muted
	h.Styles.ShortSeparator = th.Muted
	h.Styles.FullKey = th.KeyHint
	h.Styles.FullDesc = th.Muted
	h.Styles.FullSeparator = th.Muted
	h.Styles.Ellipsis = th.Muted

	return model{
		theme:         th,
		flags:         f,
		screen:        screenPicker,
		pathInput:     ti,
		parseSpinner:  ps,
		downSpinner:   ds,
		progressFile:  pf,
		progressBatch: pb,
		rateLimiter:   rl,
		keys:          newKeyMap(),
		help:          h,
	}
}

// Init seeds the cursor blink, the picker spinner, and (if a positional
// path was supplied) an immediate parse.
func (m model) Init() tea.Cmd {
	cmds := []tea.Cmd{textinput.Blink, m.parseSpinner.Tick}
	if m.flags.initialDLC != "" {
		path := normalizeDroppedPath(m.flags.initialDLC)
		if path != "" {
			cmds = append(cmds, parseDLCCmd(path))
		}
	}
	return tea.Batch(cmds...)
}

// Update is the root reducer. Cross-screen messages (window size, help
// toggle, quit on summary) are handled here; everything else is dispatched
// to the per-screen update function.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.help.Width = msg.Width
		inputWidth := msg.Width - 20
		if inputWidth < 20 {
			inputWidth = 20
		}
		m.pathInput.Width = inputWidth
		barWidth := msg.Width - 20
		if barWidth < 20 {
			barWidth = 20
		}
		m.progressFile.Width = barWidth
		m.progressBatch.Width = barWidth
		if m.screen == screenParsed {
			m.table.SetWidth(msg.Width - 8)
			m.table.SetHeight(maxInt(msg.Height-12, 6))
		}
	case tea.KeyMsg:
		if m.screen != screenPicker && key.Matches(msg, m.keys.Help) {
			m.help.ShowAll = !m.help.ShowAll
			return m, nil
		}

	// queueEventMsg / queueDoneMsg can arrive while the user is in the
	// add-DLC sub-flow (picker or parsed selection table). Always apply
	// them and keep the event channel draining, regardless of screen.
	case queueEventMsg:
		cmds := []tea.Cmd{waitForEvent(m.events)}
		if cmd := applyQueueEvent(&m, msg.Ev); cmd != nil {
			cmds = append(cmds, cmd)
		}
		return m, tea.Batch(cmds...)
	case queueDoneMsg:
		if errors.Is(msg.Err, context.Canceled) {
			m.cancelled = true
		}
		m.summaryErr = msg.Err
		// Tear down the add-DLC flow if it was still open; either way, the
		// download session is over.
		m.addMode = false
		m.addSourcePath = ""
		m.links = nil
		m.selected = nil
		m.pathInput.Reset()
		m.parseBanner = ""
		m.screen = screenSummary
		m.cancel = nil
		return m, nil

	// Keep the 500ms refresh tick alive across screen transitions so
	// speed/ETA stay current if the user briefly visits the add picker.
	case tickMsg:
		if m.runner != nil && m.cancel != nil {
			return m, tickCmd()
		}
		return m, nil
	}

	switch m.screen {
	case screenPicker:
		return updatePicker(m, msg)
	case screenParsed:
		return updateParsed(m, msg)
	case screenDownloading:
		return updateDownloading(m, msg)
	case screenSummary:
		return updateSummary(m, msg)
	}
	return m, nil
}

// View renders the framed cyberpunk shell around the active screen's body.
func (m model) View() string {
	if m.width > 0 && (m.width < 80 || m.height < 20) {
		msg := m.theme.Banner.
			Foreground(lipgloss.Color(HexErrorRed)).
			Render("terminal too small — please resize to at least 80×20")
		return lipgloss.Place(maxInt(m.width, 1), maxInt(m.height, 1),
			lipgloss.Center, lipgloss.Center, msg)
	}

	m.keys.activeScreen = m.screen

	var body string
	switch m.screen {
	case screenPicker:
		body = viewPicker(m)
	case screenParsed:
		body = viewParsed(m)
	case screenDownloading:
		body = viewDownloading(m)
	case screenSummary:
		body = viewSummary(m)
	}

	helpView := m.help.View(m.keys)

	frame := lipgloss.JoinVertical(lipgloss.Left,
		m.theme.Title.Render("▓▒░ DOWNLOADCLEAN ░▒▓"),
		m.theme.Subtitle.Render("sequential dlc grabber · neon edition"),
		"",
		body,
		"",
		helpView,
	)
	return m.theme.Frame.Render(frame)
}

// innerWidth returns the renderable column count inside the outer frame's
// border and padding.
func (m model) innerWidth() int {
	if m.width <= 0 {
		return 80
	}
	w := m.width - 6
	if w < 20 {
		return 20
	}
	return w
}

// resetBatchState clears per-batch counters and the batches table so the
// downloading screen starts fresh on a rerun.
func (m *model) resetBatchState() {
	m.batches = nil
	m.nextBatchID = 0
	m.runner = nil
	m.done = 0
	m.failed = 0
	m.skipped = 0
	m.cancelled = false
	m.failedJobs = nil
	m.active = activeJob{}
	m.err = nil
	m.summaryErr = nil
	m.addMode = false
}

// humanSize formats a byte count in 1024-base units, mirroring the CLI.
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

// formatDuration renders a duration as H:MM:SS, dropping the hour
// component when it would be zero.
func formatDuration(d time.Duration) string {
	if d < 0 {
		return "--"
	}
	total := int(d.Round(time.Second).Seconds())
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%d:%02d", m, s)
}

// maxInt is a small helper so we can stay clear of importing math just for
// integer maxes that the language builtin already covers.
func maxInt(a, b int) int { return max(a, b) }
