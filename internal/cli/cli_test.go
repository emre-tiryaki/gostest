package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emre-tiryaki/gostest/internal/backend"
	"github.com/emre-tiryaki/gostest/internal/distro"
)

type fakeHost struct {
	mu     sync.Mutex
	result backend.Result
	err    error
	specs  []backend.Spec
}

func (h *fakeHost) Run(context.Context, backend.Spec) (backend.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.result, h.err
}

func (h *fakeHost) RunSpec(spec backend.Spec) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.specs = append(h.specs, spec)
}

type recordingHost struct{ *fakeHost }

func (h recordingHost) Run(ctx context.Context, spec backend.Spec) (backend.Result, error) {
	h.RunSpec(spec)
	return h.fakeHost.Run(ctx, spec)
}

type prepareCall struct {
	distro  string
	pkgs    []string
	rebuild bool
}

type fakeContainers struct {
	name     string
	availErr error

	mu         sync.Mutex
	results    map[string]backend.Result // distro name -> result
	prepareErr map[string]error
	delay      time.Duration
	block      bool
	prepares   []prepareCall
	runs       []backend.Spec
	active     int
	peak       int
}

func (c *fakeContainers) Name() string                    { return c.name }
func (c *fakeContainers) Available(context.Context) error { return c.availErr }

func (c *fakeContainers) Prepare(_ context.Context, d distro.Distro, pkgs []string, rebuild bool) (backend.Environment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prepares = append(c.prepares, prepareCall{d.Name, pkgs, rebuild})
	if err := c.prepareErr[d.Name]; err != nil {
		return backend.Environment{}, err
	}
	// The image encodes the distro name so Run can find its result.
	return backend.Environment{Distro: d, Image: "img-" + d.Name, ImageID: "sha256:" + d.Name, Shell: "dash"}, nil
}

func (c *fakeContainers) Run(ctx context.Context, spec backend.Spec) (backend.Result, error) {
	c.mu.Lock()
	c.runs = append(c.runs, spec)
	c.active++
	c.peak = max(c.peak, c.active)
	res, ok := c.results[strings.TrimPrefix(spec.Image, "img-")]
	if !ok {
		res = backend.Result{Stdout: "ok\n"}
	}
	delay, block := c.delay, c.block
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.active--
		c.mu.Unlock()
	}()
	if block {
		<-ctx.Done()
		return backend.Result{}, ctx.Err()
	}
	time.Sleep(delay)
	return res, nil
}

func (c *fakeContainers) prepared() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, p := range c.prepares {
		out = append(out, p.distro)
	}
	slices.Sort(out)
	return out
}

type harness struct {
	app        *App
	stdout     *bytes.Buffer
	stderr     *bytes.Buffer
	host       *fakeHost
	containers *fakeContainers
	engines    []string // engines requested from NewContainers
	env        map[string]string
	wd         string
	paths      map[string]bool // binaries found by LookPath
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{
		stdout:     &bytes.Buffer{},
		stderr:     &bytes.Buffer{},
		host:       &fakeHost{result: backend.Result{Stdout: "ok\n"}},
		containers: &fakeContainers{name: "docker", results: map[string]backend.Result{}, prepareErr: map[string]error{}},
		env:        map[string]string{},
		wd:         t.TempDir(),
		paths:      map[string]bool{"docker": true},
	}
	cacheDir := filepath.Join(t.TempDir(), "cache")
	h.app = &App{
		Stdout:    h.stdout,
		Stderr:    h.stderr,
		Host:      recordingHost{h.host},
		HostShell: "bash",
		NewContainers: func(engine string) (Containers, error) {
			h.engines = append(h.engines, engine)
			h.containers.name = engine
			return h.containers, nil
		},
		LookPath: func(name string) (string, error) {
			if h.paths[name] {
				return "/usr/bin/" + name, nil
			}
			return "", errors.New("not found")
		},
		Getwd:    func() (string, error) { return h.wd, nil },
		Getenv:   func(k string) string { return h.env[k] },
		CacheDir: func() (string, error) { return cacheDir, nil },
		Version:  "1.2.3",
	}
	return h
}

