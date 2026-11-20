// Command gostest runs a shell command on the host and inside several Linux
// distributions in parallel, then reports where the behaviour differs and why.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/emre-tiryaki/gostest/internal/backend"
	"github.com/emre-tiryaki/gostest/internal/cli"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app := &cli.App{
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
		Host:      backend.Local{},
		HostShell: backend.HostShell(),
		NewContainers: func(engine string) (cli.Containers, error) {
			return backend.NewDocker(engine), nil
		},
		Version: version,
	}
	os.Exit(app.Run(ctx, os.Args[1:]))
}
