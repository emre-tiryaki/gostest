package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emre-tiryaki/gostest/internal/distro"
)

// fakeEngine simulates the docker CLI.
type fakeEngine struct {
	mu       sync.Mutex
	calls    [][]string
	stdins   []string
	binaries []string

	images   map[string]string            // local ref -> image ID
	labels   map[string]map[string]string // local ref -> labels set by builds
	remote   map[string]string            // pullable ref -> image ID
	probe    map[string]string            // image -> probe stdout
	buildErr string                       // non-empty: builds fail with this stderr
	rmErr    map[string]string            // ref -> stderr of a failing image rm
	builds   int

	version func() ([]byte, []byte, int, error)
	run     func(ctx context.Context, image, command string) ([]byte, []byte, int, error)
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		images: map[string]string{},
		labels: map[string]map[string]string{},
		remote: map[string]string{},
		rmErr:  map[string]string{},
		probe:  map[string]string{},
	}
}

func (f *fakeEngine) Exec(ctx context.Context, stdin io.Reader, name string, args ...string) ([]byte, []byte, int, error) {
	var in string
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	f.mu.Lock()
	f.calls = append(f.calls, append([]string(nil), args...))
	f.stdins = append(f.stdins, in)
	f.binaries = append(f.binaries, name)
	f.mu.Unlock()

	switch args[0] {
	case "version":
		if f.version != nil {
			return f.version()
		}
		return []byte("29.0.0\n"), nil, 0, nil
	case "image":
		ref := args[len(args)-1]
		if args[1] == "rm" {
			f.mu.Lock()
			defer f.mu.Unlock()
			if msg := f.rmErr[ref]; msg != "" {
				return nil, []byte(msg), 1, nil
			}
			if _, ok := f.images[ref]; !ok {
				return nil, []byte("Error response from daemon: No such image: " + ref + "\n"), 1, nil
			}
			delete(f.images, ref)
			return []byte("Untagged: " + ref + "\n"), nil, 0, nil
		}
		f.mu.Lock()
		id, ok := f.images[ref]
		labels := f.labels[ref]
		f.mu.Unlock()
		if !ok {
			return nil, []byte("Error: No such image: " + ref + "\n"), 1, nil
		}
		if format := argAfter(args, "--format"); strings.Contains(format, ".Config.Labels") {
			key := strings.Split(format, `"`)[1]
			return []byte(labels[key] + "\n"), nil, 0, nil
		}
		return []byte(id + "\n"), nil, 0, nil
	case "images":
		// images --filter label=<key>=<value> --format ...
		key, value, _ := strings.Cut(strings.TrimPrefix(argAfter(args, "--filter"), "label="), "=")
		f.mu.Lock()
		defer f.mu.Unlock()
		var out []string
		for ref, l := range f.labels {
			if _, ok := f.images[ref]; ok && l[key] == value {
				out = append(out, ref+"\n")
			}
		}
		slices.Sort(out)
		return []byte(strings.Join(out, "")), nil, 0, nil
	case "pull":
		ref := args[len(args)-1]
		f.mu.Lock()
		defer f.mu.Unlock()
		id, ok := f.remote[ref]
		if !ok {
			return nil, []byte("Error response from daemon: pull access denied for " + ref + "\n"), 1, nil
		}
		f.images[ref] = id
		return []byte("Status: Downloaded newer image for " + ref + "\n"), nil, 0, nil
	case "build":
		f.mu.Lock()
		defer f.mu.Unlock()
		f.builds++
		if f.buildErr != "" {
			return []byte("step 1/2\n"), []byte(f.buildErr), 1, nil
		}
		tag := args[slices.Index(args, "-t")+1]
		f.images[tag] = fmt.Sprintf("sha256:built%d", f.builds)
		labels := map[string]string{}
		for l := range strings.SplitSeq(in, "\n") {
			if kv, ok := strings.CutPrefix(l, "LABEL "); ok {
				k, v, _ := strings.Cut(kv, "=")
				labels[k] = strings.Trim(v, `"`)
			}
		}
		f.labels[tag] = labels
		return nil, nil, 0, nil
	case "rm":
		return nil, nil, 0, nil
	case "run":
		image, command := args[len(args)-3], args[len(args)-1]
		if f.run != nil {
			return f.run(ctx, image, command)
		}
		if command == ProbeCommand {
			f.mu.Lock()
			out, ok := f.probe[image]
			f.mu.Unlock()
			if !ok {
				out = "/bin/busybox\n"
			}
			return []byte(out), nil, 0, nil
		}
		return nil, nil, 0, nil
	}
	return nil, []byte("unknown command " + args[0]), 1, nil
}

func (f *fakeEngine) callsTo(sub string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if c[0] == sub {
			out = append(out, c)
		}
	}
	return out
}

func (f *fakeEngine) stdinOf(sub string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.calls {
		if c[0] == sub {
			return f.stdins[i]
		}
	}
	return ""
}

// dockerfiles returns the Dockerfile of every build, in order.
func (f *fakeEngine) dockerfiles() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for i, c := range f.calls {
		if c[0] == "build" {
			out = append(out, f.stdins[i])
		}
	}
	return out
}

func newTestDocker(f *fakeEngine) *Docker {
	d := NewDocker("docker")
	d.Exec = f
	return d
}

func argAfter(args []string, flag string) string {
	i := slices.Index(args, flag)
	if i < 0 || i+1 >= len(args) {
		return ""
	}
	return args[i+1]
}

func TestNewDockerDefaults(t *testing.T) {
	d := NewDocker("podman")
	if d.Binary != "podman" {
		t.Errorf("Binary = %q", d.Binary)
	}
	if _, ok := d.Exec.(OSExecutor); !ok {
		t.Errorf("Exec = %T, want OSExecutor", d.Exec)
	}
	if d.Name() != "podman" {
		t.Errorf("Name() = %q", d.Name())
	}
}

