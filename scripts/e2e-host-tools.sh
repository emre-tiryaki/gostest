#!/bin/sh
# End-to-end check of host tool mounting with the real gostest binary.
#
# Creates temporary scripts in a temporary directory, puts it first on PATH
# and runs gostest on them in real containers. Needs Docker or Podman.
#
#   scripts/e2e-host-tools.sh                   # alpine,debian
#   DISTROS=alpine,fedora scripts/e2e-host-tools.sh
#   GOSTEST=./gostest scripts/e2e-host-tools.sh  # use an existing binary
#   KEEP=1 scripts/e2e-host-tools.sh             # keep the temporary files
set -u

DISTROS=${DISTROS:-alpine,debian}
root=$(cd "$(dirname "$0")/.." && pwd)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/gostest-e2e.XXXXXX")
if [ "${KEEP:-}" = 1 ]; then
	echo "temporary files: $tmp"
else
	trap 'rm -rf "$tmp"' EXIT
fi
mkdir -p "$tmp/bin" "$tmp/work" "$tmp/cache"

if [ -z "${GOSTEST:-}" ]; then
	GOSTEST=$tmp/gostest
	(cd "$root" && go build -o "$GOSTEST" .) || exit 1
fi
PATH=$tmp/bin:$PATH
export PATH
GOSTEST_CACHE_DIR=$tmp/cache
export GOSTEST_CACHE_DIR
cd "$tmp/work" || exit 1

passed=0
failed=0
out=$tmp/out

# check NAME WANT_EXIT GOSTEST_ARGS... runs gostest and compares its exit code.
check() {
	name=$1 want=$2
	shift 2
	"$GOSTEST" --color never -q -d "$DISTROS" "$@" >"$out" 2>&1
	got=$?
	if [ "$got" -ne "$want" ]; then
		fail "$name: exit code $got, want $want"
		return 1
	fi
	return 0
}

# expect TEXT: the last output contains TEXT.
expect() {
	grep -qF -- "$1" "$out" || fail "$name: output lacks: $1"
}

# reject TEXT: the last output does not contain TEXT.
reject() {
	! grep -qF -- "$1" "$out" || fail "$name: output should not contain: $1"
}

fail() {
	echo "FAIL  $1"
	sed 's/^/      | /' "$out"
	failed=$((failed + 1))
	case_failed=1
}

begin() { case_failed=0; }
end() {
	if [ "$case_failed" = 0 ]; then
		echo "ok    $name"
		passed=$((passed + 1))
	fi
}

ndistros=$(echo "$DISTROS" | tr ',' '\n' | grep -c .)

tool() { # tool NAME CONTENT
	printf '%s' "$2" >"$tmp/bin/$1"
	chmod +x "$tmp/bin/$1"
}

tool gostest-probe '#!/bin/sh
echo "hello from gostest-probe on $(uname -s)"
'

begin
name="a host script is mounted and passes"
if check "$name" 0 gostest-probe; then
	expect 'mounted from the host because they were missing here: `gostest-probe`'
	[ "$(grep -c 'mounted from the host' "$out")" -eq "$ndistros" ] || fail "$name: not mounted in every distro"
	reject "installed automatically"
fi
end

begin
name="an unchanged script is served from the cache"
if check "$name" 0 gostest-probe; then
	[ "$(grep -c '(cached)' "$out")" -eq "$ndistros" ] || fail "$name: not every result was cached"
fi
end

begin
name="editing the script invalidates the cache"
sed 's/hello/edited/' "$tmp/bin/gostest-probe" >"$tmp/edited" && cat "$tmp/edited" >"$tmp/bin/gostest-probe"
if check "$name" 0 gostest-probe; then
	reject "(cached)"
fi
end

begin
name="--no-auto-install does not mount"
if check "$name" 1 --no-auto-install gostest-probe; then
	reject "mounted from the host"
	expect '`gostest-probe` is not installed here'
fi
end

begin
name="the JSON report lists the mount"
if check "$name" 0 --format json gostest-probe; then
	expect '"target": "/usr/local/bin/gostest-probe"'
fi
end

begin
name="a dynamically linked host binary is not mounted"
# A copy under an unknown name, so no distro has it and no package provides it.
sh_bin=$(readlink -f /bin/sh)
# A dynamic binary names its loader (PT_INTERP), e.g. /lib64/ld-linux-x86-64.so.2.
if head -c 4 "$sh_bin" | grep -q ELF && grep -qa -e /ld-linux -e /ld-musl "$sh_bin"; then
	cp "$sh_bin" "$tmp/bin/gostest-dynamic"
	if check "$name" 1 'gostest-dynamic -c "echo hi"'; then
		reject "mounted from the host"
	fi
else
	echo "skip  $name (/bin/sh is not a dynamic ELF binary)"
fi
end

begin
tool gostest-bash-probe '#!/bin/bash
[[ -n $BASH_VERSION ]] && echo "bash works"
'
name="a mounted bash script gets bash installed"
if check "$name" 0 gostest-bash-probe; then
	expect 'mounted from the host because they were missing here: `gostest-bash-probe`'
fi
end

echo
echo "$passed passed, $failed failed"
[ "$failed" -eq 0 ]
