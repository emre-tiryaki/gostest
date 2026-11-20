package compare

import (
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/emre-tiryaki/gostest/internal/backend"
)

func eq(s string) Line  { return Line{Op: Equal, Text: s} }
func del(s string) Line { return Line{Op: Delete, Text: s} }
func ins(s string) Line { return Line{Op: Insert, Text: s} }

// sides rebuilds both inputs from a diff.
func sides(lines []Line) (ref, got []Line) {
	for _, l := range lines {
		plain := Line{Text: l.Text, NoEOL: l.NoEOL}
		if l.Op != Insert {
			ref = append(ref, plain)
		}
		if l.Op != Delete {
			got = append(got, plain)
		}
	}
	return ref, got
}

func asLines(s string) []Line {
	var out []Line
	for _, l := range SplitLines(s) {
		out = append(out, Line{Text: l.Text, NoEOL: l.NoEOL})
	}
	return out
}

func lcsLen(a, b []Line) int {
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	return dp[0][0]
}

func countOp(lines []Line, op Op) int {
	n := 0
	for _, l := range lines {
		if l.Op == op {
			n++
		}
	}
	return n
}

func checkDiff(t *testing.T, a, b string) []Line {
	t.Helper()
	d := Diff(a, b)
	ref, got := sides(d)
	if !reflect.DeepEqual(ref, asLines(a)) {
		t.Fatalf("diff does not reproduce the reference side\n a=%q\n diff=%v", a, d)
	}
	if !reflect.DeepEqual(got, asLines(b)) {
		t.Fatalf("diff does not reproduce the target side\n b=%q\n diff=%v", b, d)
	}
	return d
}

func TestSplitLines(t *testing.T) {
	tests := []struct {
		in   string
		want []Line
	}{
		{"", nil},
		{"a\n", []Line{eq("a")}},
		{"a", []Line{{Text: "a", NoEOL: true}}},
		{"a\nb\n", []Line{eq("a"), eq("b")}},
		{"a\nb", []Line{eq("a"), {Text: "b", NoEOL: true}}},
		{"\n", []Line{eq("")}},
		{"\n\n", []Line{eq(""), eq("")}},
	}
	for _, tt := range tests {
		if got := SplitLines(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("SplitLines(%q) = %#v, want %#v", tt.in, got, tt.want)
		}
	}
}

func TestDiffCases(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want []Line
	}{
		{"both empty", "", "", nil},
		{"identical", "a\nb\n", "a\nb\n", []Line{eq("a"), eq("b")}},
		{"insert into empty", "", "a\n", []Line{ins("a")}},
		{"delete to empty", "a\n", "", []Line{del("a")}},
		{"replace middle", "a\nb\nc\n", "a\nx\nc\n", []Line{eq("a"), del("b"), ins("x"), eq("c")}},
		{"insert middle", "a\nc\n", "a\nb\nc\n", []Line{eq("a"), ins("b"), eq("c")}},
		{"delete middle", "a\nb\nc\n", "a\nc\n", []Line{eq("a"), del("b"), eq("c")}},
		{"deletes before inserts", "a\nb\nc\n", "a\nx\ny\nc\n", []Line{eq("a"), del("b"), ins("x"), ins("y"), eq("c")}},
		{"append", "a\n", "a\nb\n", []Line{eq("a"), ins("b")}},
		{"prepend", "b\n", "a\nb\n", []Line{ins("a"), eq("b")}},
		{"missing trailing newline", "a\n", "a", []Line{del("a"), {Op: Insert, Text: "a", NoEOL: true}}},
		{"readme example", "matched\n", "", []Line{del("matched")}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := checkDiff(t, tt.a, tt.b)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Diff(%q, %q)\n got: %v\nwant: %v", tt.a, tt.b, got, tt.want)
			}
		})
	}
}

func TestDiffIsMinimal(t *testing.T) {
	tests := [][2]string{
		{"a\nb\n", "b\na\n"},
		{"a\nb\nc\nd\n", "b\nd\na\nc\n"},
		{"x\ny\nz\n", "1\n2\n3\n"},
		{"a\na\na\n", "a\nb\na\n"},
	}
	for _, tt := range tests {
		d := checkDiff(t, tt[0], tt[1])
		if want := lcsLen(asLines(tt[0]), asLines(tt[1])); countOp(d, Equal) != want {
			t.Errorf("Diff(%q, %q) keeps %d lines, LCS is %d: %v", tt[0], tt[1], countOp(d, Equal), want, d)
		}
	}
}

