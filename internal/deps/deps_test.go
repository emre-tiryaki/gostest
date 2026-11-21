package deps

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestCommands(t *testing.T) {
	tests := []struct {
		name   string
		script string
		want   []string
	}{
		{"empty", "", nil},
		{"simple", "curl -fsSL https://example.com", []string{"curl"}},
		{"order and duplicates", "jq . a.json; curl x; jq . b.json", []string{"jq", "curl"}},
		{"pipeline", "ps aux | grep foo | awk '{print $2}' | sort -u", []string{"ps", "grep", "awk", "sort"}},
		{"and or", "git pull && make || notify-send failed", []string{"git", "make", "notify-send"}},
		{"background", "sleep 1 & wait", []string{"sleep"}},
		{"newlines", "tar xf a.tar\nunzip b.zip\n", []string{"tar", "unzip"}},
		{"builtins left out", "cd /tmp; echo hi; printf '%s\\n' x; test -f a; [ -d b ]; read x; export A=1; set -e; true; false; exit 0", nil},
		{"bashisms left out", "local x; declare -a y; shopt -s nullglob; source ./lib.sh; [[ -n $x ]] && let i++", nil},
		{"arguments are not commands", "grep -r curl .", []string{"grep"}},
		{"assignments before a command", "LC_ALL=C FOO='a b' sort file", []string{"sort"}},
		{"assignment only", "X=1 Y=2", nil},
		{"comments", "# curl is not used\nwget x # jq neither", []string{"wget"}},
		{"hash inside a word", "echo a#b; sed s#a#b# f", []string{"sed"}},
		{"single quotes", "echo 'curl; jq' ; date", []string{"date"}},
		{"double quotes", `echo "curl; jq" ; date`, []string{"date"}},
		{"quoted command name", `"jq" . f; 'yq' . g`, []string{"jq", "yq"}},
		{"escaped separator", `echo a\; curl`, nil},
		{"dynamic command name", `$CC -o a a.c; "${TOOL}" x; $(which cc) y`, []string{"which", "cc"}},
		{"command substitution", "x=$(git rev-parse HEAD); echo \"$(date +%s)\"", []string{"git", "date"}},
		{"nested substitution", "echo $(basename $(dirname $(pwd)))", []string{"basename", "dirname"}},
		{"backticks", "v=`uname -r`", []string{"uname"}},
		{"arithmetic is not a command", "echo $((1 + 2)); i=$(( i * 3 ))", nil},
		{"parameter expansion", "echo ${x:-$(id -u)} ${#y} ${z//a/b}", []string{"id"}},
		{"process substitution", "diff <(sort a) <(sort b)", []string{"diff", "sort"}},
		{"redirections", "make >out.log 2>&1 </dev/null; cat >>log 2>/dev/null", []string{"make", "cat"}},
		{"redirection before command", ">out 2>&1 curl x", []string{"curl"}},
		{"here string", "base64 -d <<<\"$x\"", []string{"base64"}},
		{"heredoc body skipped", "cat <<EOF\ncurl not run\njq neither\nEOF\nwc -l f", []string{"cat", "wc"}},
		{"heredoc with tabs", "cat <<-END\n\tcurl\n\tEND\nwc f", []string{"cat", "wc"}},
		{"quoted heredoc", "cat <<'EOF'\n$(curl x)\nEOF\nwc f", []string{"cat", "wc"}},
		{"heredoc expansions", "cat <<EOF\nnow: $(date)\nEOF", []string{"cat", "date"}},
		{"two heredocs", "cat <<A <<B\na\nA\nb\nB\ntr a b", []string{"cat", "tr"}},
		{"subshell and group", "(cd x && make); { gzip f; }", []string{"make", "gzip"}},
		{"if", "if grep -q x f; then curl a; elif wget b; then :; else jq c; fi", []string{"grep", "curl", "wget", "jq"}},
		{"negation", "if ! command -v git >/dev/null; then apt-get install git; fi", []string{"git", "apt-get"}},
		{"while and until", "while read l; do md5sum \"$l\"; done < f; until ping -c1 h; do sleep 1; done", []string{"md5sum", "ping", "sleep"}},
		{"for", "for f in curl wget *.sh; do shellcheck \"$f\"; done", []string{"shellcheck"}},
		{"for without in", "for a; do stat \"$a\"; done", []string{"stat"}},
		{"case", "case $1 in\n  start|run) systemctl start x ;;\n  (stop) systemctl stop x;;\n  *) logger bad ;;\nesac", []string{"systemctl", "logger"}},
		{"case words are not commands", "case curl in curl) date;; esac", []string{"date"}},
		{"function definitions", "build() { make all; }\nfunction deploy { rsync -a . h:; }\nbuild; deploy", []string{"make", "rsync"}},
		{"function defined later", "main; main() { tar cf a b; }", []string{"tar"}},
		{"function with spaces", "f () (curl x)\nf", []string{"curl"}},
		{"alias", "alias ll='ls -l'; ll", nil},
		{"wrappers", "env -i PATH=/bin FOO=1 curl x; exec nginx -g x; nohup node app.js & nice -n 5 gzip f; sudo -u root apt-get update", []string{"env", "curl", "nginx", "nohup", "node", "nice", "gzip", "sudo", "apt-get"}},
		{"command builtin", "command ls; command -p cat f", []string{"ls", "cat"}},
		{"time", "time make", []string{"make"}},
		{"timeout and xargs", "timeout 5s curl x; find . -name '*.go' | xargs -0 -n1 gofmt -l", []string{"timeout", "curl", "find", "xargs", "gofmt"}},
		{"find exec", `find . -type f -exec sha256sum {} \; -exec chmod 644 {} +`, []string{"find", "sha256sum", "chmod"}},
		{"checked commands", "command -v jq >/dev/null || type yq; hash git 2>/dev/null; which -a node", []string{"jq", "yq", "git", "which", "node"}},
		{"sh -c", "sh -c 'curl x | jq .'; bash -ec \"wget y\"", []string{"sh", "curl", "jq", "bash", "wget"}},
		{"interpreters", "python3 script.py; perl -e 'print 1'; node -e x", []string{"python3", "perl", "node"}},
		{"paths left out", "./build.sh; ../x; /opt/tool/bin/run", nil},
		{"wrapper by path", "/usr/bin/env ls", []string{"ls"}},
		{"invalid names left out", "a=; =b; ++ x", nil},
		{"keywords as arguments", "echo if then fi; grep done f", []string{"grep"}},
		{"reserved word in argument position", "echo do; ls", []string{"ls"}},
		{"pipe stderr", "make |& tee log", []string{"make", "tee"}},
		{"line continuation", "curl \\\n  -o f \\\n  https://x && unzip f", []string{"curl", "unzip"}},
		{"unterminated quote", "echo 'oops; curl x", nil},
		{"unterminated substitution", "echo $(date", []string{"date"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Commands(tt.script); !slices.Equal(got, tt.want) {
				t.Errorf("Commands(%q)\n got %q\nwant %q", tt.script, got, tt.want)
			}
		})
	}
}