func (h *harness) run(args ...string) int {
	return h.app.Run(context.Background(), args)
}

func (h *harness) writeConfig(t *testing.T, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.wd, ".gostest.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func expectExit(t *testing.T, h *harness, got, want int) {
	t.Helper()
	if got != want {
		t.Fatalf("exit code = %d, want %d\nstdout:\n%s\nstderr:\n%s", got, want, h.stdout, h.stderr)
	}
}

func TestHelp(t *testing.T) {
	for _, flag := range []string{"-h", "--help"} {
		h := newHarness(t)
		expectExit(t, h, h.run(flag), ExitOK)
		out := h.stdout.String() + h.stderr.String()
		for _, want := range []string{"Usage: gostest", "--distros", "--pkg", "--no-cache", "--timeout", "--concurrency", "Exit codes"} {
			if !strings.Contains(out, want) {
				t.Errorf("%s output should mention %q", flag, want)
			}
		}
	}
}

func TestVersion(t *testing.T) {
	h := newHarness(t)
	expectExit(t, h, h.run("--version"), ExitOK)
	if strings.TrimSpace(h.stdout.String()) != "gostest 1.2.3" {
		t.Fatalf("version output = %q", h.stdout)
	}
}

func TestUsageErrors(t *testing.T) {
	tests := map[string][]string{
		"no command":           {},
		"blank command":        {"   "},
		"unknown flag":         {"--bogus", "true"},
		"unknown distro":       {"-d", "nope", "true"},
		"bad package":          {"--pkg", "curl;id", "true"},
		"bad format":           {"--format", "xml", "true"},
		"bad color":            {"--color", "sometimes", "true"},
		"negative concurrency": {"-j", "-1", "true"},
		"negative timeout":     {"-t", "-5s", "true"},
		"negative context":     {"--context", "-1", "true"},
		"bad engine":           {"--engine", "lxc", "true"},
		"missing workdir":      {"--workdir", "/definitely/not/here", "true"},
		"missing config":       {"--config", "/definitely/not/here.json", "true"},
	}
	for name, args := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			expectExit(t, h, h.run(args...), ExitUsage)
			if h.stderr.Len() == 0 {
				t.Error("usage errors must be explained on stderr")
			}
			if len(h.containers.runs) != 0 {
				t.Error("nothing should run after a usage error")
			}
		})
	}
}

func TestInvalidConfigIsAUsageError(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(t, `{"distro": ["alpine"]}`)
	expectExit(t, h, h.run("true"), ExitUsage)
	if !strings.Contains(h.stderr.String(), ".gostest.json") {
		t.Fatalf("stderr should name the config file: %s", h.stderr)
	}
}

func TestAllPass(t *testing.T) {
	h := newHarness(t)
	expectExit(t, h, h.run("-d", "alpine,debian", "echo ok"), ExitOK)
	out := h.stdout.String()
	for _, want := range []string{"$ echo ok", "PASS", "alpine", "debian", "2 passed, 0 failed, 0 errors"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout should contain %q:\n%s", want, out)
		}
	}
}

func TestFailureExitCode(t *testing.T) {
	h := newHarness(t)
	h.containers.results["debian"] = backend.Result{Stderr: "sh: 1: [[: not found\n"}
	expectExit(t, h, h.run("-d", "alpine,debian", "x"), ExitFailed)
	if !strings.Contains(h.stdout.String(), "FAIL") {
		t.Fatalf("stdout:\n%s", h.stdout)
	}
}

func TestErrorOutcomeExitCode(t *testing.T) {
	h := newHarness(t)
	h.containers.prepareErr["alpine"] = errors.New("pull failed")
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitFailed)
	if !strings.Contains(h.stdout.String(), "pull failed") {
		t.Fatalf("stdout:\n%s", h.stdout)
	}
}

