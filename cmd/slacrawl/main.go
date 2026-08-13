package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/openclaw/slacrawl/internal/cli"
)

func main() {
	// Ctrl-C and SIGTERM cancel the context so long-running commands (sync,
	// tail, watch) unwind through their normal cleanup paths instead of being
	// hard-killed; a second signal falls through to the default handler.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app := cli.New()
	if err := app.Run(ctx, os.Args[1:]); err != nil {
		stop()
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
