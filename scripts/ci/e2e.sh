#!/usr/bin/env bash
# The CI e2e job, run inside scripts/e2e/run.sh so both steps share one
# cluster: the e2e tests other than the acceptance tests, then the acceptance
# tests, so each test runs once. The job fails when any of these tests fails.
#
# With E2E_SHARD=i/n (the CI matrix), both steps run shard i of n only, and
# the e2e-scoreboard job scores the results of every shard together
# (scripts/ci/e2e-scoreboard.sh). Without it, the scoreboard of this run goes
# to the job summary.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)" || exit 1

E2E_SKIP_ACCEPTANCE=true ./scripts/test-e2e.sh
status=$?

PHASE="${PHASE:-1}" ./scripts/acceptance.sh
# acceptance.sh reports failing tests on the scoreboard only; here they fail
# the job, as they did when the e2e step ran them too.
if grep -qs '"Action":"fail"' build/acceptance/results/go-*.json; then
	echo "e2e: some acceptance tests failed; their output:" >&2
	python3 - build/acceptance/results/go-*.json <<'PY' >&2
import json
import sys

events = [json.loads(line) for path in sys.argv[1:] for line in open(path, encoding="utf-8") if line.startswith("{")]
failed = {(event.get("Package"), event.get("Test")) for event in events if event.get("Action") == "fail"}
for event in events:
    if event.get("Action") == "output" and (event.get("Package"), event.get("Test")) in failed:
        sys.stdout.write(event.get("Output", ""))
PY
	cat build/acceptance/go.log >&2
	status=1
fi
if [[ -z "${E2E_SHARD:-}" && -n "${GITHUB_STEP_SUMMARY:-}" && -f build/acceptance/scoreboard.txt ]]; then
	{
		echo '## Acceptance scoreboard'
		echo '```'
		cat build/acceptance/scoreboard.txt
		echo '```'
	} >>"$GITHUB_STEP_SUMMARY"
fi

exit "$status"