func TestReferenceFailureIsARuntimeError(t *testing.T) {
	h := newHarness(t)
	h.host.err = errors.New("cannot start /bin/sh")
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitRuntime)
	if !strings.Contains(h.stderr.String(), "cannot start /bin/sh") {
		t.Fatalf("stderr:\n%s", h.stderr)
	}
}

func TestInterruptedRun(t *testing.T) {
	h := newHarness(t)
	h.containers.block = true
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	code := h.app.Run(ctx, []string{"-d", "alpine", "x"})
	expectExit(t, h, code, ExitInterrupted)
}

func TestJSONFormat(t *testing.T) {
	h := newHarness(t)
	expectExit(t, h, h.run("--format", "json", "-d", "alpine", "echo ok"), ExitOK)
	var doc struct {
		Command  string `json:"command"`
		OK       bool   `json:"ok"`
		Outcomes []struct {
			Status string `json:"status"`
		} `json:"outcomes"`
	}
	if err := json.Unmarshal(h.stdout.Bytes(), &doc); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, h.stdout)
	}
	if doc.Command != "echo ok" || !doc.OK || len(doc.Outcomes) != 1 || doc.Outcomes[0].Status != "PASS" {
		t.Fatalf("doc = %+v", doc)
	}
}

func TestCommandFromSeveralArgs(t *testing.T) {
	h := newHarness(t)
	expectExit(t, h, h.run("-d", "alpine", "--", "echo", "-n", "hi"), ExitOK)
	if got := h.containers.runs[0].Command; got != "echo -n hi" {
		t.Fatalf("command = %q, want %q", got, "echo -n hi")
	}
	if got := h.host.specs[0].Command; got != "echo -n hi" {
		t.Fatalf("reference command = %q", got)
	}
}

func TestDefaultDistros(t *testing.T) {
	h := newHarness(t)
	expectExit(t, h, h.run("x"), ExitOK)
	reg, _ := distro.NewRegistry()
	var want []string
	for _, d := range reg.Defaults() {
		want = append(want, d.Name)
	}
	slices.Sort(want)
	if got := h.containers.prepared(); !slices.Equal(got, want) {
		t.Fatalf("prepared %v, want defaults %v", got, want)
	}
}

func TestDistrosFromConfigAndFlag(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(t, `{"distros": ["fedora", "mine"], "custom_distros": [{"name": "mine", "image": "m:1", "package_manager": "apk"}]}`)
	expectExit(t, h, h.run("x"), ExitOK)
	if got := h.containers.prepared(); !slices.Equal(got, []string{"fedora", "mine"}) {
		t.Fatalf("config distros: prepared %v", got)
	}

	h2 := newHarness(t)
	h2.wd = h.wd
	expectExit(t, h2, h2.run("--distros", "mine,alpine", "x"), ExitOK)
	if got := h2.containers.prepared(); !slices.Equal(got, []string{"alpine", "mine"}) {
		t.Fatalf("flag must override config and see custom distros: prepared %v", got)
	}
}

func TestPackagesMergeConfigAndFlags(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(t, `{"packages": ["curl", "alpine=bash"]}`)
	expectExit(t, h, h.run("-d", "alpine,fedora", "-p", "git", "--pkg", "fedora=which,jq", "x"), ExitOK)
	got := map[string][]string{}
	for _, p := range h.containers.prepares {
		got[p.distro] = p.pkgs
	}
	if !slices.Equal(got["alpine"], []string{"curl", "git", "jq", "bash"}) {
		t.Errorf("alpine packages = %v", got["alpine"])
	}
	if !slices.Equal(got["fedora"], []string{"curl", "git", "jq", "which"}) {
		t.Errorf("fedora packages = %v", got["fedora"])
	}
}