func TestDiffRandomized(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	gen := func() string {
		var b strings.Builder
		for range r.IntN(12) {
			b.WriteString(string(rune('a' + r.IntN(4))))
			b.WriteByte('\n')
		}
		if r.IntN(4) == 0 {
			b.WriteString("tail")
		}
		return b.String()
	}
	for range 500 {
		a, b := gen(), gen()
		d := checkDiff(t, a, b)
		if want := lcsLen(asLines(a), asLines(b)); countOp(d, Equal) != want {
			t.Fatalf("Diff(%q, %q) keeps %d lines, LCS is %d", a, b, countOp(d, Equal), want)
		}
	}
}

func numbered(n int, prefix string) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "%s%d\n", prefix, i)
	}
	return b.String()
}

func TestDiffLargeSimilarInputs(t *testing.T) {
	a := numbered(20000, "line ")
	b := strings.Replace(a, "line 10000\n", "changed\n", 1)
	start := time.Now()
	d := checkDiff(t, a, b)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("diff took %v", elapsed)
	}
	if countOp(d, Delete) != 1 || countOp(d, Insert) != 1 {
		t.Fatalf("got %d deletes and %d inserts, want 1 each", countOp(d, Delete), countOp(d, Insert))
	}
}

func TestDiffLargeDifferentInputs(t *testing.T) {
	a := numbered(5000, "a")
	b := numbered(5000, "b")
	start := time.Now()
	checkDiff(t, a, b)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("diff took %v", elapsed)
	}
}

func TestCompareIdentical(t *testing.T) {
	r := backend.Result{Stdout: "hi\n", Stderr: "warn\n", ExitCode: 1}
	c := Compare(r, r, Options{})
	if !c.Match() || !c.ExitCodeMatch || !c.StdoutMatch || !c.StderrMatch {
		t.Fatalf("identical results should match: %+v", c)
	}
	if c.StdoutDiff != nil || c.StderrDiff != nil {
		t.Fatalf("no diff expected for identical results: %+v", c)
	}
}

func TestCompareIgnoresDurationAndTruncation(t *testing.T) {
	ref := backend.Result{Stdout: "x", Duration: time.Second}
	got := backend.Result{Stdout: "x", Duration: time.Minute}
	if !Compare(ref, got, Options{}).Match() {
		t.Fatal("durations must not affect the comparison")
	}
}

func TestCompareExitCode(t *testing.T) {
	c := Compare(backend.Result{ExitCode: 0}, backend.Result{ExitCode: 127}, Options{})
	if c.Match() || c.ExitCodeMatch {
		t.Fatalf("exit codes differ: %+v", c)
	}
	if !c.StdoutMatch || !c.StderrMatch {
		t.Fatalf("outputs are equal: %+v", c)
	}
}

func TestCompareTimeoutIsNotARealExitCode(t *testing.T) {
	ref := backend.Result{ExitCode: backend.ExitTimeout}
	got := backend.Result{ExitCode: backend.ExitTimeout, TimedOut: true}
	if c := Compare(ref, got, Options{}); c.ExitCodeMatch {
		t.Fatal("a timeout must not match a command that really exited 124")
	}
	both := backend.Result{ExitCode: backend.ExitTimeout, TimedOut: true}
	if c := Compare(both, both, Options{}); !c.ExitCodeMatch {
		t.Fatal("two timeouts match")
	}
}

func TestCompareStdout(t *testing.T) {
	c := Compare(backend.Result{Stdout: "a\nb\n"}, backend.Result{Stdout: "a\nc\n"}, Options{})
	if c.Match() || c.StdoutMatch {
		t.Fatalf("stdout differs: %+v", c)
	}
	want := []Line{eq("a"), del("b"), ins("c")}
	if !reflect.DeepEqual(c.StdoutDiff, want) {
		t.Fatalf("StdoutDiff = %v, want %v", c.StdoutDiff, want)
	}
	if c.StderrDiff != nil {
		t.Fatalf("StderrDiff = %v, want nil", c.StderrDiff)
	}
}

