// Package report renders engine reports for humans (text) and machines
// (JSON).
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/emre-tiryaki/gostest/internal/backend"
	"github.com/emre-tiryaki/gostest/internal/compare"
	"github.com/emre-tiryaki/gostest/internal/distro"
	"github.com/emre-tiryaki/gostest/internal/engine"
	"github.com/emre-tiryaki/gostest/internal/explain"
)

// ANSI escape sequences used when colour is enabled.
const (
	reset  = "\x1b[0m"
	bold   = "\x1b[1m"
	dim    = "\x1b[2m"
	red    = "\x1b[31m"
	green  = "\x1b[32m"
	yellow = "\x1b[33m"
)

// MaxDiffLines caps the rendered diff of one stream.
const MaxDiffLines = 40

// DefaultContext is the recommended number of unchanged lines shown around
// each change.
const DefaultContext = 3

// indent aligns detail lines with the distro name column.
const indent = "       "

// TextOptions tunes the text report.
type TextOptions struct {
	// Color enables ANSI colours.
	Color bool
	// Verbose also prints the full output of the reference and of every
	// distribution.
	Verbose bool
	// Context is the number of unchanged lines shown around changes.
	Context int
}

// writer remembers the first write error so rendering code stays linear.
type writer struct {
	w     io.Writer
	err   error
	color bool
}

func (w *writer) printf(format string, args ...any) {
	if w.err == nil {
		_, w.err = fmt.Fprintf(w.w, format, args...)
	}
}

func (w *writer) paint(color, s string) string {
	if !w.color || s == "" {
		return s
	}
	return color + s + reset
}

// Text writes a human readable report: the command and reference, one line
// per distribution, details for failures and a summary.
func Text(out io.Writer, rep engine.Report, opt TextOptions) error {
	w := &writer{w: out, color: opt.Color}

	w.printf("%s %s\n", w.paint(dim, "$"), w.paint(bold, rep.Command))
	ref := rep.Reference
	w.printf("%s\n", w.paint(dim, fmt.Sprintf("reference: host, /bin/sh = %s, %s, %s",
		shell(ref.Shell), exitText(ref.Result), formatDuration(ref.Result.Duration))))
	if opt.Verbose {
		writeOutput(w, ref.Result)
	}
	w.printf("\n")

	nameWidth, shellWidth, exitWidth := 0, 0, 0
	for _, o := range rep.Outcomes {
		nameWidth = max(nameWidth, len(o.Distro.Name))
		if o.Status != engine.Error {
			shellWidth = max(shellWidth, len(shellColumn(o)))
			exitWidth = max(exitWidth, len(exitText(o.Result)))
		}
	}

	for _, o := range rep.Outcomes {
		status := w.paint(statusColor(o.Status), string(o.Status)) + strings.Repeat(" ", 5-len(o.Status))
		name := fmt.Sprintf("%-*s", nameWidth, o.Distro.Name)
		if o.Status == engine.Error {
			lines := strings.Split(strings.TrimRight(o.Error, "\n"), "\n")
			w.printf("%s  %s  %s\n", status, name, lines[0])
			for _, l := range lines[1:] {
				w.printf("%s%s\n", indent, w.paint(dim, l))
			}
			continue
		}
		line := fmt.Sprintf("%-*s  %-*s  %s", shellWidth, shellColumn(o), exitWidth, exitText(o.Result), formatDuration(o.Result.Duration))
		if o.Cached {
			line += " (cached)"
		}
		w.printf("%s  %s  %s\n", status, name, w.paint(dim, line))
		if opt.Verbose {
			writeOutput(w, o.Result)
		}
		for _, n := range o.Notes {
			w.printf("%snote: %s\n", indent, w.paint(dim, n.Message))
		}
		if o.Status == engine.Fail {
			writeDetails(w, o, opt.Context)
		}
	}

	p, f, e := rep.Counts()
	summary := fmt.Sprintf("%d passed, %d failed, %d %s", p, f, e, plural(e, "error", "errors"))
	color := green
	if f > 0 || e > 0 {
		color = red
	}
	w.printf("\n%s\n", w.paint(color, summary))
	return w.err
}

func writeOutput(w *writer, r backend.Result) {
	for _, s := range []struct{ label, text string }{{"stdout", r.Stdout}, {"stderr", r.Stderr}} {
		for i, l := range compare.SplitLines(s.text) {
			label := "      "
			if i == 0 {
				label = s.label
			}
			w.printf("%s%s %s %s\n", indent, w.paint(dim, label), w.paint(dim, "│"), l.Text)
		}
	}
}

