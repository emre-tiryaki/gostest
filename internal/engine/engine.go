// Package engine orchestrates a gostest run: the reference run on the host,
// the parallel runs in every target distribution, caching, comparison and
// explanation.
package engine

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/emre-tiryaki/gostest/internal/backend"
	"github.com/emre-tiryaki/gostest/internal/cache"
	"github.com/emre-tiryaki/gostest/internal/compare"
	"github.com/emre-tiryaki/gostest/internal/distro"
	"github.com/emre-tiryaki/gostest/internal/explain"
)

// Status is the verdict for one distribution.
type Status string

// Verdicts.
const (
	Pass  Status = "PASS"
	Fail  Status = "FAIL"
	Error Status = "ERROR"
)

// Containers prepares images and runs commands in them.
type Containers interface {
	Prepare(ctx context.Context, d distro.Distro, pkgs []string, rebuild bool) (backend.Environment, error)
	// Provide installs packages providing cmds on top of env and returns
	// the new environment; its Provided field lists the commands that
	// are available in it.
	Provide(ctx context.Context, env backend.Environment, cmds []string, rebuild bool) (backend.Environment, error)
	backend.Runner
}

// Options configures a run.
type Options struct {
	// Command is the shell command to test.
	Command string
	// Dir is the working directory, mounted read-only into containers.
	Dir string
	// Fingerprint identifies the contents of Dir. When Dir is set, results
	// are only cached if Fingerprint is set too.
	Fingerprint string
	// Timeout bounds each run; zero means no limit.
	Timeout time.Duration
	// Concurrency limits how many distributions are processed at once;
	// zero or less means all of them.
	Concurrency int
	// NoCache ignores cached results and rebuilds images. Fresh results
	// are still written to the cache.
	NoCache bool
	// Compare tunes the comparison with the reference.
	Compare compare.Options
	// Packages are installed into the distributions before running.
	Packages distro.Packages
	// AutoInstall installs packages for commands that are missing in a
	// distribution but not on the host, and runs the command again, for
	// at most MaxInstallRounds rounds.
	AutoInstall bool
	// Dependencies are commands the command is known to need, e.g. found
	// by scanning it (see deps.Scan). With AutoInstall, those a
	// distribution lacks are mounted or installed before the first run,
	// so one round finds them all.
	Dependencies []string
}

// probeMarker starts the script that finds which Dependencies a
// distribution lacks.
const probeMarker = "# gostest: dependency probe\n"

// MaxInstallRounds bounds how often missing commands are installed for one
// distribution: a command found in one round may need others that only
// show up once it runs.
const MaxInstallRounds = 3

// Reference is the host run every distribution is compared with.
type Reference struct {
	Shell  string         `json:"shell"`
	Result backend.Result `json:"result"`
}

// Outcome is the result for one distribution.
type Outcome struct {
	Distro     distro.Distro       `json:"distro"`
	Env        backend.Environment `json:"environment"`
	Status     Status              `json:"status"`
	Result     backend.Result      `json:"result"`
	Cached     bool                `json:"cached,omitempty"`
	Error      string              `json:"error,omitempty"`
	Comparison compare.Comparison  `json:"comparison"`
	Reasons    []explain.Reason    `json:"reasons,omitempty"`
	// Notes are remarks that do not affect the status.
	Notes []explain.Reason `json:"notes,omitempty"`
}

// Report is the outcome of a whole run.
type Report struct {
	Command   string    `json:"command"`
	Reference Reference `json:"reference"`
	Outcomes  []Outcome `json:"outcomes"`
}

// Stage is a step of a distribution's run, reported through Progress.
type Stage string

// Stages.
const (
	StagePreparing  Stage = "preparing"
	StageRunning    Stage = "running"
	StageInstalling Stage = "installing"
	StageCached     Stage = "cached"
	StageDone       Stage = "done"
)

// Event reports progress for one distribution.
type Event struct {
	Distro string
	Stage  Stage
	// Status is set for StageDone.
	Status Status
}

// Engine runs commands on the host and in containers.
type Engine struct {
	// Host produces the reference result.
	Host backend.Runner
	// HostShell is what /bin/sh is on the host.
	HostShell string
	// HostLocale is the collation locale of the host reference run.
	HostLocale string
	// Containers prepares and runs the distributions.
	Containers Containers
	// Cache stores container results; nil disables caching.
	Cache *cache.Store
	// HostTool resolves a command missing in a distribution to a host file
	// that can be mounted into its containers instead of installing a
	// package; nil disables mounting.
	HostTool func(name string) (backend.Mount, bool)
	// Progress, if set, is called as distributions advance. Calls are
	// serialised, so it does not need to be safe for concurrent use.
	Progress func(Event)

	progressMu sync.Mutex
}