func TestCommandsNeverPanics(t *testing.T) {
	inputs := []string{"$(", "`", "${", "<<", "<<EOF", "case", "case x in", "for", "function", "f(", "((", "$((", "'", "\"", "\\", "a |", "&&", ";;", ")", "}", "<(", "cat <<", "x=$(", "sh -c"}
	for _, in := range inputs {
		Commands(in)
		Commands(in + " curl")
	}
}

func write(t *testing.T, dir, name, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestScanCommandOnly(t *testing.T) {
	if got := Scan("curl x | jq .", t.TempDir()); !slices.Equal(got, []string{"curl", "jq"}) {
		t.Errorf("Scan = %q", got)
	}
}

func TestScanFollowsExecutedScript(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "install.sh", "#!/bin/bash\nset -e\ncurl -fsSL x | tar xz\n", 0o755)
	got := Scan("./install.sh --prefix /opt", dir)
	want := []string{"bash", "curl", "tar"}
	if !slices.Equal(got, want) {
		t.Errorf("Scan = %q, want %q", got, want)
	}
}

func TestScanEnvShebang(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "run", "#!/usr/bin/env -S bash -e\ngit status\n", 0o755)
	if got, want := Scan("./run", dir), []string{"bash", "git"}; !slices.Equal(got, want) {
		t.Errorf("Scan = %q, want %q", got, want)
	}
}

func TestScanNonShellScript(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "tool.py", "#!/usr/bin/env python3\nimport os\nos.system('curl x')\n", 0o755)
	if got, want := Scan("./tool.py", dir), []string{"python3"}; !slices.Equal(got, want) {
		t.Errorf("Scan = %q, want %q (python code is not parsed)", got, want)
	}
}

