package backend

import (
	"debug/elf"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, name, content string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return p
}

// staticBinary builds a tiny Go program without cgo, which links
// statically.
func staticBinary(t *testing.T) string {
	t.Helper()
	gobin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not found")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() { println(\"hi\") }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module probe\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "probe")
	cmd := exec.Command(gobin, "build", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOFLAGS=")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("building a static binary: %v\n%s", err, b)
	}
	return out
}

// dynamicBinary returns the host's /bin/sh when it is a dynamically linked
// ELF binary.
func dynamicBinary(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Skipf("resolving /bin/sh: %v", err)
	}
	f, err := elf.Open(p)
	if err != nil {
		t.Skipf("/bin/sh is not ELF: %v", err)
	}
	defer f.Close()
	for _, prog := range f.Progs {
		if prog.Type == elf.PT_INTERP {
			return p
		}
	}
	t.Skip("/bin/sh is statically linked")
	return ""
}

func TestClassifyHostTool(t *testing.T) {
	tests := []struct {
		name string
		path func(t *testing.T) string
		want ToolKind
	}{
		{"shebang script", func(t *testing.T) string { return writeFile(t, "s", "#!/bin/sh\necho hi\n", 0o755) }, ToolScript},
		{"env shebang", func(t *testing.T) string { return writeFile(t, "s", "#!/usr/bin/env bash\necho hi\n", 0o755) }, ToolScript},
		{"static ELF", staticBinary, ToolStaticELF},
		{"dynamic ELF", dynamicBinary, ToolUnsupported},
		{"plain text", func(t *testing.T) string { return writeFile(t, "s", "echo hi\n", 0o755) }, ToolUnsupported},
		{"empty file", func(t *testing.T) string { return writeFile(t, "s", "", 0o755) }, ToolUnsupported},
		{"single byte", func(t *testing.T) string { return writeFile(t, "s", "#", 0o755) }, ToolUnsupported},
		{"truncated ELF", func(t *testing.T) string { return writeFile(t, "s", "\x7fELF\x02\x01", 0o755) }, ToolUnsupported},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ClassifyHostTool(tt.path(t))
			if err != nil {
				t.Fatalf("ClassifyHostTool: %v", err)
			}
			if got != tt.want {
				t.Errorf("kind = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestClassifyHostToolErrors(t *testing.T) {
	if _, err := ClassifyHostTool(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("missing file: want an error")
	}
	if _, err := ClassifyHostTool(t.TempDir()); err == nil {
		t.Error("directory: want an error")
	}
	if os.Geteuid() == 0 {
		t.Skip("root can read any file")
	}
	p := writeFile(t, "s", "#!/bin/sh\n", 0o000)
	if _, err := ClassifyHostTool(p); err == nil {
		t.Error("unreadable file: want an error")
	}
}
