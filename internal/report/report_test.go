package report

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/emre-tiryaki/gostest/internal/backend"
	"github.com/emre-tiryaki/gostest/internal/compare"
	"github.com/emre-tiryaki/gostest/internal/distro"
	"github.com/emre-tiryaki/gostest/internal/engine"
	"github.com/emre-tiryaki/gostest/internal/explain"
)

func outcome(name, shell string, status engine.Status) engine.Outcome {
	d := distro.Distro{Name: name, Image: name + ":latest", PackageManager: distro.APK}
	return engine.Outcome{
		Distro: d,
		Env:    backend.Environment{Distro: d, Image: d.Image, Shell: shell},
		Status: status,
		Result: backend.Result{Duration: 450 * time.Millisecond},
	}
}

func sampleReport() engine.Report {
	ref := backend.Result{Stdout: "matched\n", Duration: 3 * time.Millisecond}

	pass := outcome("archlinux", "bash", engine.Pass)
	pass.Result.Stdout = "matched\n"
	pass.Comparison = compare.Compare(ref, pass.Result, compare.Options{})

	fail := outcome("debian", "dash", engine.Fail)
	fail.Result = backend.Result{Stderr: "sh: 1: [[: not found\n", Duration: 1200 * time.Millisecond}
	fail.Comparison = compare.Compare(ref, fail.Result, compare.Options{})
	fail.Reasons = []explain.Reason{
		{Kind: explain.KindBashism, Message: "`[[` is not available here"},
		{Kind: explain.KindStdout, Message: "stdout differs from the reference"},
	}

	errOut := outcome("gentoo", "", engine.Error)
	errOut.Error = "gentoo: installing curl failed:\n!!! no ebuilds\nemerge: there are no ebuilds"

	return engine.Report{
		Command:   `if [[ $name == ani* ]]; then echo matched; fi`,
		Reference: engine.Reference{Shell: "bash", Result: ref},
		Outcomes:  []engine.Outcome{pass, fail, errOut},
	}
}

func text(t *testing.T, rep engine.Report, opt TextOptions) string {
	t.Helper()
	var b bytes.Buffer
	if err := Text(&b, rep, opt); err != nil {
		t.Fatalf("Text: %v", err)
	}
	return b.String()
}

func lineWith(t *testing.T, out, substr string) string {
	t.Helper()
	for l := range strings.SplitSeq(out, "\n") {
		if strings.Contains(l, substr) {
			return l
		}
	}
	t.Fatalf("no line containing %q in:\n%s", substr, out)
	return ""
}

func TestTextHeader(t *testing.T) {
	out := text(t, sampleReport(), TextOptions{})
	lineWith(t, out, "if [[ $name == ani* ]]; then echo matched; fi")
	ref := lineWith(t, out, "reference")
	for _, want := range []string{"host", "/bin/sh = bash", "exit 0", "3ms"} {
		if !strings.Contains(ref, want) {
			t.Errorf("reference line %q should contain %q", ref, want)
		}
	}
}

func TestTextStatusLines(t *testing.T) {
	out := text(t, sampleReport(), TextOptions{})
	pass := lineWith(t, out, "archlinux")
	for _, want := range []string{"PASS", "/bin/sh = bash", "exit 0", "450ms"} {
		if !strings.Contains(pass, want) {
			t.Errorf("pass line %q should contain %q", pass, want)
		}
	}
	fail := lineWith(t, out, "debian")
	for _, want := range []string{"FAIL", "/bin/sh = dash", "exit 0", "1.2s"} {
		if !strings.Contains(fail, want) {
			t.Errorf("fail line %q should contain %q", fail, want)
		}
	}
	errLine := lineWith(t, out, "gentoo")
	for _, want := range []string{"ERROR", "installing curl failed"} {
		if !strings.Contains(errLine, want) {
			t.Errorf("error line %q should contain %q", errLine, want)
		}
	}
}

