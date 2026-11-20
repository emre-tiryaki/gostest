// Package explain turns the difference between a target result and the
// reference into human readable reasons.
package explain

import (
	"fmt"

	"github.com/emre-tiryaki/gostest/internal/backend"
	"github.com/emre-tiryaki/gostest/internal/compare"
)

// Kind classifies a reason.
type Kind string

// Reason kinds.
const (
	KindExitCode Kind = "exit-code"
)

// Reason explains one aspect of a difference.
type Reason struct {
	Kind    Kind   `json:"kind"`
	Message string `json:"message"`
}

// Context describes the environments being compared.
type Context struct {
	// Distro is the target distribution name, e.g. "alpine".
	Distro string
	// Shell is what /bin/sh is in the target, e.g. "dash".
	Shell string
	// RefShell is what /bin/sh is on the host.
	RefShell string
}

// Explain returns the reasons why got differs from the reference ref.
func Explain(ref, got backend.Result, cmp compare.Comparison, c Context) []Reason {
	if cmp.ExitCodeMatch {
		return nil
	}
	return []Reason{{Kind: KindExitCode, Message: fmt.Sprintf("exit code %d, reference exited %d", got.ExitCode, ref.ExitCode)}}
}
