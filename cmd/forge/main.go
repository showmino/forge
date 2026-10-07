// Command forge is the Forge platform CLI.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"

	"github.com/showmino/forge/internal/cli"
)

func main() {
	os.Exit(run())
}

func run() int {
	// Ctrl-C cancels in-flight requests cleanly; a second Ctrl-C kills the process.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	deps := cli.DefaultDeps(os.Stdin, os.Stdout, os.Stderr,
		term.IsTerminal(int(os.Stdin.Fd())), term.IsTerminal(int(os.Stdout.Fd())))
	return cli.Run(ctx, deps, os.Args[1:])
}