func TestTextColumnsAreAligned(t *testing.T) {
	out := text(t, sampleReport(), TextOptions{})
	a := lineWith(t, out, "archlinux")
	d := lineWith(t, out, "debian")
	if strings.Index(a, "/bin/sh") != strings.Index(d, "/bin/sh") {
		t.Fatalf("columns not aligned:\n%s\n%s", a, d)
	}
}

func TestTextFailureDetails(t *testing.T) {
	out := text(t, sampleReport(), TextOptions{})
	for _, want := range []string{
		"why:",
		"• `[[` is not available here",
		"• stdout differs from the reference",
		"stdout:",
		"- matched",
		"stderr:",
		"+ sh: 1: [[: not found",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report should contain %q:\n%s", want, out)
		}
	}
	// Details belong to the failing distro, after its status line.
	if strings.Index(out, "why:") < strings.Index(out, "debian") {
		t.Error("details should follow the failing distro's line")
	}
}

func TestTextPassHasNoDetails(t *testing.T) {
	rep := sampleReport()
	rep.Outcomes = rep.Outcomes[:1]
	out := text(t, rep, TextOptions{})
	for _, unwanted := range []string{"why:", "stdout:", "stderr:"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("passing report should not contain %q:\n%s", unwanted, out)
		}
	}
}

func TestTextMultiLineErrorIsIndented(t *testing.T) {
	out := text(t, sampleReport(), TextOptions{})
	l := lineWith(t, out, "!!! no ebuilds")
	if !strings.HasPrefix(l, "       ") {
		t.Fatalf("continuation line should be indented: %q", l)
	}
}

func TestTextSummary(t *testing.T) {
	out := text(t, sampleReport(), TextOptions{})
	lineWith(t, out, "1 passed, 1 failed, 1 error")

	rep := sampleReport()
	rep.Outcomes = append(rep.Outcomes, rep.Outcomes[2], rep.Outcomes[0])
	out = text(t, rep, TextOptions{})
	lineWith(t, out, "2 passed, 1 failed, 2 errors")
}

func TestTextCachedAndTimeout(t *testing.T) {
	rep := sampleReport()
	rep.Outcomes[0].Cached = true
	rep.Outcomes[1].Result.TimedOut = true
	rep.Outcomes[1].Result.ExitCode = backend.ExitTimeout
	out := text(t, rep, TextOptions{})
	if l := lineWith(t, out, "archlinux"); !strings.Contains(l, "(cached)") {
		t.Errorf("cached marker missing: %q", l)
	}
	if l := lineWith(t, out, "debian"); !strings.Contains(l, "timed out") || strings.Contains(l, "exit 124") {
		t.Errorf("timeouts should read 'timed out', not an exit code: %q", l)
	}
}

func TestTextUnknownShell(t *testing.T) {
	rep := sampleReport()
	rep.Outcomes[0].Env.Shell = "unknown"
	out := text(t, rep, TextOptions{})
	if l := lineWith(t, out, "archlinux"); !strings.Contains(l, "/bin/sh = ?") {
		t.Errorf("unknown shells should be shown as ?: %q", l)
	}
}

func TestTextNoColor(t *testing.T) {
	if out := text(t, sampleReport(), TextOptions{}); strings.Contains(out, "\x1b[") {
		t.Fatalf("uncoloured output contains escape codes:\n%q", out)
	}
}

func TestTextColor(t *testing.T) {
	out := text(t, sampleReport(), TextOptions{Color: true})
	for _, want := range []string{green + "PASS", red + "FAIL", yellow + "ERROR", red + "- matched", green + "+ sh: 1: [[: not found", reset} {
		if !strings.Contains(out, want) {
			t.Errorf("coloured output should contain %q", want)
		}
	}
}

func TestTextVerbose(t *testing.T) {
	rep := sampleReport()
	rep.Reference.Result.Stderr = "ref warning\n"
	quiet := text(t, rep, TextOptions{})
	verbose := text(t, rep, TextOptions{Verbose: true})
	if strings.Contains(quiet, "ref warning") {
		t.Error("reference output is only shown in verbose mode")
	}
	for _, want := range []string{"ref warning", "│ matched"} {
		if !strings.Contains(verbose, want) {
			t.Errorf("verbose report should contain %q:\n%s", want, verbose)
		}
	}
}

