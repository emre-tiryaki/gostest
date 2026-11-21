package engine

import (
	"context"
	"errors"
	"fmt"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emre-tiryaki/gostest/internal/backend"
	"github.com/emre-tiryaki/gostest/internal/cache"
	"github.com/emre-tiryaki/gostest/internal/compare"
	"github.com/emre-tiryaki/gostest/internal/distro"
	"github.com/emre-tiryaki/gostest/internal/explain"
)

type fakeHost struct {
	mu     sync.Mutex
	result backend.Result
	err    error
	specs  []backend.Spec
}

func (h *fakeHost) Run(_ context.Context, spec backend.Spec) (backend.Result, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.specs = append(h.specs, spec)
	return h.result, h.err
}

type provideCall struct {
	distro  string
	image   string
	cmds    []string
	rebuild bool
}

type prepareCall struct {
	distro  string
	pkgs    []string
	rebuild bool
}

type fakeContainers struct {
	mu         sync.Mutex
	envs       map[string]backend.Environment // distro name -> env
	prepareErr map[string]error
	results    map[string]backend.Result // image -> result
	runErr     map[string]error          // image -> error
	delay      map[string]time.Duration  // image -> run delay
	block      bool                      // runs block until ctx is done

	provides   map[string][]string // distro name -> commands a package exists for
	provideErr map[string]error

	prepares     []prepareCall
	provideCalls []provideCall
	runs         []backend.Spec

	// Dependency probes are answered from present and recorded in probes,
	// not in runs.
	present   map[string][]string // image -> commands it has
	probeFail map[string]bool     // image -> the probe exits 1
	probes    []backend.Spec
	active    int
	peak      int
}

func newFakeContainers() *fakeContainers {
	return &fakeContainers{
		envs:       map[string]backend.Environment{},
		prepareErr: map[string]error{},
		results:    map[string]backend.Result{},
		runErr:     map[string]error{},
		delay:      map[string]time.Duration{},
		provides:   map[string][]string{},
		provideErr: map[string]error{},
		present:    map[string][]string{},
		probeFail:  map[string]bool{},
	}
}

func (c *fakeContainers) Prepare(_ context.Context, d distro.Distro, pkgs []string, rebuild bool) (backend.Environment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prepares = append(c.prepares, prepareCall{d.Name, pkgs, rebuild})
	if err := c.prepareErr[d.Name]; err != nil {
		return backend.Environment{}, err
	}
	if env, ok := c.envs[d.Name]; ok {
		return env, nil
	}
	return backend.Environment{Distro: d, Image: d.Image, ImageID: "sha256:" + d.Name, Shell: "dash"}, nil
}

// Provide derives the image "<image>+<cmd>+..." whose Provided commands
// are those of cmds listed in c.provides for the distribution.
func (c *fakeContainers) Provide(_ context.Context, env backend.Environment, cmds []string, rebuild bool) (backend.Environment, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	name := env.Distro.Name
	c.provideCalls = append(c.provideCalls, provideCall{name, env.Image, slices.Clone(cmds), rebuild})
	if err := c.provideErr[name]; err != nil {
		return backend.Environment{}, err
	}
	out := env
	out.Image = env.Image + "+" + strings.Join(cmds, "+")
	out.ImageID = env.ImageID + "+" + strings.Join(cmds, "+")
	out.Provided = slices.Clone(env.Provided)
	for _, cmd := range cmds {
		if slices.Contains(c.provides[name], cmd) {
			out.Provided = append(out.Provided, cmd)
		}
	}
	return out, nil
}

func (c *fakeContainers) provided() []provideCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.provideCalls)
}

var probeNameRe = regexp.MustCompile(`'([^']+)'`)

// probe answers a dependency probe: it prints the probed commands that
// c.present does not list for the image.
func (c *fakeContainers) probe(spec backend.Spec) backend.Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.probes = append(c.probes, spec)
	if c.probeFail[spec.Image] {
		return backend.Result{Stderr: "probe failed\n", ExitCode: 1}
	}
	var out strings.Builder
	for _, m := range probeNameRe.FindAllStringSubmatch(spec.Command, -1) {
		if !slices.Contains(c.present[spec.Image], m[1]) {
			out.WriteString(m[1] + "\n")
		}
	}
	return backend.Result{Stdout: out.String()}
}

func (c *fakeContainers) probeSpecs() []backend.Spec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.probes)
}

func (c *fakeContainers) Run(ctx context.Context, spec backend.Spec) (backend.Result, error) {
	if strings.HasPrefix(spec.Command, probeMarker) {
		return c.probe(spec), nil
	}
	c.mu.Lock()
	c.runs = append(c.runs, spec)
	c.active++
	c.peak = max(c.peak, c.active)
	delay, res, err, block := c.delay[spec.Image], c.results[resultKey(spec)], c.runErr[spec.Image], c.block
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
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return backend.Result{}, ctx.Err()
	}
	return res, err
}

// resultKey is the key of c.results for spec: its image, followed by
// "@<command>" for every mounted host tool.
func resultKey(spec backend.Spec) string {
	key := spec.Image
	for _, m := range spec.Mounts {
		key += "@" + path.Base(m.Target)
	}
	return key
}

func (c *fakeContainers) lastRun() backend.Spec {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runs[len(c.runs)-1]
}

func (c *fakeContainers) runCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.runs)
}

func distros(names ...string) []distro.Distro {
	out := make([]distro.Distro, len(names))
	for i, n := range names {
		out[i] = distro.Distro{Name: n, Image: n + ":latest", PackageManager: distro.APK}
	}
	return out
}

func newEngine(host *fakeHost, c *fakeContainers) *Engine {
	return &Engine{Host: host, HostShell: "bash", Containers: c}
}

func run(t *testing.T, e *Engine, opts Options, ds []distro.Distro) Report {
	t.Helper()
	rep, err := e.Run(context.Background(), opts, ds)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return rep
}

func outcomeNames(rep Report) []string {
	var out []string
	for _, o := range rep.Outcomes {
		out = append(out, o.Distro.Name)
	}
	return out
}

func TestPassFailAndError(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "hi\n"}}
	c := newFakeContainers()
	c.results["same:latest"] = backend.Result{Stdout: "hi\n"}
	c.results["diff:latest"] = backend.Result{Stdout: "ho\n"}
	c.prepareErr["broken"] = errors.New("pulling broken:latest failed: not found")
	c.runErr["crash:latest"] = errors.New("docker run crash:latest: daemon exploded")

	rep := run(t, newEngine(host, c), Options{Command: "echo hi"}, distros("same", "diff", "broken", "crash"))
	want := map[string]Status{"same": Pass, "diff": Fail, "broken": Error, "crash": Error}
	for _, o := range rep.Outcomes {
		if o.Status != want[o.Distro.Name] {
			t.Errorf("%s: status %s, want %s", o.Distro.Name, o.Status, want[o.Distro.Name])
		}
	}
	byName := map[string]Outcome{}
	for _, o := range rep.Outcomes {
		byName[o.Distro.Name] = o
	}
	if !strings.Contains(byName["broken"].Error, "not found") {
		t.Errorf("prepare error not reported: %q", byName["broken"].Error)
	}
	if !strings.Contains(byName["crash"].Error, "daemon exploded") {
		t.Errorf("run error not reported: %q", byName["crash"].Error)
	}
	if len(byName["same"].Reasons) != 0 {
		t.Errorf("passing outcome has reasons: %+v", byName["same"].Reasons)
	}
	if len(byName["diff"].Reasons) == 0 {
		t.Error("failing outcome needs reasons")
	}
	if byName["diff"].Comparison.StdoutMatch {
		t.Error("comparison not recorded")
	}
	if p, f, e := rep.Counts(); p != 1 || f != 1 || e != 2 {
		t.Errorf("Counts = %d/%d/%d, want 1/1/2", p, f, e)
	}
	if rep.OK() {
		t.Error("report with failures is not OK")
	}
}

