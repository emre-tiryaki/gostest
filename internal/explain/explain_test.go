package explain

import (
	"strings"
	"testing"
	"time"

	"github.com/emre-tiryaki/gostest/internal/backend"
	"github.com/emre-tiryaki/gostest/internal/compare"
)

var dashCtx = Context{Distro: "debian", Shell: "dash", RefShell: "bash"}

func explain(ref, got backend.Result, c Context) []Reason {
	return Explain(ref, got, compare.Compare(ref, got, compare.Options{}), c)
}

// explainStderr explains a target that printed stderr where the reference
// printed nothing, with the given exit codes.
func explainStderr(stderr string, refExit, gotExit int, c Context) []Reason {
	return explain(backend.Result{ExitCode: refExit}, backend.Result{Stderr: stderr, ExitCode: gotExit}, c)
}

func find(reasons []Reason, k Kind) (Reason, bool) {
	for _, r := range reasons {
		if r.Kind == k {
			return r, true
		}
	}
	return Reason{}, false
}

func mustFind(t *testing.T, reasons []Reason, k Kind, substrings ...string) Reason {
	t.Helper()
	r, ok := find(reasons, k)
	if !ok {
		t.Fatalf("no %s reason in %+v", k, reasons)
	}
	for _, s := range substrings {
		if !strings.Contains(r.Message, s) {
			t.Errorf("%s reason %q should contain %q", k, r.Message, s)
		}
	}
	return r
}

func mustNotFind(t *testing.T, reasons []Reason, k Kind) {
	t.Helper()
	if r, ok := find(reasons, k); ok {
		t.Fatalf("unexpected %s reason: %+v", k, r)
	}
}

func TestNoReasonsWhenMatching(t *testing.T) {
	r := backend.Result{Stdout: "ok\n", Stderr: "warn\n", ExitCode: 0}
	if got := explain(r, r, dashCtx); len(got) != 0 {
		t.Fatalf("matching results need no explanation: %+v", got)
	}
}

func TestReadmeExampleBashism(t *testing.T) {
	ref := backend.Result{Stdout: "matched\n"}
	got := backend.Result{Stderr: "sh: 1: [[: not found\n"}
	reasons := explain(ref, got, dashCtx)
	mustFind(t, reasons, KindBashism, "`[[`", "dash", "bash", "[ ... ]")
	mustFind(t, reasons, KindSilentError, "exit code 0")
	mustNotFind(t, reasons, KindMissingCommand)
	mustNotFind(t, reasons, KindExitCode)
}

func TestBashismBuiltins(t *testing.T) {
	tests := map[string]string{
		"sh: 1: source: not found":    ". file",
		"sh: 1: function: not found":  "name()",
		"sh: 1: declare: not found":   "declare",
		"sh: 1: typeset: not found":   "typeset",
		"sh: 1: shopt: not found":     "shopt",
		"sh: 1: let: not found":       "$((",
		"sh: 1: pushd: not found":     "cd",
		"sh: 1: popd: not found":      "cd",
		"sh: 1: mapfile: not found":   "while read",
		"sh: 1: readarray: not found": "while read",
		"sh: [[: not found":           "[ ... ]",
	}
	for line, hint := range tests {
		t.Run(line, func(t *testing.T) {
			reasons := explainStderr(line+"\n", 0, 0, dashCtx)
			mustFind(t, reasons, KindBashism, hint)
			mustNotFind(t, reasons, KindMissingCommand)
		})
	}
}

func TestBashismMentionsUnknownShellGracefully(t *testing.T) {
	reasons := explainStderr("sh: 1: [[: not found\n", 0, 0, Context{Distro: "x"})
	r := mustFind(t, reasons, KindBashism, "`[[`")
	if strings.Contains(r.Message, "()") || strings.Contains(r.Message, "  ") {
		t.Fatalf("message should not contain empty shell placeholders: %q", r.Message)
	}
}

