package backend

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/emre-tiryaki/gostest/internal/distro"
)

// realDocker returns a Docker backend talking to the local engine, or skips
// the test when running with -short or when no engine is reachable.
func realDocker(t *testing.T) *Docker {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test skipped in -short mode")
	}
	d := NewDocker("docker")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := d.Available(ctx); err != nil {
		t.Skipf("docker not available: %v", err)
	}
	return d
}

func prepare(t *testing.T, d *Docker, dist distro.Distro, pkgs []string) Environment {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	env, err := d.Prepare(ctx, dist, pkgs, false)
	if err != nil {
		t.Fatalf("Prepare(%s): %v", dist.Name, err)
	}
	return env
}

func TestIntegrationAlpine(t *testing.T) {
	d := realDocker(t)
	env := prepare(t, d, alpine, nil)
	if env.Shell != "busybox ash" {
		t.Errorf("alpine shell = %q, want busybox ash", env.Shell)
	}
	if !strings.HasPrefix(env.ImageID, "sha256:") {
		t.Errorf("ImageID = %q", env.ImageID)
	}

	res, err := d.Run(context.Background(), Spec{Image: env.Image, Command: "echo out; echo err >&2; exit 3"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "out\n" || res.Stderr != "err\n" || res.ExitCode != 3 {
		t.Fatalf("unexpected result %+v", res)
	}
}

func TestIntegrationMountsDirReadOnly(t *testing.T) {
	d := realDocker(t)
	env := prepare(t, d, alpine, nil)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hi from host\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := d.Run(context.Background(), Spec{Image: env.Image, Dir: dir, Command: "pwd; cat hello.txt; touch new.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != dir+"\nhi from host\n" {
		t.Errorf("stdout = %q", res.Stdout)
	}
	if res.ExitCode == 0 || !strings.Contains(res.Stderr, "Read-only") {
		t.Errorf("writing into the mount should fail: %+v", res)
	}
	if _, err := os.Stat(filepath.Join(dir, "new.txt")); err == nil {
		t.Error("container modified the host directory")
	}
}

func TestIntegrationTimeoutRemovesContainer(t *testing.T) {
	d := realDocker(t)
	env := prepare(t, d, alpine, nil)
	start := time.Now()
	res, err := d.Run(context.Background(), Spec{Image: env.Image, Command: "echo started; sleep 60", Timeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || res.ExitCode != ExitTimeout {
		t.Fatalf("unexpected result %+v", res)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatalf("timeout took %v", time.Since(start))
	}
	psOut, _, _, err := OSExecutor{}.Exec(context.Background(), nil, "docker", "ps", "-a", "--no-trunc", "--filter", "name=gostest-", "--format", "{{.Names}} {{.Command}}")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(psOut), "sleep 60") {
		t.Fatalf("timed out container still exists:\n%s", psOut)
	}
}

func TestIntegrationDashRejectsBashisms(t *testing.T) {
	d := realDocker(t)
	debian := distro.Distro{Name: "debian", Image: "debian:latest", PackageManager: distro.APT}
	env := prepare(t, d, debian, nil)
	if env.Shell != "dash" {
		t.Fatalf("debian shell = %q, want dash", env.Shell)
	}
	script := `name="ani-cli"; if [[ $name == ani* ]]; then echo "matched"; fi`
	res, err := d.Run(context.Background(), Spec{Image: env.Image, Command: script})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || res.Stdout != "" || !strings.Contains(res.Stderr, "[[: not found") {
		t.Fatalf("dash should print '[[: not found' and still exit 0, got %+v", res)
	}
}

func TestIntegrationInstallsPackages(t *testing.T) {
	d := realDocker(t)
	env := prepare(t, d, alpine, []string{"bash"})
	if !strings.HasPrefix(env.Image, "gostest/alpine:") {
		t.Fatalf("image = %q", env.Image)
	}
	res, err := d.Run(context.Background(), Spec{Image: env.Image, Command: `bash -c 'echo "${BASH_VERSION%%.*}"'`})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 || strings.TrimSpace(res.Stdout) == "" {
		t.Fatalf("bash should be installed: %+v", res)
	}
}

func TestIntegrationProvideMissingCommands(t *testing.T) {
	d := realDocker(t)
	env := prepare(t, d, alpine, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	got, err := d.Provide(ctx, env, []string{"jq", "gostest-no-such-command"}, false)
	if err != nil {
		t.Fatalf("Provide: %v", err)
	}
	if len(got.Provided) != 1 || got.Provided[0] != "jq" {
		t.Fatalf("Provided = %v, want [jq]", got.Provided)
	}
	res, err := d.Run(ctx, Spec{Image: got.Image, Command: "echo '{\"a\":1}' | jq -c .a"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Stdout != "1\n" || res.ExitCode != 0 {
		t.Fatalf("jq did not run in the derived image: %+v", res)
	}
}
