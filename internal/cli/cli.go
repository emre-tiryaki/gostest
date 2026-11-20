// Package cli implements the gostest command line interface.
package cli

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/emre-tiryaki/gostest/internal/backend"
	"github.com/emre-tiryaki/gostest/internal/distro"
	"github.com/emre-tiryaki/gostest/internal/engine"
	"github.com/emre-tiryaki/gostest/internal/report"
)

// Exit codes.
const (
	ExitOK      = 0
	ExitFailed  = 1
	ExitUsage   = 2
	ExitRuntime = 3
)

// Containers is a container engine usable by gostest.
type Containers interface {
	engine.Containers
	Available(ctx context.Context) error
	Name() string
}

// App is the command line application with its dependencies.
type App struct {
	Stdout, Stderr io.Writer
	Host           backend.Runner
	HostShell      string
	NewContainers  func(engine string) (Containers, error)
	Version        string
}

// Run executes the command line args and returns the exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("gostest", flag.ContinueOnError)
	fs.SetOutput(a.Stderr)
	distros := fs.String("distros", "", "comma separated distributions")
	if err := fs.Parse(args); err != nil {
		return ExitUsage
	}
	if fs.NArg() == 0 {
		fmt.Fprintln(a.Stderr, "usage: gostest [flags] command")
		return ExitUsage
	}
	reg, _ := distro.NewRegistry()
	ds := reg.Defaults()
	if *distros != "" {
		var err error
		if ds, err = reg.ParseList(*distros); err != nil {
			fmt.Fprintln(a.Stderr, err)
			return ExitUsage
		}
	}
	c, err := a.NewContainers("docker")
	if err != nil {
		fmt.Fprintln(a.Stderr, err)
		return ExitRuntime
	}
	e := &engine.Engine{Host: a.Host, HostShell: a.HostShell, Containers: c}
	rep, err := e.Run(ctx, engine.Options{Command: strings.Join(fs.Args(), " ")}, ds)
	if err != nil {
		fmt.Fprintln(a.Stderr, err)
		return ExitRuntime
	}
	_ = report.Text(a.Stdout, rep, report.TextOptions{Context: report.DefaultContext})
	if !rep.OK() {
		return ExitFailed
	}
	return ExitOK
}
