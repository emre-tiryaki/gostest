# gostest

> Test how shell commands and scripts behave across Linux distributions with a single command — and see *why* they fail when they do.

## The problem

When building a portable shell tool, the most common sentence you hear is: *"It works on your machine, but it doesn't work on distro X."*

The cause is rarely the logic of the code. It is the environment differences between distributions:

- `/bin/sh` may be a different shell: `dash` on Debian/Ubuntu, `bash` on Arch, `busybox ash` on Alpine
- Core tools have different implementations: GNU `grep`/`sed` and BusyBox support different flags
- Package names, versions and availability differ
- File paths and directory layouts differ

Today, a developer tests this by setting up a virtual machine per distribution, or by waiting for someone to say "it doesn't work here". Both are slow, and neither shows **why** it doesn't work.

## The solution

`gostest` runs the given command on your own system and, **in parallel**, inside isolated environments of the selected Linux distributions. It then **compares** every result with your system and reports the differences.

```bash
gostest "ani-cli -S 1 -e 1 'Vinland Saga'"
```

```mermaid
flowchart TB
    U(["Developer"]) --> C["gostest 'command'"]
    C --> R["Reference: your system"]
    C --> UB["Ubuntu environment"] & F["Fedora environment"] & G["Gentoo environment"]
    R & UB & F & G --> K["Comparator"]
    K --> O["Report: works / doesn't work per distro + why"]
    O --> U
```

## Why "just running it" is not enough

Running a command in a few containers and looking at the exit code is misleading. The following script works correctly in bash:

```sh
name="ani-cli"
if [[ $name == ani* ]]; then echo "matched"; fi
```

| Environment | Output | Exit code |
|---|---|---|
| bash | `matched` | 0 |
| dash | `[[: not found` | **0** |

The script is broken under dash, yet the exit code is still 0. A tool that only checks exit codes calls both environments "successful". That is why `gostest` compares stdout, stderr **and** the exit code against the reference run.

## Workflow

1. The user provides a command: `gostest "command"`
2. The target distribution list is resolved (default list or configuration)
3. Required images are pulled and dependencies are installed with each distribution's own package manager (the result is cached)
4. The command is run on the user's own system. This run is the **reference**.
5. A container per distribution is started in parallel with goroutines (with a concurrency limit and a timeout)
6. The stdout, stderr and exit code of every environment are collected
7. The results are compared with the reference
8. PASS/FAIL and the differences are reported per distribution

## Use cases

- **Open source contributions:** check that a change doesn't break other distributions before sending a PR
- **Debugging:** reproduce an "it doesn't work on distro X" issue without installing that distribution
- **CI integration:** run the same tool in a CI environment such as GitHub Actions with the Docker backend

## Technical design

- **Language:** Go. It produces a single binary, and goroutines are a natural fit for parallel execution.
- **Isolation:** Docker/Podman containers. Containers share the host kernel, but their userland (shell, core tools, packages) really belongs to the given distribution — and that is the layer where most shell-script portability problems appear.
- **Backend abstraction:** the execution layer sits behind an interface. System container / VM backends such as Incus can be added later for scenarios that need systemd or a kernel.
- **Cache:** results are reused when nothing has changed. `--no-cache` re-runs everything.

## Scope and limits

- Only **Linux distributions** are targeted. macOS and Windows cannot be tested with containers.
- Actually opening GUI applications (e.g. video players), systemd/journald and kernel-dependent behaviour cannot be tested with the default Docker backend.
- The result of network-dependent commands may change with the current state of external services. This can cause result differences unrelated to environment differences.

## Installation

Requirements: Linux, Go 1.27+ to build, and Docker or Podman to run.

```bash
go install github.com/emre-tiryaki/gostest@latest
# or, from a checkout
make build        # produces ./gostest
```

## Usage

```bash
gostest [flags] [--] command [args...]
```

The command runs with `/bin/sh -c` on your machine (the reference) and in
every selected distribution. The working directory is mounted read-only into
the containers at the same path, so local scripts can be tested directly:

```bash
gostest ./install.sh                              # default distros
gostest -d alpine,ubuntu:22.04,fedora ./install.sh
gostest -p curl -p alpine=bash 'curl --version | head -1'
gostest --format json 'my-tool --version' > report.json
```

Example report:

