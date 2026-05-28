// Command downloadclean-tui is a Bubble Tea front-end for the DownloadClean
// sequential link grabber. It wraps the same internal packages as the CLI
// behind a cyberpunk-themed terminal UI with file picking, link selection,
// live progress, and a summary screen.
package main

import (
	"flag"
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/janbalmer/downloadclean/internal/config"
)

func main() {
	var (
		accountsPath = flag.String("accounts", config.DefaultPath(), "path to accounts.json")
		configPath   = flag.String("config", config.DefaultConfigPath(), "path to config.json (archive passwords, etc.)")
		outDir       = flag.String("output", config.DefaultOutputDir(), "directory to download into")
		insecure     = flag.Bool("insecure-config", false, "skip permission checks on accounts.json and config.json")
	)
	flag.Parse()

	var initialDLC string
	if args := flag.Args(); len(args) > 0 {
		initialDLC = args[0]
	}

	// A missing config file is not an error: LoadConfig returns a zero-valued
	// *Config. A malformed or insecure file is fatal so the user notices.
	cfg, err := config.LoadConfig(*configPath, *insecure)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	m := newModel(flags{
		accountsPath: *accountsPath,
		configPath:   *configPath,
		outputDir:    *outDir,
		insecure:     *insecure,
		initialDLC:   initialDLC,
	}, cfg)

	p := tea.NewProgram(m, tea.WithAltScreen(), tea.WithMouseCellMotion())
	finalModel, err := p.Run()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if fm, ok := finalModel.(model); ok && fm.failed > 0 {
		os.Exit(1)
	}
}