func TestTimeoutPrecedence(t *testing.T) {
	h := newHarness(t)
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitOK)
	if got := h.containers.runs[0].Timeout; got != DefaultTimeout {
		t.Errorf("default timeout = %v, want %v", got, DefaultTimeout)
	}

	h = newHarness(t)
	h.writeConfig(t, `{"timeout": "45s"}`)
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitOK)
	if got := h.containers.runs[0].Timeout; got != 45*time.Second {
		t.Errorf("config timeout = %v", got)
	}

	h2 := newHarness(t)
	h2.wd = h.wd
	expectExit(t, h2, h2.run("-d", "alpine", "--timeout", "3s", "x"), ExitOK)
	if got := h2.containers.runs[0].Timeout; got != 3*time.Second {
		t.Errorf("flag timeout = %v", got)
	}
	if got := h2.host.specs[0].Timeout; got != 3*time.Second {
		t.Errorf("reference timeout = %v", got)
	}
}

func TestZeroTimeoutDisablesIt(t *testing.T) {
	h := newHarness(t)
	expectExit(t, h, h.run("-d", "alpine", "-t", "0", "x"), ExitOK)
	if got := h.containers.runs[0].Timeout; got != 0 {
		t.Fatalf("timeout = %v, want none", got)
	}
}

func TestConcurrencyPrecedence(t *testing.T) {
	names := "alpine,debian,fedora,ubuntu,archlinux"
	h := newHarness(t)
	h.containers.delay = 30 * time.Millisecond
	expectExit(t, h, h.run("-d", names, "-j", "1", "x"), ExitOK)
	if h.containers.peak != 1 {
		t.Errorf("-j 1: peak = %d", h.containers.peak)
	}

	h = newHarness(t)
	h.containers.delay = 30 * time.Millisecond
	h.writeConfig(t, `{"concurrency": 2}`)
	expectExit(t, h, h.run("-d", names, "x"), ExitOK)
	if h.containers.peak != 2 {
		t.Errorf("config concurrency 2: peak = %d", h.containers.peak)
	}
}

func TestDefaultConcurrency(t *testing.T) {
	if DefaultConcurrency() != max(runtime.NumCPU(), 2) {
		t.Fatalf("DefaultConcurrency = %d", DefaultConcurrency())
	}
}

func TestIgnoreStderr(t *testing.T) {
	h := newHarness(t)
	h.containers.results["alpine"] = backend.Result{Stdout: "ok\n", Stderr: "warning\n"}
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitFailed)

	h = newHarness(t)
	h.containers.results["alpine"] = backend.Result{Stdout: "ok\n", Stderr: "warning\n"}
	expectExit(t, h, h.run("-d", "alpine", "--ignore-stderr", "x"), ExitOK)

	h = newHarness(t)
	h.containers.results["alpine"] = backend.Result{Stdout: "ok\n", Stderr: "warning\n"}
	h.writeConfig(t, `{"ignore_stderr": true}`)
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitOK)
}

func TestEngineSelection(t *testing.T) {
	h := newHarness(t)
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitOK)
	if !slices.Equal(h.engines, []string{"docker"}) {
		t.Errorf("auto-detect with docker installed: %v", h.engines)
	}

	h = newHarness(t)
	h.paths = map[string]bool{"podman": true}
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitOK)
	if !slices.Equal(h.engines, []string{"podman"}) {
		t.Errorf("auto-detect with only podman: %v", h.engines)
	}

	h = newHarness(t)
	h.paths = map[string]bool{"docker": true, "podman": true}
	expectExit(t, h, h.run("-d", "alpine", "--engine", "podman", "x"), ExitOK)
	if !slices.Equal(h.engines, []string{"podman"}) {
		t.Errorf("--engine podman: %v", h.engines)
	}

	h = newHarness(t)
	h.paths = map[string]bool{"docker": true, "podman": true}
	h.writeConfig(t, `{"engine": "podman"}`)
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitOK)
	if !slices.Equal(h.engines, []string{"podman"}) {
		t.Errorf("config engine podman: %v", h.engines)
	}
}