func TestReportBasics(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "x"}}
	c := newFakeContainers()
	c.results["a:latest"] = backend.Result{Stdout: "x"}
	rep := run(t, newEngine(host, c), Options{Command: "printf x"}, distros("a"))
	if rep.Command != "printf x" {
		t.Errorf("Command = %q", rep.Command)
	}
	if rep.Reference.Shell != "bash" || rep.Reference.Result.Stdout != "x" {
		t.Errorf("Reference = %+v", rep.Reference)
	}
	if !rep.OK() {
		t.Error("all passing report should be OK")
	}
	if rep.Outcomes[0].Env.ImageID != "sha256:a" {
		t.Errorf("environment not recorded: %+v", rep.Outcomes[0].Env)
	}
}

func TestOutcomesKeepDistroOrder(t *testing.T) {
	c := newFakeContainers()
	c.delay["a:latest"] = 60 * time.Millisecond
	c.delay["b:latest"] = 10 * time.Millisecond
	c.delay["c:latest"] = 30 * time.Millisecond
	rep := run(t, newEngine(&fakeHost{}, c), Options{Command: "x"}, distros("a", "b", "c"))
	if got := outcomeNames(rep); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("outcome order = %v", got)
	}
}

func TestRunsInParallelByDefault(t *testing.T) {
	c := newFakeContainers()
	names := []string{"a", "b", "c", "d", "e", "f"}
	for _, n := range names {
		c.delay[n+":latest"] = 50 * time.Millisecond
	}
	start := time.Now()
	run(t, newEngine(&fakeHost{}, c), Options{Command: "x"}, distros(names...))
	if c.peak != len(names) {
		t.Errorf("peak concurrency = %d, want %d", c.peak, len(names))
	}
	if elapsed := time.Since(start); elapsed > 250*time.Millisecond {
		t.Errorf("runs were not parallel: took %v", elapsed)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	c := newFakeContainers()
	names := []string{"a", "b", "c", "d", "e", "f"}
	for _, n := range names {
		c.delay[n+":latest"] = 30 * time.Millisecond
	}
	run(t, newEngine(&fakeHost{}, c), Options{Command: "x", Concurrency: 2}, distros(names...))
	if c.peak != 2 {
		t.Fatalf("peak concurrency = %d, want 2", c.peak)
	}
	if c.runCount() != len(names) {
		t.Fatalf("ran %d targets, want %d", c.runCount(), len(names))
	}
}

func TestSpecsPassedThrough(t *testing.T) {
	host := &fakeHost{}
	c := newFakeContainers()
	c.envs["a"] = backend.Environment{Image: "gostest/a:123", ImageID: "sha256:built"}
	opts := Options{Command: "ls", Dir: "/work/proj", Timeout: 42 * time.Second}
	run(t, newEngine(host, c), opts, distros("a"))
	wantHost := backend.Spec{Command: "ls", Dir: "/work/proj", Timeout: 42 * time.Second}
	if len(host.specs) != 1 || !reflect.DeepEqual(host.specs[0], wantHost) {
		t.Errorf("host specs = %+v, want %+v", host.specs, wantHost)
	}
	wantRun := backend.Spec{Command: "ls", Dir: "/work/proj", Timeout: 42 * time.Second, Image: "gostest/a:123"}
	if len(c.runs) != 1 || !reflect.DeepEqual(c.runs[0], wantRun) {
		t.Errorf("container specs = %+v, want %+v", c.runs, wantRun)
	}
}

func TestPackagesAndRebuildPassedToPrepare(t *testing.T) {
	c := newFakeContainers()
	pkgs := distro.Packages{Common: []string{"curl"}, PerDistro: map[string][]string{"alpine": {"bash"}}}
	run(t, newEngine(&fakeHost{}, c), Options{Command: "x", Packages: pkgs, NoCache: true}, distros("alpine", "fedora"))
	got := map[string]prepareCall{}
	for _, p := range c.prepares {
		got[p.distro] = p
	}
	if !slices.Equal(got["alpine"].pkgs, []string{"curl", "bash"}) || !slices.Equal(got["fedora"].pkgs, []string{"curl"}) {
		t.Errorf("packages = %+v", got)
	}
	if !got["alpine"].rebuild || !got["fedora"].rebuild {
		t.Errorf("NoCache should rebuild images: %+v", got)
	}
}

func TestReferenceFailureAbortsRun(t *testing.T) {
	host := &fakeHost{err: errors.New("cannot start /bin/sh")}
	_, err := newEngine(host, newFakeContainers()).Run(context.Background(), Options{Command: "x"}, distros("a"))
	if err == nil || !strings.Contains(err.Error(), "cannot start /bin/sh") {
		t.Fatalf("err = %v, want reference error", err)
	}
}

func TestValidation(t *testing.T) {
	e := newEngine(&fakeHost{}, newFakeContainers())
	if _, err := e.Run(context.Background(), Options{}, distros("a")); err == nil {
		t.Error("empty command should fail")
	}
	if _, err := e.Run(context.Background(), Options{Command: "x"}, nil); err == nil {
		t.Error("no distros should fail")
	}
	if _, err := (&Engine{Containers: newFakeContainers()}).Run(context.Background(), Options{Command: "x"}, distros("a")); err == nil {
		t.Error("missing host backend should fail")
	}
	if _, err := (&Engine{Host: &fakeHost{}}).Run(context.Background(), Options{Command: "x"}, distros("a")); err == nil {
		t.Error("missing container backend should fail")
	}
}

func TestCancellation(t *testing.T) {
	c := newFakeContainers()
	c.block = true
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, err := newEngine(&fakeHost{}, c).Run(ctx, Options{Command: "x"}, distros("a", "b", "c"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if time.Since(start) > time.Second {
		t.Fatalf("cancellation took %v", time.Since(start))
	}
}

func TestCompareOptionsPassedThrough(t *testing.T) {
	c := newFakeContainers()
	c.results["a:latest"] = backend.Result{Stderr: "warning\n"}
	rep := run(t, newEngine(&fakeHost{}, c), Options{Command: "x", Compare: compare.Options{IgnoreStderr: true}}, distros("a"))
	if rep.Outcomes[0].Status != Pass {
		t.Fatalf("stderr should be ignored: %+v", rep.Outcomes[0])
	}
}

func TestReasonsUseShells(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "matched\n"}}
	c := newFakeContainers()
	c.envs["debian"] = backend.Environment{Image: "debian:latest", ImageID: "sha256:d", Shell: "dash"}
	c.results["debian:latest"] = backend.Result{Stderr: "sh: 1: [[: not found\n"}
	rep := run(t, newEngine(host, c), Options{Command: "x"}, distros("debian"))
	o := rep.Outcomes[0]
	if o.Status != Fail {
		t.Fatalf("status = %s", o.Status)
	}
	var bashism *explain.Reason
	for i := range o.Reasons {
		if o.Reasons[i].Kind == explain.KindBashism {
			bashism = &o.Reasons[i]
		}
	}
	if bashism == nil || !strings.Contains(bashism.Message, "dash") || !strings.Contains(bashism.Message, "bash") {
		t.Fatalf("expected a bashism reason naming both shells: %+v", o.Reasons)
	}
}

func TestProgressEvents(t *testing.T) {
	c := newFakeContainers()
	c.prepareErr["bad"] = errors.New("boom")
	for _, n := range []string{"a", "b"} {
		c.delay[n+":latest"] = 10 * time.Millisecond
	}
	var (
		mu       sync.Mutex
		events   = map[string][]Stage{}
		statuses = map[string]Status{}
		inside   atomic.Int32
		overlap  atomic.Bool
	)
	e := newEngine(&fakeHost{}, c)
	e.Progress = func(ev Event) {
		if inside.Add(1) > 1 {
			overlap.Store(true)
		}
		time.Sleep(time.Millisecond)
		mu.Lock()
		events[ev.Distro] = append(events[ev.Distro], ev.Stage)
		if ev.Stage == StageDone {
			statuses[ev.Distro] = ev.Status
		}
		mu.Unlock()
		inside.Add(-1)
	}
	run(t, e, Options{Command: "x"}, distros("a", "b", "bad"))
	if overlap.Load() {
		t.Error("Progress must not be called concurrently")
	}
	for _, n := range []string{"a", "b"} {
		if !slices.Equal(events[n], []Stage{StagePreparing, StageRunning, StageDone}) {
			t.Errorf("%s events = %v", n, events[n])
		}
		if statuses[n] != Pass {
			t.Errorf("%s done status = %s, want PASS", n, statuses[n])
		}
	}
	if !slices.Equal(events["bad"], []Stage{StagePreparing, StageDone}) || statuses["bad"] != Error {
		t.Errorf("bad events = %v status = %s", events["bad"], statuses["bad"])
	}
}

// --- caching ---

func cachedEngine(t *testing.T, host *fakeHost, c *fakeContainers) *Engine {
	t.Helper()
	store, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(host, c)
	e.Cache = store
	return e
}

func TestCacheReusesResults(t *testing.T) {
	c := newFakeContainers()
	c.results["a:latest"] = backend.Result{Stdout: "hi\n"}
	e := cachedEngine(t, &fakeHost{result: backend.Result{Stdout: "hi\n"}}, c)
	opts := Options{Command: "echo hi", Dir: "/p", Fingerprint: "fp1"}

	first := run(t, e, opts, distros("a"))
	if first.Outcomes[0].Cached {
		t.Fatal("first run cannot be cached")
	}
	second := run(t, e, opts, distros("a"))
	if !second.Outcomes[0].Cached {
		t.Fatal("second run should be served from the cache")
	}
	if c.runCount() != 1 {
		t.Fatalf("container ran %d times, want 1", c.runCount())
	}
	if second.Outcomes[0].Result.Stdout != "hi\n" || second.Outcomes[0].Status != Pass {
		t.Fatalf("cached outcome = %+v", second.Outcomes[0])
	}
}

func TestCachedRunsStillCompareWithFreshReference(t *testing.T) {
	c := newFakeContainers()
	c.results["a:latest"] = backend.Result{Stdout: "hi\n"}
	host := &fakeHost{result: backend.Result{Stdout: "hi\n"}}
	e := cachedEngine(t, host, c)
	opts := Options{Command: "echo hi", Fingerprint: "fp"}
	run(t, e, opts, distros("a"))
	host.result = backend.Result{Stdout: "changed\n"}
	rep := run(t, e, opts, distros("a"))
	if !rep.Outcomes[0].Cached || rep.Outcomes[0].Status != Fail {
		t.Fatalf("cached result must be compared with the new reference: %+v", rep.Outcomes[0])
	}
	if len(host.specs) != 2 {
		t.Fatalf("reference ran %d times, want every time", len(host.specs))
	}
}

func TestCacheKeyInputs(t *testing.T) {
	base := Options{Command: "echo hi", Dir: "/p", Fingerprint: "fp", Timeout: time.Minute}
	variants := map[string]func(*Options, *fakeContainers){
		"command":     func(o *Options, _ *fakeContainers) { o.Command = "echo ho" },
		"dir":         func(o *Options, _ *fakeContainers) { o.Dir = "/q" },
		"fingerprint": func(o *Options, _ *fakeContainers) { o.Fingerprint = "fp2" },
		"timeout":     func(o *Options, _ *fakeContainers) { o.Timeout = 2 * time.Minute },
		"image id": func(_ *Options, c *fakeContainers) {
			c.envs["a"] = backend.Environment{Image: "a:latest", ImageID: "sha256:new"}
		},
	}
	for name, change := range variants {
		t.Run(name, func(t *testing.T) {
			c := newFakeContainers()
			e := cachedEngine(t, &fakeHost{}, c)
			run(t, e, base, distros("a"))
			opts := base
			change(&opts, c)
			rep := run(t, e, opts, distros("a"))
			if rep.Outcomes[0].Cached {
				t.Fatalf("changing the %s must invalidate the cache", name)
			}
		})
	}
}

func TestNoCacheRunsAgainAndRefreshes(t *testing.T) {
	c := newFakeContainers()
	c.results["a:latest"] = backend.Result{Stdout: "v1\n"}
	e := cachedEngine(t, &fakeHost{}, c)
	opts := Options{Command: "x", Fingerprint: "fp"}
	run(t, e, opts, distros("a"))

	c.results["a:latest"] = backend.Result{Stdout: "v2\n"}
	noCache := opts
	noCache.NoCache = true
	rep := run(t, e, noCache, distros("a"))
	if rep.Outcomes[0].Cached || rep.Outcomes[0].Result.Stdout != "v2\n" {
		t.Fatalf("--no-cache must run again: %+v", rep.Outcomes[0])
	}
	rep = run(t, e, opts, distros("a"))
	if !rep.Outcomes[0].Cached || rep.Outcomes[0].Result.Stdout != "v2\n" {
		t.Fatalf("--no-cache results should refresh the cache: %+v", rep.Outcomes[0])
	}
}

func TestTimeoutsAreNotCached(t *testing.T) {
	c := newFakeContainers()
	c.results["a:latest"] = backend.Result{TimedOut: true, ExitCode: backend.ExitTimeout}
	e := cachedEngine(t, &fakeHost{}, c)
	opts := Options{Command: "x", Fingerprint: "fp"}
	run(t, e, opts, distros("a"))
	rep := run(t, e, opts, distros("a"))
	if rep.Outcomes[0].Cached {
		t.Fatal("timed out results must not be cached")
	}
}

func TestErrorsAreNotCached(t *testing.T) {
	c := newFakeContainers()
	c.runErr["a:latest"] = errors.New("boom")
	e := cachedEngine(t, &fakeHost{}, c)
	opts := Options{Command: "x", Fingerprint: "fp"}
	run(t, e, opts, distros("a"))
	delete(c.runErr, "a:latest")
	rep := run(t, e, opts, distros("a"))
	if rep.Outcomes[0].Cached || rep.Outcomes[0].Status != Pass {
		t.Fatalf("errors must not be cached: %+v", rep.Outcomes[0])
	}
}

func TestNilCacheAlwaysRuns(t *testing.T) {
	c := newFakeContainers()
	e := newEngine(&fakeHost{}, c)
	for range 2 {
		run(t, e, Options{Command: "x", Fingerprint: "fp"}, distros("a"))
	}
	if c.runCount() != 2 {
		t.Fatalf("ran %d times, want 2", c.runCount())
	}
}

func TestCachedProgressStage(t *testing.T) {
	c := newFakeContainers()
	e := cachedEngine(t, &fakeHost{}, c)
	opts := Options{Command: "x", Fingerprint: "fp"}
	run(t, e, opts, distros("a"))
	var stages []Stage
	e.Progress = func(ev Event) { stages = append(stages, ev.Stage) }
	run(t, e, opts, distros("a"))
	if !slices.Equal(stages, []Stage{StagePreparing, StageCached, StageDone}) {
		t.Fatalf("stages = %v", stages)
	}
}

func TestManyDistros(t *testing.T) {
	c := newFakeContainers()
	var names []string
	for i := range 50 {
		names = append(names, fmt.Sprintf("d%02d", i))
	}
	rep := run(t, newEngine(&fakeHost{}, c), Options{Command: "x", Concurrency: 8}, distros(names...))
	if got := outcomeNames(rep); !slices.Equal(got, names) {
		t.Fatalf("outcomes = %v", got)
	}
	if c.peak > 8 {
		t.Fatalf("peak = %d, want <= 8", c.peak)
	}
}

func TestReorderedOutputPassesWithNote(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "CLAUDE.md\ngo.mod\nMakefile\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = backend.Result{Stdout: "CLAUDE.md\nMakefile\ngo.mod\n"}
	e := newEngine(host, c)
	e.HostLocale = "en_US.UTF-8"
	o := run(t, e, Options{Command: "ls"}, distros("a")).Outcomes[0]
	if o.Status != Pass || o.Reasons != nil {
		t.Fatalf("reordered output should pass: %+v", o)
	}
	if len(o.Notes) != 1 || o.Notes[0].Kind != explain.KindReordered || !strings.Contains(o.Notes[0].Message, "en_US.UTF-8") {
		t.Fatalf("expected a reordered note naming the host locale: %+v", o.Notes)
	}
}

func TestReorderedOutputFailsWithStrictOrder(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "a\nb\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = backend.Result{Stdout: "b\na\n"}
	o := run(t, newEngine(host, c), Options{Command: "x", Compare: compare.Options{StrictOrder: true}}, distros("a")).Outcomes[0]
	if o.Status != Fail || o.Notes != nil {
		t.Fatalf("strict order should fail without notes: %+v", o)
	}
	found := false
	for _, r := range o.Reasons {
		found = found || r.Kind == explain.KindReordered
	}
	if !found {
		t.Fatalf("expected a reordered reason: %+v", o.Reasons)
	}
}

func TestIdenticalOutputHasNoNotes(t *testing.T) {
	o := run(t, newEngine(&fakeHost{}, newFakeContainers()), Options{Command: "x"}, distros("a")).Outcomes[0]
	if o.Status != Pass || o.Notes != nil {
		t.Fatalf("outcome = %+v", o)
	}
}

func notFoundResult(cmds ...string) backend.Result {
	var stderr strings.Builder
	for _, c := range cmds {
		fmt.Fprintf(&stderr, "sh: %s: not found\n", c)
	}
	return backend.Result{Stderr: stderr.String(), ExitCode: 127}
}

func hasNote(o Outcome, kind explain.Kind, substr string) bool {
	for _, n := range o.Notes {
		if n.Kind == kind && strings.Contains(n.Message, substr) {
			return true
		}
	}
	return false
}

func TestAutoInstallRerunsWithMissingCommand(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "1.7\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("jq")
	c.results["a:latest+jq"] = backend.Result{Stdout: "1.7\n"}
	c.provides["a"] = []string{"jq"}

	rep := run(t, newEngine(host, c), Options{Command: "jq --version", AutoInstall: true}, distros("a"))
	o := rep.Outcomes[0]
	if o.Status != Pass {
		t.Fatalf("status %s, want PASS after installing jq: %+v", o.Status, o)
	}
	calls := c.provided()
	if len(calls) != 1 || !slices.Equal(calls[0].cmds, []string{"jq"}) || calls[0].image != "a:latest" {
		t.Fatalf("Provide calls = %+v, want one for jq on a:latest", calls)
	}
	if o.Env.Image != "a:latest+jq" || !slices.Equal(o.Env.Provided, []string{"jq"}) {
		t.Errorf("Env = %+v, want the derived environment", o.Env)
	}
	if c.runCount() != 2 {
		t.Errorf("runs = %d, want 2", c.runCount())
	}
	if !hasNote(o, explain.KindInstalled, "jq") {
		t.Errorf("notes = %+v, want an installed note naming jq", o.Notes)
	}
}

func TestAutoInstallDisabled(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "1.7\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("jq")
	c.provides["a"] = []string{"jq"}

	rep := run(t, newEngine(host, c), Options{Command: "jq --version"}, distros("a"))
	if rep.Outcomes[0].Status != Fail {
		t.Errorf("status %s, want FAIL", rep.Outcomes[0].Status)
	}
	if calls := c.provided(); len(calls) != 0 {
		t.Errorf("Provide called without AutoInstall: %+v", calls)
	}
	if c.runCount() != 1 {
		t.Errorf("runs = %d, want 1", c.runCount())
	}
}

func TestAutoInstallSkipsCommandsMissingOnHost(t *testing.T) {
	host := &fakeHost{result: notFoundResult("jq")}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("jq")
	c.provides["a"] = []string{"jq"}

	rep := run(t, newEngine(host, c), Options{Command: "jq --version", AutoInstall: true}, distros("a"))
	if rep.Outcomes[0].Status != Pass {
		t.Errorf("status %s, want PASS", rep.Outcomes[0].Status)
	}
	if calls := c.provided(); len(calls) != 0 {
		t.Errorf("Provide called for a command the host lacks too: %+v", calls)
	}
}

func TestAutoInstallWithoutPackage(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("mytool")

	rep := run(t, newEngine(host, c), Options{Command: "mytool", AutoInstall: true}, distros("a"))
	o := rep.Outcomes[0]
	if o.Status != Fail {
		t.Fatalf("status %s, want FAIL", o.Status)
	}
	if len(c.provided()) != 1 {
		t.Errorf("Provide calls = %+v, want one attempt", c.provided())
	}
	if c.runCount() != 1 {
		t.Errorf("runs = %d, want 1: nothing was installed, so there is nothing to run again", c.runCount())
	}
	if o.Env.Image != "a:latest" || len(o.Env.Provided) != 0 {
		t.Errorf("Env = %+v, want the original environment", o.Env)
	}
	if hasNote(o, explain.KindInstalled, "") {
		t.Errorf("unexpected installed note: %+v", o.Notes)
	}
	found := false
	for _, r := range o.Reasons {
		if r.Kind == explain.KindMissingCommand && strings.Contains(r.Message, "no package") {
			found = true
		}
	}
	if !found {
		t.Errorf("reasons = %+v, want a missing-command reason saying no package was found", o.Reasons)
	}
}

func TestAutoInstallFailureKeepsResult(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "1.7\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("jq")
	c.provideErr["a"] = errors.New("a: installing jq failed:\nnetwork unreachable")

	rep := run(t, newEngine(host, c), Options{Command: "jq --version", AutoInstall: true}, distros("a"))
	o := rep.Outcomes[0]
	if o.Status != Fail {
		t.Fatalf("status %s, want FAIL (an installation failure is not an ERROR)", o.Status)
	}
	if o.Result.ExitCode != 127 {
		t.Errorf("result = %+v, want the original run", o.Result)
	}
	if !hasNote(o, explain.KindInstalled, "network unreachable") {
		t.Errorf("notes = %+v, want the installation failure", o.Notes)
	}
}

func TestAutoInstallSeveralRounds(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("jq")
	c.results["a:latest+jq"] = notFoundResult("curl")
	c.results["a:latest+jq+curl"] = backend.Result{Stdout: "ok\n"}
	c.provides["a"] = []string{"jq", "curl"}

	rep := run(t, newEngine(host, c), Options{Command: "jq . | curl", AutoInstall: true}, distros("a"))
	o := rep.Outcomes[0]
	if o.Status != Pass {
		t.Fatalf("status %s, want PASS", o.Status)
	}
	calls := c.provided()
	if len(calls) != 2 || calls[1].image != "a:latest+jq" || !slices.Equal(calls[1].cmds, []string{"curl"}) {
		t.Fatalf("Provide calls = %+v, want jq then curl on top of it", calls)
	}
	if !slices.Equal(o.Env.Provided, []string{"jq", "curl"}) {
		t.Errorf("Provided = %v", o.Env.Provided)
	}
}

func TestAutoInstallInstallsEveryMissingCommandAtOnce(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("jq", "curl", "jq")
	c.results["a:latest+jq+curl"] = backend.Result{Stdout: "ok\n"}
	c.provides["a"] = []string{"jq", "curl"}

	rep := run(t, newEngine(host, c), Options{Command: "x", AutoInstall: true}, distros("a"))
	if rep.Outcomes[0].Status != Pass {
		t.Fatalf("status %s, want PASS", rep.Outcomes[0].Status)
	}
	if calls := c.provided(); len(calls) != 1 || !slices.Equal(calls[0].cmds, []string{"jq", "curl"}) {
		t.Fatalf("Provide calls = %+v, want one with jq and curl", calls)
	}
}

func TestAutoInstallRoundsAreBounded(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	image := "a:latest"
	var all []string
	for i := range MaxInstallRounds + 2 {
		cmd := fmt.Sprintf("tool%d", i)
		all = append(all, cmd)
		c.results[image] = notFoundResult(cmd)
		image += "+" + cmd
	}
	c.provides["a"] = all

	rep := run(t, newEngine(host, c), Options{Command: "x", AutoInstall: true}, distros("a"))
	if n := len(c.provided()); n != MaxInstallRounds {
		t.Errorf("Provide calls = %d, want at most %d", n, MaxInstallRounds)
	}
	if rep.Outcomes[0].Status != Fail {
		t.Errorf("status %s, want FAIL", rep.Outcomes[0].Status)
	}
}

func TestAutoInstallDoesNotRetryCommands(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("jq", "nopkg")
	c.results["a:latest+jq+nopkg"] = notFoundResult("nopkg")
	c.provides["a"] = []string{"jq"}

	run(t, newEngine(host, c), Options{Command: "x", AutoInstall: true}, distros("a"))
	if calls := c.provided(); len(calls) != 1 {
		t.Errorf("Provide calls = %+v, want one: nopkg was already tried", calls)
	}
}

func TestAutoInstallIgnoresBashisms(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = backend.Result{Stderr: "sh: 1: [[: not found\nsh: 1: source: not found\n"}

	run(t, newEngine(host, c), Options{Command: "x", AutoInstall: true}, distros("a"))
	if calls := c.provided(); len(calls) != 0 {
		t.Errorf("Provide called for shell keywords: %+v", calls)
	}
}

func TestAutoInstallPassesRebuild(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("jq")
	run(t, newEngine(host, c), Options{Command: "jq", AutoInstall: true, NoCache: true}, distros("a"))
	if calls := c.provided(); len(calls) != 1 || !calls[0].rebuild {
		t.Errorf("Provide calls = %+v, want rebuild with NoCache", calls)
	}
}

func TestAutoInstallAfterCachedResult(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("jq")
	c.results["a:latest+jq"] = backend.Result{Stdout: "ok\n"}
	c.provides["a"] = []string{"jq"}
	e := cachedEngine(t, host, c)
	opts := Options{Command: "jq", AutoInstall: true}

	run(t, e, opts, distros("a"))
	rep := run(t, e, opts, distros("a"))
	o := rep.Outcomes[0]
	if o.Status != Pass || !o.Cached {
		t.Errorf("second run: status %s cached %v, want a cached PASS", o.Status, o.Cached)
	}
	if c.runCount() != 2 {
		t.Errorf("runs = %d, want 2: both results come from the cache the second time", c.runCount())
	}
}

func TestAutoInstallProgressStage(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("jq")
	c.results["a:latest+jq"] = backend.Result{Stdout: "ok\n"}
	c.provides["a"] = []string{"jq"}
	var stages []Stage
	e := newEngine(host, c)
	e.Progress = func(ev Event) { stages = append(stages, ev.Stage) }

	run(t, e, Options{Command: "jq", AutoInstall: true}, distros("a"))
	want := []Stage{StagePreparing, StageRunning, StageInstalling, StageRunning, StageDone}
	if !slices.Equal(stages, want) {
		t.Errorf("stages = %v, want %v", stages, want)
	}
}

func TestAutoInstallCancelled(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("jq")
	ctx, cancel := context.WithCancel(context.Background())
	c.provideErr["a"] = context.Canceled
	e := newEngine(host, c)
	e.Progress = func(ev Event) {
		if ev.Stage == StageInstalling {
			cancel()
		}
	}
	if _, err := e.Run(ctx, Options{Command: "jq", AutoInstall: true}, distros("a")); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

// --- host tools ---

// fakeHostTools resolves the commands in hashes to /host/bin/<name>.
type fakeHostTools struct {
	mu           sync.Mutex
	hashes       map[string]string // command -> content hash
	interpreters map[string]string // command -> shebang interpreter
	calls        []string
}

func (h *fakeHostTools) resolve(name string) (backend.Mount, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, name)
	hash, ok := h.hashes[name]
	if !ok {
		return backend.Mount{}, false
	}
	return backend.Mount{Source: "/host/bin/" + name, Target: backend.HostToolTarget + "/" + name, Hash: hash, Interpreter: h.interpreters[name]}, true
}

func (h *fakeHostTools) called() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.calls)
}

