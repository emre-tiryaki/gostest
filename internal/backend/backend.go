// Package backend runs commands either on the host or inside containers and
// prepares the container images the commands run in.
package backend

import (
	"context"
	"time"

	"github.com/emre-tiryaki/gostest/internal/distro"
)

// Spec describes one command execution.
type Spec struct {
	// Command is run with /bin/sh -c.
	Command string
	// Image is the container image to run in. The host runner ignores it.
	Image string
	// Dir is the host working directory. Containers get it mounted
	// read-only at the same path.
	Dir string
	// Timeout bounds the execution; zero means no limit.
	Timeout time.Duration
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

// Runner executes a Spec. A non-zero exit code is reported in the Result;
// the error is reserved for failures to run the command at all.
type Runner interface {
	Run(ctx context.Context, spec Spec) (Result, error)
}

// Environment is a prepared container environment for one distribution.
type Environment struct {
	Distro   distro.Distro `json:"distro"`
	Image    string        `json:"image"`
	ImageID  string        `json:"image_id"`
	Shell    string        `json:"shell"`
	Packages []string      `json:"packages,omitempty"`
}

// ProbeCommand prints what /bin/sh resolves to.
const ProbeCommand = "readlink -f /bin/sh"

// ShellName turns ProbeCommand output into a shell name.
func ShellName(probe string) string { return probe }

// HostShell reports what /bin/sh is on the host.
func HostShell() string { return "" }
