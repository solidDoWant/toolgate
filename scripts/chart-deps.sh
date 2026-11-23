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

[[ -f "$chart/charts/common-$version.tgz" ]] && exit 0
rm -f "$chart"/charts/common-*.tgz
mkdir -p "$chart/charts"

# A pulled chart is kept in the user cache (CI keeps that directory between
# runs), so a warm run makes no network call.
cache="${XDG_CACHE_HOME:-$HOME/.cache}/toolgate/charts"
if [[ -f "$cache/common-$version.tgz" ]]; then
	cp "$cache/common-$version.tgz" "$chart/charts/"
	exit 0
fi
helm pull common --repo "$repository" --version "$version" --destination "$chart/charts"
mkdir -p "$cache"
cp "$chart/charts/common-$version.tgz" "$cache/" || true