```text
$ ./check.sh
reference: host, /bin/sh = bash, exit 0, 20ms

FAIL   alpine     /bin/sh = busybox ash  exit 2  268ms
       why:
         • `grep` here is the BusyBox implementation and does not support option `-P`
         • exit code 2, reference exited 0 (2 usually means a usage error or a misused shell builtin)
         • stdout differs from the reference
PASS   archlinux  /bin/sh = bash         exit 0  254ms
FAIL   debian     /bin/sh = dash         exit 0  296ms
       why:
         • `[[` is not available (here /bin/sh is dash, on the host it is bash): it is a bash/ksh feature, use [ ... ] or case instead
         • errors were printed but /bin/sh kept going and returned exit code 0 like the reference: checking the exit code alone would call this run a success
         • stdout differs from the reference
       stdout:
         - matched
           grep-ok
       stderr:
         + ./check.sh: 3: [[: not found
PASS   fedora     /bin/sh = bash         exit 0  400ms

2 passed, 2 failed, 0 errors
```

### Flags

| Flag | Description |
|---|---|
| `-d, --distros LIST` | Comma separated distributions. Default: `alpine,archlinux,debian,fedora,ubuntu`. Any distro accepts a tag: `ubuntu:22.04`. |
| `-p, --pkg SPEC` | Packages to install: `curl` (everywhere) or `alpine=bash` (one distro). Repeatable, comma separated. |
| `-j, --concurrency N` | Distributions processed at once. Default: number of CPUs. |
| `-t, --timeout DURATION` | Timeout of every run, `0` disables it. Default: `2m`. |
| `--ignore-stderr` | Compare only stdout and exit codes. |
| `--engine NAME` | `docker` or `podman`. Default: auto-detect. |
| `--workdir DIR` | Working directory mounted into the containers. Default: current directory. |
| `--config FILE` | Configuration file. Default: nearest `.gostest.json`. |
| `--no-cache` | Ignore cached results and rebuild images. |
| `--clear-cache` | Remove cached results. |
| `--format FORMAT` | `text` or `json`. |
| `--color WHEN` | `auto`, `always` or `never`. `NO_COLOR` is honoured. |
| `--context N` | Unchanged lines shown around each change. Default: `3`. |
| `-v, --verbose` | Show the full output of every run. |
| `-q, --quiet` | Do not show progress. |
| `--list` | List the known distributions. |

Exit codes: `0` everything matches, `1` at least one distribution differs or
could not run, `2` invalid usage or configuration, `3` the reference or the
container engine could not run, `130` interrupted.

### Distributions

| Name | Image | Package manager | Default |
|---|---|---|---|
| alpine | `alpine:latest` | apk | ✓ |
| archlinux (`arch`) | `archlinux:latest` | pacman | ✓ |
| debian | `debian:latest` | apt | ✓ |
| fedora | `fedora:latest` | dnf | ✓ |
| ubuntu | `ubuntu:latest` | apt | ✓ |
| almalinux | `almalinux:latest` | dnf | |
| gentoo | `gentoo/stage3:latest` | emerge | |
| opensuse (`suse`) | `opensuse/tumbleweed:latest` | zypper | |

### Configuration

A `.gostest.json` in the working directory or any parent directory sets
project defaults. Command line flags take precedence.

```json
{
  "distros": ["alpine", "ubuntu:22.04", "fedora", "void"],
  "custom_distros": [
    {"name": "void", "image": "ghcr.io/void-linux/void-musl:latest", "package_manager": "apk"}
  ],
  "packages": ["curl", "alpine=bash", "fedora=which"],
  "concurrency": 4,
  "timeout": "90s",
  "engine": "docker",
  "ignore_stderr": false
}
```

### What gets explained

When a distribution differs from the reference, gostest looks at the new
stderr lines and the exit code and reports the likely cause:

- bash-only syntax and builtins under a POSIX `/bin/sh` (`[[`, `source`,
  `function`, `==` in `[`, `${var:0:3}`, arrays …), with POSIX alternatives
- missing commands (with a `--pkg` hint) and missing shebang interpreters
- options the local implementation of a tool does not support (GNU vs BusyBox)
- missing files, permission errors and writes to the read-only mount
- timeouts, signals and exit code meanings (126, 127, 128+n)
- *silent failures*: errors were printed but the exit code is still 0

### Caching

Images with installed packages are built once and tagged by a hash of the
base image and the package list. Results are cached in
`$XDG_CACHE_HOME/gostest` (override with `GOSTEST_CACHE_DIR`) and keyed by the
image ID, the command, the timeout and a fingerprint of the working directory,
so editing a script invalidates them. The reference always runs fresh.
`--no-cache` re-runs everything.

Working directories that would shadow system paths (`/`, `/usr/...`,
`/etc/...`, `/home` …) are mounted at `/work` instead of their host path.

## Development

```bash
make test          # all tests, including Docker integration tests
make test-short    # unit tests only, no container engine needed
make lint          # gofmt + go vet
```

## Status

The project is at an early stage.

- [x] Run the command in several images in parallel with goroutines
- [x] Collect stdout, stderr and exit code
- [x] Reference run and comparison
- [x] Per-distro dependency installation and image cache
- [x] Result cache and `--no-cache`
- [x] Concurrency limit and timeout configuration
- [x] Detailed reporting that shows the cause of a difference

## Similar tools

- **ShellSpec:** a BDD test framework for POSIX shells. It supports Docker and CI, but requires writing test files.
- **Test Kitchen:** tests infrastructure code on multiple platforms. Focused on the Chef ecosystem.
- **distrobox:** lets you enter different distributions, but doesn't compare or report.

What sets `gostest` apart: it runs any command instantly on multiple distributions without writing test files, and shows the difference by comparing the result with a reference.
