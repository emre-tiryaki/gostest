// Package cli implements the gostest command line interface.
package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/emre-tiryaki/gostest/internal/backend"
	"github.com/emre-tiryaki/gostest/internal/cache"
	"github.com/emre-tiryaki/gostest/internal/compare"
	"github.com/emre-tiryaki/gostest/internal/config"
	"github.com/emre-tiryaki/gostest/internal/distro"
	"github.com/emre-tiryaki/gostest/internal/engine"
	"github.com/emre-tiryaki/gostest/internal/report"
)

// Exit codes.
const (
	// ExitOK: every distribution matches the reference.
	ExitOK = 0
	// ExitFailed: at least one distribution differs or could not run.
	ExitFailed = 1
	// ExitUsage: invalid flags, arguments or configuration.
	ExitUsage = 2
	// ExitRuntime: the reference or the container engine could not run.
	ExitRuntime = 3
	// ExitInterrupted: the run was cancelled (Ctrl-C).
	ExitInterrupted = 130
)

// DefaultTimeout bounds each run unless configured otherwise.
const DefaultTimeout = 2 * time.Minute

// DefaultMaxFingerprintFiles bounds the working directory size for which
// results are cached.
const DefaultMaxFingerprintFiles = 20000

// DefaultConcurrency is the number of distributions processed at once
// unless configured otherwise.
func DefaultConcurrency() int { return max(runtime.NumCPU(), 2) }

// Containers is a container engine usable by gostest.
type Containers interface {
	engine.Containers
	Available(ctx context.Context) error
	Name() string
	// Cleanup removes the images the run left behind, see
	// backend.Docker.Cleanup.
	Cleanup(ctx context.Context) error
}

// App is the command line application with its dependencies, which tests
// replace with fakes.
type App struct {
	Stdout, Stderr io.Writer
	// StdoutTTY and StderrTTY tell whether the streams are terminals;
	// they enable colours and progress output.
	StdoutTTY, StderrTTY bool

	Host          backend.Runner
	HostShell     string
	NewContainers func(engine string) (Containers, error)
	LookPath      func(file string) (string, error)
	Getwd         func() (string, error)
	Getenv        func(key string) string
	CacheDir      func() (string, error)
	// HostTool resolves commands missing in a distribution to host files
	// mounted into its containers, see HostToolResolver; nil disables
	// mounting.
	HostTool func(name string) (backend.Mount, bool)

	// MaxFingerprintFiles overrides DefaultMaxFingerprintFiles when > 0.
	MaxFingerprintFiles int
	Version             string
}

// New returns an App wired to the real system.
func New(version string) *App {
	return &App{
		Stdout:    os.Stdout,
		Stderr:    os.Stderr,
		StdoutTTY: isTerminal(os.Stdout),
		StderrTTY: isTerminal(os.Stderr),
		Host:      backend.Local{},
		HostShell: backend.HostShell(),
		NewContainers: func(engine string) (Containers, error) {
			return backend.NewDocker(engine), nil
		},
		LookPath: exec.LookPath,
		Getwd:    os.Getwd,
		Getenv:   os.Getenv,
		CacheDir: cache.DefaultDir,
		HostTool: HostToolResolver(exec.LookPath),
		Version:  version,
	}
}

// HostToolResolver returns a resolver for App.HostTool. It finds a command
// on the host with lookPath, follows symlinks and accepts only files that
// run the same anywhere (scripts and static binaries, see
// backend.ClassifyHostTool). They are mounted at
// backend.HostToolTarget/<name>, and the hash of their contents keeps
// cached results apart once they change.
func HostToolResolver(lookPath func(string) (string, error)) func(name string) (backend.Mount, bool) {
	return func(name string) (backend.Mount, bool) {
		found, err := lookPath(name)
		if err != nil {
			return backend.Mount{}, false
		}
		src, err := filepath.EvalSymlinks(found)
		if err != nil {
			return backend.Mount{}, false
		}
		if src, err = filepath.Abs(src); err != nil || strings.Contains(src, ":") {
			return backend.Mount{}, false
		}
		kind, err := backend.ClassifyHostTool(src)
		if err != nil || kind == backend.ToolUnsupported {
			return backend.Mount{}, false
		}
		var interpreter string
		if kind == backend.ToolScript {
			interpreter, _ = backend.ScriptInterpreter(src)
		}
		content, err := os.ReadFile(src)
		if err != nil {
			return backend.Mount{}, false
		}
		sum := sha256.Sum256(content)
		return backend.Mount{Source: src, Target: path.Join(backend.HostToolTarget, name), Hash: hex.EncodeToString(sum[:]), Interpreter: interpreter}, true
	}
}