func hostToolEngine(host *fakeHost, c *fakeContainers, tools map[string]string) (*Engine, *fakeHostTools) {
	ht := &fakeHostTools{hashes: tools}
	e := newEngine(host, c)
	e.HostTool = ht.resolve
	return e, ht
}

func toolMount(name, hash string) backend.Mount {
	return backend.Mount{Source: "/host/bin/" + name, Target: "/usr/local/bin/" + name, Hash: hash}
}

func TestHostToolMountedInsteadOfInstalled(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "1.0\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("mytool")
	c.results["a:latest@mytool"] = backend.Result{Stdout: "1.0\n"}
	c.provides["a"] = []string{"mytool"} // a package exists, but the host's version wins
	e, ht := hostToolEngine(host, c, map[string]string{"mytool": "h1"})

	rep := run(t, e, Options{Command: "mytool -V", AutoInstall: true}, distros("a"))
	o := rep.Outcomes[0]
	if o.Status != Pass {
		t.Fatalf("status %s, want PASS after mounting mytool: %+v", o.Status, o)
	}
	if !slices.Equal(ht.called(), []string{"mytool"}) {
		t.Errorf("HostTool calls = %v, want [mytool]", ht.called())
	}
	if calls := c.provided(); len(calls) != 0 {
		t.Errorf("Provide called for a mounted command: %+v", calls)
	}
	want := []backend.Mount{toolMount("mytool", "h1")}
	if got := c.lastRun(); got.Image != "a:latest" || !reflect.DeepEqual(got.Mounts, want) {
		t.Errorf("re-run spec = %+v, want image a:latest with mounts %+v", got, want)
	}
	if !reflect.DeepEqual(o.Env.Mounts, want) || len(o.Env.Provided) != 0 {
		t.Errorf("Env = %+v, want mounts %+v and nothing provided", o.Env, want)
	}
	if c.runCount() != 2 {
		t.Errorf("runs = %d, want 2", c.runCount())
	}
	if !hasNote(o, explain.KindInstalled, "mounted from the host") || !hasNote(o, explain.KindInstalled, "`mytool`") {
		t.Errorf("notes = %+v, want a note saying mytool was mounted from the host", o.Notes)
	}
	if hasNote(o, explain.KindInstalled, "installed automatically") {
		t.Errorf("notes = %+v: nothing was installed", o.Notes)
	}
}

