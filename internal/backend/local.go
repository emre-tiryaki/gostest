package backend

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"time"
)

// Local runs commands on the host. Its results are the reference.
type Local struct{}

// Run executes spec.Command with /bin/sh -c in spec.Dir.
func (Local) Run(ctx context.Context, spec Spec) (Result, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", spec.Command)
	cmd.Dir = spec.Dir
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	res := Result{Stdout: stdout.String(), Stderr: stderr.String(), Duration: time.Since(start)}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	return res, err
}
