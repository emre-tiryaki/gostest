// Package explain turns the difference between a target result and the
// reference into human readable reasons: not just "it failed", but which
// shell feature, command, option or file is the likely cause.
package explain

import (
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/emre-tiryaki/gostest/internal/backend"
	"github.com/emre-tiryaki/gostest/internal/compare"
	"github.com/emre-tiryaki/gostest/internal/distro"
)

// Kind classifies a reason.
type Kind string

// Reason kinds, roughly from most to least specific.
const (
	KindTimeout            Kind = "timeout"
	KindBashism            Kind = "bashism"
	KindSyntax             Kind = "syntax"
	KindMissingCommand     Kind = "missing-command"
	KindMissingInterpreter Kind = "missing-interpreter"
	KindUnsupportedOption  Kind = "unsupported-option"
	KindMissingFile        Kind = "missing-file"
	KindPermission         Kind = "permission"
	KindReadOnly           Kind = "read-only"
	KindSilentError        Kind = "silent-error"
	KindExitCode           Kind = "exit-code"
	KindStdout             Kind = "stdout"
	KindStderr             Kind = "stderr"
	KindTruncated          Kind = "truncated"
	KindReordered          Kind = "reordered"
	KindInstalled          Kind = "installed"
)

// Reason explains one aspect of a difference.
type Reason struct {
	Kind    Kind   `json:"kind"`
	Message string `json:"message"`
}

// Context describes the environments being compared.
type Context struct {
	// Distro is the target distribution name, e.g. "alpine".
	Distro string
	// Shell is what /bin/sh is in the target, e.g. "dash".
	Shell string
	// RefShell is what /bin/sh is on the host.
	RefShell string
	// HostLocale is the collation locale of the host, e.g. "en_US.UTF-8";
	// empty when none is set.
	HostLocale string
	// NotProvided lists commands that were installed automatically
	// without success because no package provides them.
	NotProvided []string
	// MountedInterpreters maps host tools mounted into the target to the
	// interpreter of their shebang line.
	MountedInterpreters map[string]string
}

// MaxCauses bounds the number of causes derived from stderr.
const MaxCauses = 10

// maxLines bounds how many new stderr lines are analysed.
const maxLines = 500

// Explain returns the reasons why got differs from the reference ref, most
// important first: timeouts, causes found in the new stderr lines, silent
// failures, exit codes and finally generic output differences. It returns
// nil when cmp reports a match.
func Explain(ref, got backend.Result, cmp compare.Comparison, c Context) []Reason {
	if cmp.Match() {
		return nil
	}
	var out []Reason
	seen := map[Reason]bool{}
	add := func(r Reason) {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}

	switch {
	case got.TimedOut && !ref.TimedOut:
		add(Reason{KindTimeout, fmt.Sprintf("timed out after %s; the reference finished in %s", round(got.Duration), round(ref.Duration))})
	case ref.TimedOut && !got.TimedOut:
		add(Reason{KindTimeout, "the reference run timed out, but this run finished; the results cannot be compared reliably"})
	}

	// BusyBox prints its banner on a separate line after usage errors.
	busybox := strings.Contains(got.Stderr, "BusyBox")
	causes := 0
	for _, line := range newLines(ref.Stderr, got.Stderr) {
		r, ok := diagnose(line, c, busybox)
		if !ok || seen[r] {
			continue
		}
		add(r)
		if causes++; causes == MaxCauses {
			break
		}
	}

	if causes > 0 && cmp.ExitCodeMatch && got.ExitCode == 0 {
		add(Reason{KindSilentError, "errors were printed but /bin/sh kept going and returned exit code 0 like the reference: checking the exit code alone would call this run a success"})
	}
	if !cmp.ExitCodeMatch && !got.TimedOut && !ref.TimedOut {
		add(Reason{KindExitCode, exitMessage(got.ExitCode, ref.ExitCode)})
	}
	if !cmp.StdoutMatch {
		if compare.SameLines(ref.Stdout, got.Stdout) {
			add(reordered("stdout", c, true))
		} else {
			add(Reason{KindStdout, "stdout differs from the reference"})
		}
	}
	if !cmp.StderrMatch && causes == 0 {
		if compare.SameLines(ref.Stderr, got.Stderr) {
			add(reordered("stderr", c, true))
		} else {
			add(Reason{KindStderr, "stderr differs from the reference"})
		}
	}
	if got.Truncated {
		add(Reason{KindTruncated, fmt.Sprintf("output was truncated at %d MiB; later differences are not shown", backend.MaxOutput>>20)})
	}
	return out
}

func round(d time.Duration) time.Duration {
	if d >= time.Second {
		return d.Round(100 * time.Millisecond)
	}
	return d.Round(time.Millisecond)
}

// newLines returns the non-empty lines of got that do not appear in ref.
func newLines(ref, got string) []string {
	known := map[string]bool{}
	for l := range strings.SplitSeq(ref, "\n") {
		known[strings.TrimSpace(l)] = true
	}
	var out []string
	for l := range strings.SplitSeq(got, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || known[l] {
			continue
		}
		known[l] = true
		if out = append(out, l); len(out) == maxLines {
			break
		}
	}
	return out
}

