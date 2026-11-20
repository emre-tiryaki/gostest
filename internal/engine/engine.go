// Package engine orchestrates a gostest run: the reference run on the host,
// the parallel runs in every target distribution, caching, comparison and
// explanation.
package engine

import (
	"context"
	"errors"
	"fmt"
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
	// distribution but not on the host, and runs the command again.
	AutoInstall bool
}

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

	cacheable := e.Cache != nil && (opts.Dir == "" || opts.Fingerprint != "")
	key := cacheKey(opts, env)
	if cacheable && !opts.NoCache {
		var cached backend.Result
		if ok, _ := e.Cache.Get(key, &cached); ok {
			o.Result, o.Cached = cached, true
		}
	}

	if o.Cached {
		e.emit(Event{Distro: d.Name, Stage: StageCached})
	} else {
		e.emit(Event{Distro: d.Name, Stage: StageRunning})
		s := spec
		s.Image = env.Image
		res, err := e.Containers.Run(ctx, s)
		if err != nil {
			if ctx.Err() != nil {
				return o
			}
			return fail(err)
		}
		o.Result = res
		if cacheable && !res.TimedOut {
			// A failed write only costs a re-run next time.
			_ = e.Cache.Put(key, res)
		}
	}

	select {
	case <-ref.done:
	case <-ctx.Done():
		return o
	}
	if ref.err != nil {
		return o
	}
	if opts.AutoInstall {
		if missing := explain.MissingCommands(ref.result, o.Result); len(missing) > 0 {
			e.emit(Event{Distro: d.Name, Stage: StageInstalling})
			if env, err := e.Containers.Provide(ctx, o.Env, missing, opts.NoCache); err == nil {
				o.Env = env
				s := spec
				s.Image = env.Image
				if res, err := e.Containers.Run(ctx, s); err == nil {
					o.Result = res
				}
			}
		}
	}
	o.Comparison = compare.Compare(ref.result, o.Result, opts.Compare)
	ec := explain.Context{
		Distro:     d.Name,
		Shell:      env.Shell,
		RefShell:   e.HostShell,
		HostLocale: e.HostLocale,
	}
	if o.Comparison.Match() {
		o.Status = Pass
	} else {
		o.Status = Fail
		o.Reasons = explain.Explain(ref.result, o.Result, o.Comparison, ec)
	}
	o.Notes = explain.Notes(o.Comparison, ec)
	e.emit(Event{Distro: d.Name, Stage: StageDone, Status: o.Status})
	return o
}

// cacheKey identifies a container result: the exact image, command,
// working directory (path and contents) and timeout.
func cacheKey(opts Options, env backend.Environment) string {
	return cache.Key("result", env.ImageID, opts.Command, opts.Dir, opts.Fingerprint, opts.Timeout.String())
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
