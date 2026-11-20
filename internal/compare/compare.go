// Package compare compares a command's result in a target environment with
// the reference result from the host.
package compare

import (
	"fmt"
	"slices"
	"strings"

	"github.com/emre-tiryaki/gostest/internal/backend"
)

// Op says where a diff line comes from.
type Op int

// Diff operations.
const (
	Equal  Op = iota // present in both
	Delete           // only in the reference
	Insert           // only in the target
)

var opNames = [...]string{Equal: "equal", Delete: "delete", Insert: "insert"}

func (op Op) String() string {
	if op >= 0 && int(op) < len(opNames) {
		return opNames[op]
	}
	return fmt.Sprintf("Op(%d)", int(op))
}

// MarshalText encodes the operation by name for JSON output.
func (op Op) MarshalText() ([]byte, error) { return []byte(op.String()), nil }

// UnmarshalText decodes an operation name.
func (op *Op) UnmarshalText(b []byte) error {
	for i, n := range opNames {
		if n == string(b) {
			*op = Op(i)
			return nil
		}
	}
	return fmt.Errorf("unknown diff op %q", b)
}

// Line is one line of a diff.
type Line struct {
	Op   Op     `json:"op"`
	Text string `json:"text"`
	// NoEOL marks a final line that is not terminated by a newline.
	NoEOL bool `json:"no_eol,omitempty"`
}

// SplitLines splits s into lines with Op Equal, without their newlines. A
// final line without a newline is marked NoEOL.
func SplitLines(s string) []Line {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "\n")
	noEOL := parts[len(parts)-1] != ""
	if !noEOL {
		parts = parts[:len(parts)-1]
	}
	out := make([]Line, len(parts))
	for i, p := range parts {
		out[i] = Line{Text: p}
	}
	if noEOL {
		out[len(out)-1].NoEOL = true
	}
	return out
}

// maxCells bounds the LCS table; larger middles fall back to a plain
// "replace everything" diff, which is still correct but not minimal.
const maxCells = 4_000_000

// Diff returns a minimal line diff turning ref into got. Within a changed
// block, deletions come before insertions.
func Diff(ref, got string) []Line {
	a, b := SplitLines(ref), SplitLines(got)

	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}

	out := make([]Line, 0, len(a)+len(b)-prefix-suffix)
	out = append(out, a[:prefix]...)
	out = append(out, diffMiddle(a[prefix:len(a)-suffix], b[prefix:len(b)-suffix])...)
	out = append(out, a[len(a)-suffix:]...)
	if len(out) == 0 {
		return nil
	}
	return out
}

func diffMiddle(a, b []Line) []Line {
	n, m := len(a), len(b)
	var out []Line
	if n == 0 || m == 0 || n*m > maxCells {
		for _, l := range a {
			out = append(out, Line{Op: Delete, Text: l.Text, NoEOL: l.NoEOL})
		}
		for _, l := range b {
			out = append(out, Line{Op: Insert, Text: l.Text, NoEOL: l.NoEOL})
		}
		return out
	}

	// lcs[i*(m+1)+j] is the LCS length of a[i:] and b[j:].
	w := m + 1
	lcs := make([]int32, (n+1)*w)
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				lcs[i*w+j] = lcs[(i+1)*w+j+1] + 1
			} else {
				lcs[i*w+j] = max(lcs[(i+1)*w+j], lcs[i*w+j+1])
			}
		}
	}

	i, j := 0, 0
	for i < n && j < m {
		switch {
		case a[i] == b[j]:
			out = append(out, a[i])
			i++
			j++
		case lcs[(i+1)*w+j] >= lcs[i*w+j+1]:
			out = append(out, Line{Op: Delete, Text: a[i].Text, NoEOL: a[i].NoEOL})
			i++
		default:
			out = append(out, Line{Op: Insert, Text: b[j].Text, NoEOL: b[j].NoEOL})
			j++
		}
	}
	for ; i < n; i++ {
		out = append(out, Line{Op: Delete, Text: a[i].Text, NoEOL: a[i].NoEOL})
	}
	for ; j < m; j++ {
		out = append(out, Line{Op: Insert, Text: b[j].Text, NoEOL: b[j].NoEOL})
	}
	return out
}

// Options tunes what is compared.
type Options struct {
	// IgnoreStderr treats stderr differences as a match.
	IgnoreStderr bool
	// StrictOrder treats a stream with the same lines in a different order
	// as a difference. By default such a stream matches and is only marked
	// as reordered.
	StrictOrder bool
}