// shellDesc describes the target and reference shells for messages, e.g.
// " (here /bin/sh is dash, on the host it is bash)".
func shellDesc(c Context) string {
	switch {
	case known(c.Shell) && known(c.RefShell):
		return fmt.Sprintf(" (here /bin/sh is %s, on the host it is %s)", c.Shell, c.RefShell)
	case known(c.Shell):
		return fmt.Sprintf(" (here /bin/sh is %s)", c.Shell)
	}
	return ""
}

func known(shell string) bool { return shell != "" && shell != "unknown" }

// bashisms maps bash/ksh-only keywords and builtins to a POSIX alternative.
var bashisms = map[string]string{
	"[[":        "use [ ... ] or case instead",
	"source":    "use . file instead",
	"function":  "define functions as name() { ...; } instead",
	"declare":   "declare is bash-only; use plain assignments",
	"typeset":   "typeset is ksh/bash-only; use plain assignments",
	"shopt":     "shopt options only exist in bash",
	"let":       "use $(( ... )) arithmetic instead",
	"pushd":     "use cd (and remember the old directory) instead",
	"popd":      "use cd (and remember the old directory) instead",
	"mapfile":   "read lines with a while read loop instead",
	"readarray": "read lines with a while read loop instead",
}

var (
	reBadInterpreter = regexp.MustCompile(`(\S+): (\S+): bad interpreter`)
	reEnvMissing     = regexp.MustCompile(`^env: (?:can't execute )?['‘]?([^'’:\s]+)['’]?: No such file`)
	reWhich          = regexp.MustCompile(`^which: no (\S+) in `)
	reZshNotFound    = regexp.MustCompile(`command not found: (\S+)`)
	reCmdNotFound    = regexp.MustCompile(`(\S+): command not found`)
	reNotFound       = regexp.MustCompile(`(\S+): not found$`)
	reOption         = regexp.MustCompile(`^(\S+?): (?:invalid|unrecognized|illegal|unknown) option\b(.*)$`)
	reBadSubst       = regexp.MustCompile(`(?i)bad substitution`)
	reUnexpectedOp   = regexp.MustCompile(`\[: .*unexpected operator`)
	reSyntax         = regexp.MustCompile(`(?i)syntax error:?\s*(.*)$`)
	reReadOnly       = regexp.MustCompile(`Read-only file system`)
	rePermission     = regexp.MustCompile(`(\S+): Permission denied`)
	reFileOp         = regexp.MustCompile(`(?:cannot access|cannot open|can't open|cannot stat|can't cd to|cannot touch)\s+['‘"]?([^'’":]+?)['’"]?(?::|$)`)
	reNoSuchFile     = regexp.MustCompile(`([^\s:'‘’]+)['’]?: No such file`)
)

// diagnose finds the cause behind one stderr line. Rules are ordered from
// most to least specific; the first that matches wins.
func diagnose(line string, c Context, busybox bool) (Reason, bool) {
	if m := reBadInterpreter.FindStringSubmatch(line); m != nil {
		return Reason{KindMissingInterpreter, fmt.Sprintf("`%s` cannot run: its shebang interpreter `%s` does not exist here", m[1], m[2])}, true
	}
	for _, re := range []*regexp.Regexp{reEnvMissing, reWhich, reZshNotFound, reCmdNotFound, reNotFound} {
		if m := re.FindStringSubmatch(line); m != nil {
			return notFound(m[1], c), true
		}
	}
	if m := reOption.FindStringSubmatch(line); m != nil {
		return unsupportedOption(path.Base(m[1]), m[2], busybox), true
	}
	if reBadSubst.MatchString(line) {
		return Reason{KindBashism, "bash-only parameter expansion such as ${var:0:3}, ${var//a/b} or ${var^^} is not supported" + shellDesc(c)}, true
	}
	if reUnexpectedOp.MatchString(line) {
		return Reason{KindBashism, "`[` got an operator it does not know" + shellDesc(c) + "; `==` is a bash extension, POSIX `[` compares strings with `=`"}, true
	}
	if m := reSyntax.FindStringSubmatch(line); m != nil {
		detail := strings.TrimSpace(m[1])
		if detail != "" {
			detail = " (" + detail + ")"
		}
		return Reason{KindSyntax, "/bin/sh cannot parse the script" + detail + shellDesc(c) + "; arrays, process substitution <(...) and the function keyword are common bash-only culprits"}, true
	}
	if reReadOnly.MatchString(line) {
		return Reason{KindReadOnly, "the working directory is mounted read-only in containers; write temporary files under /tmp instead"}, true
	}
	if m := rePermission.FindStringSubmatch(line); m != nil {
		return Reason{KindPermission, fmt.Sprintf("permission denied for `%s`; check its file mode (is it executable?)", m[1])}, true
	}
	if m := reFileOp.FindStringSubmatch(line); m != nil {
		return missingFile(m[1]), true
	}
	if m := reNoSuchFile.FindStringSubmatch(line); m != nil {
		return missingFile(m[1]), true
	}
	return Reason{}, false
}

