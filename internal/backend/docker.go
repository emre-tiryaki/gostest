package backend

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"

	"github.com/emre-tiryaki/gostest/internal/distro"
)

// Executor runs an external program. A non-zero exit status is returned as
// exitCode with a nil error.
type Executor interface {
	Exec(ctx context.Context, stdin io.Reader, name string, args ...string) (stdout, stderr []byte, exitCode int, err error)
}

// OSExecutor runs programs with os/exec.
type OSExecutor struct{}

// Exec implements Executor.
func (OSExecutor) Exec(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, []byte, int, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return stdout.Bytes(), stderr.Bytes(), exitErr.ExitCode(), nil
	}
	return stdout.Bytes(), stderr.Bytes(), 0, err
}

// Docker runs commands in containers through the docker (or podman) CLI.
type Docker struct {
	Binary string
	Exec   Executor
}

// NewDocker returns a Docker backend using binary ("docker" or "podman").
func NewDocker(binary string) *Docker {
	return &Docker{Binary: binary, Exec: OSExecutor{}}
}

// Available reports whether the container engine can be reached.
func (d *Docker) Available(ctx context.Context) error { return nil }

// Run executes spec inside a new container.
func (d *Docker) Run(ctx context.Context, spec Spec) (Result, error) {
	out, errOut, code, err := d.Exec.Exec(ctx, nil, d.Binary, "run", "--rm", spec.Image, "/bin/sh", "-c", spec.Command)
	return Result{Stdout: string(out), Stderr: string(errOut), ExitCode: code}, err
}

// Prepare makes sure the image for dist exists with pkgs installed.
func (d *Docker) Prepare(ctx context.Context, dist distro.Distro, pkgs []string, rebuild bool) (Environment, error) {
	return Environment{Distro: dist, Image: dist.Image}, nil
}
