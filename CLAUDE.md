# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

`gostest` runs a shell command on the host (the **reference**) and, in parallel, inside Docker/Podman containers of several Linux distributions, compares stdout/stderr/exit code of each with the reference, and explains *why* a distribution differs (dash vs bash, BusyBox vs GNU tools, missing commands …). Go stdlib only — no third-party dependencies.

## Commands

```bash
make build                 # ./gostest, version from git describe (-X main.version)
make lint                  # gofmt check + go vet
make test                  # go test -race ./...  (includes Docker integration tests)
make test-short            # -short: skips integration tests, no container engine needed
make e2e                   # scripts/e2e-host-tools.sh: real binary, temporary host scripts
go test -run TestParseList ./internal/distro          # single test
go test -run Integration -v ./internal/backend        # real-Docker tests only
```

Integration tests live in `internal/backend/docker_integration_test.go` and, end to end through the real `cli.App`, in `internal/cli/hosttool_integration_test.go`; they skip under `-short` or when Docker is unreachable, and pull `alpine` and `debian` images on first run. `make e2e` runs `scripts/e2e-host-tools.sh`, which builds the binary and checks host tool mounting with temporary scripts on `PATH` (`DISTROS=`, `GOSTEST=`, `KEEP=1` to tune it).

## Architecture

Data flows `cli → engine → backend`, then `compare → explain → report`:

- **`internal/cli`** – flag parsing, precedence (flags > `.gostest.json` > defaults), engine auto-detection (docker, then podman), cache/fingerprint setup, progress line, exit codes (0 pass, 1 differences/errors, 2 usage, 3 runtime, 130 interrupted). `App` takes all system dependencies (`Host`, `NewContainers`, `LookPath`, `Getwd`, `Getenv`, `CacheDir`, TTY flags) as fields so tests inject fakes; `New()` wires the real ones, and `main.go` only adds signal handling.
- **`internal/engine`** – orchestration. The host reference runs concurrently with the targets; each target goroutine (behind a semaphore of `Concurrency`) does Prepare → cache lookup → Run, then waits on the reference only to compare and explain. With `AutoInstall` (on unless `--no-auto-install`), commands that `explain.MissingCommands` finds missing in the target but not on the host are passed to `Containers.Provide`, and the command re-runs on the derived image, for up to `MaxInstallRounds`; an installation failure is a note, not an `ERROR`. Before `Provide`, each missing command is offered to `Engine.HostTool` (wired by the CLI to `cli.HostToolResolver`, and only when auto-install is on): host scripts and static ELF binaries (`backend.ClassifyHostTool`; dynamic ELF is refused) become `backend.Mount`s in `Environment.Mounts`, copied into `Spec.Mounts` on every run and included (target + content hash) in the cache key. With `Options.Dependencies` (set by the CLI from `deps.Scan`, keeping only commands the host has), a `command -v` probe (`probeMarker`, cached per image ID + names) finds those the target lacks, and they go through the same mount/`Provide` step before the first run; a failed probe falls back to the rounds. Prepare/run failures become `ERROR` outcomes; reference failure or ctx cancellation aborts the run. Results are cached by `(image ID, command, dir, dir fingerprint, timeout)`; timeouts and errors are never cached; `NoCache` skips reads but still writes. When `Dir` is set, caching requires a non-empty `Fingerprint`.
- **`internal/backend`** – `Runner` interface (`Spec` → `Result`; non-zero exits and timeouts are results, `error` only means it couldn't run). `Local` runs `/bin/sh -c` in its own process group (whole group killed on timeout, exit code 124, signals map to 128+n, output capped at 1 MiB per stream). `Docker` shells out to the docker/podman CLI through an `Executor` interface (faked in tests): `run --rm --pull=never --entrypoint /bin/sh`, working dir bind-mounted read-only **at the same host path** so `pwd`-style output matches (system paths go to `/work`, see `MountPoint`), `Spec.Mounts` bind-mounted read-only after it (host tools go to `HostToolTarget`, `/usr/local/bin/<name>`), and `rm -f` on timeout/cancel. `Prepare` pulls the base image, builds a derived image `gostest/<distro>:<hash(baseID+install script)>` when packages are requested, and probes what `/bin/sh` is. `Provide` builds another derived image on top of an environment that runs `distro.ProvideScript` (one tolerant install per command) and reports in `Environment.Provided` which commands are actually available. `Cleanup` (`--cleanup`, called by the CLI after the run, even when interrupted) removes, newest first, the base images this `Docker` pulled and every `gostest/` image it built or reused; base images that were already present are kept.
- **`internal/deps`** – best-effort static scan, no shell parser dependency: a lexer (quotes, heredocs, `$()`/backticks/`<()` collected per token and parsed after it, `${}`, arithmetic) and a command-position parser (reserved words, `case`/`for` headers, function/alias definitions, wrappers such as `env`/`sudo`/`xargs`/`find -exec`, `sh -c`, `command -v`/`type`/`which` arguments). `Scan` follows scripts run by path, given to a shell or sourced (relative to the working dir), adding shebang interpreters; non-shell scripts are not parsed.
- **`internal/distro`** – built-in registry (defaults: alpine, archlinux, debian, fedora, ubuntu), `name:tag` specs, custom distros, per-package-manager install scripts, package-name validation (blocks shell metacharacters, since names are interpolated into a Dockerfile `RUN`), `Packages` (common + per-distro `alpine=bash`).
- **`internal/compare`** – line diff (prefix/suffix trim + exact LCS, replace-all fallback above a size cap) that tracks missing trailing newlines; `Compare` also treats a timeout as different from a real exit 124. Streams with the same lines in a different order match by default (marked `StdoutReordered`/`StderrReordered`, surfaced as `explain.Notes` on the outcome); `Options.StrictOrder` (`--strict-order`) makes them differences.
- **`internal/explain`** – analyzes only stderr lines that are *new* compared with the reference, using ordered regex rules (most specific first) over real dash/bash/busybox/zsh/GNU message formats, plus exit-code meanings and the "silent failure" case (errors printed, exit 0). When adding a rule, keep the order in `diagnose` in mind: missing-command patterns must run before missing-file ones.
- **`internal/cache`** – atomic (temp file + rename) versioned JSON store, `GOSTEST_CACHE_DIR` / `$XDG_CACHE_HOME/gostest`; `Fingerprint` hashes names, modes, symlink targets and contents of the working tree (skips `.git`, bounded by a file limit).
- **`internal/report`** – aligned text report (colors, diffs capped at `MaxDiffLines`) and a JSON document with its own schema (durations in ms, diff ops by name). It is not a raw dump of `engine.Report`.

## Working conventions (from the project owner)

- Write everything (code, comments, docs, commit messages) in English.
- Develop each feature in three commits: (1) minimal code, (2) complete tests for it (these may fail against the minimal code), (3) the full implementation that makes them pass. Fixes follow the same order: test commit, then fix commit.
- each feature shoud be in a branch.
- Commit at every step with descriptive conventional-style messages (`feat(pkg):`, `test(pkg):`, `fix(pkg):`, `docs:`, `build:`).