func TestCompareStderr(t *testing.T) {
	c := Compare(backend.Result{}, backend.Result{Stderr: "oops\n"}, Options{})
	if c.Match() || c.StderrMatch {
		t.Fatalf("stderr differs: %+v", c)
	}
	if !reflect.DeepEqual(c.StderrDiff, []Line{ins("oops")}) {
		t.Fatalf("StderrDiff = %v", c.StderrDiff)
	}
}

func TestCompareIgnoreStderr(t *testing.T) {
	c := Compare(backend.Result{}, backend.Result{Stderr: "oops\n"}, Options{IgnoreStderr: true})
	if !c.Match() || !c.StderrMatch || c.StderrDiff != nil {
		t.Fatalf("stderr should be ignored: %+v", c)
	}
}

func TestCompareReadmeExample(t *testing.T) {
	ref := backend.Result{Stdout: "matched\n"}
	got := backend.Result{Stderr: "sh: 1: [[: not found\n"}
	c := Compare(ref, got, Options{})
	if c.Match() {
		t.Fatal("dash result must not match the bash reference even though both exit 0")
	}
	if !c.ExitCodeMatch {
		t.Fatal("both exit codes are 0")
	}
	if !reflect.DeepEqual(c.StdoutDiff, []Line{del("matched")}) {
		t.Errorf("StdoutDiff = %v", c.StdoutDiff)
	}
	if !reflect.DeepEqual(c.StderrDiff, []Line{ins("sh: 1: [[: not found")}) {
		t.Errorf("StderrDiff = %v", c.StderrDiff)
	}
}

func TestCompareNormalizesLineEndings(t *testing.T) {
	c := Compare(backend.Result{Stdout: "a\r\nb\r\n"}, backend.Result{Stdout: "a\nb\n"}, Options{})
	if !c.Match() {
		t.Fatalf("CRLF and LF output should match: %+v", c)
	}
}

func TestUnifiedNoChanges(t *testing.T) {
	if got := Unified([]Line{eq("a"), eq("b")}, 3); got != nil {
		t.Fatalf("Unified without changes = %q, want nil", got)
	}
	if got := Unified(nil, 3); got != nil {
		t.Fatalf("Unified(nil) = %q, want nil", got)
	}
}

func TestUnified(t *testing.T) {
	lines := []Line{eq("a"), eq("b"), eq("c"), del("d"), ins("e"), eq("f"), eq("g"), eq("h")}
	tests := []struct {
		name    string
		context int
		want    []string
	}{
		{"context 1", 1, []string{"  … 2 unchanged lines", "  c", "- d", "+ e", "  f", "  … 2 unchanged lines"}},
		{"context 3 shows all", 3, []string{"  a", "  b", "  c", "- d", "+ e", "  f", "  g", "  h"}},
		{"context 2 never hides a single line", 2, []string{"  a", "  b", "  c", "- d", "+ e", "  f", "  g", "  h"}},
		{"context 0", 0, []string{"  … 3 unchanged lines", "- d", "+ e", "  … 3 unchanged lines"}},
		{"negative context is 0", -5, []string{"  … 3 unchanged lines", "- d", "+ e", "  … 3 unchanged lines"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Unified(lines, tt.context)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Unified(context=%d)\n got: %q\nwant: %q", tt.context, got, tt.want)
			}
		})
	}
}

func TestUnifiedCollapsesBetweenChanges(t *testing.T) {
	lines := []Line{del("x"), eq("1"), eq("2"), eq("3"), eq("4"), eq("5"), ins("y")}
	want := []string{"- x", "  1", "  … 3 unchanged lines", "  5", "+ y"}
	if got := Unified(lines, 1); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
	short := []Line{del("x"), eq("1"), eq("2"), eq("3"), ins("y")}
	wantShort := []string{"- x", "  1", "  2", "  3", "+ y"}
	if got := Unified(short, 1); !reflect.DeepEqual(got, wantShort) {
		t.Fatalf("got %q, want %q", got, wantShort)
	}
}

