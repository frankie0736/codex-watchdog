package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/frankie0736/codex-watchdog/internal/attach"
	"github.com/frankie0736/codex-watchdog/internal/config"
	"github.com/frankie0736/codex-watchdog/internal/home"
	"github.com/frankie0736/codex-watchdog/internal/queue"
	"github.com/frankie0736/codex-watchdog/internal/state"
	"github.com/frankie0736/codex-watchdog/internal/tui"
	"github.com/frankie0736/codex-watchdog/internal/watcher"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "codex-watchdog:", err)
		os.Exit(1)
	}
}

func run() error {
	cfgPath := flag.String("config", "", "config file (default ~/.config/codex-watchdog/config.toml)")
	once := flag.Bool("once", false, "poll once, print JSON snapshot to stdout, never send")
	live := flag.Bool("live", false, "force real sending (overrides dry_run)")
	dryRun := flag.Bool("dry-run", false, "force dry-run: record decisions, never run codex queue")
	onlyThread := flag.String("thread", "", "only act on this thread id (others are shown but never sent to)")
	printCfg := flag.Bool("print-config", false, "print effective config and discovered homes, then exit")
	flag.Parse()

	path, explicit := config.DefaultPath(), false
	if *cfgPath != "" {
		path, explicit = config.ExpandHome(*cfgPath), true
	}
	cfg, err := config.Load(path, explicit)
	if err != nil {
		return err
	}
	if *live && *dryRun {
		return fmt.Errorf("--live and --dry-run are mutually exclusive")
	}
	if *live {
		cfg.DryRun = false
	}
	if *dryRun {
		cfg.DryRun = true
	}
	cfg.StateDir = config.ExpandHome(cfg.StateDir)

	homes, homeErrs := home.Resolve(cfg)
	if *printCfg {
		return printJSON(map[string]any{"config_path": path, "config": cfg, "homes": homes, "home_errors": errStrings(homeErrs)})
	}
	if len(homes) == 0 {
		return fmt.Errorf("no Codex home found (errors: %v)", homeErrs)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return fmt.Errorf("state dir: %w", err)
	}
	logFile, err := os.OpenFile(filepath.Join(cfg.StateDir, "watchdog.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open log: %w", err)
	}
	defer logFile.Close()
	logger := slog.New(slog.NewJSONHandler(logFile, nil))

	st, interrupted, err := state.Open(ctx, cfg.StateDir)
	if err != nil {
		return err
	}
	defer st.Close()

	w := watcher.New(watcher.Options{
		Config:     cfg,
		Logger:     logger,
		State:      st,
		Sender:     queue.CLI{Bin: cfg.CodexBin, Timeout: cfg.QueueTimeout()},
		Homes:      homes,
		HomeErrs:   homeErrs,
		ReadOnly:   *once,
		OnlyThread: *onlyThread,
		Loaded:     attach.LoadedRollouts,
	})
	defer w.Close()

	homePaths := make([]string, len(homes))
	for i, h := range homes {
		homePaths[i] = h.Path
	}
	logger.Info("startup", "run_id", w.RunID, "once", *once, "dry_run", cfg.DryRun, "homes", homePaths,
		"interrupted_attempts_recovered", interrupted, "only_thread", *onlyThread, "config_path", path)
	defer logger.Info("shutdown", "run_id", w.RunID)

	if *once {
		return printJSON(w.Poll(ctx))
	}

	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	p := tea.NewProgram(tui.New(w), tea.WithAltScreen(), tea.WithContext(ctx))
	_, err = p.Run()
	signalled := ctx.Err() != nil
	stop() // stop polling before w.Close releases database handles
	<-done
	if err != nil && !signalled {
		return err
	}
	return nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func errStrings(errs []error) []string {
	out := make([]string, len(errs))
	for i, e := range errs {
		out[i] = e.Error()
	}
	return out
}