func TestDockerRunArguments(t *testing.T) {
	f := newFakeEngine()
	d := newTestDocker(f)
	if _, err := d.Run(context.Background(), Spec{Command: "echo hi", Image: "alpine:latest", Dir: "/home/u/proj"}); err != nil {
		t.Fatal(err)
	}
	runs := f.callsTo("run")
	if len(runs) != 1 {
		t.Fatalf("got %d run calls", len(runs))
	}
	args := runs[0]
	for _, want := range []string{"--rm", "--pull=never"} {
		if !slices.Contains(args, want) {
			t.Errorf("args %v missing %q", args, want)
		}
	}
	if name := argAfter(args, "--name"); !strings.HasPrefix(name, "gostest-") {
		t.Errorf("container name = %q, want gostest- prefix", name)
	}
	if got := argAfter(args, "-v"); got != "/home/u/proj:/home/u/proj:ro" {
		t.Errorf("mount = %q", got)
	}
	if got := argAfter(args, "-w"); got != "/home/u/proj" {
		t.Errorf("workdir = %q", got)
	}
	if got := argAfter(args, "--entrypoint"); got != "/bin/sh" {
		t.Errorf("entrypoint = %q", got)
	}
	tail := args[len(args)-3:]
	if !slices.Equal(tail, []string{"alpine:latest", "-c", "echo hi"}) {
		t.Errorf("args tail = %v", tail)
	}
	if f.binaries[0] != "docker" {
		t.Errorf("binary = %q", f.binaries[0])
	}
}

func TestDockerRunUsesAnInitProcess(t *testing.T) {
	f := newFakeEngine()
	d := newTestDocker(f)
	if _, err := d.Run(context.Background(), Spec{Command: "true", Image: "alpine"}); err != nil {
		t.Fatal(err)
	}
	args := f.callsTo("run")[0]
	// Without an init the shell is PID 1, which ignores signals it sends
	// to itself. The option has to precede the image to apply to docker.
	if i := slices.Index(args, "--init"); i < 0 || i > slices.Index(args, "alpine") {
		t.Errorf("args %v: want --init before the image", args)
	}
}

func TestDockerRunWithoutDirHasNoMount(t *testing.T) {
	f := newFakeEngine()
	d := newTestDocker(f)
	if _, err := d.Run(context.Background(), Spec{Command: "true", Image: "alpine"}); err != nil {
		t.Fatal(err)
	}
	args := f.callsTo("run")[0]
	if slices.Contains(args, "-v") || slices.Contains(args, "-w") {
		t.Fatalf("unexpected mount in %v", args)
	}
}

func TestDockerRunUsesConfiguredBinary(t *testing.T) {
	f := newFakeEngine()
	d := NewDocker("podman")
	d.Exec = f
	if _, err := d.Run(context.Background(), Spec{Command: "true", Image: "alpine"}); err != nil {
		t.Fatal(err)
	}
	if f.binaries[0] != "podman" {
		t.Fatalf("binary = %q, want podman", f.binaries[0])
	}
}

func TestDockerRunUniqueContainerNames(t *testing.T) {
	f := newFakeEngine()
	d := newTestDocker(f)
	for range 2 {
		if _, err := d.Run(context.Background(), Spec{Command: "true", Image: "alpine"}); err != nil {
			t.Fatal(err)
		}
	}
	runs := f.callsTo("run")
	if argAfter(runs[0], "--name") == argAfter(runs[1], "--name") {
		t.Fatal("container names must be unique")
	}
}

