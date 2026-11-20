package backend

import "context"

// Local runs commands on the host. Its results are the reference every
// container result is compared with.
type Local struct{}

// Run executes spec.Command with /bin/sh -c in spec.Dir (the current
// directory when empty), with an empty stdin and the host environment.
func (Local) Run(ctx context.Context, spec Spec) (Result, error) {
	return runProcess(ctx, spec.Timeout, spec.Dir, nil, "/bin/sh", "-c", spec.Command)
}
