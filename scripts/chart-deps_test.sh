#!/usr/bin/env bash
# Tests scripts/chart-deps.sh against a fake `helm` in a throwaway git repo:
# a cold run, a warm-cache run and many runs at once must all end with the
# chart in place and no failure.
set -euo pipefail

script="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/chart-deps.sh"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

repo="$work/repo"
mkdir -p "$repo/deploy/charts/toolgate" "$work/bin"
git -C "$repo" init -q
cat >"$repo/deploy/charts/toolgate/Chart.lock" <<'LOCK'
dependencies:
  - name: common
    repository: https://example.invalid/charts
    version: 5.3.0
LOCK
cat >"$work/bin/helm" <<'FAKE'
#!/usr/bin/env bash
dest=""
while [[ $# -gt 0 ]]; do
	[[ $1 == --destination ]] && dest=$2
	shift
done
sleep 0.3
echo chart >"$dest/common-5.3.0.tgz"
FAKE
# A cp that pauses and then refuses to overwrite a file another run created in
# the meantime, which is what GNU cp does when it loses the race to create it.
cat >"$work/bin/cp" <<'FAKE'
#!/usr/bin/env bash
src=$1
dest=$2
[[ -d $dest ]] && dest="$dest/$(basename "$src")"
sleep 0.2
set -o noclobber
cat "$src" >"$dest"
FAKE
chmod +x "$work/bin/helm" "$work/bin/cp"
export PATH="$work/bin:$PATH"
export XDG_CACHE_HOME="$work/cache"
chart="$repo/deploy/charts/toolgate/charts/common-5.3.0.tgz"

fail() { echo "FAIL: $*" >&2; exit 1; }
run() { (cd "$repo" && "$script"); }

# Cold run, many at once: the cache and the chart dir are both empty.
pids=()
for _ in 1 2 3 4 5 6; do
	run &
	pids+=($!)
done
for pid in "${pids[@]}"; do
	wait "$pid" || fail "concurrent cold run failed"
done
[[ -s $chart ]] || fail "chart missing after concurrent cold runs"
[[ -s $XDG_CACHE_HOME/toolgate/charts/common-5.3.0.tgz ]] || fail "cache not filled"

# Warm cache, many at once: every run copies from the cache,
# 
for _ in $(seq 1 5); do
	rm -f "$chart"
	pids=()
	for _ in 1 2 3 4 5 6 7 8; do
		run &
		pids+=($!)
	done
	for pid in "${pids[@]}"; do
		wait "$pid" || fail "concurrent warm run failed"
	done
	[[ -s $chart ]] || fail "chart missing after concurrent warm runs"
done

# No temporary files are left behind.
leftover=$(find "$repo/deploy/charts/toolgate/charts" "$XDG_CACHE_HOME" -name '.*' -not -name charts -not -name . | head -1)
[[ -z $leftover ]] || fail "temporary file left behind: $leftover"
echo "chart-deps_test: ok"
