#!/usr/bin/env bash
# Fetches the chart's library dependency (the bjw-s common chart) into
# deploy/charts/toolgate/charts/, at the version Chart.lock pins, unless it is
# there already or in the user cache. Unlike `helm dependency build`, it needs no `helm repo add`.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

chart="deploy/charts/toolgate"
version="$(yq '.dependencies[] | select(.name == "common") | .version' "$chart/Chart.lock")"
repository="$(yq '.dependencies[] | select(.name == "common") | .repository' "$chart/Chart.lock")"
[[ -n "$version" && -n "$repository" ]] || { echo "chart-deps: no common dependency in $chart/Chart.lock" >&2; exit 1; }

target="$chart/charts/common-$version.tgz"
[[ -f "$target" ]] && exit 0
mkdir -p "$chart/charts"
# Remove other versions only; the target may be placed by a concurrent run.
for old in "$chart"/charts/common-*.tgz; do
	[[ -e "$old" && "$old" != "$target" ]] && rm -f "$old"
done

# Every write goes to a private temporary file first and is moved into place
# with an atomic rename, so two runs at once never see a half-written file or
# fail on one another's file.
tmp="$(mktemp -d "$chart/charts/.chart-deps.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

# A pulled chart is kept in the user cache (CI keeps that directory between
# runs), so a warm run makes no network call.
cache="${XDG_CACHE_HOME:-$HOME/.cache}/toolgate/charts"
if [[ -f "$cache/common-$version.tgz" ]]; then
	cp "$cache/common-$version.tgz" "$tmp/common.tgz"
else
	mkdir -p "$tmp/pull"
	helm pull common --repo "$repository" --version "$version" --destination "$tmp/pull"
	mv -f "$tmp/pull/common-$version.tgz" "$tmp/common.tgz"
	if mkdir -p "$cache"; then
		# A cache write failure is not fatal: the next run pulls again.
		if cp "$tmp/common.tgz" "$cache/.common-$version.$$.tmp"; then
			mv -f "$cache/.common-$version.$$.tmp" "$cache/common-$version.tgz" || true
		fi
		rm -f "$cache/.common-$version.$$.tmp"
	fi
fi
mv -f "$tmp/common.tgz" "$target"
