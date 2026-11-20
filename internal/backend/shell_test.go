package backend

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestShellName(t *testing.T) {
	tests := map[string]string{
		"/usr/bin/bash\n": "bash",
		"/bin/bash":       "bash",
		"  /bin/bash  \n": "bash",
		"/usr/bin/dash\n": "dash",
		"/bin/busybox\n":  "busybox ash",
		"/usr/bin/mksh":   "mksh",
		"/bin/sh":         "sh",
		"dash":            "dash",
		"":                "unknown",
		"   \n":           "unknown",
		"lrwxrwxrwx 1 root root 4 Jan  1  2024 /bin/sh -> dash\n": "dash",
		"noise\n/usr/bin/dash\n":                                  "dash",
	}
	for in, want := range tests {
		if got := ShellName(in); got != want {
			t.Errorf("ShellName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestHostShell(t *testing.T) {
	target, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		t.Skipf("cannot resolve /bin/sh: %v", err)
	}
	got := HostShell()
	if got != ShellName(target) {
		t.Fatalf("HostShell() = %q, want %q", got, ShellName(target))
	}
	if got == "" || got == "unknown" {
		t.Fatalf("HostShell() = %q, want a real shell name", got)
	}
}

func TestProbeCommandFallsBack(t *testing.T) {
	if !strings.Contains(ProbeCommand, "readlink -f /bin/sh") {
		t.Errorf("ProbeCommand should resolve /bin/sh with readlink: %q", ProbeCommand)
	}
	if !strings.Contains(ProbeCommand, "||") {
		t.Errorf("ProbeCommand should fall back when readlink is missing: %q", ProbeCommand)
	}
}

func TestProbeCommandOnHost(t *testing.T) {
	res := runLocal(t, Spec{Command: ProbeCommand})
	if res.ExitCode != 0 {
		t.Fatalf("probe failed: %+v", res)
	}
	if got := ShellName(res.Stdout); got != HostShell() {
		t.Fatalf("probe says %q, HostShell says %q", got, HostShell())
	}
}
