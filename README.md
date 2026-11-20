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

## Status

The project is at an early stage.

## Similar tools

- **ShellSpec:** a BDD test framework for POSIX shells. It supports Docker and CI, but requires writing test files.
- **Test Kitchen:** tests infrastructure code on multiple platforms. Focused on the Chef ecosystem.
- **distrobox:** lets you enter different distributions, but doesn't compare or report.

What sets `gostest` apart: it runs any command instantly on multiple distributions without writing test files, and shows the difference by comparing the result with a reference.
