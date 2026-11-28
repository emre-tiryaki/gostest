package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emre-tiryaki/gostest/internal/backend"
	"github.com/emre-tiryaki/gostest/internal/config"
	"github.com/emre-tiryaki/gostest/internal/deps"
	"github.com/emre-tiryaki/gostest/internal/distro"
	"github.com/emre-tiryaki/gostest/internal/engine"
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
	provides   [][]string
	runs       []backend.Spec
	probes     []string // dependency probe scripts
	active     int
	peak       int

	cleanups      int
	cleanupErr    error
	cleanupCtxErr error // ctx.Err() seen by the last Cleanup
}

func (c *fakeContainers) Name() string                    { return c.name }
func (c *fakeContainers) Available(context.Context) error { return c.availErr }

func (c *fakeContainers) Cleanup(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.cleanups++
	c.cleanupCtxErr = ctx.Err()
	return c.cleanupErr
}

func (c *fakeContainers) cleanupCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cleanups
}

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

// Provide derives the image "<image>+<cmd>+...", whose result is looked up
// as "<distro>+<cmd>+..."; every command is reported as installed.
func (c *fakeContainers) Provide(_ context.Context, env backend.Environment, cmds []string, rebuild bool) (backend.Environment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.provides = append(c.provides, cmds)
	out := env
	out.Image = env.Image + "+" + strings.Join(cmds, "+")
	out.ImageID = env.ImageID + "+" + strings.Join(cmds, "+")
	out.Provided = append(slices.Clone(env.Provided), cmds...)
	return out, nil
}

func (c *fakeContainers) provideCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.provides)
}

var probeNameRe = regexp.MustCompile(`'([^']+)'`)