func TestHostToolNotResolvedFallsBackToProvide(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "1.7\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("jq")
	c.results["a:latest+jq"] = backend.Result{Stdout: "1.7\n"}
	c.provides["a"] = []string{"jq"}
	e, ht := hostToolEngine(host, c, nil)

	rep := run(t, e, Options{Command: "jq --version", AutoInstall: true}, distros("a"))
	o := rep.Outcomes[0]
	if o.Status != Pass {
		t.Fatalf("status %s, want PASS", o.Status)
	}
	if !slices.Equal(ht.called(), []string{"jq"}) {
		t.Errorf("HostTool calls = %v, want [jq]", ht.called())
	}
	if calls := c.provided(); len(calls) != 1 || !slices.Equal(calls[0].cmds, []string{"jq"}) {
		t.Errorf("Provide calls = %+v, want one for jq", calls)
	}
	if len(o.Env.Mounts) != 0 || len(c.lastRun().Mounts) != 0 {
		t.Errorf("nothing should be mounted: env %+v, last run %+v", o.Env, c.lastRun())
	}
	if hasNote(o, explain.KindInstalled, "mounted") {
		t.Errorf("notes = %+v: nothing was mounted", o.Notes)
	}
}

func TestHostToolAndProvideInSameRound(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("mytool", "jq")
	c.results["a:latest+jq@mytool"] = backend.Result{Stdout: "ok\n"}
	c.provides["a"] = []string{"jq", "mytool"}
	e, _ := hostToolEngine(host, c, map[string]string{"mytool": "h1"})

	rep := run(t, e, Options{Command: "mytool | jq", AutoInstall: true}, distros("a"))
	o := rep.Outcomes[0]
	if o.Status != Pass {
		t.Fatalf("status %s, want PASS: %+v", o.Status, o)
	}
	calls := c.provided()
	if len(calls) != 1 || !slices.Equal(calls[0].cmds, []string{"jq"}) || calls[0].image != "a:latest" {
		t.Fatalf("Provide calls = %+v, want one for jq only", calls)
	}
	if c.runCount() != 2 {
		t.Errorf("runs = %d, want 2: mounting and installing happen in one round", c.runCount())
	}
	if !slices.Equal(o.Env.Provided, []string{"jq"}) || !reflect.DeepEqual(o.Env.Mounts, []backend.Mount{toolMount("mytool", "h1")}) {
		t.Errorf("Env = %+v", o.Env)
	}
	if !hasNote(o, explain.KindInstalled, "mounted from the host because they were missing here: `mytool`") {
		t.Errorf("notes = %+v, want a mounted note for mytool", o.Notes)
	}
	if !hasNote(o, explain.KindInstalled, "installed automatically because they were missing here: `jq`") {
		t.Errorf("notes = %+v, want an installed note for jq", o.Notes)
	}
}

