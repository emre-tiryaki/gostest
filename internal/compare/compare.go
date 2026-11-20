// Package compare compares a command's result in a target environment with
// the reference result from the host.
package compare

import (
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

// Line is one line of a diff.
type Line struct {
	Op   Op     `json:"op"`
	Text string `json:"text"`
}

// Diff returns a line diff turning ref into got.
func Diff(ref, got string) []Line {
	var out []Line
	for _, l := range strings.Split(ref, "\n") {
		out = append(out, Line{Op: Delete, Text: l})
	}
	for _, l := range strings.Split(got, "\n") {
		out = append(out, Line{Op: Insert, Text: l})
	}
	return out
}

// Options tunes what is compared.
type Options struct {
	IgnoreStderr bool
}

// Comparison is the outcome of comparing a result with the reference.
type Comparison struct {
	ExitCodeMatch bool   `json:"exit_code_match"`
	StdoutMatch   bool   `json:"stdout_match"`
	StderrMatch   bool   `json:"stderr_match"`
	StdoutDiff    []Line `json:"stdout_diff,omitempty"`
	StderrDiff    []Line `json:"stderr_diff,omitempty"`
}

// Match reports whether the result is equivalent to the reference.
func (c Comparison) Match() bool { return c.ExitCodeMatch && c.StdoutMatch && c.StderrMatch }

// Compare compares got with the reference result ref.
func Compare(ref, got backend.Result, opt Options) Comparison {
	return Comparison{
		ExitCodeMatch: ref.ExitCode == got.ExitCode,
		StdoutMatch:   ref.Stdout == got.Stdout,
		StderrMatch:   ref.Stderr == got.Stderr,
	}
}

// Unified renders a diff with context lines around each change.
func Unified(lines []Line, context int) []string {
	var out []string
	for _, l := range lines {
		out = append(out, l.Text)
	}
	return out
}