func TestUnifiedNoNewlineMarker(t *testing.T) {
	got := Unified([]Line{del("a"), {Op: Insert, Text: "a", NoEOL: true}}, 3)
	want := []string{"- a", "+ a", `\ no newline at end`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestOpString(t *testing.T) {
	for op, want := range map[Op]string{Equal: "equal", Delete: "delete", Insert: "insert", Op(9): "Op(9)"} {
		if got := op.String(); got != want {
			t.Errorf("Op(%d).String() = %q, want %q", op, got, want)
		}
	}
}

func TestOpJSON(t *testing.T) {
	b, err := Insert.MarshalText()
	if err != nil || string(b) != "insert" {
		t.Fatalf("MarshalText = %q, %v", b, err)
	}
	var op Op
	if err := op.UnmarshalText([]byte("delete")); err != nil || op != Delete {
		t.Fatalf("UnmarshalText(delete) = %v, %v", op, err)
	}
	if err := op.UnmarshalText([]byte("bogus")); err == nil {
		t.Fatal("UnmarshalText(bogus) should fail")
	}
}

func TestSameLines(t *testing.T) {
	for _, tt := range []struct {
		a, b string
		want bool
	}{
		{"", "", true},
		{"a\nb\n", "a\nb\n", true},
		{"a\nb\n", "b\na\n", true},
		{"a\nb", "b\na", true},
		{"B\na\nC\n", "a\nB\nC\n", true},
		{"a\na\nb\n", "a\nb\na\n", true},
		{"a\na\nb\n", "a\nb\nb\n", false}, // duplicates are counted
		{"a\nb\n", "a\n", false},
		{"a\n", "a\nb\n", false},
		{"a\nb\n", "b\na", false}, // trailing newline differs
		{"a\nb\n", "a\nc\n", false},
		{"", "\n", false},
	} {
		if got := SameLines(tt.a, tt.b); got != tt.want {
			t.Errorf("SameLines(%q, %q) = %v, want %v", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestCompareReorderedStdoutMatches(t *testing.T) {
	ref := backend.Result{Stdout: "CLAUDE.md\ngo.mod\nMakefile\n"}
	got := backend.Result{Stdout: "CLAUDE.md\nMakefile\ngo.mod\n"}
	c := Compare(ref, got, Options{})
	if !c.Match() || !c.StdoutMatch {
		t.Fatalf("reordered stdout should match by default: %+v", c)
	}
	if !c.StdoutReordered || c.StderrReordered || !c.Reordered() {
		t.Fatalf("stdout should be marked as reordered: %+v", c)
	}
	if c.StdoutDiff != nil {
		t.Fatalf("a matching stream has no diff: %v", c.StdoutDiff)
	}
}

func TestCompareReorderedStderrMatches(t *testing.T) {
	c := Compare(backend.Result{Stderr: "x\ny\n"}, backend.Result{Stderr: "y\nx\n"}, Options{})
	if !c.Match() || !c.StderrReordered || c.StdoutReordered || c.StderrDiff != nil {
		t.Fatalf("reordered stderr should match by default: %+v", c)
	}
}

func TestCompareStrictOrder(t *testing.T) {
	ref := backend.Result{Stdout: "a\nb\n", Stderr: "x\ny\n"}
	got := backend.Result{Stdout: "b\na\n", Stderr: "y\nx\n"}
	c := Compare(ref, got, Options{StrictOrder: true})
	if c.Match() || c.StdoutMatch || c.StderrMatch {
		t.Fatalf("strict order must fail reordered streams: %+v", c)
	}
	if c.Reordered() {
		t.Fatalf("strict order must not mark streams as reordered: %+v", c)
	}
	if c.StdoutDiff == nil || c.StderrDiff == nil {
		t.Fatalf("strict order must keep the diffs: %+v", c)
	}
}

func TestCompareIdenticalIsNotReordered(t *testing.T) {
	r := backend.Result{Stdout: "a\nb\n", Stderr: "x\n"}
	if c := Compare(r, r, Options{}); !c.Match() || c.Reordered() {
		t.Fatalf("identical results: %+v", c)
	}
}

func TestCompareDifferentContentStillFails(t *testing.T) {
	c := Compare(backend.Result{Stdout: "a\nb\n"}, backend.Result{Stdout: "b\nc\n"}, Options{})
	if c.Match() || c.StdoutReordered || c.StdoutDiff == nil {
		t.Fatalf("different lines must still fail: %+v", c)
	}
}

func TestCompareIgnoredStderrIsNotReordered(t *testing.T) {
	c := Compare(backend.Result{Stderr: "x\ny\n"}, backend.Result{Stderr: "y\nx\n"}, Options{IgnoreStderr: true})
	if !c.Match() || c.StderrReordered {
		t.Fatalf("ignored stderr is not compared, so it is not reordered: %+v", c)
	}
}