// Comparison is the outcome of comparing a result with the reference.
type Comparison struct {
	ExitCodeMatch bool   `json:"exit_code_match"`
	StdoutMatch   bool   `json:"stdout_match"`
	StderrMatch   bool   `json:"stderr_match"`
	StdoutDiff    []Line `json:"stdout_diff,omitempty"`
	StderrDiff    []Line `json:"stderr_diff,omitempty"`
	// StdoutReordered and StderrReordered mark streams that match only
	// because their order was ignored.
	StdoutReordered bool `json:"stdout_reordered,omitempty"`
	StderrReordered bool `json:"stderr_reordered,omitempty"`
}

// Match reports whether the result is equivalent to the reference.
func (c Comparison) Match() bool { return c.ExitCodeMatch && c.StdoutMatch && c.StderrMatch }

// Reordered reports whether a stream matched only because its order was
// ignored.
func (c Comparison) Reordered() bool { return c.StdoutReordered || c.StderrReordered }

// SameLines reports whether a and b contain the same lines, counting
// duplicates, in any order.
func SameLines(a, b string) bool {
	a, b = normalize(a), normalize(b)
	if a == b {
		return true
	}
	if len(a) != len(b) || strings.HasSuffix(a, "\n") != strings.HasSuffix(b, "\n") {
		return false
	}
	x := strings.Split(strings.TrimSuffix(a, "\n"), "\n")
	y := strings.Split(strings.TrimSuffix(b, "\n"), "\n")
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

// compareStream compares one normalized stream. A stream with the same
// lines in a different order matches and is marked as reordered unless
// strict is set.
func compareStream(ref, got string, strict bool) (match, reordered bool, diff []Line) {
	switch {
	case ref == got:
		return true, false, nil
	case !strict && SameLines(ref, got):
		return true, true, nil
	default:
		return false, false, Diff(ref, got)
	}
}

func normalize(s string) string { return strings.ReplaceAll(s, "\r\n", "\n") }

// Compare compares got with the reference result ref. Exit codes, timeouts,
// stdout and stderr are compared; durations are not. Diffs are only
// computed for streams that differ.
func Compare(ref, got backend.Result, opt Options) Comparison {
	c := Comparison{ExitCodeMatch: ref.ExitCode == got.ExitCode && ref.TimedOut == got.TimedOut}

	c.StdoutMatch, c.StdoutReordered, c.StdoutDiff = compareStream(normalize(ref.Stdout), normalize(got.Stdout), opt.StrictOrder)
	if opt.IgnoreStderr {
		c.StderrMatch = true
	} else {
		c.StderrMatch, c.StderrReordered, c.StderrDiff = compareStream(normalize(ref.Stderr), normalize(got.Stderr), opt.StrictOrder)
	}
	return c
}

// Unified renders a diff for humans: changed lines prefixed with "- "
// (reference) or "+ " (target), surrounded by up to context unchanged lines.
// Longer unchanged runs are collapsed into a single marker line. It returns
// nil when the diff has no changes.
func Unified(lines []Line, context int) []string {
	context = max(context, 0)
	changed := false
	for _, l := range lines {
		if l.Op != Equal {
			changed = true
			break
		}
	}
	if !changed {
		return nil
	}

	var out []string
	for i := 0; i < len(lines); {
		if lines[i].Op != Equal {
			out = append(out, renderChange(lines[i])...)
			i++
			continue
		}
		start := i
		for i < len(lines) && lines[i].Op == Equal {
			i++
		}
		out = append(out, renderEqualRun(lines[start:i], start == 0, i == len(lines), context)...)
	}
	return out
}

func renderChange(l Line) []string {
	prefix := "+ "
	if l.Op == Delete {
		prefix = "- "
	}
	out := []string{prefix + l.Text}
	if l.NoEOL {
		out = append(out, `\ no newline at end`)
	}
	return out
}

// renderEqualRun shows the lines of an unchanged run that are within
// context of a change. Hiding a single line saves nothing, so runs are only
// collapsed when at least two lines would be hidden.
func renderEqualRun(run []Line, leading, trailing bool, context int) []string {
	head, tail := context, context // lines kept at the start / end of the run
	if leading {
		head = 0
	}
	if trailing {
		tail = 0
	}
	hidden := len(run) - head - tail
	if hidden < 2 {
		head, tail, hidden = len(run), 0, 0
	}

	var out []string
	for _, l := range run[:head] {
		out = append(out, "  "+l.Text)
	}
	if hidden > 0 {
		out = append(out, fmt.Sprintf("  … %d unchanged lines", hidden))
	}
	for _, l := range run[len(run)-tail:] {
		out = append(out, "  "+l.Text)
	}
	return out
}