func TestHostToolThenInstallsItsInterpreter(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("mytool")
	c.results["a:latest@mytool"] = backend.Result{Stderr: "env: can't execute 'bash': No such file or directory\n", ExitCode: 127}
	c.results["a:latest+bash@mytool"] = backend.Result{Stdout: "ok\n"}
	c.provides["a"] = []string{"bash"}
	e, ht := hostToolEngine(host, c, map[string]string{"mytool": "h1"})

	rep := run(t, e, Options{Command: "mytool", AutoInstall: true}, distros("a"))
	o := rep.Outcomes[0]
	if o.Status != Pass {
		t.Fatalf("status %s, want PASS: %+v", o.Status, o)
	}
	if !slices.Equal(ht.called(), []string{"mytool", "bash"}) {
		t.Errorf("HostTool calls = %v, want [mytool bash]", ht.called())
	}
	calls := c.provided()
	if len(calls) != 1 || !slices.Equal(calls[0].cmds, []string{"bash"}) {
		t.Fatalf("Provide calls = %+v, want one for bash", calls)
	}
	if !reflect.DeepEqual(o.Env.Mounts, []backend.Mount{toolMount("mytool", "h1")}) {
		t.Errorf("mounts must survive Provide: %+v", o.Env)
	}
}

func TestHostToolNilKeepsBehaviour(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("mytool")
	c.results["a:latest@mytool"] = backend.Result{Stdout: "ok\n"}

	rep := run(t, newEngine(host, c), Options{Command: "mytool", AutoInstall: true}, distros("a"))
	o := rep.Outcomes[0]
	if o.Status != Fail || len(o.Env.Mounts) != 0 {
		t.Errorf("outcome = %+v, want FAIL without mounts", o)
	}
	if len(c.provided()) != 1 || c.runCount() != 1 {
		t.Errorf("Provide calls = %+v, runs = %d; want one attempt and one run", c.provided(), c.runCount())
	}
}

