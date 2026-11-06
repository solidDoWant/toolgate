#!/usr/bin/env bash
# Runs every acceptance test (a Go test tagged with a checklist scenario:
# a `[AC-N-nn] ...` subtest or a `TestACN_nn...` function) with the
# integration and e2e build tags, and prints the scoreboard for the phase's
# checklist, docs/acceptance/phase-$PHASE.md (default 1): one line per
# scenario (pass, fail, not run, no test, manual) and the number passing out of the automated scenarios.
#
# The scoreboard is also written to build/acceptance/scoreboard.txt, with the
# test output in build/acceptance/go.log. A failing test does not stop the
# scoreboard: it shows as fail.
#
# E2E_SHARD=i/n runs only the acceptance tests in shard i of n
# (scripts/e2e/shard.py), with the results in
# build/acceptance/results/go-acceptance-shard-i.json, so the results of every
# shard can be put in one directory and scored together (CI does this). The
# scoreboard printed then covers that shard only.
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
result_file="$results/go-acceptance.json"
if [[ -n "${E2E_SHARD:-}" && -n "$pattern" ]]; then
	listing=$(go test -tags 'integration e2e' -list '.*' ./...) || exit 1
	pattern=$(./scripts/e2e/shard.py --shard "$E2E_SHARD" --match "$pattern" <<<"$listing") || exit 1
	result_file="$results/go-acceptance-shard-${E2E_SHARD%%/*}.json"
	echo "acceptance: shard $E2E_SHARD" >&2
fi
if [[ -n "$pattern" ]]; then
	echo "acceptance: go test -run '$pattern' (log: $out/go.log)" >&2
	timeout 90m go test -tags 'integration e2e' -count=1 -timeout 80m -json -run "$pattern" ./... \
		>"$result_file" 2>"$out/go.log" ||
		echo "acceptance: some acceptance tests failed; see $out/go.log and $result_file" >&2
else
	echo "acceptance: no acceptance tests to run" >&2
fi

{
	echo "Phase $phase ($checklist)"
	./scripts/acceptance-scoreboard.py --checklist "$checklist" --results "$results" --sources .
} | tee "$out/scoreboard.txt"