func TestTextDiffContext(t *testing.T) {
	var refOut strings.Builder
	for i := range 30 {
		refOut.WriteString("line\n")
		_ = i
	}
	ref := backend.Result{Stdout: refOut.String() + "end\n"}
	got := backend.Result{Stdout: refOut.String() + "END\n"}
	o := outcome("debian", "dash", engine.Fail)
	o.Result = got
	o.Comparison = compare.Compare(ref, got, compare.Options{})
	rep := engine.Report{Command: "x", Reference: engine.Reference{Result: ref}, Outcomes: []engine.Outcome{o}}

	out := text(t, rep, TextOptions{Context: 2})
	if !strings.Contains(out, "… 28 unchanged lines") {
		t.Fatalf("long unchanged runs should be collapsed:\n%s", out)
	}
	if strings.Count(out, "  line") > 3 {
		t.Fatalf("too much context shown:\n%s", out)
	}
}

func TestTextHugeDiffIsCapped(t *testing.T) {
	var a, b strings.Builder
	for range 1000 {
		a.WriteString("a\n")
		b.WriteString("b\n")
	}
	ref := backend.Result{Stdout: a.String()}
	got := backend.Result{Stdout: b.String()}
	o := outcome("debian", "dash", engine.Fail)
	o.Result = got
	o.Comparison = compare.Compare(ref, got, compare.Options{})
	rep := engine.Report{Command: "x", Reference: engine.Reference{Result: ref}, Outcomes: []engine.Outcome{o}}
	out := text(t, rep, TextOptions{})
	if n := strings.Count(out, "\n"); n > MaxDiffLines+20 {
		t.Fatalf("diff should be capped, got %d lines", n)
	}
	if !strings.Contains(out, "more diff lines") {
		t.Fatalf("capped diff should say so:\n%s", out[len(out)-300:])
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestTextWriteError(t *testing.T) {
	if err := Text(failingWriter{}, sampleReport(), TextOptions{}); err == nil {
		t.Fatal("write errors must be returned")
	}
}

func TestJSON(t *testing.T) {
	var b bytes.Buffer
	if err := JSON(&b, sampleReport()); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Command string `json:"command"`
		OK      bool   `json:"ok"`
		Summary struct {
			Passed int `json:"passed"`
			Failed int `json:"failed"`
			Errors int `json:"errors"`
		} `json:"summary"`
		Reference struct {
			Shell  string `json:"shell"`
			Result struct {
				Stdout     string  `json:"stdout"`
				DurationMS float64 `json:"duration_ms"`
			} `json:"result"`
		} `json:"reference"`
		Outcomes []struct {
			Distro struct {
				Name string `json:"name"`
			} `json:"distro"`
			Status     string `json:"status"`
			Error      string `json:"error"`
			Comparison struct {
				StdoutDiff []struct {
					Op   string `json:"op"`
					Text string `json:"text"`
				} `json:"stdout_diff"`
			} `json:"comparison"`
			Reasons []struct {
				Kind string `json:"kind"`
			} `json:"reasons"`
		} `json:"outcomes"`
	}
	if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, b.String())
	}
	if doc.Command == "" || doc.OK {
		t.Errorf("command/ok wrong: %+v", doc)
	}
	if doc.Summary.Passed != 1 || doc.Summary.Failed != 1 || doc.Summary.Errors != 1 {
		t.Errorf("summary = %+v", doc.Summary)
	}
	if doc.Reference.Shell != "bash" || doc.Reference.Result.Stdout != "matched\n" || doc.Reference.Result.DurationMS != 3 {
		t.Errorf("reference = %+v", doc.Reference)
	}
	if len(doc.Outcomes) != 3 || doc.Outcomes[1].Status != "FAIL" || doc.Outcomes[2].Error == "" {
		t.Fatalf("outcomes = %+v", doc.Outcomes)
	}
	if d := doc.Outcomes[1].Comparison.StdoutDiff; len(d) != 1 || d[0].Op != "delete" || d[0].Text != "matched" {
		t.Errorf("stdout diff = %+v", d)
	}
	if r := doc.Outcomes[1].Reasons; len(r) != 2 || r[0].Kind != "bashism" {
		t.Errorf("reasons = %+v", r)
	}
}