func TestHostToolNeedsAutoInstall(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("mytool")
	c.results["a:latest@mytool"] = backend.Result{Stdout: "ok\n"}
	e, ht := hostToolEngine(host, c, map[string]string{"mytool": "h1"})

	rep := run(t, e, Options{Command: "mytool"}, distros("a"))
	if rep.Outcomes[0].Status != Fail {
		t.Errorf("status %s, want FAIL", rep.Outcomes[0].Status)
	}
	if len(ht.called()) != 0 || c.runCount() != 1 {
		t.Errorf("HostTool calls = %v, runs = %d; want no mounting without AutoInstall", ht.called(), c.runCount())
	}
}

func TestHostToolThatDoesNotHelpStops(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("mytool")
	c.results["a:latest@mytool"] = notFoundResult("mytool")
	e, ht := hostToolEngine(host, c, map[string]string{"mytool": "h1"})

	rep := run(t, e, Options{Command: "mytool", AutoInstall: true}, distros("a"))
	o := rep.Outcomes[0]
	if o.Status != Fail {
		t.Errorf("status %s, want FAIL", o.Status)
	}
	if len(ht.called()) != 1 || len(c.provided()) != 0 || c.runCount() != 2 {
		t.Errorf("HostTool calls = %v, Provide calls = %+v, runs = %d; want one mount, no install and two runs",
			ht.called(), c.provided(), c.runCount())
	}
	for _, r := range o.Reasons {
		if strings.Contains(r.Message, "no package") {
			t.Errorf("reason %q: no package was looked for", r.Message)
		}
	}
}

func TestHostToolProgressStages(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("mytool")
	c.results["a:latest@mytool"] = backend.Result{Stdout: "ok\n"}
	e, _ := hostToolEngine(host, c, map[string]string{"mytool": "h1"})
	var stages []Stage
	e.Progress = func(ev Event) { stages = append(stages, ev.Stage) }

	run(t, e, Options{Command: "mytool", AutoInstall: true}, distros("a"))
	want := []Stage{StagePreparing, StageRunning, StageRunning, StageDone}
	if !slices.Equal(stages, want) {
		t.Errorf("stages = %v, want %v: mounting builds nothing", stages, want)
	}
}

