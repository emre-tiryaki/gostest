// Package deps finds the external commands a shell command or script needs
// without running it, so they can be installed before the first run.
//
// It is a best-effort scanner, not a shell parser: it tokenizes POSIX shell
// (with the common bash extensions) well enough to tell command words from
// arguments, and leaves out what it cannot know without running the code,
// such as commands named by variables.
package deps

import (
	"bytes"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/emre-tiryaki/gostest/internal/backend"
	"github.com/emre-tiryaki/gostest/internal/distro"
)

// MaxFileSize is the largest script Scan parses; of larger files only the
// shebang line is used.
const MaxFileSize = 1 << 20

const (
	// maxFiles bounds the scripts Scan follows.
	maxFiles = 256
	// maxDepth bounds nested command substitutions and sh -c scripts.
	maxDepth = 64
)

// Commands returns the external commands script runs, in order of first
// appearance and without duplicates. Shell builtins and keywords, functions
// and aliases the script defines, paths and names only known at run time
// are left out. Arguments of command -v, type, hash and which count too:
// they are the commands a script checks for.
func Commands(script string) []string {
	r := newResult()
	r.parse(script)
	return r.commands()
}

// Scan returns Commands of command and, recursively, of the shell scripts
// it runs or sources by path (relative paths are resolved against dir),
// plus the interpreters in the shebang lines of the files it executes.
func Scan(command, dir string) []string {
	r := newResult()
	r.parse(command)
	seen := map[string]bool{}
	for k := 0; k < len(r.files) && len(seen) < maxFiles; k++ {
		f := r.files[k]
		p := f.path
		if !filepath.IsAbs(p) {
			p = filepath.Join(dir, p)
		}
		p = filepath.Clean(p)
		if seen[p] {
			continue
		}
		seen[p] = true
		content, ok := readScript(p)
		if !ok {
			continue
		}
		shell := true
		if f.exec && bytes.HasPrefix(content, []byte("#!")) {
			interp, _ := backend.ScriptInterpreter(p)
			r.add(interp)
			shell = shells[interp]
		}
		if shell && len(content) <= MaxFileSize {
			r.parse(string(content))
		}
	}
	return r.commands()
}

// readScript returns up to MaxFileSize+1 bytes of the regular text file p.
func readScript(p string) ([]byte, bool) {
	f, err := os.Open(p)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	if info, err := f.Stat(); err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	b, err := io.ReadAll(io.LimitReader(f, MaxFileSize+1))
	if err != nil || bytes.IndexByte(b, 0) >= 0 {
		return nil, false
	}
	return b, true
}

// shells are the interpreters whose scripts are parsed.
var shells = set("sh", "bash", "dash", "ash", "ksh", "mksh", "zsh", "posh", "yash")

// builtins are never reported: every shell has them, or installing a
// package would not make them available.
var builtins = set(
	":", ".", "[", "[[", "]]", "alias", "bg", "bind", "break", "builtin", "caller", "cd",
	"command", "compgen", "complete", "compopt", "continue", "coproc", "declare", "dirs",
	"disown", "echo", "enable", "eval", "exec", "exit", "export", "false", "fc", "fg",
	"getopts", "hash", "help", "history", "jobs", "kill", "let", "local", "logout",
	"mapfile", "popd", "printf", "pushd", "pwd", "read", "readarray", "readonly",
	"return", "set", "shift", "shopt", "source", "suspend", "test", "time", "times",
	"trap", "true", "type", "typeset", "ulimit", "umask", "unalias", "unset", "wait",
)

// wrapper describes a command that runs another command given as its
// arguments.
type wrapper struct {
	// argOpts are the single-letter options that take the next word as
	// their value.
	argOpts string
	// skipFirst is set when the first operand is not the command, as the
	// duration of timeout.
	skipFirst bool
}

var wrappers = map[string]wrapper{
	"command": {},
	"doas":    {argOpts: "uC"},
	"env":     {argOpts: "uCS"},
	"exec":    {argOpts: "a"},
	"flock":   {argOpts: "wEn", skipFirst: true},
	"ionice":  {argOpts: "cnp"},
	"nice":    {argOpts: "n"},
	"nohup":   {},
	"setsid":  {},
	"stdbuf":  {argOpts: "ioe"},
	"sudo":    {argOpts: "ugCDhprtUT"},
	"time":    {argOpts: "fo"},
	"timeout": {argOpts: "sk", skipFirst: true},
	"xargs":   {argOpts: "adEILnPs"},
}

var assignRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\[[^]]*\])?\+?=`)

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

type fileRef struct {
	path string
	// exec is set when the file is executed, so its shebang line counts;
	// a file given to a shell or sourced is read by that shell.
	exec bool
}

// result collects what the parsed sources need.
type result struct {
	cmds    []string
	seen    map[string]bool
	defined map[string]bool // functions and aliases
	files   []fileRef
	depth   int
}

func newResult() *result {
	return &result{seen: map[string]bool{}, defined: map[string]bool{}}
}

func (r *result) add(name string) {
	if name == "" || builtins[name] || !distro.ValidCommandName(name) || r.seen[name] {
		return
	}
	r.seen[name] = true
	r.cmds = append(r.cmds, name)
}

func (r *result) define(name string) { r.defined[name] = true }

func (r *result) file(p string, exec bool) {
	r.files = append(r.files, fileRef{p, exec})
}

func (r *result) commands() []string {
	var out []string
	for _, c := range r.cmds {
		if !r.defined[c] {
			out = append(out, c)
		}
	}
	return out
}

func (r *result) parse(src string) {
	if r.depth >= maxDepth {
		return
	}
	r.depth++
	defer func() { r.depth-- }()
	l := &lexer{s: src}
	l.run()
	p := &parser{toks: l.toks, res: r}
	p.run()
}

// --- lexer ---

// token is an operator or a word.
type token struct {
	op   string
	word string // the word with quotes removed and expansions left out
	// quoted words are never reserved words.
	quoted bool
	// dynamic words contain expansions or globs: their value is only
	// known at run time.
	dynamic bool
	// subs are the command substitutions in the token (or, for a newline,
	// in the heredoc bodies it ends), parsed after the token itself.
	subs []string
}

type heredoc struct {
	delim         string
	strip, quoted bool
}

type lexer struct {
	s       string
	i       int
	toks    []token
	subs    []string
	pending []heredoc
}

func (l *lexer) peek(k int) byte {
	if l.i+k < len(l.s) {
		return l.s[l.i+k]
	}
	return 0
}

func (l *lexer) emit(t token) {
	t.subs, l.subs = l.subs, nil
	l.toks = append(l.toks, t)
}

func isOpStart(c byte) bool { return strings.IndexByte(";&|()<>", c) >= 0 }

func (l *lexer) run() {
	for l.i < len(l.s) {
		c := l.s[l.i]
		switch {
		case c == ' ' || c == '\t' || c == '\r':
			l.i++
		case c == '\\' && l.peek(1) == '\n':
			l.i += 2
		case c == '#':
			for l.i < len(l.s) && l.s[l.i] != '\n' {
				l.i++
			}
		case c == '\n':
			l.i++
			l.heredocBodies()
			l.emit(token{op: "\n"})
		case c == '(' && l.peek(1) == '(':
			// Arithmetic command: (( i++ )).
			inner, end := balanced(l.s, l.i+1, '(', ')')
			l.scanText(inner)
			l.i = end
			l.emit(token{word: "((", dynamic: true})
		case isOpStart(c) && !((c == '<' || c == '>') && l.peek(1) == '('):
			l.operator()
		case c >= '0' && c <= '9' && l.ioNumber():
		default:
			start := l.i
			l.emit(l.word())
			if l.i == start {
				l.i++
			}
		}
	}
}

// ioNumber skips the file descriptor of a redirection such as 2>&1.
func (l *lexer) ioNumber() bool {
	j := l.i
	for j < len(l.s) && l.s[j] >= '0' && l.s[j] <= '9' {
		j++
	}
	if j < len(l.s) && (l.s[j] == '<' || l.s[j] == '>') {
		l.i = j
		return true
	}
	return false
}

var operators = []string{
	";;&", ";;", ";&", ";", "&&", "&>>", "&>", "&", "||", "|&", "|", "(", ")",
	"<<<", "<<-", "<<", "<&", "<>", "<", ">>", ">&", ">|", ">",
}

func (l *lexer) operator() {
	for _, op := range operators {
		if !strings.HasPrefix(l.s[l.i:], op) {
			continue
		}
		l.i += len(op)
		l.emit(token{op: op})
		if op == "<<" || op == "<<-" {
			l.heredocDelim(op == "<<-")
		}
		return
	}
	l.i++
}

func (l *lexer) heredocDelim(strip bool) {
	for l.i < len(l.s) && (l.s[l.i] == ' ' || l.s[l.i] == '\t') {
		l.i++
	}
	if l.i >= len(l.s) || l.s[l.i] == '\n' || isOpStart(l.s[l.i]) {
		return
	}
	t := l.word()
	l.pending = append(l.pending, heredoc{delim: t.word, strip: strip, quoted: t.quoted})
	l.emit(t)
}

// heredocBodies skips the bodies of the pending heredocs, which start at
// l.i, scanning those with unquoted delimiters for command substitutions.
func (l *lexer) heredocBodies() {
	for _, h := range l.pending {
		var body strings.Builder
		for l.i < len(l.s) {
			line := l.s[l.i:]
			if e := strings.IndexByte(line, '\n'); e >= 0 {
				line = line[:e]
				l.i += e + 1
			} else {
				l.i = len(l.s)
			}
			check := strings.TrimSuffix(line, "\r")
			if h.strip {
				check = strings.TrimLeft(check, "\t")
			}
			if check == h.delim {
				break
			}
			body.WriteString(line)
			body.WriteByte('\n')
		}
		if !h.quoted {
			l.scanText(body.String())
		}
	}
	l.pending = nil
}

func (l *lexer) word() token {
	var b strings.Builder
	var t token
	start := l.i
loop:
	for l.i < len(l.s) {
		c := l.s[l.i]
		switch {
		case strings.IndexByte(" \t\r\n;&|)", c) >= 0:
			break loop
		case c == '(':
			if b.Len() == 0 || !strings.HasSuffix(b.String(), "=") {
				break loop
			}
			// Array assignment: a=(x y).
			inner, end := balanced(l.s, l.i+1, '(', ')')
			l.scanText(inner)
			b.WriteString("(" + inner + ")")
			l.i = end
		case c == '<' || c == '>':
			if l.peek(1) != '(' {
				break loop
			}
			// Process substitution: <(cmd).
			inner, end := balanced(l.s, l.i+2, '(', ')')
			l.subs = append(l.subs, inner)
			t.dynamic = true
			l.i = end
		case c == '\\':
			if n := l.peek(1); n != 0 && n != '\n' {
				b.WriteByte(n)
				t.quoted = true
			}
			l.i = min(l.i+2, len(l.s))
		case c == '\'':
			t.quoted = true
			end := strings.IndexByte(l.s[l.i+1:], '\'')
			if end < 0 {
				b.WriteString(l.s[l.i+1:])
				l.i = len(l.s)
			} else {
				b.WriteString(l.s[l.i+1 : l.i+1+end])
				l.i += end + 2
			}
		case c == '"':
			t.quoted = true
			l.i++
			l.doubleQuoted(&b, &t)
		case c == '$':
			l.dollar(&b, &t)
		case c == '`':
			l.backtick()
			t.dynamic = true
		case c == '*' || c == '?' || c == '[' || (c == '~' && l.i == start):
			t.dynamic = true
			b.WriteByte(c)
			l.i++
		default:
			b.WriteByte(c)
			l.i++
		}
	}
	t.word = b.String()
	return t
}