func TestJSONIsIndented(t *testing.T) {
	var b bytes.Buffer
	_ = JSON(&b, sampleReport())
	if !strings.Contains(b.String(), "\n  \"command\"") {
		t.Fatal("JSON output should be indented for readability")
	}
}

func TestFormatDuration(t *testing.T) {
	tests := map[time.Duration]string{
		0:                       "0ms",
		3 * time.Millisecond:    "3ms",
		450 * time.Millisecond:  "450ms",
		999 * time.Millisecond:  "999ms",
		1200 * time.Millisecond: "1.2s",
		59 * time.Second:        "59.0s",
		65 * time.Second:        "1m5s",
		500 * time.Microsecond:  "0.5ms",
	}
	for d, want := range tests {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%v) = %q, want %q", d, got, want)
		}
	}
}

func reorderedReport() engine.Report {
	rep := sampleReport()
	pass := rep.Outcomes[0]
	pass.Notes = []explain.Reason{{Kind: explain.KindReordered, Message: "stdout has the same lines in a different order"}}
	rep.Outcomes[0] = pass
	return rep
}

func TestTextNotesOnPass(t *testing.T) {
	out := text(t, reorderedReport(), TextOptions{})
	if !strings.Contains(out, "note:") || !strings.Contains(out, "different order") {
		t.Fatalf("passing outcome should show its notes:\n%s", out)
	}
	if strings.Index(out, "note:") > strings.Index(out, "debian") {
		t.Error("the note should follow its own distro's line")
	}
	if l := lineWith(t, out, "archlinux"); !strings.HasPrefix(l, "PASS") {
		t.Errorf("status should stay PASS: %q", l)
	}
}

func TestTextNoNotesWithoutThem(t *testing.T) {
	if out := text(t, sampleReport(), TextOptions{}); strings.Contains(out, "note:") {
		t.Fatalf("unexpected note:\n%s", out)
	}
}

func TestJSONNotes(t *testing.T) {
	var b bytes.Buffer
	if err := JSON(&b, reorderedReport()); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Outcomes []struct {
			Notes []struct {
				Kind string `json:"kind"`
			} `json:"notes"`
		} `json:"outcomes"`
	}
	if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if n := doc.Outcomes[0].Notes; len(n) != 1 || n[0].Kind != "reordered" {
		t.Fatalf("notes = %+v", n)
	}
	if doc.Outcomes[1].Notes != nil {
		t.Fatalf("outcome without notes: %+v", doc.Outcomes[1].Notes)
	}
}

func TestJSONProvided(t *testing.T) {
	rep := engine.Report{
		Command: "jq",
		Outcomes: []engine.Outcome{{
			Distro: distro.Distro{Name: "alpine"},
			Status: engine.Pass,
			Env:    backend.Environment{Image: "gostest/alpine:abc", Packages: []string{"bash"}, Provided: []string{"jq"}},
		}},
	}
	var b bytes.Buffer
	if err := JSON(&b, rep); err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Outcomes []struct {
			Environment struct {
				Packages []string `json:"packages"`
				Provided []string `json:"provided"`
			} `json:"environment"`
		} `json:"outcomes"`
	}
	if err := json.Unmarshal(b.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	env := doc.Outcomes[0].Environment
	if len(env.Provided) != 1 || env.Provided[0] != "jq" || len(env.Packages) != 1 {
		t.Fatalf("environment = %+v, want packages and provided commands", env)
	}
}
