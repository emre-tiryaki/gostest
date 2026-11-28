package backend

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/emre-tiryaki/gostest/internal/distro"
)

// Executor runs an external program. A non-zero exit status is returned as
// exitCode with a nil error; err reports failures to run the program and
// the end of ctx.
type Executor interface {
	Exec(ctx context.Context, stdin io.Reader, name string, args ...string) (stdout, stderr []byte, exitCode int, err error)
}

// OSExecutor runs programs with os/exec.
type OSExecutor struct{}

// Exec implements Executor. It keeps one byte more than MaxOutput of each
// stream, so that the caller can tell that output was cut.
func (OSExecutor) Exec(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, []byte, int, error) {
	res, err := runCapped(ctx, MaxOutput+1, 0, "", stdin, name, args...)
	if err != nil {
		return []byte(res.Stdout), []byte(res.Stderr), -1, err
	}
	return []byte(res.Stdout), []byte(res.Stderr), res.ExitCode, nil
}

// Docker runs commands in containers through the docker CLI. Podman is
// supported through its docker-compatible CLI.
type Docker struct {
	// Binary is the CLI to call, "docker" or "podman".
	Binary string
	// Exec runs Binary; replaceable in tests.
	Exec Executor

	mu sync.Mutex
	// owned lists, in the order they were first used, the images Cleanup
	// removes: base images this Docker pulled and every gostest image it
	// built or reused.
	owned []string
}

// NewDocker returns a Docker backend using binary ("docker" or "podman").
func NewDocker(binary string) *Docker {
	return &Docker{Binary: binary, Exec: OSExecutor{}}
}

// Name returns the name of the container engine CLI.
func (d *Docker) Name() string { return d.Binary }

const (
	removeTimeout = 15 * time.Second
	probeTimeout  = time.Minute
	// engineExitCode is what docker/podman run return for their own errors.
	engineExitCode = 125
)

