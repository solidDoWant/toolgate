#!/usr/bin/env bash
# Prints the acceptance scoreboard for the `go test -json` results of every
# e2e shard, put together in one directory (the CI e2e-scoreboard job), and
# appends it to the job summary when GITHUB_STEP_SUMMARY is set.
#
#   scripts/ci/e2e-scoreboard.sh RESULTS_DIR
#
# The checklist is docs/acceptance/phase-$PHASE.md (default 1), as in
# scripts/ci/e2e.sh. It fails when RESULTS_DIR holds no results.
set -euo pipefail
(($# == 1)) || { echo "usage: $0 RESULTS_DIR" >&2; exit 2; }
results="$(realpath "$1")"
cd "$(git rev-parse --show-toplevel)"

phase="${PHASE:-1}"
checklist="docs/acceptance/phase-$phase.md"
shopt -s nullglob
files=("$results"/go-*.json)
((${#files[@]} > 0)) || { echo "e2e-scoreboard: no results in $results" >&2; exit 1; }

scoreboard="$(
	echo "Phase $phase ($checklist)"
	./scripts/acceptance-scoreboard.py --checklist "$checklist" --results "$results" --sources .
)"
echo "$scoreboard"
if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
	{
		echo '## Acceptance scoreboard'
		echo '```'
		echo "$scoreboard"
		echo '```'
	} >>"$GITHUB_STEP_SUMMARY"
fi
