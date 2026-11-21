package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// e2eApp is an App wired to the real system, as main.go builds it, with
// the output captured, the cache in a temporary directory and the working
// directory set to dir.
type e2eApp struct {
	app            *App
	stdout, stderr bytes.Buffer
}

// newE2E returns a real App and a directory on PATH to put host tools in.
// It skips with -short and when Docker cannot be reached.
func newE2E(t *testing.T) (*e2eApp, string) {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	a := New("e2e")
	if _, err := a.containers(context.Background(), "docker"); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	bin := t.TempDir()
	// The host reference and the resolver both find tools through PATH.
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	work, cache := t.TempDir(), t.TempDir()

	e := &e2eApp{app: a}
	a.Stdout, a.Stderr = &e.stdout, &e.stderr
	a.StdoutTTY, a.StderrTTY = false, false
	a.Getwd = func() (string, error) { return work, nil }
	a.CacheDir = func() (string, error) { return cache, nil }
	return e, bin
}

// writeTool creates an executable host tool in bin.
func writeTool(t *testing.T, bin, name, content string) string {
	t.Helper()
	p := filepath.Join(bin, name)
	if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// run runs gostest with args and returns the exit code and the output.
func (e *e2eApp) run(t *testing.T, args ...string) (int, string) {
	t.Helper()
	e.stdout.Reset()
	e.stderr.Reset()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	code := e.app.Run(ctx, append([]string{"--color", "never", "-d", "alpine,debian"}, args...))
	return code, e.stdout.String() + e.stderr.String()
}

func expectCode(t *testing.T, got, want int, out string) {
	t.Helper()
	if got != want {
		t.Fatalf("exit code = %d, want %d\n%s", got, want, out)
	}
}

const probeScript = "#!/bin/sh\necho \"hello from gostest-probe on $(uname -s)\"\n"

func TestIntegrationHostToolIsMounted(t *testing.T) {
	e, bin := newE2E(t)
	writeTool(t, bin, "gostest-probe", probeScript)

	code, out := e.run(t, "gostest-probe")
	expectCode(t, code, ExitOK, out)
	if n := strings.Count(out, "note: mounted from the host because they were missing here: `gostest-probe`"); n != 2 {
		t.Errorf("want a mounted note for alpine and debian, got %d:\n%s", n, out)
	}
	if strings.Contains(out, "installed automatically") {
		t.Errorf("nothing should have been installed:\n%s", out)
	}
}

func TestIntegrationHostToolNotMountedWithoutAutoInstall(t *testing.T) {
	e, bin := newE2E(t)
	writeTool(t, bin, "gostest-probe", probeScript)

	code, out := e.run(t, "--no-auto-install", "gostest-probe")
	expectCode(t, code, ExitFailed, out)
	if strings.Contains(out, "mounted from the host") {
		t.Errorf("--no-auto-install must not mount host tools:\n%s", out)
	}
	if !strings.Contains(out, "`gostest-probe` is not installed here") {
		t.Errorf("want a missing-command reason:\n%s", out)
	}
}

func TestIntegrationEditingHostToolInvalidatesCache(t *testing.T) {
	e, bin := newE2E(t)
	tool := writeTool(t, bin, "gostest-probe", probeScript)

	code, out := e.run(t, "gostest-probe")
	expectCode(t, code, ExitOK, out)
	if strings.Contains(out, "(cached)") {
		t.Fatalf("first run cannot be cached:\n%s", out)
	}

	code, out = e.run(t, "gostest-probe")
	expectCode(t, code, ExitOK, out)
	if strings.Count(out, "(cached)") != 2 {
		t.Fatalf("an unchanged tool should be served from the cache:\n%s", out)
	}

	if err := os.WriteFile(tool, []byte(strings.Replace(probeScript, "hello", "edited", 1)), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out = e.run(t, "gostest-probe")
	expectCode(t, code, ExitOK, out)
	if strings.Contains(out, "(cached)") {
		t.Errorf("an edited tool must run again:\n%s", out)
	}
}

func TestIntegrationHostToolDifferencesAreReported(t *testing.T) {
	e, bin := newE2E(t)
	// The same script, but the shells it runs under differ.
	writeTool(t, bin, "gostest-shell", "#!/bin/sh\nreadlink -f /bin/sh\n")

	code, out := e.run(t, "gostest-shell")
	expectCode(t, code, ExitFailed, out)
	if !strings.Contains(out, "mounted from the host") {
		t.Errorf("want the tool mounted:\n%s", out)
	}
	for _, want := range []string{"+ /bin/busybox", "+ /usr/bin/dash"} {
		if !strings.Contains(out, want) {
			t.Errorf("diff should show %q:\n%s", want, out)
		}
	}
}

func TestIntegrationHostToolInterpreterIsInstalled(t *testing.T) {
	e, bin := newE2E(t)
	writeTool(t, bin, "gostest-bash-probe", "#!/bin/bash\n[[ -n $BASH_VERSION ]] && echo bash works\n")

	code, out := e.run(t, "-d", "alpine", "gostest-bash-probe")
	expectCode(t, code, ExitOK, out)
	for _, want := range []string{"mounted from the host because they were missing here: `gostest-bash-probe`", "installed automatically because they were missing here: `bash`"} {
		if !strings.Contains(out, want) {
			t.Errorf("want note %q:\n%s", want, out)
		}
	}
}

func TestIntegrationHostToolInJSON(t *testing.T) {
	e, bin := newE2E(t)
	tool := writeTool(t, bin, "gostest-probe", probeScript)

	code, out := e.run(t, "--format", "json", "gostest-probe")
	expectCode(t, code, ExitOK, out)
	var doc struct {
		Outcomes []struct {
			Environment struct {
				Mounts []struct{ Source, Target string } `json:"mounts"`
			} `json:"environment"`
		} `json:"outcomes"`
	}
	if err := json.Unmarshal(e.stdout.Bytes(), &doc); err != nil {
		t.Fatalf("decoding the report: %v\n%s", err, out)
	}
	want, _ := filepath.EvalSymlinks(tool)
	for i, o := range doc.Outcomes {
		m := o.Environment.Mounts
		if len(m) != 1 || m[0].Source != want || m[0].Target != "/usr/local/bin/gostest-probe" {
			t.Errorf("outcome %d mounts = %+v, want %s at /usr/local/bin/gostest-probe", i, m, want)
		}
	}
}
