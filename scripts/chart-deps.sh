#!/usr/bin/env bash
# Fetches the chart's library dependency (the bjw-s common chart) into
# deploy/charts/toolgate/charts/, at the version Chart.lock pins, unless it is
# there already. Unlike `helm dependency build`, it needs no `helm repo add`.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

chart="deploy/charts/toolgate"
version="$(yq '.dependencies[] | select(.name == "common") | .version' "$chart/Chart.lock")"
repository="$(yq '.dependencies[] | select(.name == "common") | .repository' "$chart/Chart.lock")"
[[ -n "$version" && -n "$repository" ]] || { echo "chart-deps: no common dependency in $chart/Chart.lock" >&2; exit 1; }

[[ -f "$chart/charts/common-$version.tgz" ]] && exit 0
rm -f "$chart"/charts/common-*.tgz
mkdir -p "$chart/charts"
helm pull common --repo "$repository" --version "$version" --destination "$chart/charts"
