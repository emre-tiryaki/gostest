// Command gostest runs a shell command on the host and inside several Linux
// distributions in parallel, then reports where the behaviour differs and why.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/emre-tiryaki/gostest/internal/cli"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	code := cli.New(version).Run(ctx, os.Args[1:])
	stop()
	os.Exit(code)
}
