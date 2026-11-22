// Command taskmoor shows and drives the tasks a repository keeps as files
// (tasks/*.md + tasks/tasks.toml): a TUI board, and a CLI for agents and
// launchers.
package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/proshik/taskmoor/internal/cli"
	"github.com/proshik/taskmoor/internal/tui"
)

// version is set at build time with -ldflags "-X main.version=…".
var version = ""

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM)
	defer stop()
	dir, err := os.Getwd()
	if err != nil {
		fmt.Fprintf(os.Stderr, "taskmoor: %v\n", err)
		os.Exit(cli.ExitUsage)
	}
	os.Exit(cli.Run(ctx, os.Args[1:], cli.Env{
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Getenv:  os.Getenv,
		Dir:     dir,
		Now:     time.Now,
		Version: buildVersion(),
		TUI:     tui.Run,
		OpenURL: func(u string) error { return exec.Command("open", u).Run() },
	}))
}

func buildVersion() string {
	if version != "" {
		return version
	}
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}