func TestDockerRunResult(t *testing.T) {
	f := newFakeEngine()
	f.run = func(context.Context, string, string) ([]byte, []byte, int, error) {
		return []byte("out\n"), []byte("err\n"), 3, nil
	}
	res, err := newTestDocker(f).Run(context.Background(), Spec{Command: "x", Image: "alpine"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "out\n" || res.Stderr != "err\n" || res.ExitCode != 3 || res.TimedOut {
		t.Fatalf("unexpected result %+v", res)
	}
}

func TestDockerRunEngineErrorIsAnError(t *testing.T) {
	f := newFakeEngine()
	f.run = func(context.Context, string, string) ([]byte, []byte, int, error) {
		return nil, []byte("docker: Error response from daemon: No such image: alpine:latest.\n"), 125, nil
	}
	_, err := newTestDocker(f).Run(context.Background(), Spec{Command: "x", Image: "alpine:latest"})
	if err == nil || !strings.Contains(err.Error(), "No such image") {
		t.Fatalf("err = %v, want engine error", err)
	}
}

func TestDockerRunCommandExiting125IsAResult(t *testing.T) {
	f := newFakeEngine()
	f.run = func(context.Context, string, string) ([]byte, []byte, int, error) {
		return nil, []byte("my tool failed\n"), 125, nil
	}
	res, err := newTestDocker(f).Run(context.Background(), Spec{Command: "exit 125", Image: "alpine"})
	if err != nil {
		t.Fatalf("a command exiting 125 is not an engine error: %v", err)
	}
	if res.ExitCode != 125 {
		t.Fatalf("exit code = %d", res.ExitCode)
	}
}

func TestDockerRunExecFailure(t *testing.T) {
	f := newFakeEngine()
	f.run = func(context.Context, string, string) ([]byte, []byte, int, error) {
		return nil, nil, -1, errors.New("exec: \"docker\": executable file not found in $PATH")
	}
	if _, err := newTestDocker(f).Run(context.Background(), Spec{Command: "x", Image: "alpine"}); err == nil {
		t.Fatal("expected an error")
	}
}

func blockingRun(ctx context.Context, _, _ string) ([]byte, []byte, int, error) {
	<-ctx.Done()
	return []byte("partial\n"), nil, -1, ctx.Err()
}

func TestDockerRunTimeoutRemovesContainer(t *testing.T) {
	f := newFakeEngine()
	f.run = blockingRun
	d := newTestDocker(f)
	res, err := d.Run(context.Background(), Spec{Command: "sleep 100", Image: "alpine", Timeout: 50 * time.Millisecond})
	if err != nil {
		t.Fatalf("timeout is a result, not an error: %v", err)
	}
	if !res.TimedOut || res.ExitCode != ExitTimeout {
		t.Fatalf("unexpected result %+v", res)
	}
	if res.Stdout != "partial\n" {
		t.Errorf("stdout = %q, want partial output", res.Stdout)
	}
	name := argAfter(f.callsTo("run")[0], "--name")
	rms := f.callsTo("rm")
	if len(rms) != 1 || !slices.Equal(rms[0], []string{"rm", "-f", name}) {
		t.Fatalf("rm calls = %v, want rm -f %s", rms, name)
	}
}

func TestDockerRunCancellationRemovesContainer(t *testing.T) {
	f := newFakeEngine()
	f.run = blockingRun
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := newTestDocker(f).Run(ctx, Spec{Command: "sleep 100", Image: "alpine"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if len(f.callsTo("rm")) != 1 {
		t.Fatal("container should be removed after cancellation")
	}
}

func TestDockerRunSuccessDoesNotRemove(t *testing.T) {
	f := newFakeEngine()
	if _, err := newTestDocker(f).Run(context.Background(), Spec{Command: "true", Image: "alpine"}); err != nil {
		t.Fatal(err)
	}
	if len(f.callsTo("rm")) != 0 {
		t.Fatal("--rm already removes finished containers")
	}
}

func TestDockerRunTruncatesHugeOutput(t *testing.T) {
	f := newFakeEngine()
	f.run = func(context.Context, string, string) ([]byte, []byte, int, error) {
		return []byte(strings.Repeat("a", MaxOutput+10)), []byte("e"), 0, nil
	}
	res, err := newTestDocker(f).Run(context.Background(), Spec{Command: "x", Image: "alpine"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Truncated || len(res.Stdout) != MaxOutput || res.Stderr != "e" {
		t.Fatalf("truncated=%v len=%d stderr=%q", res.Truncated, len(res.Stdout), res.Stderr)
	}
}

func TestDockerRunRecordsDuration(t *testing.T) {
	f := newFakeEngine()
	f.run = func(context.Context, string, string) ([]byte, []byte, int, error) {
		time.Sleep(20 * time.Millisecond)
		return nil, nil, 0, nil
	}
	res, err := newTestDocker(f).Run(context.Background(), Spec{Command: "x", Image: "alpine"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Duration < 20*time.Millisecond {
		t.Fatalf("duration = %v", res.Duration)
	}
}

func TestDockerAvailable(t *testing.T) {
	f := newFakeEngine()
	d := newTestDocker(f)
	if err := d.Available(context.Background()); err != nil {
		t.Fatalf("Available: %v", err)
	}
	if c := f.callsTo("version"); len(c) != 1 {
		t.Fatalf("version calls = %v", c)
	}
}

func TestDockerAvailableDaemonDown(t *testing.T) {
	f := newFakeEngine()
	f.version = func() ([]byte, []byte, int, error) {
		return nil, []byte("Cannot connect to the Docker daemon at unix:///var/run/docker.sock.\n"), 1, nil
	}
	err := newTestDocker(f).Available(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Cannot connect") {
		t.Fatalf("err = %v, want daemon error", err)
	}
}

func TestDockerAvailableBinaryMissing(t *testing.T) {
	f := newFakeEngine()
	f.version = func() ([]byte, []byte, int, error) {
		return nil, nil, -1, errors.New("executable file not found")
	}
	err := newTestDocker(f).Available(context.Background())
	if err == nil || !strings.Contains(err.Error(), "docker") {
		t.Fatalf("err = %v, want an error naming the binary", err)
	}
}

var alpine = distro.Distro{Name: "alpine", Image: "alpine:latest", PackageManager: distro.APK}

func TestPrepareExistingImageWithoutPackages(t *testing.T) {
	f := newFakeEngine()
	f.images["alpine:latest"] = "sha256:aaa"
	env, err := newTestDocker(f).Prepare(context.Background(), alpine, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	want := Environment{Distro: alpine, Image: "alpine:latest", ImageID: "sha256:aaa", Shell: "busybox ash"}
	if env.Distro != want.Distro || env.Image != want.Image || env.ImageID != want.ImageID || env.Shell != want.Shell || len(env.Packages) != 0 {
		t.Fatalf("env = %+v, want %+v", env, want)
	}
	if len(f.callsTo("pull")) != 0 || len(f.callsTo("build")) != 0 {
		t.Fatal("nothing should be pulled or built")
	}
}

func TestPreparePullsMissingImage(t *testing.T) {
	f := newFakeEngine()
	f.remote["alpine:latest"] = "sha256:aaa"
	env, err := newTestDocker(f).Prepare(context.Background(), alpine, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	pulls := f.callsTo("pull")
	if len(pulls) != 1 || pulls[0][len(pulls[0])-1] != "alpine:latest" {
		t.Fatalf("pull calls = %v", pulls)
	}
	if env.ImageID != "sha256:aaa" {
		t.Fatalf("ImageID = %q", env.ImageID)
	}
}

func TestPreparePullFailure(t *testing.T) {
	f := newFakeEngine()
	_, err := newTestDocker(f).Prepare(context.Background(), alpine, nil, false)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "alpine:latest") || !strings.Contains(err.Error(), "pull access denied") {
		t.Fatalf("err = %v, want image name and engine message", err)
	}
}

func TestPrepareBuildsImageWithPackages(t *testing.T) {
	f := newFakeEngine()
	f.images["alpine:latest"] = "sha256:aaa"
	env, err := newTestDocker(f).Prepare(context.Background(), alpine, []string{"curl", "bash"}, false)
	if err != nil {
		t.Fatal(err)
	}
	builds := f.callsTo("build")
	if len(builds) != 1 {
		t.Fatalf("build calls = %v", builds)
	}
	tag := argAfter(builds[0], "-t")
	if !strings.HasPrefix(tag, "gostest/alpine:") {
		t.Errorf("tag = %q, want gostest/alpine:<hash>", tag)
	}
	if argAfter(builds[0], "-f") != "-" {
		t.Errorf("build should read the Dockerfile from stdin: %v", builds[0])
	}
	if slices.Contains(builds[0], "--no-cache") {
		t.Errorf("unexpected --no-cache: %v", builds[0])
	}
	dockerfile := f.stdinOf("build")
	if !strings.Contains(dockerfile, "FROM alpine:latest\n") || !strings.Contains(dockerfile, "RUN apk add --no-cache 'curl' 'bash'\n") {
		t.Errorf("Dockerfile = %q", dockerfile)
	}
	if env.Image != tag {
		t.Errorf("env.Image = %q, want built tag %q", env.Image, tag)
	}
	if env.ImageID != f.images[tag] {
		t.Errorf("env.ImageID = %q, want %q", env.ImageID, f.images[tag])
	}
	if !slices.Equal(env.Packages, []string{"curl", "bash"}) {
		t.Errorf("env.Packages = %v", env.Packages)
	}
}

func TestPrepareProbesPreparedImage(t *testing.T) {
	f := newFakeEngine()
	f.images["alpine:latest"] = "sha256:aaa"
	var probed []string
	f.run = func(_ context.Context, image, command string) ([]byte, []byte, int, error) {
		if command == ProbeCommand {
			probed = append(probed, image)
		}
		return []byte("/bin/busybox\n"), nil, 0, nil
	}
	env, err := newTestDocker(f).Prepare(context.Background(), alpine, []string{"bash"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(probed, []string{env.Image}) {
		t.Fatalf("probed %v, want the prepared image %q", probed, env.Image)
	}
}

func TestPrepareReusesBuiltImage(t *testing.T) {
	f := newFakeEngine()
	f.images["alpine:latest"] = "sha256:aaa"
	d := newTestDocker(f)
	first, err := d.Prepare(context.Background(), alpine, []string{"curl"}, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.Prepare(context.Background(), alpine, []string{"curl"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(f.callsTo("build")) != 1 {
		t.Fatalf("image should be built once, got %d builds", len(f.callsTo("build")))
	}
	if first.Image != second.Image || first.ImageID != second.ImageID {
		t.Fatalf("environments differ: %+v vs %+v", first, second)
	}
}

func TestPrepareRebuild(t *testing.T) {
	f := newFakeEngine()
	f.images["alpine:latest"] = "sha256:aaa"
	d := newTestDocker(f)
	if _, err := d.Prepare(context.Background(), alpine, []string{"curl"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Prepare(context.Background(), alpine, []string{"curl"}, true); err != nil {
		t.Fatal(err)
	}
	builds := f.callsTo("build")
	if len(builds) != 2 {
		t.Fatalf("rebuild should build again, got %d builds", len(builds))
	}
	if !slices.Contains(builds[1], "--no-cache") {
		t.Fatalf("rebuild should pass --no-cache: %v", builds[1])
	}
}

func TestPrepareTagDependsOnBaseAndPackages(t *testing.T) {
	tagFor := func(baseID string, pkgs []string) string {
		f := newFakeEngine()
		f.images["alpine:latest"] = baseID
		env, err := newTestDocker(f).Prepare(context.Background(), alpine, pkgs, false)
		if err != nil {
			t.Fatal(err)
		}
		return env.Image
	}
	a := tagFor("sha256:aaa", []string{"curl"})
	if a != tagFor("sha256:aaa", []string{"curl"}) {
		t.Error("tag must be deterministic")
	}
	if a == tagFor("sha256:aaa", []string{"git"}) {
		t.Error("tag must depend on the packages")
	}
	if a == tagFor("sha256:bbb", []string{"curl"}) {
		t.Error("tag must depend on the base image ID")
	}
}

func TestPrepareTagSanitizesTaggedNames(t *testing.T) {
	f := newFakeEngine()
	ubuntu := distro.Distro{Name: "ubuntu:22.04", Image: "ubuntu:22.04", PackageManager: distro.APT}
	f.images["ubuntu:22.04"] = "sha256:uuu"
	env, err := newTestDocker(f).Prepare(context.Background(), ubuntu, []string{"curl"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(env.Image, "gostest/ubuntu-22.04:") {
		t.Fatalf("image = %q, want gostest/ubuntu-22.04:<hash>", env.Image)
	}
}

func TestPrepareBuildFailure(t *testing.T) {
	f := newFakeEngine()
	f.images["alpine:latest"] = "sha256:aaa"
	f.buildErr = "ERROR: unable to select packages:\n  nosuchpkg (no such package)\n"
	_, err := newTestDocker(f).Prepare(context.Background(), alpine, []string{"nosuchpkg"}, false)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "nosuchpkg (no such package)") {
		t.Fatalf("err = %v, want the package manager output", err)
	}
}

func TestPrepareInvalidPackage(t *testing.T) {
	f := newFakeEngine()
	f.images["alpine:latest"] = "sha256:aaa"
	if _, err := newTestDocker(f).Prepare(context.Background(), alpine, []string{"curl;id"}, false); err == nil {
		t.Fatal("expected an error")
	}
	if len(f.callsTo("build")) != 0 {
		t.Fatal("nothing should be built for an invalid package")
	}
}

func TestPrepareProbeFailureIsNotFatal(t *testing.T) {
	f := newFakeEngine()
	f.images["alpine:latest"] = "sha256:aaa"
	f.run = func(context.Context, string, string) ([]byte, []byte, int, error) {
		return nil, []byte("sh: readlink: not found\n"), 127, nil
	}
	env, err := newTestDocker(f).Prepare(context.Background(), alpine, nil, false)
	if err != nil {
		t.Fatalf("probe failure must not fail Prepare: %v", err)
	}
	if env.Shell != "unknown" {
		t.Fatalf("Shell = %q, want unknown", env.Shell)
	}
}

func TestPrepareDetectsShell(t *testing.T) {
	f := newFakeEngine()
	debian := distro.Distro{Name: "debian", Image: "debian:latest", PackageManager: distro.APT}
	f.images["debian:latest"] = "sha256:ddd"
	f.probe["debian:latest"] = "/usr/bin/dash\n"
	env, err := newTestDocker(f).Prepare(context.Background(), debian, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if env.Shell != "dash" {
		t.Fatalf("Shell = %q, want dash", env.Shell)
	}
}

func TestDockerRunNeverMountsOverSystemDirectories(t *testing.T) {
	unsafe := []string{"/", "/tmp", "/home", "/usr", "/usr/local/src", "/etc", "/etc/nginx", "/bin", "/sbin",
		"/lib", "/lib64", "/var", "/var/lib/x", "/proc", "/sys", "/dev", "/run", "/boot", "/root", "/opt"}
	for _, dir := range unsafe {
		t.Run(dir, func(t *testing.T) {
			f := newFakeEngine()
			if _, err := newTestDocker(f).Run(context.Background(), Spec{Command: "true", Image: "alpine", Dir: dir}); err != nil {
				t.Fatal(err)
			}
			args := f.callsTo("run")[0]
			if got := argAfter(args, "-v"); got != dir+":"+FallbackMountPoint+":ro" {
				t.Errorf("mount = %q, want %s mounted at %s", got, dir, FallbackMountPoint)
			}
			if got := argAfter(args, "-w"); got != FallbackMountPoint {
				t.Errorf("workdir = %q, want %s", got, FallbackMountPoint)
			}
		})
	}
}

func TestDockerRunMountsProjectDirectoriesInPlace(t *testing.T) {
	for _, dir := range []string{"/home/u/proj", "/tmp/build", "/srv/app", "/opt/tool/src", "/root/proj", "/mnt/data/x", "/usrlocal"} {
		t.Run(dir, func(t *testing.T) {
			f := newFakeEngine()
			if _, err := newTestDocker(f).Run(context.Background(), Spec{Command: "true", Image: "alpine", Dir: dir}); err != nil {
				t.Fatal(err)
			}
			if got := argAfter(f.callsTo("run")[0], "-v"); got != dir+":"+dir+":ro" {
				t.Errorf("mount = %q, want in place", got)
			}
		})
	}
}

func TestMountPoint(t *testing.T) {
	if MountPoint("/home/u/proj") != "/home/u/proj" || MountPoint("/etc") != FallbackMountPoint || MountPoint("/usr/x/") != FallbackMountPoint {
		t.Fatal("MountPoint does not match Run's behaviour")
	}
}

func alpineEnv(f *fakeEngine) Environment {
	f.images["alpine:latest"] = "sha256:aaa"
	return Environment{Distro: alpine, Image: "alpine:latest", ImageID: "sha256:aaa", Shell: "busybox ash", Packages: []string{"bash"}}
}

// availableRun reports the given commands as installed by the
// availability check and behaves like the default fake otherwise.
func availableRun(f *fakeEngine, cmds ...string) func(context.Context, string, string) ([]byte, []byte, int, error) {
	return func(_ context.Context, image, command string) ([]byte, []byte, int, error) {
		if command == ProbeCommand {
			return []byte("/bin/busybox\n"), nil, 0, nil
		}
		var out strings.Builder
		for _, c := range cmds {
			if strings.Contains(command, "'"+c+"'") {
				out.WriteString(c + "\n")
			}
		}
		return []byte(out.String()), nil, 0, nil
	}
}

func TestProvideBuildsOnTopOfEnvironment(t *testing.T) {
	f := newFakeEngine()
	env := alpineEnv(f)
	f.run = availableRun(f, "jq", "curl")
	got, err := newTestDocker(f).Provide(context.Background(), env, []string{"jq", "curl"}, false)
	if err != nil {
		t.Fatal(err)
	}
	builds := f.callsTo("build")
	if len(builds) != 1 {
		t.Fatalf("build calls = %v", builds)
	}
	tag := argAfter(builds[0], "-t")
	if !strings.HasPrefix(tag, "gostest/alpine:") {
		t.Errorf("tag = %q, want gostest/alpine:<hash>", tag)
	}
	// The script installs the sorted command set, so the order the
	// commands were found in does not change the image.
	script, _ := distro.ProvideScript(distro.APK, []string{"curl", "jq"})
	dockerfile := f.stdinOf("build")
	if !strings.Contains(dockerfile, "FROM alpine:latest\n") || !strings.Contains(dockerfile, "RUN "+script+"\n") {
		t.Errorf("Dockerfile = %q", dockerfile)
	}
	if got.Image != tag || got.ImageID != f.images[tag] {
		t.Errorf("env = %+v, want image %s (%s)", got, tag, f.images[tag])
	}
	if !slices.Equal(got.Provided, []string{"jq", "curl"}) {
		t.Errorf("Provided = %v", got.Provided)
	}
	if got.Shell != env.Shell || !slices.Equal(got.Packages, env.Packages) || got.Distro != env.Distro {
		t.Errorf("env = %+v, want the rest of %+v kept", got, env)
	}
	if env.Provided != nil {
		t.Errorf("input environment was modified: %+v", env)
	}
}

func TestProvideReportsOnlyAvailableCommands(t *testing.T) {
	f := newFakeEngine()
	env := alpineEnv(f)
	var checked []string
	inner := availableRun(f, "jq")
	f.run = func(ctx context.Context, image, command string) ([]byte, []byte, int, error) {
		if command != ProbeCommand {
			checked = append(checked, image)
		}
		return inner(ctx, image, command)
	}
	got, err := newTestDocker(f).Provide(context.Background(), env, []string{"jq", "nopkg"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Provided, []string{"jq"}) {
		t.Errorf("Provided = %v, want only jq", got.Provided)
	}
	if !slices.Equal(checked, []string{got.Image}) {
		t.Errorf("availability checked in %v, want the derived image %s", checked, got.Image)
	}
}

func TestProvideIgnoresUnrequestedOutput(t *testing.T) {
	f := newFakeEngine()
	env := alpineEnv(f)
	f.run = func(_ context.Context, _, command string) ([]byte, []byte, int, error) {
		return []byte("jq\nmotd banner\n"), nil, 0, nil
	}
	got, err := newTestDocker(f).Provide(context.Background(), env, []string{"jq"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Provided, []string{"jq"}) {
		t.Errorf("Provided = %v", got.Provided)
	}
}

func TestProvideAccumulatesProvided(t *testing.T) {
	f := newFakeEngine()
	env := alpineEnv(f)
	env.Provided = []string{"git"}
	f.run = availableRun(f, "jq")
	got, err := newTestDocker(f).Provide(context.Background(), env, []string{"jq"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Provided, []string{"git", "jq"}) {
		t.Errorf("Provided = %v", got.Provided)
	}
}

func TestProvideNothing(t *testing.T) {
	f := newFakeEngine()
	env := alpineEnv(f)
	got, err := newTestDocker(f).Provide(context.Background(), env, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if got.Image != env.Image || len(f.callsTo("build")) != 0 {
		t.Errorf("env = %+v, builds = %v; want nothing done", got, f.callsTo("build"))
	}
}

func TestProvideReusesAndRebuilds(t *testing.T) {
	f := newFakeEngine()
	env := alpineEnv(f)
	f.run = availableRun(f, "jq")
	d := newTestDocker(f)
	first, err := d.Provide(context.Background(), env, []string{"jq"}, false)
	if err != nil {
		t.Fatal(err)
	}
	second, err := d.Provide(context.Background(), env, []string{"jq"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if f.builds != 1 || first.Image != second.Image {
		t.Errorf("builds = %d, images %s / %s; want the image reused", f.builds, first.Image, second.Image)
	}
	if _, err := d.Provide(context.Background(), env, []string{"jq"}, true); err != nil {
		t.Fatal(err)
	}
	builds := f.callsTo("build")
	if len(builds) != 2 || !slices.Contains(builds[1], "--no-cache") {
		t.Errorf("build calls = %v, want a --no-cache rebuild", builds)
	}
}

func TestProvideTagDependsOnBaseImage(t *testing.T) {
	f := newFakeEngine()
	env := alpineEnv(f)
	f.run = availableRun(f, "jq")
	d := newTestDocker(f)
	a, err := d.Provide(context.Background(), env, []string{"jq"}, false)
	if err != nil {
		t.Fatal(err)
	}
	env.ImageID = "sha256:other"
	b, err := d.Provide(context.Background(), env, []string{"jq"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if a.Image == b.Image {
		t.Errorf("same tag %s for different base images", a.Image)
	}
}

func TestProvideTagIgnoresCommandOrder(t *testing.T) {
	f := newFakeEngine()
	env := alpineEnv(f)
	f.run = availableRun(f, "jq", "curl")
	d := newTestDocker(f)
	a, err := d.Provide(context.Background(), env, []string{"jq", "curl"}, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := d.Provide(context.Background(), env, []string{"curl", "jq", "curl"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if f.builds != 1 || a.Image != b.Image {
		t.Errorf("builds = %d, images %s / %s; want one image for the same commands", f.builds, a.Image, b.Image)
	}
	if !slices.Equal(b.Provided, []string{"curl", "jq"}) {
		t.Errorf("Provided = %v, want each command once, in the order asked", b.Provided)
	}
}

func TestProvideAddsOnlyNewCommandsToClosestImage(t *testing.T) {
	f := newFakeEngine()
	env := alpineEnv(f)
	f.run = availableRun(f, "jq", "curl", "git", "wget")
	d := newTestDocker(f)
	small, err := d.Provide(context.Background(), env, []string{"jq"}, false)
	if err != nil {
		t.Fatal(err)
	}
	closest, err := d.Provide(context.Background(), env, []string{"jq", "curl"}, false)
	if err != nil {
		t.Fatal(err)
	}
	// Not a subset of what is asked next: never a starting point.
	if _, err := d.Provide(context.Background(), env, []string{"jq", "wget"}, false); err != nil {
		t.Fatal(err)
	}
	got, err := d.Provide(context.Background(), env, []string{"git", "curl", "jq"}, false)
	if err != nil {
		t.Fatal(err)
	}

	builds := f.callsTo("build")
	if len(builds) != 4 {
		t.Fatalf("build calls = %v", builds)
	}
	dockerfiles := f.dockerfiles()
	curl, _ := distro.ProvideScript(distro.APK, []string{"curl"})
	if !strings.Contains(dockerfiles[1], "FROM "+small.Image+"\n") || !strings.Contains(dockerfiles[1], "RUN "+curl+"\n") {
		t.Errorf("second Dockerfile = %q, want only curl installed on %s", dockerfiles[1], small.Image)
	}
	git, _ := distro.ProvideScript(distro.APK, []string{"git"})
	if !strings.Contains(dockerfiles[3], "FROM "+closest.Image+"\n") || !strings.Contains(dockerfiles[3], "RUN "+git+"\n") {
		t.Errorf("last Dockerfile = %q, want only git installed on %s", dockerfiles[3], closest.Image)
	}
	if l := f.labels[got.Image]; l["io.gostest.base"] != env.ImageID || l["io.gostest.commands"] != "curl git jq" {
		t.Errorf("labels = %v, want base %s and every command", l, env.ImageID)
	}
	if !slices.Equal(got.Provided, []string{"git", "curl", "jq"}) {
		t.Errorf("Provided = %v", got.Provided)
	}

	// The image a derived image starts from is cleaned up after it.
	if err := d.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	removed := f.removed()
	if i, j := slices.Index(removed, got.Image), slices.Index(removed, closest.Image); i < 0 || j < 0 || i > j {
		t.Errorf("removed %v, want %s before %s", removed, got.Image, closest.Image)
	}
}

func TestProvideReusesOnlyImagesOfSameBase(t *testing.T) {
	f := newFakeEngine()
	env := alpineEnv(f)
	f.run = availableRun(f, "jq", "curl")
	d := newTestDocker(f)
	if _, err := d.Provide(context.Background(), env, []string{"jq"}, false); err != nil {
		t.Fatal(err)
	}
	other := env
	other.Image, other.ImageID = "alpine:edge", "sha256:edge"
	f.images[other.Image] = other.ImageID
	if _, err := d.Provide(context.Background(), other, []string{"jq", "curl"}, false); err != nil {
		t.Fatal(err)
	}
	if df := f.dockerfiles()[1]; !strings.Contains(df, "FROM alpine:edge\n") {
		t.Errorf("Dockerfile = %q, want a build on alpine:edge", df)
	}
}

func TestProvideRebuildStartsFromEnvironment(t *testing.T) {
	f := newFakeEngine()
	env := alpineEnv(f)
	f.run = availableRun(f, "jq", "curl")
	d := newTestDocker(f)
	if _, err := d.Provide(context.Background(), env, []string{"jq"}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Provide(context.Background(), env, []string{"jq", "curl"}, true); err != nil {
		t.Fatal(err)
	}
	builds := f.callsTo("build")
	script, _ := distro.ProvideScript(distro.APK, []string{"curl", "jq"})
	df := f.dockerfiles()[len(builds)-1]
	if len(builds) != 2 || !slices.Contains(builds[1], "--no-cache") ||
		!strings.Contains(df, "FROM alpine:latest\n") || !strings.Contains(df, "RUN "+script+"\n") {
		t.Errorf("builds = %v, Dockerfile = %q; want a full --no-cache build on the environment", builds, df)
	}
}

func TestProvideBuildFailure(t *testing.T) {
	f := newFakeEngine()
	env := alpineEnv(f)
	f.buildErr = "ERROR: unable to select packages"
	_, err := newTestDocker(f).Provide(context.Background(), env, []string{"jq"}, false)
	if err == nil || !strings.Contains(err.Error(), "unable to select packages") || !strings.Contains(err.Error(), "jq") {
		t.Fatalf("err = %v, want the build output and the command", err)
	}
}

func TestProvideInvalidCommand(t *testing.T) {
	f := newFakeEngine()
	env := alpineEnv(f)
	if _, err := newTestDocker(f).Provide(context.Background(), env, []string{"jq;id"}, false); err == nil {
		t.Fatal("expected an error")
	}
	if len(f.callsTo("build")) != 0 {
		t.Error("nothing should be built for an invalid command")
	}
}

func TestProvideAvailabilityCheckFailure(t *testing.T) {
	f := newFakeEngine()
	env := alpineEnv(f)
	f.run = func(context.Context, string, string) ([]byte, []byte, int, error) {
		return nil, nil, -1, errors.New("exec: docker: killed")
	}
	if _, err := newTestDocker(f).Provide(context.Background(), env, []string{"jq"}, false); err == nil {
		t.Fatal("expected an error")
	}
}

func TestDerivedTagIsValidImageReference(t *testing.T) {
	dist := distro.Distro{Name: "ubuntu:Jammy", Image: "ubuntu:Jammy", PackageManager: distro.APT}
	tag := derivedTag(dist, "sha256:abc", "apt-get install -y curl")
	repo, _, _ := strings.Cut(tag, ":")
	if repo != strings.ToLower(repo) {
		t.Errorf("derivedTag(%q) = %q: repository names must be lowercase", dist.Name, tag)
	}
}

// removed returns the images removed with "image rm", in order.
func (f *fakeEngine) removed() []string {
	var out []string
	for _, c := range f.callsTo("image") {
		if c[1] == "rm" {
			out = append(out, c[len(c)-1])
		}
	}
	return out
}

func TestCleanupRemovesPulledAndBuiltImages(t *testing.T) {
	f := newFakeEngine()
	f.remote["alpine:latest"] = "sha256:aaa"
	f.run = availableRun(f, "jq")
	d := newTestDocker(f)
	ctx := context.Background()
	env, err := d.Prepare(ctx, alpine, []string{"bash"}, false)
	if err != nil {
		t.Fatal(err)
	}
	provided, err := d.Provide(ctx, env, []string{"jq"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Cleanup(ctx); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	// Images built on top of others go first, or removing the base
	// would fail because of its children.
	want := []string{provided.Image, env.Image, "alpine:latest"}
	if got := f.removed(); !slices.Equal(got, want) {
		t.Fatalf("removed %v, want %v", got, want)
	}
	if len(f.images) != 0 {
		t.Fatalf("images left: %v", f.images)
	}
}

func TestCleanupKeepsImagesThatExistedBefore(t *testing.T) {
	f := newFakeEngine()
	f.images["alpine:latest"] = "sha256:aaa"
	d := newTestDocker(f)
	env, err := d.Prepare(context.Background(), alpine, []string{"bash"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if got := f.removed(); !slices.Equal(got, []string{env.Image}) {
		t.Fatalf("removed %v, want only the derived image %s", got, env.Image)
	}
	if _, ok := f.images["alpine:latest"]; !ok {
		t.Fatal("a base image that was already there must be kept")
	}
}

func TestCleanupRemovesReusedDerivedImages(t *testing.T) {
	f := newFakeEngine()
	f.images["alpine:latest"] = "sha256:aaa"
	first := newTestDocker(f)
	env, err := first.Prepare(context.Background(), alpine, []string{"bash"}, false)
	if err != nil {
		t.Fatal(err)
	}
	// A later run reuses the image built by an earlier one.
	second := newTestDocker(f)
	if _, err := second.Prepare(context.Background(), alpine, []string{"bash"}, false); err != nil {
		t.Fatal(err)
	}
	if err := second.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if got := f.removed(); !slices.Equal(got, []string{env.Image}) {
		t.Fatalf("removed %v, want the reused gostest image %s", got, env.Image)
	}
}

func TestCleanupWithoutImages(t *testing.T) {
	f := newFakeEngine()
	f.images["alpine:latest"] = "sha256:aaa"
	d := newTestDocker(f)
	if _, err := d.Prepare(context.Background(), alpine, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := d.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	if got := f.removed(); len(got) != 0 {
		t.Fatalf("removed %v, want nothing", got)
	}
}

func TestCleanupRemovesEachImageOnce(t *testing.T) {
	f := newFakeEngine()
	f.remote["alpine:latest"] = "sha256:aaa"
	d := newTestDocker(f)
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			if _, err := d.Prepare(context.Background(), alpine, []string{"bash"}, false); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if err := d.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
	got := f.removed()
	if len(got) != 2 || got[1] != "alpine:latest" {
		t.Fatalf("removed %v, want the derived image, then alpine:latest", got)
	}
	if err := d.Cleanup(context.Background()); err != nil {
		t.Fatalf("second Cleanup: %v", err)
	}
	if again := f.removed(); len(again) != 2 {
		t.Fatalf("a second Cleanup removed again: %v", again[2:])
	}
}

func TestCleanupIgnoresImagesThatAreGone(t *testing.T) {
	f := newFakeEngine()
	f.remote["alpine:latest"] = "sha256:aaa"
	d := newTestDocker(f)
	if _, err := d.Prepare(context.Background(), alpine, nil, false); err != nil {
		t.Fatal(err)
	}
	delete(f.images, "alpine:latest")
	if err := d.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup of an image removed meanwhile: %v", err)
	}
}

func TestCleanupAfterFailedPull(t *testing.T) {
	f := newFakeEngine()
	d := newTestDocker(f)
	if _, err := d.Prepare(context.Background(), alpine, nil, false); err == nil {
		t.Fatal("expected a pull error")
	}
	if err := d.Cleanup(context.Background()); err != nil {
		t.Fatalf("Cleanup: %v", err)
	}
}

func TestCleanupReportsFailuresAndContinues(t *testing.T) {
	f := newFakeEngine()
	f.remote["alpine:latest"] = "sha256:aaa"
	d := newTestDocker(f)
	env, err := d.Prepare(context.Background(), alpine, []string{"bash"}, false)
	if err != nil {
		t.Fatal(err)
	}
	f.rmErr[env.Image] = "Error response from daemon: conflict: image is being used by running container 1234\n"
	err = d.Cleanup(context.Background())
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), env.Image) || !strings.Contains(err.Error(), "being used") {
		t.Fatalf("err = %v, want image name and engine message", err)
	}
	if got := f.removed(); !slices.Contains(got, "alpine:latest") {
		t.Fatalf("removed %v: the other images should still be removed", got)
	}
}

func TestDockerRunMountsHostTools(t *testing.T) {
	f := newFakeEngine()
	d := newTestDocker(f)
	spec := Spec{Command: "a; b", Image: "alpine", Dir: "/home/u/proj", Mounts: []Mount{
		{Source: "/home/u/.local/bin/a", Target: "/usr/local/bin/a", Hash: "1"},
		{Source: "/opt/b/b.sh", Target: "/usr/local/bin/b", Hash: "2"},
	}}
	if _, err := d.Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	args := f.callsTo("run")[0]
	var volumes []string
	for i, a := range args {
		if a == "-v" && i+1 < len(args) {
			volumes = append(volumes, args[i+1])
		}
	}
	want := []string{
		"/home/u/proj:/home/u/proj:ro",
		"/home/u/.local/bin/a:/usr/local/bin/a:ro",
		"/opt/b/b.sh:/usr/local/bin/b:ro",
	}
	if !slices.Equal(volumes, want) {
		t.Errorf("volumes = %v, want %v", volumes, want)
	}
	if i, j := slices.Index(args, "--entrypoint"), slices.Index(args, want[2]); j < 0 || j > i {
		t.Errorf("mounts must come before the image: %v", args)
	}
}

func TestDockerRunRejectsMountSourcesWithColon(t *testing.T) {
	f := newFakeEngine()
	d := newTestDocker(f)
	spec := Spec{Command: "a", Image: "alpine", Mounts: []Mount{{Source: "/home/u/a:b/a", Target: "/usr/local/bin/a"}}}
	if _, err := d.Run(context.Background(), spec); err == nil || !strings.Contains(err.Error(), ":") {
		t.Fatalf("err = %v, want an error about ':'", err)
	}
	if len(f.callsTo("run")) != 0 {
		t.Error("docker run must not be called")
	}
}

func TestProvideKeepsMounts(t *testing.T) {
	f := newFakeEngine()
	f.run = availableRun(f, "jq")
	d := newTestDocker(f)
	env := alpineEnv(f)
	env.Mounts = []Mount{{Source: "/h/tool", Target: "/usr/local/bin/tool", Hash: "x"}}
	got, err := d.Provide(context.Background(), env, []string{"jq"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Mounts, env.Mounts) {
		t.Errorf("Mounts = %v, want %v", got.Mounts, env.Mounts)
	}
}

func TestRunMarksOutputCappedByTheOSExecutor(t *testing.T) {
	// A stand-in engine that writes more than MaxOutput to stdout.
	engine := filepath.Join(t.TempDir(), "docker")
	script := fmt.Sprintf("#!/bin/sh\nhead -c %d /dev/zero\n", MaxOutput+10)
	if err := os.WriteFile(engine, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	res, err := NewDocker(engine).Run(context.Background(), Spec{Image: "alpine", Command: "true"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Stdout) != MaxOutput || !res.Truncated {
		t.Fatalf("len(Stdout) = %d, Truncated = %v, want %d and true", len(res.Stdout), res.Truncated, MaxOutput)
	}
}