func TestHostToolHashInCacheKey(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("mytool")
	c.results["a:latest@mytool"] = backend.Result{Stdout: "ok\n"}
	store, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e, ht := hostToolEngine(host, c, map[string]string{"mytool": "h1"})
	e.Cache = store
	opts := Options{Command: "mytool", AutoInstall: true}

	run(t, e, opts, distros("a"))
	rep := run(t, e, opts, distros("a"))
	if o := rep.Outcomes[0]; !o.Cached || o.Status != Pass || c.runCount() != 2 {
		t.Fatalf("same tool: cached %v status %s runs %d; want a cached PASS after 2 runs", o.Cached, o.Status, c.runCount())
	}

	ht.mu.Lock()
	ht.hashes["mytool"] = "h2"
	ht.mu.Unlock()
	rep = run(t, e, opts, distros("a"))
	if o := rep.Outcomes[0]; o.Cached || c.runCount() != 3 {
		t.Errorf("edited tool: cached %v runs %d; want a fresh run", o.Cached, c.runCount())
	}
	if got := c.lastRun().Mounts; len(got) != 1 || got[0].Hash != "h2" {
		t.Errorf("last run mounts = %+v, want the new hash", got)
	}
}

func TestCacheKeyIncludesMountTarget(t *testing.T) {
	env := backend.Environment{ImageID: "sha256:a"}
	opts := Options{Command: "x"}
	a, b := env, env
	a.Mounts = []backend.Mount{{Source: "/h/x", Target: "/usr/local/bin/x", Hash: "1"}}
	b.Mounts = []backend.Mount{{Source: "/h/x", Target: "/usr/local/bin/y", Hash: "1"}}
	keys := map[string]bool{cacheKey(opts, env): true, cacheKey(opts, a): true, cacheKey(opts, b): true}
	if len(keys) != 3 {
		t.Errorf("mount targets must be part of the cache key")
	}
}

// A shell that cannot find a mounted script's interpreter reports the script
// itself as not found (busybox ash, dash): the interpreter is what is missing.
func TestHostToolMissingInterpreterIsInstalled(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("mytool")
	c.results["a:latest@mytool"] = notFoundResult("mytool")
	c.results["a:latest+bash@mytool"] = backend.Result{Stdout: "ok\n"}
	c.provides["a"] = []string{"bash"}
	e, ht := hostToolEngine(host, c, map[string]string{"mytool": "h1"})
	ht.interpreters = map[string]string{"mytool": "bash"}

	rep := run(t, e, Options{Command: "mytool", AutoInstall: true}, distros("a"))
	o := rep.Outcomes[0]
	if o.Status != Pass {
		t.Fatalf("status %s, want PASS after installing bash: %+v", o.Status, o)
	}
	if !slices.Equal(ht.called(), []string{"mytool", "bash"}) {
		t.Errorf("HostTool calls = %v, want [mytool bash]", ht.called())
	}
	if calls := c.provided(); len(calls) != 1 || !slices.Equal(calls[0].cmds, []string{"bash"}) {
		t.Fatalf("Provide calls = %+v, want one for bash", calls)
	}
	if !hasNote(o, explain.KindInstalled, "installed automatically because they were missing here: `bash`") {
		t.Errorf("notes = %+v, want bash installed", o.Notes)
	}
}

func TestHostToolMissingInterpreterIsExplained(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("mytool")
	c.results["a:latest@mytool"] = notFoundResult("mytool")
	e, ht := hostToolEngine(host, c, map[string]string{"mytool": "h1"})
	ht.interpreters = map[string]string{"mytool": "bash"}

	rep := run(t, e, Options{Command: "mytool", AutoInstall: true}, distros("a"))
	o := rep.Outcomes[0]
	if o.Status != Fail {
		t.Fatalf("status %s, want FAIL", o.Status)
	}
	found := false
	for _, r := range o.Reasons {
		if r.Kind == explain.KindMissingCommand && strings.Contains(r.Message, "`mytool`") {
			t.Errorf("reason %q: mytool is mounted, its interpreter is missing", r.Message)
		}
		if r.Kind == explain.KindMissingInterpreter && strings.Contains(r.Message, "`mytool`") && strings.Contains(r.Message, "`bash`") {
			found = true
		}
	}
	if !found {
		t.Errorf("reasons = %+v, want a missing-interpreter reason naming mytool and bash", o.Reasons)
	}
}

// --- dependencies found before running ---

func TestDependenciesInstalledBeforeFirstRun(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "1.7\n"}}
	c := newFakeContainers()
	c.present["a:latest"] = []string{"sh"}
	c.results["a:latest"] = notFoundResult("jq")
	c.results["a:latest+jq"] = backend.Result{Stdout: "1.7\n"}
	c.provides["a"] = []string{"jq"}

	opts := Options{Command: "./run.sh", AutoInstall: true, Dependencies: []string{"sh", "jq"}}
	o := run(t, newEngine(host, c), opts, distros("a")).Outcomes[0]
	if o.Status != Pass {
		t.Fatalf("status %s, want PASS: %+v", o.Status, o)
	}
	calls := c.provided()
	if len(calls) != 1 || !slices.Equal(calls[0].cmds, []string{"jq"}) || calls[0].image != "a:latest" {
		t.Fatalf("Provide calls = %+v, want one for jq on a:latest", calls)
	}
	if c.runCount() != 1 || c.lastRun().Image != "a:latest+jq" {
		t.Errorf("runs = %d (last %+v), want a single run on a:latest+jq", c.runCount(), c.lastRun())
	}
	if !hasNote(o, explain.KindInstalled, "installed automatically") || !hasNote(o, explain.KindInstalled, "`jq`") {
		t.Errorf("notes = %+v, want an installed note naming jq", o.Notes)
	}
}

func TestDependenciesProbeSpec(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = backend.Result{Stdout: "ok\n"}
	c.present["a:latest"] = []string{"jq", "curl"}

	opts := Options{Command: "x", Dir: "/w", Timeout: time.Minute, AutoInstall: true, Dependencies: []string{"jq", "curl"}}
	run(t, newEngine(host, c), opts, distros("a"))
	probes := c.probeSpecs()
	if len(probes) != 1 {
		t.Fatalf("probes = %d, want 1", len(probes))
	}
	p := probes[0]
	if p.Image != "a:latest" || p.Dir != "" || len(p.Mounts) != 0 || p.Timeout <= 0 {
		t.Errorf("probe spec = %+v, want image a:latest, no dir, no mounts and a timeout", p)
	}
	for _, name := range []string{"'jq'", "'curl'", "command -v"} {
		if !strings.Contains(p.Command, name) {
			t.Errorf("probe command %q does not contain %s", p.Command, name)
		}
	}
}

func TestDependenciesPresentAreNotInstalled(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = backend.Result{Stdout: "ok\n"}
	c.present["a:latest"] = []string{"jq"}

	opts := Options{Command: "jq", AutoInstall: true, Dependencies: []string{"jq"}}
	o := run(t, newEngine(host, c), opts, distros("a")).Outcomes[0]
	if o.Status != Pass || len(c.provided()) != 0 || c.runCount() != 1 {
		t.Errorf("status %s, Provide calls %+v, runs %d: want PASS, none and 1", o.Status, c.provided(), c.runCount())
	}
	if len(o.Notes) != 0 {
		t.Errorf("notes = %+v, want none", o.Notes)
	}
}