func (c *fakeContainers) Run(ctx context.Context, spec backend.Spec) (backend.Result, error) {
	if strings.HasPrefix(spec.Command, "# gostest: dependency probe") {
		// Every probed command is missing.
		c.mu.Lock()
		defer c.mu.Unlock()
		c.probes = append(c.probes, spec.Command)
		var out strings.Builder
		for _, m := range probeNameRe.FindAllStringSubmatch(spec.Command, -1) {
			out.WriteString(m[1] + "\n")
		}
		return backend.Result{Stdout: out.String()}, nil
	}
	c.mu.Lock()
	c.runs = append(c.runs, spec)
	c.active++
	c.peak = max(c.peak, c.active)
	key := strings.TrimPrefix(spec.Image, "img-")
	for _, m := range spec.Mounts {
		key += "@" + filepath.Base(m.Target)
	}
	res, ok := c.results[key]
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
	hostTools  map[string]bool // commands App.HostTool resolves
	toolCalls  []string        // commands App.HostTool was asked for
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
		hostTools:  map[string]bool{},
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
		HostTool: func(name string) (backend.Mount, bool) {
			h.toolCalls = append(h.toolCalls, name)
			if !h.hostTools[name] {
				return backend.Mount{}, false
			}
			return backend.Mount{Source: "/home/u/bin/" + name, Target: backend.HostToolTarget + "/" + name, Hash: "h"}, true
		},
		Version: "1.2.3",
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

func TestConfigZeroTimeoutDisablesIt(t *testing.T) {
	h := newHarness(t)
	h.writeConfig(t, `{"timeout": 0}`)
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitOK)
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

func TestConfigZeroConcurrencyMeansNoLimit(t *testing.T) {
	// More distributions than the default limit allows at once.
	var custom, names []string
	for i := range DefaultConcurrency() + 2 {
		name := fmt.Sprintf("d%d", i)
		custom = append(custom, fmt.Sprintf(`{"name": %q, "image": "i:1", "package_manager": "apk"}`, name))
		names = append(names, name)
	}
	h := newHarness(t)
	h.containers.delay = 100 * time.Millisecond
	h.writeConfig(t, `{"concurrency": 0, "custom_distros": [`+strings.Join(custom, ",")+`]}`)
	expectExit(t, h, h.run("-d", strings.Join(names, ","), "x"), ExitOK)
	if h.containers.peak != len(names) {
		t.Fatalf("config concurrency 0: peak = %d, want %d", h.containers.peak, len(names))
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

func TestProgressBlock(t *testing.T) {
	var buf bytes.Buffer
	p := newProgress(&buf, []string{"alpine", "debian"})
	p.update(engine.Event{Distro: "alpine", Stage: engine.StagePreparing})
	first := buf.String()
	for _, want := range []string{"alpine", "preparing", "debian", "waiting", "[0/2]"} {
		if !strings.Contains(first, want) {
			t.Fatalf("first frame lacks %q: %q", want, first)
		}
	}
	if strings.Index(first, "alpine") > strings.Index(first, "debian") || strings.Index(first, "debian") > strings.Index(first, "[0/2]") {
		t.Fatalf("lines must follow the distro order with the counter last: %q", first)
	}
	if strings.Contains(first, "\x1b[2A") {
		t.Fatalf("first frame must not move the cursor up: %q", first)
	}

	buf.Reset()
	p.update(engine.Event{Distro: "alpine", Stage: engine.StageDone, Status: engine.Pass})
	second := buf.String()
	if !strings.HasPrefix(second, "\x1b[2A\r") {
		t.Fatalf("redraw must move up over the 3-line block: %q", second)
	}
	if !strings.Contains(second, "[1/2]") || !strings.Contains(second, "done PASS") {
		t.Fatalf("done not shown: %q", second)
	}

	buf.Reset()
	p.finish()
	if got := buf.String(); got != "\x1b[2A\r\x1b[J" {
		t.Fatalf("finish must erase the block, got %q", got)
	}

	buf.Reset()
	(*progress)(nil).finish()
	newProgress(&buf, []string{"a"}).finish()
	if buf.Len() != 0 {
		t.Fatalf("finish without a drawn block must print nothing: %q", buf.String())
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

func TestStrictOrder(t *testing.T) {
	reordered := backend.Result{Stdout: "b\nok\n"}
	setup := func() *harness {
		h := newHarness(t)
		h.host.result = backend.Result{Stdout: "ok\nb\n"}
		h.containers.results["alpine"] = reordered
		return h
	}

	h := setup()
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitOK)
	if !strings.Contains(h.stdout.String(), "different order") {
		t.Errorf("a passing reordered run should carry a note:\n%s", h.stdout.String())
	}

	h = setup()
	expectExit(t, h, h.run("-d", "alpine", "--strict-order", "x"), ExitFailed)

	h = setup()
	h.writeConfig(t, `{"strict_order": true}`)
	expectExit(t, h, h.run("-d", "alpine", "x"), ExitFailed)
}

func TestStrictOrderInUsage(t *testing.T) {
	h := newHarness(t)
	h.run("--help")
	if !strings.Contains(h.stdout.String()+h.stderr.String(), "--strict-order") {
		t.Error("usage should document --strict-order")
	}
}

func TestHostLocale(t *testing.T) {
	for _, tt := range []struct {
		env  map[string]string
		want string
	}{
		{map[string]string{}, ""},
		{map[string]string{"LANG": "en_US.UTF-8"}, "en_US.UTF-8"},
		{map[string]string{"LANG": "en_US.UTF-8", "LC_COLLATE": "tr_TR.UTF-8"}, "tr_TR.UTF-8"},
		{map[string]string{"LANG": "en_US.UTF-8", "LC_COLLATE": "tr_TR.UTF-8", "LC_ALL": "C"}, "C"},
		{map[string]string{"LANG": "de_DE.UTF-8", "LC_ALL": ""}, "de_DE.UTF-8"},
	} {
		if got := HostLocale(func(k string) string { return tt.env[k] }); got != tt.want {
			t.Errorf("HostLocale(%v) = %q, want %q", tt.env, got, tt.want)
		}
	}
}

func TestAutoInstall(t *testing.T) {
	setup := func() *harness {
		h := newHarness(t)
		h.containers.results["alpine"] = backend.Result{Stderr: "sh: jq: not found\n", ExitCode: 127}
		h.containers.results["alpine+jq"] = backend.Result{Stdout: "ok\n"}
		return h
	}

	h := setup()
	expectExit(t, h, h.run("-d", "alpine", "jq"), ExitOK)
	if h.containers.provideCount() != 1 {
		t.Errorf("Provide calls = %d, want 1: installing is the default", h.containers.provideCount())
	}
	if !strings.Contains(h.stdout.String(), "jq") {
		t.Errorf("the report should mention the installed command:\n%s", h.stdout.String())
	}

	h = setup()
	expectExit(t, h, h.run("-d", "alpine", "--no-auto-install", "jq"), ExitFailed)
	if h.containers.provideCount() != 0 {
		t.Errorf("--no-auto-install still installed: %v", h.containers.provides)
	}

	h = setup()
	h.writeConfig(t, `{"no_auto_install": true}`)
	expectExit(t, h, h.run("-d", "alpine", "jq"), ExitFailed)
	if h.containers.provideCount() != 0 {
		t.Errorf("no_auto_install in the configuration still installed: %v", h.containers.provides)
	}
}

func TestAutoInstallInUsage(t *testing.T) {
	h := newHarness(t)
	h.run("--help")
	if !strings.Contains(h.stdout.String()+h.stderr.String(), "--no-auto-install") {
		t.Error("usage should document --no-auto-install")
	}
}

func TestCleanup(t *testing.T) {
	h := newHarness(t)
	expectExit(t, h, h.run("-d", "alpine", "true"), ExitOK)
	if n := h.containers.cleanupCount(); n != 0 {
		t.Errorf("Cleanup calls = %d without --cleanup, want 0", n)
	}

	h = newHarness(t)
	expectExit(t, h, h.run("-d", "alpine", "--cleanup", "true"), ExitOK)
	if n := h.containers.cleanupCount(); n != 1 {
		t.Errorf("Cleanup calls = %d with --cleanup, want 1", n)
	}

	h = newHarness(t)
	h.writeConfig(t, `{"cleanup": true}`)
	expectExit(t, h, h.run("-d", "alpine", "true"), ExitOK)
	if n := h.containers.cleanupCount(); n != 1 {
		t.Errorf("Cleanup calls = %d with \"cleanup\": true, want 1", n)
	}
}

func TestCleanupAfterFailedRun(t *testing.T) {
	h := newHarness(t)
	h.containers.results["alpine"] = backend.Result{Stdout: "different\n"}
	expectExit(t, h, h.run("-d", "alpine", "--cleanup", "true"), ExitFailed)
	if n := h.containers.cleanupCount(); n != 1 {
		t.Fatalf("Cleanup calls = %d after differences, want 1", n)
	}
}

func TestCleanupAfterInterruption(t *testing.T) {
	h := newHarness(t)
	h.containers.block = true
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	expectExit(t, h, h.app.Run(ctx, []string{"-d", "alpine", "--cleanup", "x"}), ExitInterrupted)
	if n := h.containers.cleanupCount(); n != 1 {
		t.Fatalf("Cleanup calls = %d after an interruption, want 1", n)
	}
	if h.containers.cleanupCtxErr != nil {
		t.Fatalf("Cleanup got a finished context: %v", h.containers.cleanupCtxErr)
	}
}

func TestCleanupFailureIsAWarning(t *testing.T) {
	h := newHarness(t)
	h.containers.cleanupErr = errors.New("image is being used")
	expectExit(t, h, h.run("-d", "alpine", "--cleanup", "true"), ExitOK)
	if !strings.Contains(h.stderr.String(), "image is being used") {
		t.Fatalf("stderr should report the cleanup failure:\n%s", h.stderr)
	}
}

func TestCleanupWithoutEngine(t *testing.T) {
	h := newHarness(t)
	h.containers.availErr = errors.New("daemon down")
	expectExit(t, h, h.run("-d", "alpine", "--cleanup", "true"), ExitRuntime)
	if n := h.containers.cleanupCount(); n != 0 {
		t.Fatalf("Cleanup calls = %d without an engine, want 0", n)
	}
}

func TestCleanupInUsage(t *testing.T) {
	h := newHarness(t)
	h.run("--help")
	if !strings.Contains(h.stdout.String()+h.stderr.String(), "--cleanup") {
		t.Error("usage should document --cleanup")
	}
}

func TestHostToolMounting(t *testing.T) {
	setup := func() *harness {
		h := newHarness(t)
		h.hostTools["mytool"] = true
		h.containers.results["alpine"] = backend.Result{Stderr: "sh: mytool: not found\n", ExitCode: 127}
		h.containers.results["alpine@mytool"] = backend.Result{Stdout: "ok\n"}
		return h
	}

	h := setup()
	expectExit(t, h, h.run("-d", "alpine", "mytool"), ExitOK)
	if !slices.Equal(h.toolCalls, []string{"mytool"}) {
		t.Errorf("HostTool calls = %v, want [mytool]", h.toolCalls)
	}
	if h.containers.provideCount() != 0 {
		t.Errorf("Provide called for a mounted tool: %v", h.containers.provides)
	}
	if !strings.Contains(h.stdout.String(), "mounted from the host") {
		t.Errorf("the report should say mytool was mounted:\n%s", h.stdout.String())
	}

	h = setup()
	expectExit(t, h, h.run("-d", "alpine", "--no-auto-install", "mytool"), ExitFailed)
	if len(h.toolCalls) != 0 {
		t.Errorf("--no-auto-install still mounted host tools: %v", h.toolCalls)
	}

	h = setup()
	h.writeConfig(t, `{"no_auto_install": true}`)
	expectExit(t, h, h.run("-d", "alpine", "mytool"), ExitFailed)
	if len(h.toolCalls) != 0 {
		t.Errorf("no_auto_install in the configuration still mounted host tools: %v", h.toolCalls)
	}
}

func TestHostToolMountingInJSON(t *testing.T) {
	h := newHarness(t)
	h.hostTools["mytool"] = true
	h.containers.results["alpine"] = backend.Result{Stderr: "sh: mytool: not found\n", ExitCode: 127}
	h.containers.results["alpine@mytool"] = backend.Result{Stdout: "ok\n"}
	expectExit(t, h, h.run("-d", "alpine", "--format", "json", "mytool"), ExitOK)
	if !strings.Contains(h.stdout.String(), `"target": "/usr/local/bin/mytool"`) {
		t.Errorf("JSON should list the mount:\n%s", h.stdout.String())
	}
}

// lookIn returns a LookPath that finds the files in dir.
func lookIn(dir string) func(string) (string, error) {
	return func(name string) (string, error) {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err != nil {
			return "", fmt.Errorf("%s: not found", name)
		}
		return p, nil
	}
}

func TestHostToolResolver(t *testing.T) {
	dir := t.TempDir()
	content := "#!/bin/sh\necho mytool\n"
	real := filepath.Join(t.TempDir(), "mytool.sh")
	if err := os.WriteFile(real, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, filepath.Join(dir, "mytool")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "plain"), []byte("echo hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	resolve := HostToolResolver(lookIn(dir))

	m, ok := resolve("mytool")
	if !ok {
		t.Fatal("a shebang script on the host should be resolved")
	}
	wantSource, err := filepath.EvalSymlinks(real)
	if err != nil {
		t.Fatal(err)
	}
	if m.Source != wantSource {
		t.Errorf("Source = %q, want the symlink target %q", m.Source, wantSource)
	}
	if m.Target != "/usr/local/bin/mytool" {
		t.Errorf("Target = %q, want /usr/local/bin/mytool", m.Target)
	}
	if m.Hash == "" {
		t.Error("Hash is empty")
	}

	if err := os.WriteFile(real, []byte(content+"echo edited\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if m2, _ := resolve("mytool"); m2.Hash == m.Hash {
		t.Error("editing the script must change its hash")
	}

	if _, ok := resolve("missing"); ok {
		t.Error("a command missing on the host must not be resolved")
	}
	if _, ok := resolve("plain"); ok {
		t.Error("a file without a shebang must not be resolved")
	}
}

func TestHostToolResolverSkipsDynamicBinaries(t *testing.T) {
	sh, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Skip(err)
	}
	if kind, err := backend.ClassifyHostTool(sh); err != nil || kind != backend.ToolUnsupported {
		t.Skipf("/bin/sh is not a dynamic binary here (%v, %v)", kind, err)
	}
	resolve := HostToolResolver(func(string) (string, error) { return sh, nil })
	if _, ok := resolve("sh"); ok {
		t.Error("a dynamically linked binary must not be resolved")
	}
}

func TestHostToolResolverRejectsColons(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a:b")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "mytool"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, ok := HostToolResolver(lookIn(dir))("mytool"); ok {
		t.Error("paths containing ':' cannot be mounted and must not be resolved")
	}
}

func TestHostToolResolverRecordsInterpreter(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mytool"), []byte("#!/usr/bin/env bash\necho hi\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	m, ok := HostToolResolver(lookIn(dir))("mytool")
	if !ok || m.Interpreter != "bash" {
		t.Errorf("mount = %+v, %v; want interpreter bash", m, ok)
	}
}

// --- dependencies found by scanning the command ---

func TestScannedDependenciesInstalledFirst(t *testing.T) {
	h := newHarness(t)
	h.app.Deps = deps.Scan
	h.paths["jq"], h.paths["sh"] = true, true
	if err := os.WriteFile(filepath.Join(h.wd, "run.sh"), []byte("#!/bin/sh\njq . f\nnot-on-host x\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	expectExit(t, h, h.run("-d", "alpine", "./run.sh"), ExitOK)
	if len(h.containers.probes) != 1 {
		t.Fatalf("probes = %q, want one", h.containers.probes)
	}
	probe := h.containers.probes[0]
	if !strings.Contains(probe, "'sh'") || !strings.Contains(probe, "'jq'") || strings.Contains(probe, "not-on-host") {
		t.Errorf("probe = %q, want sh and jq but not a command the host lacks", probe)
	}
	if len(h.containers.provides) != 1 || !slices.Equal(h.containers.provides[0], []string{"sh", "jq"}) {
		t.Errorf("Provide calls = %q, want [[sh jq]]", h.containers.provides)
	}
}

func TestScannedDependenciesNeedAutoInstall(t *testing.T) {
	h := newHarness(t)
	h.app.Deps = deps.Scan
	h.paths["jq"] = true
	expectExit(t, h, h.run("-d", "alpine", "--no-auto-install", "jq . f"), ExitOK)
	if len(h.containers.probes) != 0 {
		t.Errorf("probes = %q, want none with --no-auto-install", h.containers.probes)
	}
}

func TestScannedDependenciesOptional(t *testing.T) {
	h := newHarness(t)
	h.paths["jq"] = true
	expectExit(t, h, h.run("-d", "alpine", "jq . f"), ExitOK)
	if len(h.containers.probes) != 0 {
		t.Errorf("probes = %q, want none when App.Deps is nil", h.containers.probes)
	}
}

func TestNewScansDependencies(t *testing.T) {
	if New("x").Deps == nil {
		t.Error("New should set Deps")
	}
}

// --- --save-deps ---

func readConfig(t *testing.T, p string) config.Config {
	t.Helper()
	c, err := config.Load(p)
	if err != nil {
		t.Fatalf("Load(%s): %v", p, err)
	}
	return c
}

func TestSaveDepsCreatesConfig(t *testing.T) {
	h := newHarness(t)
	h.app.Deps = deps.Scan
	h.paths["jq"], h.paths["sh"] = true, true
	if err := os.WriteFile(filepath.Join(h.wd, "run.sh"), []byte("#!/bin/sh\njq . f\nnot-on-host x\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	expectExit(t, h, h.run("--save-deps", "./run.sh"), ExitOK)
	p := filepath.Join(h.wd, ".gostest.json")
	if got := readConfig(t, p).Dependencies; !slices.Equal(got, []string{"sh", "jq", "not-on-host"}) {
		t.Errorf("dependencies = %q, want [sh jq not-on-host]", got)
	}
	out := h.stdout.String()
	for _, want := range []string{p, "jq", "sh"} {
		if !strings.Contains(out, want) {
			t.Errorf("output should mention %q:\n%s", want, out)
		}
	}
	if !strings.Contains(h.stderr.String(), "not-on-host") {
		t.Errorf("stderr should warn about commands missing on this machine:\n%s", h.stderr.String())
	}
	if len(h.engines) != 0 || len(h.host.specs) != 0 || len(h.containers.runs) != 0 {
		t.Errorf("--save-deps ran something: engines %v, host runs %d, container runs %d", h.engines, len(h.host.specs), len(h.containers.runs))
	}
}

func TestSaveDepsWithoutCommandCreatesEmptyConfig(t *testing.T) {
	h := newHarness(t)
	h.app.Deps = deps.Scan
	expectExit(t, h, h.run("--save-deps"), ExitOK)
	p := filepath.Join(h.wd, ".gostest.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "{\n  \"dependencies\": []\n}\n" {
		t.Errorf("file = %q", b)
	}
	if !strings.Contains(h.stdout.String(), "created") {
		t.Errorf("output should say the file was created:\n%s", h.stdout.String())
	}

	// Again: the file is kept as it is.
	h.stdout.Reset()
	expectExit(t, h, h.run("--save-deps"), ExitOK)
	if b2, _ := os.ReadFile(p); string(b2) != string(b) {
		t.Errorf("file changed: %q", b2)
	}
}

func TestSaveDepsUpdatesNearestConfig(t *testing.T) {
	h := newHarness(t)
	h.app.Deps = deps.Scan
	h.writeConfig(t, `{"distros": ["alpine"], "dependencies": ["git"]}`)
	sub := filepath.Join(h.wd, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	h.wd = sub

	expectExit(t, h, h.run("--save-deps", "curl x | jq ."), ExitOK)
	c := readConfig(t, filepath.Join(filepath.Dir(sub), ".gostest.json"))
	if !slices.Equal(c.Dependencies, []string{"git", "curl", "jq"}) || !slices.Equal(c.Distros, []string{"alpine"}) {
		t.Errorf("config = %+v, want git, curl, jq and the distros kept", c)
	}
	if _, err := os.Stat(filepath.Join(sub, ".gostest.json")); err == nil {
		t.Error("a new file was created instead of updating the nearest one")
	}
}

func TestSaveDepsExplicitConfig(t *testing.T) {
	h := newHarness(t)
	h.app.Deps = deps.Scan
	p := filepath.Join(t.TempDir(), "ci.json")
	expectExit(t, h, h.run("--config", p, "--save-deps", "jq"), ExitOK)
	if got := readConfig(t, p).Dependencies; !slices.Equal(got, []string{"jq"}) {
		t.Errorf("dependencies = %q, want [jq]", got)
	}
}

func TestSaveDepsNothingNew(t *testing.T) {
	h := newHarness(t)
	h.app.Deps = deps.Scan
	h.writeConfig(t, `{"dependencies": ["jq"]}`)
	expectExit(t, h, h.run("--save-deps", "jq ."), ExitOK)
	if !strings.Contains(h.stdout.String(), "up to date") {
		t.Errorf("output should say nothing changed:\n%s", h.stdout.String())
	}
}

func TestSaveDepsInvalidConfigIsAUsageError(t *testing.T) {
	h := newHarness(t)
	h.app.Deps = deps.Scan
	h.writeConfig(t, `{"distro": []}`)
	expectExit(t, h, h.run("--save-deps", "jq"), ExitUsage)
}

func TestSaveDepsInUsage(t *testing.T) {
	h := newHarness(t)
	h.run("--help")
	if !strings.Contains(h.stdout.String(), "--save-deps") {
		t.Error("usage should document --save-deps")
	}
}

func TestConfigDependenciesInstalledFirst(t *testing.T) {
	h := newHarness(t)
	h.paths["jq"] = true
	h.writeConfig(t, `{"dependencies": ["jq", "not-on-host"]}`)
	expectExit(t, h, h.run("-d", "alpine", "./run.sh"), ExitOK)
	if len(h.containers.probes) != 1 {
		t.Fatalf("probes = %q, want one", h.containers.probes)
	}
	if p := h.containers.probes[0]; !strings.Contains(p, "'jq'") || strings.Contains(p, "not-on-host") {
		t.Errorf("probe = %q, want jq only", p)
	}
}

func TestConfigAndScannedDependenciesMerge(t *testing.T) {
	h := newHarness(t)
	h.app.Deps = deps.Scan
	h.paths["jq"], h.paths["curl"] = true, true
	h.writeConfig(t, `{"dependencies": ["jq"]}`)
	expectExit(t, h, h.run("-d", "alpine", "curl x | jq ."), ExitOK)
	if len(h.containers.provides) != 1 || !slices.Equal(h.containers.provides[0], []string{"jq", "curl"}) {
		t.Errorf("Provide calls = %q, want [[jq curl]]", h.containers.provides)
	}
}
