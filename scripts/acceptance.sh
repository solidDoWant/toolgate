#!/usr/bin/env bash
# Runs every acceptance test (a Go test tagged with a checklist scenario:
# a `[AC-N-nn] ...` subtest or a `TestACN_nn...` function) with the
# integration and e2e build tags, and prints the scoreboard for the phase's
# checklist, docs/acceptance/phase-$PHASE.md (default 1): one line per
# scenario (pass, fail, not run, no test, manual) and the number passing.
#
# The scoreboard is also written to build/acceptance/scoreboard.txt, with the
# test output in build/acceptance/go.log. A failing test does not stop the
# scoreboard: it shows as fail.
set -uo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.." || exit 1

phase="${PHASE:-1}"
checklist="docs/acceptance/phase-$phase.md"
[[ -f "$checklist" ]] || { echo "acceptance: no checklist $checklist" >&2; exit 2; }

out=build/acceptance
results="$out/results"
rm -rf "$out"
mkdir -p "$results"

pattern=$(./scripts/acceptance-scoreboard.py --go-run-pattern --sources .) || exit 1
if [[ -n "$pattern" ]]; then
	echo "acceptance: go test -run '$pattern' (log: $out/go.log)" >&2
	timeout 90m go test -tags 'integration e2e' -count=1 -timeout 80m -json -run "$pattern" ./... \
		>"$results/go-acceptance.json" 2>"$out/go.log" ||
		echo "acceptance: some acceptance tests failed; see $out/go.log and $results/go-acceptance.json" >&2
else
	echo "acceptance: no acceptance tests yet" >&2
fi

{
	echo "Phase $phase ($checklist)"
	./scripts/acceptance-scoreboard.py --checklist "$checklist" --results "$results" --sources .
} | tee "$out/scoreboard.txt"
