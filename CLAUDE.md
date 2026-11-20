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
go test -run TestParseList ./internal/distro          # single test
go test -run Integration -v ./internal/backend        # real-Docker tests only
```

Integration tests live in `internal/backend/docker_integration_test.go`; they skip under `-short` or when `docker version` fails, and pull `alpine` and `debian` images on first run.

## Architecture

Data flows `cli → engine → backend`, then `compare → explain → report`:

- **`internal/cli`** – flag parsing, precedence (flags > `.gostest.json` > defaults), engine auto-detection (docker, then podman), cache/fingerprint setup, progress line, exit codes (0 pass, 1 differences/errors, 2 usage, 3 runtime, 130 interrupted). `App` takes all system dependencies (`Host`, `NewContainers`, `LookPath`, `Getwd`, `Getenv`, `CacheDir`, TTY flags) as fields so tests inject fakes; `New()` wires the real ones, and `main.go` only adds signal handling.
- **`internal/engine`** – orchestration. The host reference runs concurrently with the targets; each target goroutine (behind a semaphore of `Concurrency`) does Prepare → cache lookup → Run, then waits on the reference only to compare and explain. Prepare/run failures become `ERROR` outcomes; reference failure or ctx cancellation aborts the run. Results are cached by `(image ID, command, dir, dir fingerprint, timeout)`; timeouts and errors are never cached; `NoCache` skips reads but still writes. When `Dir` is set, caching requires a non-empty `Fingerprint`.
- **`internal/backend`** – `Runner` interface (`Spec` → `Result`; non-zero exits and timeouts are results, `error` only means it couldn't run). `Local` runs `/bin/sh -c` in its own process group (whole group killed on timeout, exit code 124, signals map to 128+n, output capped at 1 MiB per stream). `Docker` shells out to the docker/podman CLI through an `Executor` interface (faked in tests): `run --rm --pull=never --entrypoint /bin/sh`, working dir bind-mounted read-only **at the same host path** so `pwd`-style output matches (system paths go to `/work`, see `MountPoint`), and `rm -f` on timeout/cancel. `Prepare` pulls the base image, builds a derived image `gostest/<distro>:<hash(baseID+install script)>` when packages are requested, and probes what `/bin/sh` is.
- **`internal/distro`** – built-in registry (defaults: alpine, archlinux, debian, fedora, ubuntu), `name:tag` specs, custom distros, per-package-manager install scripts, package-name validation (blocks shell metacharacters, since names are interpolated into a Dockerfile `RUN`), `Packages` (common + per-distro `alpine=bash`).
- **`internal/compare`** – line diff (prefix/suffix trim + exact LCS, replace-all fallback above a size cap) that tracks missing trailing newlines; `Compare` also treats a timeout as different from a real exit 124. Streams with the same lines in a different order match by default (marked `StdoutReordered`/`StderrReordered`, surfaced as `explain.Notes` on the outcome); `Options.StrictOrder` (`--strict-order`) makes them differences.
- **`internal/explain`** – analyzes only stderr lines that are *new* compared with the reference, using ordered regex rules (most specific first) over real dash/bash/busybox/zsh/GNU message formats, plus exit-code meanings and the "silent failure" case (errors printed, exit 0). When adding a rule, keep the order in `diagnose` in mind: missing-command patterns must run before missing-file ones.
- **`internal/cache`** – atomic (temp file + rename) versioned JSON store, `GOSTEST_CACHE_DIR` / `$XDG_CACHE_HOME/gostest`; `Fingerprint` hashes names, modes, symlink targets and contents of the working tree (skips `.git`, bounded by a file limit).
- **`internal/report`** – aligned text report (colors, diffs capped at `MaxDiffLines`) and a JSON document with its own schema (durations in ms, diff ops by name). It is not a raw dump of `engine.Report`.

## Working conventions (from the project owner)

- Write everything (code, comments, docs, commit messages) in English.
- Develop each feature in three commits: (1) minimal code, (2) complete tests for it (these may fail against the minimal code), (3) the full implementation that makes them pass. Fixes follow the same order: test commit, then fix commit.
- Commit at every step with descriptive conventional-style messages (`feat(pkg):`, `test(pkg):`, `fix(pkg):`, `docs:`, `build:`).