func TestBadSubstitution(t *testing.T) {
	for _, line := range []string{"sh: 1: Bad substitution", "sh: syntax error: bad substitution"} {
		t.Run(line, func(t *testing.T) {
			reasons := explainStderr(line+"\n", 0, 2, dashCtx)
			mustFind(t, reasons, KindBashism, "parameter expansion", "dash")
			mustNotFind(t, reasons, KindSyntax)
		})
	}
}

func TestUnexpectedOperator(t *testing.T) {
	reasons := explainStderr("sh: 1: [: a: unexpected operator\n", 0, 0, dashCtx)
	mustFind(t, reasons, KindBashism, "==", "=")
}

func TestSyntaxError(t *testing.T) {
	for _, line := range []string{
		`sh: 1: Syntax error: "(" unexpected`,
		`sh: syntax error: unexpected "("`,
		`/bin/sh: -c: line 1: syntax error near unexpected token '('`,
	} {
		t.Run(line, func(t *testing.T) {
			reasons := explainStderr(line+"\n", 0, 2, dashCtx)
			mustFind(t, reasons, KindSyntax, "dash", "(")
		})
	}
}

func TestMissingCommand(t *testing.T) {
	tests := map[string]string{
		"sh: 1: fzf: not found":                                   "fzf",
		"sh: fzf: not found":                                      "fzf",
		"bash: line 1: fzf: command not found":                    "fzf",
		"bash: fzf: command not found":                            "fzf",
		"/bin/sh: line 1: fzf: command not found":                 "fzf",
		"zsh:1: command not found: fzf":                           "fzf",
		"exec: fzf: not found":                                    "fzf",
		"env: 'python3': No such file or directory":               "python3",
		"env: ‘python3’: No such file or directory":               "python3",
		"env: can't execute 'python3': No such file or directory": "python3",
		"which: no fzf in (/usr/bin:/bin)":                        "fzf",
	}
	for line, name := range tests {
		t.Run(line, func(t *testing.T) {
			reasons := explainStderr(line+"\n", 0, 127, Context{Distro: "alpine", Shell: "busybox ash", RefShell: "bash"})
			mustFind(t, reasons, KindMissingCommand, "`"+name+"`", "--pkg alpine=")
			mustNotFind(t, reasons, KindBashism)
			mustNotFind(t, reasons, KindMissingFile)
		})
	}
}

func TestMissingCommandUsesFullDistroName(t *testing.T) {
	reasons := explainStderr("sh: 1: fzf: not found\n", 0, 127, Context{Distro: "ubuntu:22.04", Shell: "dash"})
	mustFind(t, reasons, KindMissingCommand, "--pkg ubuntu:22.04=")
}

func TestMissingExecutablePath(t *testing.T) {
	// dash and busybox report a script whose shebang interpreter is
	// missing exactly like a missing file.
	for _, line := range []string{"sh: 1: ./run.sh: not found", "sh: ./run.sh: not found"} {
		t.Run(line, func(t *testing.T) {
			reasons := explainStderr(line+"\n", 0, 127, dashCtx)
			mustFind(t, reasons, KindMissingInterpreter, "./run.sh", "shebang")
			mustNotFind(t, reasons, KindMissingCommand)
		})
	}
}

func TestBadInterpreter(t *testing.T) {
	reasons := explainStderr("bash: ./run.sh: /usr/bin/zsh: bad interpreter: No such file or directory\n", 0, 126, dashCtx)
	mustFind(t, reasons, KindMissingInterpreter, "./run.sh", "/usr/bin/zsh")
	mustNotFind(t, reasons, KindMissingFile)
}

func TestUnsupportedOption(t *testing.T) {
	tests := []struct {
		stderr, tool, option string
		busybox              bool
	}{
		{"grep: invalid option -- 'P'\nUsage: grep [OPTION]... PATTERNS [FILE]...\n", "grep", "-P", false},
		{"grep: unrecognized option '--perl-regexp'\n", "grep", "--perl-regexp", false},
		{"grep: unrecognized option: P\nBusyBox v1.36.1 (2024-06-10 07:11:47 UTC) multi-call binary.\n", "grep", "-P", true},
		{"sed: illegal option -- r\n", "sed", "-r", false},
		{"/usr/bin/grep: invalid option -- 'P'\n", "grep", "-P", false},
		{"ls: unknown option -- y\n", "ls", "-y", false},
	}
	for _, tt := range tests {
		t.Run(tt.stderr, func(t *testing.T) {
			reasons := explainStderr(tt.stderr, 0, 2, Context{Distro: "alpine", Shell: "busybox ash"})
			r := mustFind(t, reasons, KindUnsupportedOption, "`"+tt.tool+"`", "`"+tt.option+"`")
			if got := strings.Contains(r.Message, "BusyBox implementation"); got != tt.busybox {
				t.Errorf("BusyBox mentioned = %v, want %v: %q", got, tt.busybox, r.Message)
			}
		})
	}
}

