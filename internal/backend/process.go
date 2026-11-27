package backend

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"syscall"
	"time"
)

// limitedBuffer keeps the first limit bytes written to it and silently
// discards the rest, so a chatty command cannot exhaust memory.
type limitedBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buf.Len()
	if len(p) > remaining {
		b.buf.Write(p[:max(remaining, 0)])
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func withTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d > 0 {
		return context.WithTimeout(ctx, d)
	}
	return context.WithCancel(ctx)
}

// runProcess runs name with args in its own process group. When the
// timeout expires the whole group is killed and the result is marked
// TimedOut; when ctx itself ends, ctx.Err() is returned.
func runProcess(ctx context.Context, timeout time.Duration, dir string, stdin io.Reader, name string, args ...string) (Result, error) {
	return runCapped(ctx, MaxOutput, timeout, dir, stdin, name, args...)
}

// runCapped is runProcess capturing at most limit bytes per stream.
func runCapped(ctx context.Context, limit int, timeout time.Duration, dir string, stdin io.Reader, name string, args ...string) (Result, error) {
	runCtx, cancel := withTimeout(ctx, timeout)
	defer cancel()

	stdout := &limitedBuffer{limit: limit}
	stderr := &limitedBuffer{limit: limit}
	cmd := exec.CommandContext(runCtx, name, args...)
	cmd.Dir = dir
	cmd.Stdin = stdin
	cmd.Stdout, cmd.Stderr = stdout, stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	// Stop waiting for output pipes shortly after the kill, even if some
	// process outside the group still holds them open.
	cmd.WaitDelay = 2 * time.Second

	start := time.Now()
	err := cmd.Run()
	res := Result{
		Stdout:    stdout.buf.String(),
		Stderr:    stderr.buf.String(),
		Truncated: stdout.truncated || stderr.truncated,
		Duration:  time.Since(start),
	}
	switch {
	case ctx.Err() != nil:
		return res, ctx.Err()
	case runCtx.Err() != nil:
		res.TimedOut = true
		res.ExitCode = ExitTimeout
		return res, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		res.ExitCode = exitCode(exitErr)
		return res, nil
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		// The command succeeded, but a process it left in the background
		// still held the output pipes after WaitDelay.
		return res, nil
	}
	return res, err
}

// exitCode reports 128+signal for signalled processes, like a shell does.
func exitCode(err *exec.ExitError) int {
	if ws, ok := err.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return err.ExitCode()
}