func isTerminal(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// stringList is a repeatable string flag.
type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

type flags struct {
	distros      string
	pkgs         stringList
	concurrency  int
	timeout      time.Duration
	ignoreStderr bool
	strictOrder  bool
	engine       string
	workdir      string
	configPath   string
	noCache      bool
	noAutoInst   bool
	cleanup      bool
	clearCache   bool
	format       string
	color        string
	context      int
	verbose      bool
	quiet        bool
	list         bool
	version      bool

	set map[string]bool // flags given on the command line, by long name
}

// aliases maps short flag names to their long names.
var aliases = map[string]string{"d": "distros", "p": "pkg", "j": "concurrency", "t": "timeout", "v": "verbose", "q": "quiet", "h": "help"}

func (a *App) parseFlags(args []string) (*flags, []string, error) {
	f := &flags{}
	fs := flag.NewFlagSet("gostest", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	for _, name := range []string{"d", "distros"} {
		fs.StringVar(&f.distros, name, "", "")
	}
	for _, name := range []string{"p", "pkg"} {
		fs.Var(&f.pkgs, name, "")
	}
	for _, name := range []string{"j", "concurrency"} {
		fs.IntVar(&f.concurrency, name, 0, "")
	}
	for _, name := range []string{"t", "timeout"} {
		fs.DurationVar(&f.timeout, name, DefaultTimeout, "")
	}
	for _, name := range []string{"v", "verbose"} {
		fs.BoolVar(&f.verbose, name, false, "")
	}
	for _, name := range []string{"q", "quiet"} {
		fs.BoolVar(&f.quiet, name, false, "")
	}
	fs.BoolVar(&f.ignoreStderr, "ignore-stderr", false, "")
	fs.BoolVar(&f.strictOrder, "strict-order", false, "")
	fs.StringVar(&f.engine, "engine", "", "")
	fs.StringVar(&f.workdir, "workdir", "", "")
	fs.StringVar(&f.configPath, "config", "", "")
	fs.BoolVar(&f.noCache, "no-cache", false, "")
	fs.BoolVar(&f.noAutoInst, "no-auto-install", false, "")
	fs.BoolVar(&f.cleanup, "cleanup", false, "")
	fs.BoolVar(&f.clearCache, "clear-cache", false, "")
	fs.StringVar(&f.format, "format", "text", "")
	fs.StringVar(&f.color, "color", "auto", "")
	fs.IntVar(&f.context, "context", report.DefaultContext, "")
	fs.BoolVar(&f.list, "list", false, "")
	fs.BoolVar(&f.version, "version", false, "")

	if err := fs.Parse(args); err != nil {
		return nil, nil, err
	}
	f.set = map[string]bool{}
	fs.Visit(func(fl *flag.Flag) {
		name := fl.Name
		if long, ok := aliases[name]; ok {
			name = long
		}
		f.set[name] = true
	})
	return f, fs.Args(), nil
}

func (f *flags) validate() error {
	switch {
	case f.format != "text" && f.format != "json":
		return fmt.Errorf("invalid --format %q: use text or json", f.format)
	case f.color != "auto" && f.color != "always" && f.color != "never":
		return fmt.Errorf("invalid --color %q: use auto, always or never", f.color)
	case f.concurrency < 0:
		return fmt.Errorf("--concurrency must not be negative")
	case f.timeout < 0:
		return fmt.Errorf("--timeout must not be negative")
	case f.context < 0:
		return fmt.Errorf("--context must not be negative")
	case f.engine != "" && f.engine != "docker" && f.engine != "podman":
		return fmt.Errorf("invalid --engine %q: use docker or podman", f.engine)
	}
	return nil
}

func (a *App) errorf(format string, args ...any) {
	fmt.Fprintf(a.Stderr, "gostest: "+format+"\n", args...)
}

func (a *App) usageError(err error) int {
	a.errorf("%v", err)
	fmt.Fprintln(a.Stderr, "Run 'gostest --help' for usage.")
	return ExitUsage
}

// Run executes the command line args and returns the exit code.
func (a *App) Run(ctx context.Context, args []string) int {
	f, rest, err := a.parseFlags(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Fprint(a.Stdout, usage)
		return ExitOK
	}
	if err != nil {
		return a.usageError(err)
	}
	if f.version {
		fmt.Fprintf(a.Stdout, "gostest %s\n", a.Version)
		return ExitOK
	}
	if err := f.validate(); err != nil {
		return a.usageError(err)
	}

	workdir, err := a.workdir(f.workdir)
	if err != nil {
		return a.usageError(err)
	}
	cfg, err := a.loadConfig(f.configPath, workdir)
	if err != nil {
		return a.usageError(err)
	}
	reg, err := cfg.Registry()
	if err != nil {
		return a.usageError(err)
	}
	if f.list {
		a.list(reg)
		return ExitOK
	}

	if f.clearCache {
		if err := a.clearCache(); err != nil {
			a.errorf("%v", err)
			return ExitRuntime
		}
		if len(rest) == 0 {
			fmt.Fprintln(a.Stdout, "gostest: cache cleared")
			return ExitOK
		}
	}

	command := strings.Join(rest, " ")
	if strings.TrimSpace(command) == "" {
		return a.usageError(errors.New("no command given"))
	}

	opts, distros, err := a.options(f, cfg, reg, command, workdir)
	if err != nil {
		return a.usageError(err)
	}

	engineName := f.engine
	if engineName == "" {
		engineName = cfg.Engine
	}
	containers, err := a.containers(ctx, engineName)
	if err != nil {
		a.errorf("%v", err)
		return ExitRuntime
	}

	e := &engine.Engine{Host: a.Host, HostShell: a.HostShell, HostLocale: HostLocale(a.Getenv), Containers: containers}
	if opts.AutoInstall {
		e.HostTool = a.HostTool
	}
	e.Cache, opts.Fingerprint = a.openCache(workdir)

	var prog *progress
	if a.StderrTTY && !f.quiet {
		names := make([]string, len(distros))
		for i, d := range distros {
			names[i] = d.Name
		}
		prog = newProgress(a.Stderr, names)
		e.Progress = prog.update
	}
	rep, err := e.Run(ctx, opts, distros)
	prog.finish()
	if f.cleanup || cfg.Cleanup {
		a.cleanup(ctx, containers)
	}
	if ctx.Err() != nil {
		a.errorf("interrupted")
		return ExitInterrupted
	}
	if err != nil {
		a.errorf("%v", err)
		return ExitRuntime
	}

	if f.format == "json" {
		err = report.JSON(a.Stdout, rep)
	} else {
		err = report.Text(a.Stdout, rep, report.TextOptions{
			Color:   a.useColor(f.color),
			Verbose: f.verbose,
			Context: f.context,
		})
	}
	if err != nil {
		a.errorf("writing the report: %v", err)
		return ExitRuntime
	}
	if !rep.OK() {
		return ExitFailed
	}
	return ExitOK
}

func (a *App) workdir(flagValue string) (string, error) {
	dir := flagValue
	if dir == "" {
		wd, err := a.Getwd()
		if err != nil {
			return "", fmt.Errorf("finding the current directory: %w", err)
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("working directory %s is not a directory", abs)
	}
	return abs, nil
}

func (a *App) loadConfig(path, workdir string) (config.Config, error) {
	if path == "" {
		found, ok := config.Find(workdir)
		if !ok {
			return config.Config{}, nil
		}
		path = found
	}
	return config.Load(path)
}

// options combines flags, configuration and defaults; flags win over the
// configuration, which wins over the defaults.
func (a *App) options(f *flags, cfg config.Config, reg *distro.Registry, command, workdir string) (engine.Options, []distro.Distro, error) {
	opts := engine.Options{
		Command:     command,
		Dir:         workdir,
		Timeout:     DefaultTimeout,
		Concurrency: DefaultConcurrency(),
		NoCache:     f.noCache,
		AutoInstall: !f.noAutoInst && !cfg.NoAutoInstall,
		Compare: compare.Options{
			IgnoreStderr: f.ignoreStderr || cfg.IgnoreStderr,
			StrictOrder:  f.strictOrder || cfg.StrictOrder,
		},
	}

	switch {
	case f.set["timeout"]:
		opts.Timeout = f.timeout
	case cfg.Timeout.Duration > 0:
		opts.Timeout = cfg.Timeout.Duration
	}
	switch {
	case f.set["concurrency"]:
		opts.Concurrency = f.concurrency
	case cfg.Concurrency > 0:
		opts.Concurrency = cfg.Concurrency
	}

	distros := reg.Defaults()
	var err error
	switch {
	case f.set["distros"]:
		distros, err = reg.ParseList(f.distros)
	case len(cfg.Distros) > 0:
		distros, err = reg.ParseList(strings.Join(cfg.Distros, ","))
	}
	if err != nil {
		return engine.Options{}, nil, err
	}

	cfgPkgs, err := distro.ParsePackages(cfg.Packages)
	if err != nil {
		return engine.Options{}, nil, err
	}
	flagPkgs, err := distro.ParsePackages(f.pkgs)
	if err != nil {
		return engine.Options{}, nil, err
	}
	opts.Packages = cfgPkgs.Merge(flagPkgs)
	return opts, distros, nil
}

func (a *App) containers(ctx context.Context, name string) (Containers, error) {
	if name == "" {
		for _, candidate := range []string{"docker", "podman"} {
			if _, err := a.LookPath(candidate); err == nil {
				name = candidate
				break
			}
		}
		if name == "" {
			return nil, errors.New("no container engine found: install Docker or Podman")
		}
	}
	c, err := a.NewContainers(name)
	if err != nil {
		return nil, err
	}
	if err := c.Available(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// openCache returns the result cache and the working directory fingerprint.
// Caching is a speed-up only, so problems disable it with a warning.
func (a *App) openCache(workdir string) (*cache.Store, string) {
	dir, err := a.CacheDir()
	if err != nil {
		a.errorf("warning: result cache disabled: %v", err)
		return nil, ""
	}
	store, err := cache.Open(dir)
	if err != nil {
		a.errorf("warning: result cache disabled: %v", err)
		return nil, ""
	}
	limit := a.MaxFingerprintFiles
	if limit <= 0 {
		limit = DefaultMaxFingerprintFiles
	}
	fp, err := cache.Fingerprint(workdir, limit)
	if errors.Is(err, cache.ErrTooManyFiles) {
		a.errorf("warning: result cache disabled: %s has more than %d files; use --workdir to point at a smaller directory", workdir, limit)
		return nil, ""
	}
	if err != nil {
		a.errorf("warning: result cache disabled: %v", err)
		return nil, ""
	}
	return store, fp
}

func (a *App) clearCache() error {
	dir, err := a.CacheDir()
	if err != nil {
		return err
	}
	store, err := cache.Open(dir)
	if err != nil {
		return err
	}
	return store.Clear()
}

// cleanupTimeout bounds removing the images after a run.
const cleanupTimeout = 2 * time.Minute

// cleanup removes what the run left in the container engine. It also runs
// after an interrupted run, so it does not use ctx's cancellation.
func (a *App) cleanup(ctx context.Context, c Containers) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	if err := c.Cleanup(cctx); err != nil {
		a.errorf("warning: cleanup: %v", err)
	}
}

func (a *App) useColor(mode string) bool {
	switch mode {
	case "always":
		return true
	case "never":
		return false
	}
	return a.StdoutTTY && a.Getenv("NO_COLOR") == "" && a.Getenv("TERM") != "dumb"
}

func (a *App) list(reg *distro.Registry) {
	defaults := map[string]bool{}
	for _, d := range reg.Defaults() {
		defaults[d.Name] = true
	}
	all := reg.All()
	sort.Slice(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	fmt.Fprintf(a.Stdout, "%-12s %-32s %-8s %s\n", "NAME", "IMAGE", "PACKAGES", "DEFAULT")
	for _, d := range all {
		mark := ""
		if defaults[d.Name] {
			mark = "*"
		}
		fmt.Fprintf(a.Stdout, "%-12s %-32s %-8s %s\n", d.Name, d.Image, d.PackageManager, mark)
	}
	fmt.Fprintln(a.Stdout, "\nAny distro accepts a tag, e.g. ubuntu:22.04 or alpine:3.18.")
}

// progress shows one line per distribution plus a counter line at the
// bottom, redrawn in place on a terminal while distributions are prepared
// and run.
type progress struct {
	w      io.Writer
	names  []string
	done   int
	stages map[string]string
	drawn  int
}

func newProgress(w io.Writer, names []string) *progress {
	p := &progress{w: w, names: names, stages: map[string]string{}}
	for _, n := range names {
		p.stages[n] = "waiting"
	}
	return p
}

func (p *progress) update(ev engine.Event) {
	if ev.Stage == engine.StageDone {
		p.done++
		p.stages[ev.Distro] = "done " + string(ev.Status)
	} else {
		p.stages[ev.Distro] = string(ev.Stage)
	}
	p.draw()
}

// draw rewrites the block. The cursor rests at the end of its last line.
func (p *progress) draw() {
	var b strings.Builder
	if p.drawn > 1 {
		fmt.Fprintf(&b, "\x1b[%dA", p.drawn-1)
	}
	b.WriteString("\r")
	for _, n := range p.names {
		fmt.Fprintf(&b, "\x1b[K  %-16s %s\n", n, p.stages[n])
	}
	fmt.Fprintf(&b, "\x1b[K[%d/%d]", p.done, len(p.names))
	p.drawn = len(p.names) + 1
	fmt.Fprint(p.w, b.String())
}

func (p *progress) finish() {
	if p == nil || p.drawn == 0 {
		return
	}
	if p.drawn > 1 {
		fmt.Fprintf(p.w, "\x1b[%dA", p.drawn-1)
	}
	fmt.Fprint(p.w, "\r\x1b[J")
}

const usage = `Usage: gostest [flags] [--] command [args...]

Run a shell command on this machine and, in parallel, inside several Linux
distributions; compare every result with the local one and explain why it
differs.

Examples:
  gostest 'if [[ $SHELL == *bash ]]; then echo bash; fi'
  gostest -d alpine,ubuntu:22.04 -p curl ./install.sh
  gostest -p alpine=bash,fedora=which --format json 'my-tool --version'

Flags:
  -d, --distros LIST      comma separated distributions
                          (default: alpine,archlinux,debian,fedora,ubuntu)
  -p, --pkg SPEC          packages to install: "curl" everywhere or
                          "alpine=bash" in one distro; repeatable, comma separated
  -j, --concurrency N     distributions processed at once (default: CPUs)
  -t, --timeout DURATION  timeout of every run, 0 disables it (default: 2m)
      --ignore-stderr     compare only stdout and exit codes
      --strict-order      fail when output only differs in line order
      --engine NAME       docker or podman (default: auto-detect)
      --workdir DIR       working directory, mounted read-only into the
                          containers (default: current directory)
      --config FILE       configuration file (default: nearest .gostest.json)
      --no-cache          ignore cached results and rebuild images
      --no-auto-install   do not mount host tools or install packages for
                          commands that are missing in a distribution
      --cleanup           remove the images pulled or built for the run
                          when it ends
      --clear-cache       remove cached results
      --format FORMAT     text or json (default: text)
      --color WHEN        auto, always or never (default: auto)
      --context N         unchanged lines shown around changes (default: 3)
  -v, --verbose           show the full output of every run
  -q, --quiet             do not show progress
      --list              list the known distributions
      --version           print the version
  -h, --help              show this help

Exit codes:
  0    every distribution matches the reference
  1    at least one distribution differs or could not be run
  2    invalid usage or configuration
  3    the reference or the container engine could not be run
  130  interrupted
`

// HostLocale returns the locale that decides collation on the host, as the C
// library resolves it from LC_ALL, LC_COLLATE and LANG; empty when none is set.
func HostLocale(getenv func(string) string) string {
	for _, k := range []string{"LC_ALL", "LC_COLLATE", "LANG"} {
		if v := getenv(k); v != "" {
			return v
		}
	}
	return ""
}