func TestMissingFile(t *testing.T) {
	tests := map[string]string{
		"cat: /etc/os-release2: No such file or directory":                     "/etc/os-release2",
		"ls: cannot access 'build': No such file or directory":                 "build",
		"sh: 1: cannot open config.sh: No such file":                           "config.sh",
		"sh: can't open 'config.sh': No such file or directory":                "config.sh",
		"stat: cannot stat '/usr/share/dict/words': No such file or directory": "/usr/share/dict/words",
	}
	for line, path := range tests {
		t.Run(line, func(t *testing.T) {
			reasons := explainStderr(line+"\n", 0, 1, dashCtx)
			mustFind(t, reasons, KindMissingFile, "`"+path+"`")
		})
	}
}

func TestPermissionDenied(t *testing.T) {
	reasons := explainStderr("sh: 1: ./run.sh: Permission denied\n", 0, 126, dashCtx)
	mustFind(t, reasons, KindPermission, "./run.sh")
}

func TestReadOnly(t *testing.T) {
	reasons := explainStderr("touch: cannot touch 'out.txt': Read-only file system\n", 0, 1, dashCtx)
	mustFind(t, reasons, KindReadOnly, "read-only", "/tmp")
}

func TestOnlyNewStderrLinesAreExplained(t *testing.T) {
	ref := backend.Result{Stderr: "sh: fzf: not found\n", ExitCode: 0}
	got := backend.Result{Stderr: "sh: fzf: not found\nextra\n", ExitCode: 0}
	reasons := explain(ref, got, dashCtx)
	mustNotFind(t, reasons, KindMissingCommand)
	mustFind(t, reasons, KindStderr)
}