func writeDetails(w *writer, o engine.Outcome, context int) {
	if len(o.Reasons) > 0 {
		w.printf("%swhy:\n", indent)
		for _, r := range o.Reasons {
			w.printf("%s  %s %s\n", indent, w.paint(yellow, "•"), r.Message)
		}
	}
	for _, s := range []struct {
		label string
		diff  []compare.Line
	}{{"stdout", o.Comparison.StdoutDiff}, {"stderr", o.Comparison.StderrDiff}} {
		lines := compare.Unified(s.diff, context)
		if len(lines) == 0 {
			continue
		}
		w.printf("%s%s:\n", indent, s.label)
		if len(lines) > MaxDiffLines {
			more := len(lines) - MaxDiffLines
			lines = append(lines[:MaxDiffLines:MaxDiffLines], fmt.Sprintf("  … %d more diff lines", more))
		}
		for _, l := range lines {
			w.printf("%s  %s\n", indent, paintDiff(w, l))
		}
	}
}

func paintDiff(w *writer, l string) string {
	switch {
	case strings.HasPrefix(l, "- "):
		return w.paint(red, l)
	case strings.HasPrefix(l, "+ "):
		return w.paint(green, l)
	case strings.HasPrefix(l, "  … "), strings.HasPrefix(l, `\ `):
		return w.paint(dim, l)
	}
	return l
}

func statusColor(s engine.Status) string {
	switch s {
	case engine.Pass:
		return green
	case engine.Fail:
		return red
	}
	return yellow
}

func shell(s string) string {
	if s == "" || s == "unknown" {
		return "?"
	}
	return s
}

func shellColumn(o engine.Outcome) string { return "/bin/sh = " + shell(o.Env.Shell) }

func exitText(r backend.Result) string {
	if r.TimedOut {
		return "timed out"
	}
	return fmt.Sprintf("exit %d", r.ExitCode)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func formatDuration(d time.Duration) string {
	switch {
	case d <= 0:
		return "0ms"
	case d < time.Millisecond:
		return fmt.Sprintf("%.1fms", float64(d)/float64(time.Millisecond))
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	return d.Round(time.Second).String()
}

// JSON document layout. Durations are reported in milliseconds.
type (
	jsonDoc struct {
		Command   string        `json:"command"`
		OK        bool          `json:"ok"`
		Summary   jsonSummary   `json:"summary"`
		Reference jsonReference `json:"reference"`
		Outcomes  []jsonOutcome `json:"outcomes"`
	}
	jsonSummary struct {
		Passed int `json:"passed"`
		Failed int `json:"failed"`
		Errors int `json:"errors"`
	}
	jsonReference struct {
		Shell  string     `json:"shell"`
		Result jsonResult `json:"result"`
	}
	jsonResult struct {
		Stdout     string  `json:"stdout"`
		Stderr     string  `json:"stderr"`
		ExitCode   int     `json:"exit_code"`
		TimedOut   bool    `json:"timed_out,omitempty"`
		Truncated  bool    `json:"truncated,omitempty"`
		DurationMS float64 `json:"duration_ms"`
	}
	jsonEnv struct {
		Image    string   `json:"image"`
		ImageID  string   `json:"image_id,omitempty"`
		Shell    string   `json:"shell,omitempty"`
		Packages []string `json:"packages,omitempty"`
		Provided []string `json:"provided,omitempty"`
	}
	jsonOutcome struct {
		Distro      distro.Distro       `json:"distro"`
		Status      engine.Status       `json:"status"`
		Error       string              `json:"error,omitempty"`
		Cached      bool                `json:"cached,omitempty"`
		Environment *jsonEnv            `json:"environment,omitempty"`
		Result      *jsonResult         `json:"result,omitempty"`
		Comparison  *compare.Comparison `json:"comparison,omitempty"`
		Reasons     []explain.Reason    `json:"reasons,omitempty"`
		Notes       []explain.Reason    `json:"notes,omitempty"`
	}
)

func toJSONResult(r backend.Result) jsonResult {
	return jsonResult{
		Stdout:     r.Stdout,
		Stderr:     r.Stderr,
		ExitCode:   r.ExitCode,
		TimedOut:   r.TimedOut,
		Truncated:  r.Truncated,
		DurationMS: float64(r.Duration) / float64(time.Millisecond),
	}
}

// JSON writes the report as an indented JSON document.
func JSON(w io.Writer, rep engine.Report) error {
	p, f, e := rep.Counts()
	doc := jsonDoc{
		Command:   rep.Command,
		OK:        rep.OK(),
		Summary:   jsonSummary{Passed: p, Failed: f, Errors: e},
		Reference: jsonReference{Shell: rep.Reference.Shell, Result: toJSONResult(rep.Reference.Result)},
		Outcomes:  make([]jsonOutcome, 0, len(rep.Outcomes)),
	}
	for _, o := range rep.Outcomes {
		jo := jsonOutcome{Distro: o.Distro, Status: o.Status, Error: o.Error, Cached: o.Cached, Reasons: o.Reasons, Notes: o.Notes}
		if o.Status != engine.Error {
			res := toJSONResult(o.Result)
			cmp := o.Comparison
			jo.Result, jo.Comparison = &res, &cmp
			jo.Environment = &jsonEnv{Image: o.Env.Image, ImageID: o.Env.ImageID, Shell: o.Env.Shell, Packages: o.Env.Packages, Provided: o.Env.Provided}
		}
		doc.Outcomes = append(doc.Outcomes, jo)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}