func TestDependenciesMountedFromHost(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "1.0\n"}}
	c := newFakeContainers()
	c.results["a:latest@mytool"] = backend.Result{Stdout: "1.0\n"}
	c.provides["a"] = []string{"mytool"}
	e, ht := hostToolEngine(host, c, map[string]string{"mytool": "h1"})

	opts := Options{Command: "./x.sh", AutoInstall: true, Dependencies: []string{"mytool"}}
	o := run(t, e, opts, distros("a")).Outcomes[0]
	if o.Status != Pass {
		t.Fatalf("status %s, want PASS: %+v", o.Status, o)
	}
	if !slices.Equal(ht.called(), []string{"mytool"}) || len(c.provided()) != 0 {
		t.Errorf("HostTool calls %v, Provide calls %+v: want [mytool] and none", ht.called(), c.provided())
	}
	if c.runCount() != 1 || !reflect.DeepEqual(c.lastRun().Mounts, []backend.Mount{toolMount("mytool", "h1")}) {
		t.Errorf("runs = %d, last %+v: want one run with mytool mounted", c.runCount(), c.lastRun())
	}
	if !hasNote(o, explain.KindInstalled, "mounted from the host") {
		t.Errorf("notes = %+v, want a mounted note", o.Notes)
	}
}

func TestDependenciesNeedAutoInstall(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = backend.Result{Stdout: "ok\n"}
	c.provides["a"] = []string{"jq"}

	run(t, newEngine(host, c), Options{Command: "jq", Dependencies: []string{"jq"}}, distros("a"))
	if len(c.probeSpecs()) != 0 || len(c.provided()) != 0 {
		t.Errorf("probes %d, Provide calls %+v: want none without AutoInstall", len(c.probeSpecs()), c.provided())
	}
}

func TestNoDependenciesNoProbe(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = backend.Result{Stdout: "ok\n"}
	run(t, newEngine(host, c), Options{Command: "x", AutoInstall: true}, distros("a"))
	if n := len(c.probeSpecs()); n != 0 {
		t.Errorf("probes = %d, want none", n)
	}
}

func TestDependenciesNotRetriedAfterFailedProvide(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "1.7\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = notFoundResult("jq")
	c.results["a:latest+jq"] = notFoundResult("jq") // no package provides jq

	opts := Options{Command: "jq", AutoInstall: true, Dependencies: []string{"jq"}}
	o := run(t, newEngine(host, c), opts, distros("a")).Outcomes[0]
	if o.Status != Fail {
		t.Fatalf("status %s, want FAIL", o.Status)
	}
	if calls := c.provided(); len(calls) != 1 {
		t.Errorf("Provide calls = %+v, want jq tried once", calls)
	}
	found := false
	for _, r := range o.Reasons {
		if strings.Contains(r.Message, "no package providing it was found") {
			found = true
		}
	}
	if !found {
		t.Errorf("reasons = %+v, want jq explained as not provided", o.Reasons)
	}
}

func TestDependenciesFoundLaterStillInstalled(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest+jq"] = notFoundResult("yq")
	c.results["a:latest+jq+yq"] = backend.Result{Stdout: "ok\n"}
	c.provides["a"] = []string{"jq", "yq"}

	opts := Options{Command: "x", AutoInstall: true, Dependencies: []string{"jq"}}
	o := run(t, newEngine(host, c), opts, distros("a")).Outcomes[0]
	if o.Status != Pass {
		t.Fatalf("status %s, want PASS after installing jq first and yq in a round: %+v", o.Status, o)
	}
	calls := c.provided()
	if len(calls) != 2 || !slices.Equal(calls[0].cmds, []string{"jq"}) || !slices.Equal(calls[1].cmds, []string{"yq"}) {
		t.Errorf("Provide calls = %+v, want jq then yq", calls)
	}
}

func TestDependenciesProbeFailureFallsBack(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "1.7\n"}}
	c := newFakeContainers()
	c.probeFail["a:latest"] = true
	c.results["a:latest"] = notFoundResult("jq")
	c.results["a:latest+jq"] = backend.Result{Stdout: "1.7\n"}
	c.provides["a"] = []string{"jq"}

	opts := Options{Command: "jq", AutoInstall: true, Dependencies: []string{"jq"}}
	o := run(t, newEngine(host, c), opts, distros("a")).Outcomes[0]
	if o.Status != Pass {
		t.Fatalf("status %s, want PASS through the rounds: %+v", o.Status, o)
	}
	if c.runCount() != 2 {
		t.Errorf("runs = %d, want 2: the failed probe must not install anything", c.runCount())
	}
}

func TestDependenciesInvalidNamesNotProbed(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = backend.Result{Stdout: "ok\n"}
	c.present["a:latest"] = []string{"jq"}
	e := newEngine(host, c)
	opts := Options{Command: "x", AutoInstall: true, Dependencies: []string{"jq", "bad name", "-x"}}
	run(t, e, opts, distros("a"))
	if calls := c.provided(); len(calls) != 0 {
		t.Errorf("Provide calls = %+v, want none: invalid names are not probed", calls)
	}
	if p := c.probeSpecs(); len(p) != 1 || strings.Contains(p[0].Command, "bad name") || strings.Contains(p[0].Command, "'-x'") {
		t.Errorf("probes = %+v, want one probing only jq", p)
	}
}

func TestDependenciesProbeCached(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "1.7\n"}}
	c := newFakeContainers()
	c.results["a:latest+jq"] = backend.Result{Stdout: "1.7\n"}
	c.provides["a"] = []string{"jq"}
	store, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(host, c)
	e.Cache = store

	opts := Options{Command: "jq", AutoInstall: true, Dependencies: []string{"jq"}}
	run(t, e, opts, distros("a"))
	o := run(t, e, opts, distros("a")).Outcomes[0]
	if n := len(c.probeSpecs()); n != 1 {
		t.Errorf("probes = %d, want 1: the second run uses the cached probe", n)
	}
	if o.Status != Pass || !o.Cached {
		t.Errorf("second outcome = %+v, want a cached PASS", o)
	}

	opts.NoCache = true
	run(t, e, opts, distros("a"))
	if n := len(c.probeSpecs()); n != 2 {
		t.Errorf("probes = %d, want 2: NoCache probes again", n)
	}
}

func TestDependenciesProbeCacheKeyedByDependencies(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.results["a:latest"] = backend.Result{Stdout: "ok\n"}
	c.present["a:latest"] = []string{"jq", "yq"}
	store, err := cache.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	e := newEngine(host, c)
	e.Cache = store

	run(t, e, Options{Command: "x", AutoInstall: true, Dependencies: []string{"jq"}}, distros("a"))
	run(t, e, Options{Command: "x", AutoInstall: true, Dependencies: []string{"jq", "yq"}}, distros("a"))
	if n := len(c.probeSpecs()); n != 2 {
		t.Errorf("probes = %d, want 2 for different dependencies", n)
	}
}

func TestDependenciesProbeCancelled(t *testing.T) {
	host := &fakeHost{result: backend.Result{Stdout: "ok\n"}}
	c := newFakeContainers()
	c.provides["a"] = []string{"jq"}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := newEngine(host, c).Run(ctx, Options{Command: "x", AutoInstall: true, Dependencies: []string{"jq"}}, distros("a"))
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if calls := c.provided(); len(calls) != 0 {
		t.Errorf("Provide calls = %+v after cancellation", calls)
	}
}