func (l *lexer) doubleQuoted(b *strings.Builder, t *token) {
	for l.i < len(l.s) {
		switch c := l.s[l.i]; c {
		case '"':
			l.i++
			return
		case '\\':
			n := l.peek(1)
			switch n {
			case '$', '`', '"', '\\':
				b.WriteByte(n)
			case '\n', 0:
			default:
				b.WriteByte('\\')
				b.WriteByte(n)
			}
			l.i = min(l.i+2, len(l.s))
		case '$':
			l.dollar(b, t)
		case '`':
			l.backtick()
			t.dynamic = true
		default:
			b.WriteByte(c)
			l.i++
		}
	}
}

func isNameChar(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

// dollar handles the expansion starting at l.s[l.i] == '$'.
func (l *lexer) dollar(b *strings.Builder, t *token) {
	n := l.peek(1)
	switch {
	case n == '(' && l.peek(2) == '(':
		inner, end := balanced(l.s, l.i+2, '(', ')')
		l.scanText(inner)
		t.dynamic, l.i = true, end
	case n == '(':
		inner, end := balanced(l.s, l.i+2, '(', ')')
		l.subs = append(l.subs, inner)
		t.dynamic, l.i = true, end
	case n == '{':
		inner, end := balanced(l.s, l.i+2, '{', '}')
		l.scanText(inner)
		t.dynamic, l.i = true, end
	case n == '\'':
		// ANSI-C quoting: $'...'.
		j := l.i + 2
		for j < len(l.s) && l.s[j] != '\'' {
			if l.s[j] == '\\' {
				j++
			}
			j++
		}
		j = min(j, len(l.s))
		b.WriteString(l.s[l.i+2 : j])
		t.quoted, l.i = true, min(j+1, len(l.s))
	case n == '"':
		l.i++
	case isNameChar(n):
		j := l.i + 1
		for j < len(l.s) && isNameChar(l.s[j]) {
			j++
		}
		t.dynamic, l.i = true, j
	case n != 0 && strings.IndexByte("@*#?$!-", n) >= 0:
		t.dynamic, l.i = true, l.i+2
	default:
		b.WriteByte('$')
		l.i++
	}
}

func (l *lexer) backtick() {
	var inner strings.Builder
	j := l.i + 1
	for j < len(l.s) && l.s[j] != '`' {
		if l.s[j] == '\\' && j+1 < len(l.s) {
			j++
		}
		inner.WriteByte(l.s[j])
		j++
	}
	l.subs = append(l.subs, inner.String())
	l.i = min(j+1, len(l.s))
}

// scanText adds the command substitutions in text, which is not split into
// words: the body of a heredoc or of ${...}.
func (l *lexer) scanText(text string) {
	sub := &lexer{s: text}
	var b strings.Builder
	var t token
	for sub.i < len(sub.s) {
		switch sub.s[sub.i] {
		case '\\':
			sub.i += 2
		case '$':
			sub.dollar(&b, &t)
		case '`':
			sub.backtick()
		default:
			sub.i++
		}
	}
	l.subs = append(l.subs, sub.subs...)
}

// balanced returns the text from start up to the close matching an open
// before start, and the index after that close; quotes are respected. An
// unterminated text runs to the end of s.
func balanced(s string, start int, open, close byte) (string, int) {
	start = min(start, len(s))
	depth := 1
	for j := start; j < len(s); j++ {
		switch c := s[j]; {
		case c == '\\':
			j++
		case c == '\'':
			if e := strings.IndexByte(s[j+1:], '\''); e >= 0 {
				j += e + 1
			} else {
				j = len(s)
			}
		case c == '"':
			for j++; j < len(s) && s[j] != '"'; j++ {
				if s[j] == '\\' {
					j++
				}
			}
		case c == open:
			depth++
		case c == close:
			if depth--; depth == 0 {
				return s[start:j], j + 1
			}
		}
	}
	return s[start:], len(s)
}

// --- parser ---

type mode int

const (
	modeCmd      mode = iota // the next word is a command name
	modeArgs                 // the next words are arguments of cur
	modeFor                  // the header of for/select, up to do
	modeCaseWord             // the word after case
	modeCaseIn               // up to the in of a case
	modeCasePat              // a case pattern, up to )
)

type parser struct {
	toks []token
	res  *result
	mode mode

	cur      string // the command whose arguments follow, "" if unknown
	operands int    // operands of cur seen
	skipNext bool   // the next word is the value of an option
	endOpts  bool   // -- seen
	shellC   bool   // sh -c seen: the next word is a script
	inFind   int    // nested find -exec commands
}

func (p *parser) run() {
	for i := 0; i < len(p.toks); {
		var next int
		if p.toks[i].op != "" {
			next = p.operator(i)
		} else {
			next = p.word(i)
		}
		for j := i; j < next; j++ {
			for _, s := range p.toks[j].subs {
				p.res.parse(s)
			}
		}
		i = next
	}
}

func (p *parser) args(name string) {
	p.mode, p.cur, p.operands, p.skipNext, p.endOpts, p.shellC = modeArgs, name, 0, false, false, false
}

func (p *parser) reset(m mode) {
	p.args("")
	p.mode, p.inFind = m, 0
}

func (p *parser) isWord(i int) bool { return i < len(p.toks) && p.toks[i].op == "" }

func (p *parser) isOp(i int, op string) bool { return i < len(p.toks) && p.toks[i].op == op }

func (p *parser) operator(i int) int {
	switch op := p.toks[i].op; op {
	case "<", ">", ">>", "<&", ">&", "<>", ">|", "&>", "&>>", "<<<", "<<", "<<-":
		// The next word is the target of the redirection.
		if p.isWord(i + 1) {
			return i + 2
		}
	case ";;", ";&", ";;&":
		p.reset(modeCasePat)
	default:
		switch p.mode {
		case modeCasePat:
			if op == ")" {
				p.mode = modeCmd
			}
		case modeCaseWord, modeCaseIn:
		case modeFor:
			if op == ";" || op == "\n" {
				p.mode = modeCmd
			}
		default:
			if op == ")" {
				p.reset(modeArgs)
			} else {
				p.reset(modeCmd)
			}
		}
	}
	return i + 1
}

func (p *parser) word(i int) int {
	t := p.toks[i]
	switch p.mode {
	case modeCasePat:
		if !t.quoted && t.word == "esac" {
			p.reset(modeArgs)
		}
	case modeCaseWord:
		p.mode = modeCaseIn
	case modeCaseIn:
		if t.word == "in" {
			p.mode = modeCasePat
		}
	case modeFor:
		if !t.quoted && t.word == "do" {
			p.mode = modeCmd
		}
	case modeArgs:
		return p.arg(i)
	default:
		return p.command(i)
	}
	return i + 1
}

func (p *parser) command(i int) int {
	t := p.toks[i]
	w := t.word
	if !t.quoted {
		switch w {
		case "if", "then", "else", "elif", "do", "while", "until", "!", "{":
			return i + 1
		case "fi", "done", "esac", "}":
			p.reset(modeArgs)
			return i + 1
		case "for", "select":
			p.mode = modeFor
			return i + 1
		case "case":
			p.mode = modeCaseWord
			return i + 1
		case "function":
			j := i + 1
			if p.isWord(j) {
				p.res.define(p.toks[j].word)
				j++
			}
			if p.isOp(j, "(") && p.isOp(j+1, ")") {
				j += 2
			}
			return j
		}
	}
	if assignRe.MatchString(w) {
		return i + 1
	}
	if p.isOp(i+1, "(") && p.isOp(i+2, ")") {
		p.res.define(w)
		return i + 3
	}
	if t.dynamic && !builtins[w] {
		p.args("")
		return i + 1
	}
	if strings.Contains(w, "/") {
		// A path is not a package dependency, but it may be a script
		// to follow or a wrapper such as /usr/bin/env.
		p.res.file(w, true)
		p.args(path.Base(w))
		return i + 1
	}
	p.res.add(w)
	p.args(w)
	return i + 1
}

func (p *parser) arg(i int) int {
	t := p.toks[i]
	w := t.word
	if p.inFind > 0 && p.cur != "find" && (w == ";" || w == "+") {
		p.inFind--
		p.args("find")
		return i + 1
	}
	if p.skipNext {
		p.skipNext = false
		return i + 1
	}
	if w == "--" && !p.endOpts {
		p.endOpts = true
		return i + 1
	}
	opt := !p.endOpts && len(w) > 1 && w[0] == '-'

	if spec, ok := wrappers[p.cur]; ok {
		switch {
		case opt:
			if p.cur == "command" && strings.ContainsAny(w[1:], "vV") {
				p.cur = "command -v"
			} else if len(w) == 2 && strings.IndexByte(spec.argOpts, w[1]) >= 0 {
				p.skipNext = true
			}
		case p.cur == "env" && assignRe.MatchString(w):
		case spec.skipFirst && p.operands == 0:
			p.operands++
		case t.dynamic:
			p.args("")
		default:
			// The operand is the command to run.
			p.mode = modeCmd
			return i
		}
		return i + 1
	}

	switch {
	case p.cur == "command -v" || p.cur == "type" || p.cur == "hash" || p.cur == "which":
		if !opt && !t.dynamic {
			p.res.add(w)
		}
	case shells[p.cur]:
		switch {
		case p.shellC:
			if !t.dynamic {
				p.res.parse(w)
			}
			p.args("")
		case opt || (len(w) > 1 && w[0] == '+'):
			if w[0] == '-' && !strings.HasPrefix(w, "--") && strings.ContainsRune(w[1:], 'c') {
				p.shellC = true
			}
			if len(w) == 2 && (w[1] == 'o' || w[1] == 'O') {
				p.skipNext = true
			}
		default:
			if !t.dynamic {
				p.res.file(w, false)
			}
			p.args("")
		}
	case p.cur == "." || p.cur == "source":
		if !t.dynamic {
			p.res.file(w, false)
		}
		p.args("")
	case p.cur == "find":
		if !t.quoted && (w == "-exec" || w == "-execdir" || w == "-ok" || w == "-okdir") {
			p.inFind++
			p.mode = modeCmd
		}
	case p.cur == "alias":
		if name, _, ok := strings.Cut(w, "="); ok {
			p.res.define(name)
		}
	}
	return i + 1
}