func TestReasonsAreDeduplicated(t *testing.T) {
	reasons := explainStderr("sh: 1: [[: not found\nsh: 3: [[: not found\nsh: 5: [[: not found\n", 0, 0, dashCtx)
	n := 0
	for _, r := range reasons {
		if r.Kind == KindBashism {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("got %d bashism reasons, want 1: %+v", n, reasons)
	}
}

func TestTimeout(t *testing.T) {
	ref := backend.Result{Stdout: "done\n"}
	got := backend.Result{TimedOut: true, ExitCode: backend.ExitTimeout, Duration: 30 * time.Second}
	reasons := explain(ref, got, dashCtx)
	if len(reasons) == 0 || reasons[0].Kind != KindTimeout {
		t.Fatalf("timeout should be the first reason: %+v", reasons)
	}
	mustFind(t, reasons, KindTimeout, "30s")
	mustNotFind(t, reasons, KindExitCode)
}

func TestReferenceTimedOut(t *testing.T) {
	ref := backend.Result{TimedOut: true, ExitCode: backend.ExitTimeout}
	got := backend.Result{Stdout: "done\n"}
	mustFind(t, explain(ref, got, dashCtx), KindTimeout, "reference")
}

func TestExitCodeMeanings(t *testing.T) {
	tests := []struct {
		code int
		want string
	}{
		{1, "exit code 1, reference exited 0"},
		{2, "usage"},
		{126, "not executable"},
		{127, "command not found"},
		{137, "signal 9 (killed)"},
		{139, "signal 11 (segmentation fault)"},
		{141, "signal 13 (broken pipe)"},
	}
	for _, tt := range tests {
		reasons := explain(backend.Result{}, backend.Result{ExitCode: tt.code}, dashCtx)
		mustFind(t, reasons, KindExitCode, tt.want)
	}
}

func TestExitCodeReferenceNonZero(t *testing.T) {
	reasons := explain(backend.Result{ExitCode: 3}, backend.Result{ExitCode: 0}, dashCtx)
	mustFind(t, reasons, KindExitCode, "exit code 0, reference exited 3")
}

func TestSilentErrorOnlyWhenExitCodesAreZero(t *testing.T) {
	reasons := explainStderr("sh: 1: fzf: not found\n", 0, 127, dashCtx)
	mustNotFind(t, reasons, KindSilentError)
	reasons = explainStderr("sh: 1: fzf: not found\n", 1, 1, dashCtx)
	mustNotFind(t, reasons, KindSilentError)
	reasons = explainStderr("sh: 1: fzf: not found\n", 0, 0, dashCtx)
	mustFind(t, reasons, KindSilentError)
}

func TestGenericOutputReasons(t *testing.T) {
	reasons := explain(backend.Result{Stdout: "a\n"}, backend.Result{Stdout: "b\n"}, dashCtx)
	mustFind(t, reasons, KindStdout)
	mustNotFind(t, reasons, KindStderr)

	reasons = explain(backend.Result{}, backend.Result{Stderr: "some warning\n"}, dashCtx)
	mustFind(t, reasons, KindStderr)
	mustNotFind(t, reasons, KindStdout)
}

func TestStdoutReasonEvenWithSpecificReasons(t *testing.T) {
	ref := backend.Result{Stdout: "matched\n"}
	got := backend.Result{Stderr: "sh: 1: [[: not found\n"}
	mustFind(t, explain(ref, got, dashCtx), KindStdout)
}

func TestNoGenericStderrReasonWhenExplained(t *testing.T) {
	reasons := explainStderr("sh: 1: fzf: not found\n", 0, 127, dashCtx)
	mustNotFind(t, reasons, KindStderr)
}

func TestIgnoredStderrIsStillUsedForCauses(t *testing.T) {
	ref := backend.Result{Stdout: "matched\n"}
	got := backend.Result{Stderr: "sh: 1: [[: not found\n"}
	cmp := compare.Compare(ref, got, compare.Options{IgnoreStderr: true})
	reasons := Explain(ref, got, cmp, dashCtx)
	mustFind(t, reasons, KindBashism)
	mustNotFind(t, reasons, KindStderr)
}

func TestTruncated(t *testing.T) {
	reasons := explain(backend.Result{Stdout: "a"}, backend.Result{Stdout: "b", Truncated: true}, dashCtx)
	mustFind(t, reasons, KindTruncated, "1 MiB")
}

func TestReasonOrder(t *testing.T) {
	ref := backend.Result{Stdout: "ok\n"}
	got := backend.Result{Stderr: "grep: invalid option -- 'P'\nsh: 1: fzf: not found\n", ExitCode: 127}
	reasons := explain(ref, got, dashCtx)
	var kinds []Kind
	for _, r := range reasons {
		kinds = append(kinds, r.Kind)
	}
	want := []Kind{KindUnsupportedOption, KindMissingCommand, KindExitCode, KindStdout}
	if len(kinds) != len(want) {
		t.Fatalf("kinds = %v, want %v", kinds, want)
	}
	for i := range want {
		if kinds[i] != want[i] {
			t.Fatalf("kinds = %v, want %v", kinds, want)
		}
	}
}

func TestManyStderrLinesAreBounded(t *testing.T) {
	var b strings.Builder
	for i := range 10000 {
		b.WriteString("sh: 1: cmd")
		b.WriteString(strings.Repeat("x", i%50))
		b.WriteString(": not found\n")
	}
	start := time.Now()
	reasons := explainStderr(b.String(), 0, 127, dashCtx)
	if time.Since(start) > time.Second {
		t.Fatalf("explaining took %v", time.Since(start))
	}
	n := 0
	for _, r := range reasons {
		if r.Kind == KindMissingCommand {
			n++
		}
	}
	if n == 0 || n > MaxCauses {
		t.Fatalf("got %d missing-command reasons, want 1..%d", n, MaxCauses)
	}
}
