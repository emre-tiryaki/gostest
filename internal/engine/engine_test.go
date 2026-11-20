package engine

import (
	"context"
	"errors"
	"fmt"
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

	prepares []prepareCall
	runs     []backend.Spec
	active   int
	peak     int
}

func newFakeContainers() *fakeContainers {
	return &fakeContainers{
		envs:       map[string]backend.Environment{},
		prepareErr: map[string]error{},
		results:    map[string]backend.Result{},
		runErr:     map[string]error{},
		delay:      map[string]time.Duration{},
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

func (c *fakeContainers) Provide(_ context.Context, env backend.Environment, cmds []string, rebuild bool) (backend.Environment, error) {
	return env, nil
}

func (c *fakeContainers) Run(ctx context.Context, spec backend.Spec) (backend.Result, error) {
	c.mu.Lock()
	c.runs = append(c.runs, spec)
	c.active++
	c.peak = max(c.peak, c.active)
	delay, res, err, block := c.delay[spec.Image], c.results[spec.Image], c.runErr[spec.Image], c.block
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
	if len(host.specs) != 1 || host.specs[0] != wantHost {
		t.Errorf("host specs = %+v, want %+v", host.specs, wantHost)
	}
	wantRun := backend.Spec{Command: "ls", Dir: "/work/proj", Timeout: 42 * time.Second, Image: "gostest/a:123"}
	if len(c.runs) != 1 || c.runs[0] != wantRun {
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
