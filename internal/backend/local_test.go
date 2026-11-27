package backend

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func runLocal(t *testing.T, spec Spec) Result {
	t.Helper()
	res, err := Local{}.Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run(%q): %v", spec.Command, err)
	}
	return res
}

func TestLocalCapturesStdoutStderrAndExitCode(t *testing.T) {
	res := runLocal(t, Spec{Command: "echo out; echo err >&2; exit 3"})
	if res.Stdout != "out\n" {
		t.Errorf("stdout = %q, want %q", res.Stdout, "out\n")
	}
	if res.Stderr != "err\n" {
		t.Errorf("stderr = %q, want %q", res.Stderr, "err\n")
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	if res.TimedOut || res.Truncated {
		t.Errorf("unexpected flags: %+v", res)
	}
}

func TestLocalSuccess(t *testing.T) {
	res := runLocal(t, Spec{Command: "true"})
	if res.ExitCode != 0 || res.Stdout != "" || res.Stderr != "" {
		t.Fatalf("unexpected result %+v", res)
	}
}

func TestLocalRecordsDuration(t *testing.T) {
	res := runLocal(t, Spec{Command: "sleep 0.05"})
	if res.Duration < 50*time.Millisecond {
		t.Fatalf("duration = %v, want >= 50ms", res.Duration)
	}
}

func TestLocalUsesDir(t *testing.T) {
	dir := t.TempDir()
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	res := runLocal(t, Spec{Command: "pwd -P", Dir: dir})
	if strings.TrimSpace(res.Stdout) != want {
		t.Fatalf("pwd = %q, want %q", res.Stdout, want)
	}
}

func TestLocalDefaultsToCurrentDir(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(wd)
	res := runLocal(t, Spec{Command: "pwd -P"})
	if strings.TrimSpace(res.Stdout) != want {
		t.Fatalf("pwd = %q, want %q", res.Stdout, want)
	}
}

func TestLocalStdinIsEmpty(t *testing.T) {
	res := runLocal(t, Spec{Command: "cat", Timeout: 5 * time.Second})
	if res.TimedOut || res.Stdout != "" || res.ExitCode != 0 {
		t.Fatalf("cat should see an empty stdin, got %+v", res)
	}
}

func TestLocalCommandNotFound(t *testing.T) {
	res := runLocal(t, Spec{Command: "gostest_no_such_command_xyz"})
	if res.ExitCode != 127 {
		t.Errorf("exit code = %d, want 127", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "not found") {
		t.Errorf("stderr = %q, want a 'not found' message", res.Stderr)
	}
}

func TestLocalKilledBySignal(t *testing.T) {
	res := runLocal(t, Spec{Command: "kill -9 $$"})
	if res.ExitCode != 137 {
		t.Fatalf("exit code = %d, want 137 (128+SIGKILL)", res.ExitCode)
	}
}

func TestLocalTimeout(t *testing.T) {
	start := time.Now()
	res := runLocal(t, Spec{Command: "echo before; sleep 10; echo after", Timeout: 200 * time.Millisecond})
	if !res.TimedOut {
		t.Fatal("expected TimedOut")
	}
	if res.ExitCode != ExitTimeout {
		t.Errorf("exit code = %d, want %d", res.ExitCode, ExitTimeout)
	}
	if res.Stdout != "before\n" {
		t.Errorf("stdout = %q, want partial output %q", res.Stdout, "before\n")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("timeout took %v", elapsed)
	}
}

func TestLocalTimeoutKillsChildProcesses(t *testing.T) {
	// The background sleep inherits stdout. Unless the whole process group
	// is killed it keeps the pipe open and Run blocks until it exits.
	start := time.Now()
	res := runLocal(t, Spec{Command: "sleep 10 & echo started; wait", Timeout: 200 * time.Millisecond})
	if !res.TimedOut {
		t.Fatal("expected TimedOut")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("child processes survived the timeout: took %v", elapsed)
	}
}

func TestLocalParentCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := Local{}.Run(ctx, Spec{Command: "sleep 10"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cancellation took %v", elapsed)
	}
}

func TestLocalTruncatesHugeOutput(t *testing.T) {
	// 2 MiB of output on each stream.
	cmd := "head -c 2097152 /dev/zero | tr '\\0' a; head -c 2097152 /dev/zero | tr '\\0' b >&2"
	res := runLocal(t, Spec{Command: cmd})
	if !res.Truncated {
		t.Fatal("expected Truncated")
	}
	if len(res.Stdout) != MaxOutput || len(res.Stderr) != MaxOutput {
		t.Fatalf("captured %d/%d bytes, want %d each", len(res.Stdout), len(res.Stderr), MaxOutput)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code = %d; truncation must not break the command", res.ExitCode)
	}
}

func TestLocalInheritsEnvironment(t *testing.T) {
	t.Setenv("GOSTEST_TEST_VAR", "hello")
	res := runLocal(t, Spec{Command: "printf %s \"$GOSTEST_TEST_VAR\""})
	if res.Stdout != "hello" {
		t.Fatalf("stdout = %q, want %q", res.Stdout, "hello")
	}
}

func TestLocalBackgroundProcessHoldingOutput(t *testing.T) {
	// The shell exits at once; the background process keeps the pipes open.
	res, err := Local{}.Run(context.Background(), Spec{Command: "sleep 4 & echo hi"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.ExitCode != 0 || res.TimedOut || res.Stdout != "hi\n" {
		t.Fatalf("result = %+v, want exit 0 and stdout %q", res, "hi\n")
	}
}
