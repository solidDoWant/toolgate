#!/usr/bin/env bash
# Writes the chart values reference (docs/admin/chart-values.md, or the file
# given as $1) from the comments in deploy/charts/toolgate/values.yaml, with
# helm-docs and the template deploy/charts/toolgate/README.md.gotmpl.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

output="$(realpath -m "${1:-docs/admin/chart-values.md}")"
chart="deploy/charts/toolgate"
helm-docs --log-level warning --chart-search-root "$chart" --template-files README.md.gotmpl \
	--output-file "$(realpath -m --relative-to="$chart" "$output")"