// Available reports whether the container engine can be reached.
func (d *Docker) Available(ctx context.Context) error {
	_, stderr, code, err := d.Exec.Exec(ctx, nil, d.Binary, "version")
	if err != nil {
		return fmt.Errorf("%s is not available: %w", d.Binary, err)
	}
	if code != 0 {
		return fmt.Errorf("%s is not available: %s", d.Binary, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// Run executes spec inside a new, automatically removed container. The
// working directory is bind-mounted read-only, at the same path as on the
// host when that is safe (see MountPoint), and so are spec.Mounts. On timeout or cancellation the
// container is force-removed.
func (d *Docker) Run(ctx context.Context, spec Spec) (Result, error) {
	if strings.Contains(spec.Dir, ":") {
		return Result{}, fmt.Errorf("cannot mount %q: paths containing ':' are not supported", spec.Dir)
	}
	for _, m := range spec.Mounts {
		if strings.Contains(m.Source, ":") || strings.Contains(m.Target, ":") {
			return Result{}, fmt.Errorf("cannot mount %q at %q: paths containing ':' are not supported", m.Source, m.Target)
		}
	}
	name := "gostest-" + randomHex(8)
	// --init keeps the shell from being PID 1, which the kernel shields from
	// signals it does not handle, unlike the shell of the reference run.
	args := []string{"run", "--rm", "--init", "--name", name, "--pull=never"}
	if spec.Dir != "" {
		target := MountPoint(spec.Dir)
		args = append(args, "-v", spec.Dir+":"+target+":ro", "-w", target)
	}
	for _, m := range spec.Mounts {
		args = append(args, "-v", m.Source+":"+m.Target+":ro")
	}
	args = append(args, "--entrypoint", "/bin/sh", spec.Image, "-c", spec.Command)

	runCtx, cancel := withTimeout(ctx, spec.Timeout)
	defer cancel()
	start := time.Now()
	stdout, stderr, code, err := d.Exec.Exec(runCtx, nil, d.Binary, args...)
	res := Result{Duration: time.Since(start)}
	var t1, t2 bool
	res.Stdout, t1 = truncate(stdout)
	res.Stderr, t2 = truncate(stderr)
	res.Truncated = t1 || t2

	if runCtx.Err() != nil {
		// Killing the CLI client does not stop the container.
		d.remove(name)
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		res.TimedOut = true
		res.ExitCode = ExitTimeout
		return res, nil
	}
	if err != nil {
		return res, fmt.Errorf("%s run: %w", d.Binary, err)
	}
	if code == engineExitCode && d.isEngineError(res.Stderr) {
		return res, fmt.Errorf("%s run %s: %s", d.Binary, spec.Image, strings.TrimSpace(res.Stderr))
	}
	res.ExitCode = code
	return res, nil
}

// FallbackMountPoint is where working directories that cannot be mounted
// in place are mounted inside containers.
const FallbackMountPoint = "/work"

// systemTrees are directories whose whole subtree belongs to the
// distribution; mounting anything there would replace part of the
// userland under test.
var systemTrees = []string{"/bin", "/boot", "/dev", "/etc", "/lib", "/lib32", "/lib64", "/libx32", "/proc", "/run", "/sbin", "/sys", "/usr", "/var"}

// systemRoots are directories that must not be shadowed as a whole,
// although their subdirectories are fine to mount.
var systemRoots = []string{"/", "/home", "/media", "/mnt", "/opt", "/root", "/srv", "/tmp"}

// MountPoint returns where the host directory dir is mounted inside
// containers: at the same path, so paths in the output match the host,
// unless that would shadow part of the distribution's filesystem.
func MountPoint(dir string) string {
	clean := filepath.Clean(dir)
	if slices.Contains(systemRoots, clean) {
		return FallbackMountPoint
	}
	for _, tree := range systemTrees {
		if clean == tree || strings.HasPrefix(clean, tree+"/") {
			return FallbackMountPoint
		}
	}
	return clean
}

// Cleanup removes the images this Docker pulled, built or reused from an
// earlier run (gostest/...); base images that were already there are kept.
// Images built on top of others are removed first. An image that no longer
// exists is not an error; other failures are collected and returned after
// trying every image. Containers need no cleanup: Run removes them.
func (d *Docker) Cleanup(ctx context.Context) error {
	d.mu.Lock()
	owned := d.owned
	d.owned = nil
	d.mu.Unlock()

	var errs []error
	for _, ref := range slices.Backward(owned) {
		_, stderr, code, err := d.Exec.Exec(ctx, nil, d.Binary, "image", "rm", ref)
		if err != nil {
			errs = append(errs, fmt.Errorf("removing %s: %w", ref, err))
			continue
		}
		if code == 0 {
			continue
		}
		if _, exists, ierr := d.imageID(ctx, ref); ierr == nil && !exists {
			continue
		}
		errs = append(errs, fmt.Errorf("removing %s: %s", ref, tail(string(stderr), 3)))
	}
	return errors.Join(errs...)
}

// own records ref for Cleanup.
func (d *Docker) own(ref string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !slices.Contains(d.owned, ref) {
		d.owned = append(d.owned, ref)
	}
}

func (d *Docker) isEngineError(stderr string) bool {
	s := strings.TrimSpace(stderr)
	return strings.HasPrefix(s, d.Binary+":") ||
		strings.HasPrefix(s, "Error: ") ||
		strings.Contains(s, "Error response from daemon")
}

func (d *Docker) remove(name string) {
	ctx, cancel := context.WithTimeout(context.Background(), removeTimeout)
	defer cancel()
	_, _, _, _ = d.Exec.Exec(ctx, nil, d.Binary, "rm", "-f", name)
}

func truncate(b []byte) (string, bool) {
	if len(b) > MaxOutput {
		return string(b[:MaxOutput]), true
	}
	return string(b), false
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Prepare makes sure the image for dist exists locally, pulling it when
// needed, and, when pkgs is not empty, builds a derived image with the
// packages installed by the distribution's package manager. Derived images
// are tagged by a hash of the base image ID and the install script, so they
// are reused until either changes; rebuild forces a fresh build. Finally the
// image is probed for its /bin/sh.
func (d *Docker) Prepare(ctx context.Context, dist distro.Distro, pkgs []string, rebuild bool) (Environment, error) {
	script, err := distro.InstallScript(dist.PackageManager, pkgs)
	if err != nil {
		return Environment{}, fmt.Errorf("%s: %w", dist.Name, err)
	}
	baseID, err := d.ensureImage(ctx, dist.Image)
	if err != nil {
		return Environment{}, err
	}
	env := Environment{Distro: dist, Image: dist.Image, ImageID: baseID, Packages: slices.Clone(pkgs)}

	if script != "" {
		tag := derivedTag(dist, baseID, script)
		id, err := d.derive(ctx, dist, dist.Image, tag, script, pkgs, rebuild)
		if err != nil {
			return Environment{}, err
		}
		env.Image, env.ImageID = tag, id
	}
	env.Shell = d.probe(ctx, env.Image)
	return env, nil
}

// Labels of the images Provide builds: the ID of the environment image
// they were provided for, and the sorted, space separated commands they
// tried to install on it.
const (
	LabelBase     = "io.gostest.base"
	LabelCommands = "io.gostest.commands"
)

// Provide builds, on top of env, an image in which the distribution's
// package manager tried to install a package providing each of cmds, and
// returns the environment of that image. Its Provided field adds the
// commands of cmds that are available there; a command no package
// provides is left out rather than reported as an error. Like the images
// of Prepare, the image is reused until env or the set of cmds changes,
// whatever their order. When it does not exist yet, it is built on the
// earlier image of env whose commands are the largest subset of cmds, so
// that only the new commands are installed. rebuild forces a fresh build
// of every command on env.
func (d *Docker) Provide(ctx context.Context, env Environment, cmds []string, rebuild bool) (Environment, error) {
	var asked []string
	for _, c := range cmds {
		if !slices.Contains(asked, c) {
			asked = append(asked, c)
		}
	}
	set := slices.Sorted(slices.Values(asked))
	script, err := distro.ProvideScript(env.Distro.PackageManager, set)
	if err != nil {
		return Environment{}, fmt.Errorf("%s: %w", env.Distro.Name, err)
	}
	if script == "" {
		return env, nil
	}
	tag := derivedTag(env.Distro, env.ImageID, script)
	d.own(tag)
	id, ok, err := d.imageID(ctx, tag)
	if err != nil {
		return Environment{}, err
	}
	if !ok || rebuild {
		from, todo := env.Image, script
		if !rebuild {
			if base, have := d.closestProvided(ctx, env.ImageID, set); base != "" {
				d.own(base)
				missing := slices.DeleteFunc(slices.Clone(set), func(c string) bool { return slices.Contains(have, c) })
				if todo, err = distro.ProvideScript(env.Distro.PackageManager, missing); err != nil {
					return Environment{}, fmt.Errorf("%s: %w", env.Distro.Name, err)
				}
				from = base
			}
		}
		labels := []string{LabelBase + "=" + env.ImageID, LabelCommands + `="` + strings.Join(set, " ") + `"`}
		if id, err = d.buildImage(ctx, env.Distro, from, tag, todo, asked, labels, rebuild); err != nil {
			return Environment{}, err
		}
	}
	found, err := d.available(ctx, tag, asked)
	if err != nil {
		return Environment{}, fmt.Errorf("%s: checking installed commands: %w", env.Distro.Name, err)
	}
	out := env
	out.Image, out.ImageID = tag, id
	out.Packages = slices.Clone(env.Packages)
	out.Mounts = slices.Clone(env.Mounts)
	out.Provided = append(slices.Clone(env.Provided), found...)
	return out, nil
}

// closestProvided returns, among the images Provide built for the image
// baseID, the one whose commands are the largest strict subset of set
// (sorted), with those commands; it returns "" when there is none. Errors
// only mean that nothing is reused.
func (d *Docker) closestProvided(ctx context.Context, baseID string, set []string) (string, []string) {
	stdout, _, code, err := d.Exec.Exec(ctx, nil, d.Binary, "images", "--filter", "label="+LabelBase+"="+baseID, "--format", "{{.Repository}}:{{.Tag}}")
	if err != nil || code != 0 {
		return "", nil
	}
	var best string
	var bestCmds []string
	for ref := range strings.SplitSeq(string(stdout), "\n") {
		ref = strings.TrimSpace(ref)
		if !strings.HasPrefix(ref, "gostest/") || strings.Contains(ref, "<none>") {
			continue
		}
		out, _, code, err := d.Exec.Exec(ctx, nil, d.Binary, "image", "inspect", "--format", `{{index .Config.Labels "`+LabelCommands+`"}}`, ref)
		if err != nil || code != 0 {
			continue
		}
		have := strings.Fields(string(out))
		if len(have) == 0 || len(have) >= len(set) || len(have) <= len(bestCmds) {
			continue
		}
		if !slices.ContainsFunc(have, func(c string) bool { return !slices.Contains(set, c) }) {
			best, bestCmds = ref, have
		}
	}
	return best, bestCmds
}

// available returns the commands of cmds that image can run, in the
// order of cmds.
func (d *Docker) available(ctx context.Context, image string, cmds []string) ([]string, error) {
	quoted := make([]string, len(cmds))
	for i, c := range cmds {
		quoted[i] = "'" + c + "'"
	}
	check := "for c in " + strings.Join(quoted, " ") + `; do command -v "$c" >/dev/null 2>&1 && echo "$c"; done; true`
	res, err := d.Run(ctx, Spec{Image: image, Command: check, Timeout: probeTimeout})
	if err != nil {
		return nil, err
	}
	if res.TimedOut || res.ExitCode != 0 {
		return nil, fmt.Errorf("exit code %d: %s", res.ExitCode, tail(res.Stderr, 5))
	}
	var lines []string
	for l := range strings.SplitSeq(res.Stdout, "\n") {
		lines = append(lines, strings.TrimSpace(l))
	}
	var found []string
	for _, c := range cmds {
		if slices.Contains(lines, c) && !slices.Contains(found, c) {
			found = append(found, c)
		}
	}
	return found, nil
}

// derive returns the ID of tag, building it from the image from with
// script when it does not exist yet or rebuild is set.
func (d *Docker) derive(ctx context.Context, dist distro.Distro, from, tag, script string, what []string, rebuild bool) (string, error) {
	d.own(tag)
	id, ok, err := d.imageID(ctx, tag)
	if err != nil {
		return "", err
	}
	if ok && !rebuild {
		return id, nil
	}
	return d.buildImage(ctx, dist, from, tag, script, what, nil, rebuild)
}

// buildImage builds tag with build and returns its ID.
func (d *Docker) buildImage(ctx context.Context, dist distro.Distro, from, tag, script string, what, labels []string, noCache bool) (string, error) {
	if err := d.build(ctx, dist, from, tag, script, what, labels, noCache); err != nil {
		return "", err
	}
	id, ok, err := d.imageID(ctx, tag)
	if err != nil {
		return "", err
	} else if !ok {
		return "", fmt.Errorf("%s: built image %s not found", dist.Name, tag)
	}
	return id, nil
}

func derivedTag(dist distro.Distro, baseID, script string) string {
	sum := sha256.Sum256([]byte(baseID + "\n" + script))
	// Repository names must be lowercase, but image tags such as "Jammy" are not.
	return "gostest/" + strings.ToLower(strings.ReplaceAll(dist.Name, ":", "-")) + ":" + hex.EncodeToString(sum[:])[:12]
}

// imageID returns the ID of a local image and whether it exists.
func (d *Docker) imageID(ctx context.Context, ref string) (string, bool, error) {
	stdout, _, code, err := d.Exec.Exec(ctx, nil, d.Binary, "image", "inspect", "--format", "{{.Id}}", ref)
	if err != nil {
		return "", false, fmt.Errorf("%s image inspect %s: %w", d.Binary, ref, err)
	}
	if code != 0 {
		return "", false, nil
	}
	return strings.TrimSpace(string(stdout)), true, nil
}

func (d *Docker) ensureImage(ctx context.Context, ref string) (string, error) {
	if id, ok, err := d.imageID(ctx, ref); err != nil || ok {
		return id, err
	}
	// Owned before pulling, so an interrupted pull is cleaned up too.
	d.own(ref)
	stdout, stderr, code, err := d.Exec.Exec(ctx, nil, d.Binary, "pull", "--quiet", ref)
	if err != nil {
		return "", fmt.Errorf("pulling %s: %w", ref, err)
	}
	if code != 0 {
		return "", fmt.Errorf("pulling %s failed: %s", ref, tail(string(stderr)+string(stdout), 10))
	}
	id, ok, err := d.imageID(ctx, ref)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("image %s not found after pulling it", ref)
	}
	return id, nil
}

// build builds tag from the image from by running script, which installs
// what (packages or commands, for messages), and sets labels (key=value).
func (d *Docker) build(ctx context.Context, dist distro.Distro, from, tag, script string, what, labels []string, noCache bool) error {
	dir, err := os.MkdirTemp("", "gostest-build-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)

	dockerfile := "FROM " + from + "\n" +
		"LABEL io.gostest.distro=" + dist.Name + "\n"
	for _, l := range labels {
		dockerfile += "LABEL " + l + "\n"
	}
	dockerfile += "RUN " + script + "\n"
	args := []string{"build", "-t", tag, "-f", "-"}
	if noCache {
		args = append(args, "--no-cache")
	}
	args = append(args, dir)
	stdout, stderr, code, err := d.Exec.Exec(ctx, strings.NewReader(dockerfile), d.Binary, args...)
	if err != nil {
		return fmt.Errorf("%s: installing %s: %w", dist.Name, strings.Join(what, " "), err)
	}
	if code != 0 {
		return fmt.Errorf("%s: installing %s failed:\n%s", dist.Name, strings.Join(what, " "), tail(string(stdout)+string(stderr), 20))
	}
	return nil
}

// probe reports the image's /bin/sh, or "unknown" when it cannot be told.
func (d *Docker) probe(ctx context.Context, image string) string {
	res, err := d.Run(ctx, Spec{Image: image, Command: ProbeCommand, Timeout: probeTimeout})
	if err != nil || res.ExitCode != 0 {
		return "unknown"
	}
	return ShellName(res.Stdout)
}

// tail returns the last n non-empty lines of s.
func tail(s string, n int) string {
	var lines []string
	for l := range strings.SplitSeq(s, "\n") {
		if strings.TrimSpace(l) != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
