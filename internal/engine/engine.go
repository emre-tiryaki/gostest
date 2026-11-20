// Package engine orchestrates a gostest run: the reference run on the host,
// the parallel runs in every target distribution, caching, comparison and
// explanation.
package engine

import (
	"context"
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
	backend.Runner
}

// Options configures a run.
type Options struct {
	Command     string
	Dir         string
	Fingerprint string
	Timeout     time.Duration
	Concurrency int
	NoCache     bool
	Compare     compare.Options
	Packages    distro.Packages
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
	StagePreparing Stage = "preparing"
	StageRunning   Stage = "running"
	StageCached    Stage = "cached"
	StageDone      Stage = "done"
)

// Event reports progress for one distribution.
type Event struct {
	Distro string
	Stage  Stage
	Status Status
}

// Engine runs commands on the host and in containers.
type Engine struct {
	Host       backend.Runner
	HostShell  string
	Containers Containers
	Cache      *cache.Store
	Progress   func(Event)
}

// Run executes opts.Command on the host and in every distro.
func (e *Engine) Run(ctx context.Context, opts Options, distros []distro.Distro) (Report, error) {
	spec := backend.Spec{Command: opts.Command, Dir: opts.Dir, Timeout: opts.Timeout}
	ref, err := e.Host.Run(ctx, spec)
	if err != nil {
		return Report{}, err
	}
	rep := Report{Command: opts.Command, Reference: Reference{Shell: e.HostShell, Result: ref}}
	for _, d := range distros {
		o := Outcome{Distro: d}
		env, err := e.Containers.Prepare(ctx, d, opts.Packages.For(d), opts.NoCache)
		if err != nil {
			o.Status, o.Error = Error, err.Error()
			rep.Outcomes = append(rep.Outcomes, o)
			continue
		}
		o.Env = env
		s := spec
		s.Image = env.Image
		o.Result, _ = e.Containers.Run(ctx, s)
		o.Comparison = compare.Compare(ref, o.Result, opts.Compare)
		o.Status = Fail
		if o.Comparison.Match() {
			o.Status = Pass
		}
		rep.Outcomes = append(rep.Outcomes, o)
	}
	return rep, nil
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
