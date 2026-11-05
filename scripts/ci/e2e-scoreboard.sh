#!/usr/bin/env bash
# Prints the acceptance scoreboard for the `go test -json` results of every
# e2e shard, put together in one directory (the CI e2e-scoreboard job), and
# appends it to the job summary when GITHUB_STEP_SUMMARY is set.
#
#   scripts/ci/e2e-scoreboard.sh RESULTS_DIR
#
# It prints one scoreboard per phase in ACCEPTANCE_PHASES (space-separated,
# default 1; CI sets it once for the whole workflow), each from
# docs/acceptance/phase-N.md. It fails when RESULTS_DIR holds no results.
set -euo pipefail
(($# == 1)) || { echo "usage: $0 RESULTS_DIR" >&2; exit 2; }
results="$(realpath "$1")"
cd "$(git rev-parse --show-toplevel)"

shopt -s nullglob
files=("$results"/go-*.json)
((${#files[@]} > 0)) || { echo "e2e-scoreboard: no results in $results" >&2; exit 1; }

scoreboard=""
for phase in ${ACCEPTANCE_PHASES:-1}; do
	checklist="docs/acceptance/phase-$phase.md"
	[[ -f "$checklist" ]] || { echo "e2e-scoreboard: no checklist $checklist" >&2; exit 2; }
	board="$(
		echo "Phase $phase ($checklist)"
		./scripts/acceptance-scoreboard.py --checklist "$checklist" --results "$results" --sources .
	)"
	scoreboard+="${scoreboard:+$'\n\n'}$board"
done
echo "$scoreboard"
if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
	{
		echo '## Acceptance scoreboard'
		echo '```'
		echo "$scoreboard"
		echo '```'
	} >>"$GITHUB_STEP_SUMMARY"
fi