func TestNoEngineInstalled(t *testing.T) {
	h := newHarness(t)
	h.paths = map[string]bool{}
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitRuntime)
	if !strings.Contains(h.stderr.String(), "Docker or Podman") {
		t.Fatalf("stderr should tell how to fix it: %s", h.stderr)
	}
}

func TestEngineUnavailable(t *testing.T) {
	h := newHarness(t)
	h.containers.availErr = errors.New("docker is not available: Cannot connect to the Docker daemon")
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitRuntime)
	if !strings.Contains(h.stderr.String(), "Cannot connect") {
		t.Fatalf("stderr: %s", h.stderr)
	}
}

func TestWorkdir(t *testing.T) {
	h := newHarness(t)
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitOK)
	if got := h.containers.runs[0].Dir; got != h.wd {
		t.Errorf("default dir = %q, want %q", got, h.wd)
	}
	if got := h.host.specs[0].Dir; got != h.wd {
		t.Errorf("reference dir = %q, want %q", got, h.wd)
	}

	other := t.TempDir()
	h = newHarness(t)
	expectExit(t, h, h.run("-d", "alpine", "--workdir", other, "x"), ExitOK)
	if got := h.containers.runs[0].Dir; got != other {
		t.Errorf("--workdir dir = %q, want %q", got, other)
	}
}

func TestWorkdirMustBeADirectory(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(f, nil, 0o644)
	h := newHarness(t)
	expectExit(t, h, h.run("-d", "alpine", "--workdir", f, "x"), ExitUsage)
}

func TestExplicitConfigFlag(t *testing.T) {
	p := filepath.Join(t.TempDir(), "custom.json")
	_ = os.WriteFile(p, []byte(`{"distros": ["fedora"]}`), 0o644)
	h := newHarness(t)
	expectExit(t, h, h.run("--config", p, "x"), ExitOK)
	if got := h.containers.prepared(); !slices.Equal(got, []string{"fedora"}) {
		t.Fatalf("prepared %v", got)
	}
}

func TestCacheAcrossRuns(t *testing.T) {
	h := newHarness(t)
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitOK)
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitOK)
	if n := len(h.containers.runs); n != 1 {
		t.Fatalf("container ran %d times, want 1 (second run cached)", n)
	}
	if !strings.Contains(h.stdout.String(), "(cached)") {
		t.Fatalf("cached result should be marked:\n%s", h.stdout)
	}
}

func TestCacheInvalidatedByWorkdirChanges(t *testing.T) {
	h := newHarness(t)
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitOK)
	_ = os.WriteFile(filepath.Join(h.wd, "script.sh"), []byte("echo new\n"), 0o755)
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitOK)
	if n := len(h.containers.runs); n != 2 {
		t.Fatalf("container ran %d times, want 2", n)
	}
}

func TestNoCache(t *testing.T) {
	h := newHarness(t)
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitOK)
	expectExit(t, h, h.run("-d", "alpine", "--no-cache", "x"), ExitOK)
	if n := len(h.containers.runs); n != 2 {
		t.Fatalf("container ran %d times, want 2", n)
	}
	last := h.containers.prepares[len(h.containers.prepares)-1]
	if !last.rebuild {
		t.Fatal("--no-cache should rebuild images")
	}
}

func TestClearCache(t *testing.T) {
	h := newHarness(t)
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitOK)
	expectExit(t, h, h.run("--clear-cache"), ExitOK)
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitOK)
	if n := len(h.containers.runs); n != 2 {
		t.Fatalf("container ran %d times, want 2 after clearing", n)
	}
}

func TestClearCacheThenRun(t *testing.T) {
	h := newHarness(t)
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitOK)
	expectExit(t, h, h.run("--clear-cache", "-d", "alpine", "x"), ExitOK)
	if n := len(h.containers.runs); n != 2 {
		t.Fatalf("container ran %d times, want 2", n)
	}
}

