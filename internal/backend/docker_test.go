package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
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

	images   map[string]string // local ref -> image ID
	remote   map[string]string // pullable ref -> image ID
	probe    map[string]string // image -> probe stdout
	buildErr string            // non-empty: builds fail with this stderr
	builds   int

	version func() ([]byte, []byte, int, error)
	run     func(ctx context.Context, image, command string) ([]byte, []byte, int, error)
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{
		images: map[string]string{},
		remote: map[string]string{},
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
		f.mu.Lock()
		id, ok := f.images[ref]
		f.mu.Unlock()
		if !ok {
			return nil, []byte("Error: No such image: " + ref + "\n"), 1, nil
		}
		return []byte(id + "\n"), nil, 0, nil
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