// reference is the host result, shared with every target goroutine.
type reference struct {
	done   chan struct{}
	result backend.Result
	err    error
}

// Run executes opts.Command on the host and in every distribution. The
// host run (the reference) and the distributions run concurrently, at most
// opts.Concurrency distributions at a time. Each distribution's result is
// compared with the reference and, when they differ, explained. Failures
// to prepare or run a distribution are reported as ERROR outcomes; a
// failing reference run or the end of ctx aborts the whole run.
func (e *Engine) Run(ctx context.Context, opts Options, distros []distro.Distro) (Report, error) {
	switch {
	case strings.TrimSpace(opts.Command) == "":
		return Report{}, errors.New("no command given")
	case len(distros) == 0:
		return Report{}, errors.New("no distros given")
	case e.Host == nil:
		return Report{}, errors.New("engine has no host backend")
	case e.Containers == nil:
		return Report{}, errors.New("engine has no container backend")
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	spec := backend.Spec{Command: opts.Command, Dir: opts.Dir, Timeout: opts.Timeout}

	ref := &reference{done: make(chan struct{})}
	go func() {
		ref.result, ref.err = e.Host.Run(runCtx, spec)
		close(ref.done)
	}()

	limit := opts.Concurrency
	if limit <= 0 || limit > len(distros) {
		limit = len(distros)
	}
	sem := make(chan struct{}, limit)
	outcomes := make([]Outcome, len(distros))
	var wg sync.WaitGroup
	for i, d := range distros {
		wg.Go(func() {
			select {
			case sem <- struct{}{}:
			case <-runCtx.Done():
				return
			}
			defer func() { <-sem }()
			outcomes[i] = e.runTarget(runCtx, opts, spec, d, ref)
		})
	}

	<-ref.done
	if ref.err != nil {
		cancel()
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}
	if ref.err != nil {
		return Report{}, fmt.Errorf("reference run on the host: %w", ref.err)
	}
	return Report{
		Command:   opts.Command,
		Reference: Reference{Shell: e.HostShell, Result: ref.result},
		Outcomes:  outcomes,
	}, nil
}

func (e *Engine) runTarget(ctx context.Context, opts Options, spec backend.Spec, d distro.Distro, ref *reference) Outcome {
	o := Outcome{Distro: d}
	fail := func(err error) Outcome {
		o.Status, o.Error = Error, err.Error()
		e.emit(Event{Distro: d.Name, Stage: StageDone, Status: Error})
		return o
	}

	e.emit(Event{Distro: d.Name, Stage: StagePreparing})
	env, err := e.Containers.Prepare(ctx, d, opts.Packages.For(d), opts.NoCache)
	if err != nil {
		if ctx.Err() != nil {
			return o
		}
		return fail(err)
	}
	o.Env = env

	var st installState
	if opts.AutoInstall && len(opts.Dependencies) > 0 {
		// Known dependencies first, so the rounds below only have to
		// find what the scan missed.
		missing := e.missingDependencies(ctx, opts, o.Env)
		if ctx.Err() != nil {
			return o
		}
		if len(missing) > 0 {
			if _, cancelled := e.install(ctx, opts, &o, missing, &st); cancelled {
				return o
			}
		}
	}

	if err := e.execute(ctx, opts, spec, &o); err != nil {
		if ctx.Err() != nil {
			return o
		}
		return fail(err)
	}

	select {
	case <-ref.done:
	case <-ctx.Done():
		return o
	}
	if ref.err != nil {
		return o
	}

	for round := 0; opts.AutoInstall && round < MaxInstallRounds && st.err == nil; round++ {
		var missing []string
		for _, c := range explain.MissingCommands(ref.result, o.Result) {
			if interp, ok := mountedInterpreter(o.Env.Mounts, c); ok {
				// The mounted script is there: busybox ash and dash
				// report a missing interpreter as the script being
				// not found.
				c = interp
			}
			if !slices.Contains(st.tried, c) && !slices.Contains(missing, c) {
				missing = append(missing, c)
			}
		}
		if len(missing) == 0 {
			break
		}
		changed, cancelled := e.install(ctx, opts, &o, missing, &st)
		if cancelled {
			return o
		}
		if !changed {
			// Nothing new is available: running again would not help.
			break
		}
		if err := e.execute(ctx, opts, spec, &o); err != nil {
			if ctx.Err() != nil {
				return o
			}
			return fail(err)
		}
	}

	o.Comparison = compare.Compare(ref.result, o.Result, opts.Compare)
	ec := explain.Context{
		Distro:     d.Name,
		Shell:      o.Env.Shell,
		RefShell:   e.HostShell,
		HostLocale: e.HostLocale,
	}
	for _, m := range o.Env.Mounts {
		if m.Interpreter != "" {
			if ec.MountedInterpreters == nil {
				ec.MountedInterpreters = map[string]string{}
			}
			ec.MountedInterpreters[path.Base(m.Target)] = m.Interpreter
		}
	}
	for _, c := range st.tried {
		if !slices.Contains(o.Env.Provided, c) && !slices.Contains(st.failed, c) && !slices.Contains(st.mounted, c) {
			ec.NotProvided = append(ec.NotProvided, c)
		}
	}
	if o.Comparison.Match() {
		o.Status = Pass
	} else {
		o.Status = Fail
		o.Reasons = explain.Explain(ref.result, o.Result, o.Comparison, ec)
	}
	o.Notes = append(installNotes(st.mounted, o.Env.Provided, st.failed, st.err), explain.Notes(o.Comparison, ec)...)
	e.emit(Event{Distro: d.Name, Stage: StageDone, Status: o.Status})
	return o
}

// installState is what the automatic installation did for one
// distribution.
type installState struct {
	// tried are the commands it looked for; mounted those mounted from
	// the host; failed those whose installation failed with err.
	tried, mounted, failed []string
	err                    error
}

// install makes the missing commands available in o.Env: host tools are
// mounted (the user tests their own version of a tool, which a packaged
// one would replace) and packages are installed for the others. It
// reports whether o.Env changed, and whether ctx ended.
func (e *Engine) install(ctx context.Context, opts Options, o *Outcome, missing []string, st *installState) (changed, cancelled bool) {
	st.tried = append(st.tried, missing...)
	next := o.Env
	next.Mounts = slices.Clone(o.Env.Mounts)
	var toProvide []string
	for _, c := range missing {
		if e.HostTool != nil {
			if m, ok := e.HostTool(c); ok {
				next.Mounts = append(next.Mounts, m)
				st.mounted = append(st.mounted, c)
				continue
			}
		}
		toProvide = append(toProvide, c)
	}
	changed = len(next.Mounts) > len(o.Env.Mounts)

	if len(toProvide) > 0 {
		e.emit(Event{Distro: o.Distro.Name, Stage: StageInstalling})
		provided, err := e.Containers.Provide(ctx, next, toProvide, opts.NoCache)
		switch {
		case err != nil && ctx.Err() != nil:
			return false, true
		case err != nil:
			// What was mounted is still used.
			st.failed, st.err = toProvide, err
		case len(provided.Provided) > len(next.Provided):
			next, changed = provided, true
		}
	}
	if changed {
		o.Env = next
	}
	return changed, false
}

// probeTimeout bounds the dependency probe.
const probeTimeout = time.Minute

// missingDependencies returns the opts.Dependencies that env lacks, as a
// probe in env reports them (cached by image). A failing probe reports
// nothing: the rounds after the first run still find what is missing.
func (e *Engine) missingDependencies(ctx context.Context, opts Options, env backend.Environment) []string {
	var cmds []string
	for _, c := range opts.Dependencies {
		if distro.ValidCommandName(c) && !slices.Contains(cmds, c) {
			cmds = append(cmds, c)
		}
	}
	if len(cmds) == 0 {
		return nil
	}
	cacheable := e.Cache != nil && env.ImageID != ""
	key := cache.Key(append([]string{"probe", env.ImageID}, cmds...)...)
	if cacheable && !opts.NoCache {
		var cached []string
		if ok, _ := e.Cache.Get(key, &cached); ok {
			return cached
		}
	}
	res, err := e.Containers.Run(ctx, backend.Spec{Command: probeCommand(cmds), Image: env.Image, Timeout: probeTimeout})
	if err != nil || res.ExitCode != 0 || res.TimedOut {
		return nil
	}
	missing := []string{}
	for _, line := range strings.Split(res.Stdout, "\n") {
		line = strings.TrimSpace(line)
		if slices.Contains(cmds, line) && !slices.Contains(missing, line) {
			missing = append(missing, line)
		}
	}
	if cacheable {
		_ = e.Cache.Put(key, missing)
	}
	return missing
}

// probeCommand prints those of cmds that are not on the PATH, one per
// line. The names are valid command names, so quoting them is safe.
func probeCommand(cmds []string) string {
	quoted := make([]string, len(cmds))
	for i, c := range cmds {
		quoted[i] = "'" + c + "'"
	}
	return probeMarker + "for c in " + strings.Join(quoted, " ") + `; do command -v "$c" >/dev/null 2>&1 || echo "$c"; done`
}

// execute sets o.Result to the result of running spec in o.Env, from the
// cache when possible.
func (e *Engine) execute(ctx context.Context, opts Options, spec backend.Spec, o *Outcome) error {
	cacheable := e.Cache != nil && (opts.Dir == "" || opts.Fingerprint != "")
	key := cacheKey(opts, o.Env)
	o.Cached = false
	if cacheable && !opts.NoCache {
		var cached cachedResult
		if ok, _ := e.Cache.Get(key, &cached); ok {
			o.Result, o.Cached = cached.result(), true
			e.emit(Event{Distro: o.Distro.Name, Stage: StageCached})
			return nil
		}
	}

	e.emit(Event{Distro: o.Distro.Name, Stage: StageRunning})
	spec.Image = o.Env.Image
	spec.Mounts = o.Env.Mounts
	res, err := e.Containers.Run(ctx, spec)
	if err != nil {
		return err
	}
	o.Result = res
	if cacheable && !res.TimedOut {
		// A failed write only costs a re-run next time.
		_ = e.Cache.Put(key, newCachedResult(res))
	}
	return nil
}

// cachedResult is how a result is stored: the output as bytes (base64 in
// JSON), because a JSON string would replace bytes that are not valid UTF-8.
type cachedResult struct {
	backend.Result
	Stdout []byte `json:"stdout"`
	Stderr []byte `json:"stderr"`
}

func newCachedResult(r backend.Result) cachedResult {
	return cachedResult{Result: r, Stdout: []byte(r.Stdout), Stderr: []byte(r.Stderr)}
}

func (c cachedResult) result() backend.Result {
	r := c.Result
	r.Stdout, r.Stderr = string(c.Stdout), string(c.Stderr)
	return r
}

// mountedInterpreter returns the interpreter of the host script mounted as
// cmd, if there is one with a usable name.
func mountedInterpreter(mounts []backend.Mount, cmd string) (string, bool) {
	for _, m := range mounts {
		if path.Base(m.Target) == cmd {
			return m.Interpreter, m.Interpreter != "" && distro.ValidCommandName(m.Interpreter)
		}
	}
	return "", false
}

// installNotes describes what the automatic installation did.
func installNotes(mounted, provided, failed []string, err error) []explain.Reason {
	var out []explain.Reason
	if len(mounted) > 0 {
		out = append(out, explain.Reason{Kind: explain.KindInstalled, Message: "mounted from the host because they were missing here: " + commandList(mounted)})
	}
	if len(provided) > 0 {
		out = append(out, explain.Reason{Kind: explain.KindInstalled, Message: "installed automatically because they were missing here: " + commandList(provided)})
	}
	if err != nil {
		// Notes are one line: keep the end of the build output.
		lines := strings.Split(strings.TrimSpace(err.Error()), "\n")
		if len(lines) > 3 {
			lines = append(lines[:1], lines[len(lines)-2:]...)
		}
		msg := strings.Join(lines, " / ")
		out = append(out, explain.Reason{Kind: explain.KindInstalled, Message: fmt.Sprintf("could not install %s automatically: %s", commandList(failed), msg)})
	}
	return out
}

func commandList(cmds []string) string {
	quoted := make([]string, len(cmds))
	for i, c := range cmds {
		quoted[i] = "`" + c + "`"
	}
	return strings.Join(quoted, ", ")
}

// cacheKey identifies a container result: the exact image, command,
// working directory (path and contents), timeout and mounted host tools
// (where and with which contents).
func cacheKey(opts Options, env backend.Environment) string {
	// The first part changes with the layout of cachedResult, so entries
	// stored in another layout are not read.
	parts := []string{"result-v2", env.ImageID, opts.Command, opts.Dir, opts.Fingerprint, opts.Timeout.String()}
	for _, m := range env.Mounts {
		parts = append(parts, "mount", m.Target, m.Hash)
	}
	return cache.Key(parts...)
}

func (e *Engine) emit(ev Event) {
	if e.Progress == nil {
		return
	}
	e.progressMu.Lock()
	defer e.progressMu.Unlock()
	e.Progress(ev)
}

// Counts returns the number of outcomes per status.
func (r Report) Counts() (pass, fail, errs int) {
	for _, o := range r.Outcomes {
		switch o.Status {
		case Pass:
			pass++
		case Fail:
			fail++
		default:
			errs++
		}
	}
	return
}

// OK reports whether every distribution passed.
func (r Report) OK() bool {
	_, f, e := r.Counts()
	return f == 0 && e == 0
}