func TestHugeWorkdirDisablesCache(t *testing.T) {
	h := newHarness(t)
	h.app.MaxFingerprintFiles = 3
	for i := range 5 {
		_ = os.WriteFile(filepath.Join(h.wd, fmt.Sprintf("f%d", i)), nil, 0o644)
	}
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitOK)
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitOK)
	if n := len(h.containers.runs); n != 2 {
		t.Fatalf("container ran %d times, want 2 (cache disabled)", n)
	}
	if !strings.Contains(h.stderr.String(), "cache") {
		t.Fatalf("a warning should explain why caching is off: %s", h.stderr)
	}
}

func TestColor(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		tty     bool
		noColor string
		term    string
		want    bool
	}{
		{"auto without tty", nil, false, "", "xterm", false},
		{"auto with tty", nil, true, "", "xterm", true},
		{"auto with NO_COLOR", nil, true, "1", "xterm", false},
		{"auto with dumb terminal", nil, true, "", "dumb", false},
		{"always", []string{"--color", "always"}, false, "1", "dumb", true},
		{"never", []string{"--color", "never"}, true, "", "xterm", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t)
			h.app.StdoutTTY = tt.tty
			h.env["NO_COLOR"] = tt.noColor
			h.env["TERM"] = tt.term
			args := append(append([]string{}, tt.args...), "-d", "alpine", "-q", "x")
			expectExit(t, h, h.run(args...), ExitOK)
			if got := strings.Contains(h.stdout.String(), "\x1b["); got != tt.want {
				t.Fatalf("colour = %v, want %v:\n%q", got, tt.want, h.stdout)
			}
		})
	}
}

func TestProgress(t *testing.T) {
	h := newHarness(t)
	h.app.StderrTTY = true
	expectExit(t, h, h.run("-d", "alpine,debian", "x"), ExitOK)
	if !strings.Contains(h.stderr.String(), "alpine") {
		t.Fatalf("progress should be shown on a terminal: %q", h.stderr)
	}

	for _, args := range [][]string{{"-q"}, {"--quiet"}} {
		h = newHarness(t)
		h.app.StderrTTY = true
		expectExit(t, h, h.run(append(args, "-d", "alpine", "x")...), ExitOK)
		if h.stderr.Len() != 0 {
			t.Fatalf("%v should silence progress: %q", args, h.stderr)
		}
	}

	h = newHarness(t)
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitOK)
	if h.stderr.Len() != 0 {
		t.Fatalf("no progress without a terminal: %q", h.stderr)
	}
}

func TestVerbose(t *testing.T) {
	h := newHarness(t)
	expectExit(t, h, h.run("-v", "-d", "alpine", "x"), ExitOK)
	if !strings.Contains(h.stdout.String(), "│ ok") {
		t.Fatalf("verbose should show outputs:\n%s", h.stdout)
	}
}

func TestListDistros(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(t, `{"custom_distros": [{"name": "mine", "image": "registry.local/mine:1", "package_manager": "apk"}]}`)
	expectExit(t, h, h.run("--list"), ExitOK)
	out := h.stdout.String()
	for _, want := range []string{"alpine", "alpine:latest", "apk", "gentoo/stage3:latest", "emerge", "mine", "registry.local/mine:1"} {
		if !strings.Contains(out, want) {
			t.Errorf("--list should contain %q:\n%s", want, out)
		}
	}
	for l := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(l)
		if len(fields) == 0 {
			continue
		}
		isDefault := strings.Contains(l, "*")
		switch fields[0] {
		case "alpine", "archlinux", "debian", "fedora", "ubuntu":
			if !isDefault {
				t.Errorf("default distro not marked: %q", l)
			}
		case "gentoo", "mine", "opensuse", "almalinux":
			if isDefault {
				t.Errorf("non-default distro marked: %q", l)
			}
		}
	}
	if len(h.containers.runs) != 0 || len(h.engines) != 0 {
		t.Fatal("--list must not touch the container engine")
	}
}
