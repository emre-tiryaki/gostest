// Package backend runs commands either on the host or inside containers and
// prepares the container images the commands run in.
package backend

import (
	"context"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/emre-tiryaki/gostest/internal/distro"
)

// ExitTimeout is the exit code reported for commands that hit their timeout.
// It matches the convention of timeout(1).
const ExitTimeout = 124

// MaxOutput is the maximum number of bytes captured per output stream.
const MaxOutput = 1 << 20

// Spec describes one command execution.
type Spec struct {
	// Command is run with /bin/sh -c.
	Command string
	// Image is the container image to run in. The host runner ignores it.
	Image string
	// Dir is the host working directory. Containers get it mounted
	// read-only at the same path, so paths in the output match the host.
	Dir string
	// Timeout bounds the execution; zero means no limit.
	Timeout time.Duration
	// Mounts are host files mounted read-only into the container. The
	// host runner ignores them.
	Mounts []Mount
}

// Mount is a host file mounted read-only into containers.
type Mount struct {
	// Source is the path on the host.
	Source string `json:"source"`
	// Target is the path inside the container.
	Target string `json:"target"`
	// Hash identifies the contents of Source, so cached results are not
	// reused once the file changes.
	Hash string `json:"hash,omitempty"`
}

// Result is what a command produced.
type Result struct {
	Stdout    string        `json:"stdout"`
	Stderr    string        `json:"stderr"`
	ExitCode  int           `json:"exit_code"`
	TimedOut  bool          `json:"timed_out,omitempty"`
	Truncated bool          `json:"truncated,omitempty"`
	Duration  time.Duration `json:"duration"`
}

// Runner executes a Spec. A non-zero exit code or a timeout is reported in
// the Result; the error is reserved for failures to run the command at all
// and for cancellation of ctx.
type Runner interface {
	Run(ctx context.Context, spec Spec) (Result, error)
}

// Environment is a prepared container environment for one distribution.
type Environment struct {
	Distro distro.Distro `json:"distro"`
	// Image is the image commands run in: the distribution's image, or a
	// derived image with Packages installed.
	Image   string `json:"image"`
	ImageID string `json:"image_id"`
	// Shell is what /bin/sh is in the image, e.g. "dash".
	Shell    string   `json:"shell"`
	Packages []string `json:"packages,omitempty"`
	// Provided lists the missing commands that were installed
	// automatically, see Docker.Provide.
	Provided []string `json:"provided,omitempty"`
	// Mounts are the host tools mounted into every run because they were
	// missing in the image.
	Mounts []Mount `json:"mounts,omitempty"`
}

// ProbeCommand prints what /bin/sh resolves to. The ls fallback covers
// images without readlink.
const ProbeCommand = "readlink -f /bin/sh 2>/dev/null || ls -l /bin/sh"

// ShellName turns ProbeCommand output (or a resolved path) into a shell
// name such as "dash", "bash" or "busybox ash".
func ShellName(probe string) string {
	lines := strings.Split(strings.TrimSpace(probe), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if i := strings.LastIndex(last, " -> "); i >= 0 {
		last = strings.TrimSpace(last[i+len(" -> "):])
	} else if fields := strings.Fields(last); len(fields) > 0 {
		last = fields[len(fields)-1]
	}
	if last == "" {
		return "unknown"
	}
	name := path.Base(last)
	if name == "busybox" {
		return "busybox ash"
	}
	return name
}

// HostShell reports what /bin/sh is on the host.
func HostShell() string {
	target, err := filepath.EvalSymlinks("/bin/sh")
	if err != nil {
		return "unknown"
	}
	return ShellName(target)
}