// MissingCommands returns the commands that got reports as not found while
// the reference does not, without duplicates and in order of appearance:
// the candidates for installing automatically. A missing shebang
// interpreter counts by its base name (/bin/bash -> bash). Shell keywords
// such as [[, paths and names that are not safe to look up are left out.
func MissingCommands(ref, got backend.Result) []string {
	var out []string
	add := func(name string) {
		if _, bashism := bashisms[name]; bashism || !distro.ValidCommandName(name) || slices.Contains(out, name) {
			return
		}
		out = append(out, name)
	}
	for _, line := range newLines(ref.Stderr, got.Stderr) {
		if m := reBadInterpreter.FindStringSubmatch(line); m != nil {
			add(path.Base(m[2]))
			continue
		}
		for _, re := range []*regexp.Regexp{reEnvMissing, reWhich, reZshNotFound, reCmdNotFound, reNotFound} {
			if m := re.FindStringSubmatch(line); m != nil {
				add(m[1])
				break
			}
		}
	}
	return out
}

func notFound(name string, c Context) Reason {
	if hint, ok := bashisms[name]; ok {
		return Reason{KindBashism, fmt.Sprintf("`%s` is not available%s: it is a bash/ksh feature, %s", name, shellDesc(c), hint)}
	}
	if interp, ok := c.MountedInterpreters[name]; ok {
		d := c.Distro
		if d == "" {
			d = "<distro>"
		}
		return Reason{KindMissingInterpreter, fmt.Sprintf("`%s` is mounted from the host, but its interpreter `%s` is not installed here; add its package with --pkg %s=<package>", name, interp, d)}
	}
	if strings.Contains(name, "/") {
		return Reason{KindMissingInterpreter, fmt.Sprintf("`%s` could not be executed: the file or the interpreter in its shebang line does not exist here", name)}
	}
	distro := c.Distro
	if distro == "" {
		distro = "<distro>"
	}
	if slices.Contains(c.NotProvided, name) {
		return Reason{KindMissingCommand, fmt.Sprintf("`%s` is not installed here and no package providing it was found; add its package with --pkg %s=<package>", name, distro)}
	}
	return Reason{KindMissingCommand, fmt.Sprintf("`%s` is not installed here; add its package with --pkg %s=<package>", name, distro)}
}

func unsupportedOption(tool, rest string, busybox bool) Reason {
	opt := strings.TrimSpace(rest)
	opt = strings.TrimPrefix(opt, "--")
	opt = strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(opt), ":")), `'‘’"`)
	if f := strings.Fields(opt); len(f) > 0 {
		opt = f[0]
	}
	switch {
	case opt == "":
		opt = "an option"
	case strings.HasPrefix(opt, "-"):
	case len(opt) == 1:
		opt = "-" + opt
	default:
		opt = "--" + opt
	}
	msg := fmt.Sprintf("`%s` does not support option `%s` here; core tools differ between distributions (GNU, BusyBox, BSD)", tool, opt)
	if busybox {
		msg = fmt.Sprintf("`%s` here is the BusyBox implementation and does not support option `%s`", tool, opt)
	}
	return Reason{KindUnsupportedOption, msg}
}

func missingFile(p string) Reason {
	return Reason{KindMissingFile, fmt.Sprintf("`%s` does not exist here; paths and file layouts differ between distributions", p)}
}

func exitMessage(code, ref int) string {
	msg := fmt.Sprintf("exit code %d, reference exited %d", code, ref)
	switch {
	case code == 2:
		msg += " (2 usually means a usage error or a misused shell builtin)"
	case code == 126:
		msg += " (126: found but not executable)"
	case code == 127:
		msg += " (127: command not found)"
	case code > 128 && code < 128+65:
		sig := syscall.Signal(code - 128)
		msg += fmt.Sprintf(" (killed by signal %d (%s))", int(sig), sig)
	}
	return msg
}

// Notes returns remarks about a run that are not differences, such as
// streams that matched only because their order was ignored.
func Notes(cmp compare.Comparison, c Context) []Reason {
	var out []Reason
	if cmp.StdoutReordered {
		out = append(out, reordered("stdout", c, false))
	}
	if cmp.StderrReordered {
		out = append(out, reordered("stderr", c, false))
	}
	return out
}

// reordered describes a stream with the same lines as the reference in a
// different order. strict is set when the order is being compared, so the
// hint to enable --strict-order is left out.
func reordered(stream string, c Context, strict bool) Reason {
	msg := stream + " has the same lines in a different order"
	loc := c.HostLocale
	switch {
	case loc != "" && !cLocale(loc) && strict:
		msg += fmt.Sprintf(" (host locale %s, which the container does not inherit; LC_ALL=C in the command makes them agree)", loc)
	case loc != "" && !cLocale(loc):
		msg += fmt.Sprintf(" (host locale %s, which the container does not inherit)", loc)
	}
	if !strict {
		msg += "; --strict-order fails this"
	}
	return Reason{KindReordered, msg}
}

// cLocale reports whether loc collates by byte value like the C locale.
func cLocale(loc string) bool {
	return loc == "C" || loc == "POSIX" || strings.HasPrefix(loc, "C.")
}
