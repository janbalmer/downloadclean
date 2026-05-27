package main

import "github.com/charmbracelet/lipgloss"

const (
	HexBgDeep    = "#0a0014"
	HexBgPanel   = "#140030"
	HexNeonMag   = "#ff2bd6"
	HexNeonCyan  = "#00f0ff"
	HexHotPink   = "#ff4f8b"
	HexViolet    = "#9d4dff"
	HexElectric  = "#f9ff00"
	HexAcidGreen = "#39ff14"
	HexErrorRed  = "#ff2e63"
	HexMuted     = "#6e5a90"
)

type Theme struct {
	Title         lipgloss.Style
	Subtitle      lipgloss.Style
	Accent        lipgloss.Style
	KeyHint       lipgloss.Style
	Muted         lipgloss.Style
	Success       lipgloss.Style
	Error         lipgloss.Style
	Skip          lipgloss.Style
	ProgressLabel lipgloss.Style

	Panel  lipgloss.Style
	Input  lipgloss.Style
	Banner lipgloss.Style
	Frame  lipgloss.Style

	TableHeader   lipgloss.Style
	TableRow      lipgloss.Style
	TableRowMuted lipgloss.Style
	TableSelected lipgloss.Style
}

func NewTheme() Theme {
	return Theme{
		Title:         lipgloss.NewStyle().Foreground(lipgloss.Color(HexNeonMag)).Bold(true).Padding(0, 1),
		Subtitle:      lipgloss.NewStyle().Foreground(lipgloss.Color(HexNeonCyan)).Faint(true),
		Accent:        lipgloss.NewStyle().Foreground(lipgloss.Color(HexHotPink)).Bold(true),
		KeyHint:       lipgloss.NewStyle().Foreground(lipgloss.Color(HexElectric)),
		Muted:         lipgloss.NewStyle().Foreground(lipgloss.Color(HexMuted)),
		Success:       lipgloss.NewStyle().Foreground(lipgloss.Color(HexAcidGreen)).Bold(true),
		Error:         lipgloss.NewStyle().Foreground(lipgloss.Color(HexErrorRed)).Bold(true),
		Skip:          lipgloss.NewStyle().Foreground(lipgloss.Color(HexHotPink)),
		ProgressLabel: lipgloss.NewStyle().Foreground(lipgloss.Color(HexElectric)).Bold(true),

		Panel: lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).
			BorderForeground(lipgloss.Color(HexNeonCyan)).
			Padding(1, 2),
		Input: lipgloss.NewStyle().
			Border(lipgloss.NormalBorder()).
			BorderForeground(lipgloss.Color(HexNeonMag)).
			Padding(0, 1),
		Banner: lipgloss.NewStyle().
			Border(lipgloss.ThickBorder()).
			BorderForeground(lipgloss.Color(HexHotPink)).
			Padding(0, 2),
		Frame: lipgloss.NewStyle().
			Border(lipgloss.DoubleBorder()).
			BorderForeground(lipgloss.Color(HexNeonMag)).
			Padding(1, 2),

		TableHeader: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#000000")).
			Background(lipgloss.Color(HexNeonMag)).
			Bold(true).
			Padding(0, 1),
		TableRow:      lipgloss.NewStyle().Padding(0, 1),
		TableRowMuted: lipgloss.NewStyle().Foreground(lipgloss.Color(HexMuted)).Padding(0, 1),
		TableSelected: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#000000")).
			Background(lipgloss.Color(HexHotPink)).
			Padding(0, 1),
	}
}