func TestScanScriptWithoutShebang(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "plain.sh", "rsync -a src/ dst/\n", 0o755)
	if got, want := Scan("./plain.sh", dir), []string{"rsync"}; !slices.Equal(got, want) {
		t.Errorf("Scan = %q, want %q", got, want)
	}
}

func TestScanScriptGivenToShell(t *testing.T) {
	dir := t.TempDir()
	// The shell named on the command line runs it: its shebang is not used.
	write(t, dir, "a.sh", "#!/bin/zsh\nxz -d f\n", 0o644)
	write(t, dir, "b.sh", "zstd -d g\n", 0o644)
	got := Scan("sh a.sh && bash -x b.sh arg", dir)
	want := []string{"sh", "bash", "xz", "zstd"}
	if !slices.Equal(got, want) {
		t.Errorf("Scan = %q, want %q", got, want)
	}
}

func TestScanSourcedFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "lib/common.sh", "log() { logger \"$@\"; }\n", 0o644)
	write(t, dir, "main.sh", "#!/bin/sh\n. ./lib/common.sh\nsource lib/common.sh\nlog hi\n", 0o755)
	if got, want := Scan("./main.sh", dir), []string{"sh", "logger"}; !slices.Equal(got, want) {
		t.Errorf("Scan = %q, want %q", got, want)
	}
}

func TestScanFunctionsFromSourcedFile(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "lib.sh", "fetch() { curl \"$1\"; }\n", 0o644)
	if got, want := Scan(". ./lib.sh; fetch x", dir), []string{"curl"}; !slices.Equal(got, want) {
		t.Errorf("Scan = %q, want %q (fetch is a function of lib.sh)", got, want)
	}
}

func TestScanNested(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.sh", "#!/bin/sh\n./scripts/b.sh\n", 0o755)
	write(t, dir, "scripts/b.sh", "#!/bin/sh\njq . f\n", 0o755)
	if got, want := Scan("./a.sh", dir), []string{"sh", "jq"}; !slices.Equal(got, want) {
		t.Errorf("Scan = %q, want %q", got, want)
	}
}

func TestScanRelativeToScriptDirIsNotGuessed(t *testing.T) {
	// Paths in a script are relative to the working directory, not to the
	// script.
	dir := t.TempDir()
	write(t, dir, "sub/a.sh", "#!/bin/sh\n./b.sh\n", 0o755)
	write(t, dir, "sub/b.sh", "#!/bin/sh\njq . f\n", 0o755)
	if got, want := Scan("./sub/a.sh", dir), []string{"sh"}; !slices.Equal(got, want) {
		t.Errorf("Scan = %q, want %q", got, want)
	}
}

func TestScanCycles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.sh", "#!/bin/sh\n./b.sh\ngzip f\n", 0o755)
	write(t, dir, "b.sh", "#!/bin/sh\n./a.sh\nbzip2 f\n", 0o755)
	if got, want := Scan("./a.sh", dir), []string{"sh", "gzip", "bzip2"}; !slices.Equal(got, want) {
		t.Errorf("Scan = %q, want %q", got, want)
	}
}

func TestScanAbsolutePath(t *testing.T) {
	dir := t.TempDir()
	p := write(t, dir, "x.sh", "#!/bin/dash\nfile f\n", 0o755)
	if got, want := Scan(p, t.TempDir()), []string{"dash", "file"}; !slices.Equal(got, want) {
		t.Errorf("Scan = %q, want %q", got, want)
	}
}

func TestScanSkipsMissingAndBinaryFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "bin", "\x7fELF\x02\x01\x01\x00curl\x00", 0o755)
	if err := os.Mkdir(filepath.Join(dir, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got, want := Scan("./missing.sh; ./bin; ./d; sh nope.sh", dir), []string{"sh"}; !slices.Equal(got, want) {
		t.Errorf("Scan = %q, want %q", got, want)
	}
}

func TestScanSkipsHugeFiles(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "big.sh", "#!/bin/sh\ncurl x\n"+strings.Repeat("# filler\n", MaxFileSize/9+1), 0o755)
	if got, want := Scan("./big.sh", dir), []string{"sh"}; !slices.Equal(got, want) {
		t.Errorf("Scan = %q, want %q (the body of a file over MaxFileSize is not parsed)", got, want)
	}
}

func TestScanDynamicPathsAreNotFollowed(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a.sh", "#!/bin/sh\ncurl x\n", 0o755)
	if got := Scan("$PWD/a.sh; ./*.sh", dir); len(got) != 0 {
		t.Errorf("Scan = %q, want nothing", got)
	}
}
