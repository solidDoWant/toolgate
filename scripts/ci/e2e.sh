#!/usr/bin/env bash
# The CI e2e job, run inside scripts/e2e/run.sh so both steps share one
# cluster: the e2e tests, then the acceptance scoreboard (also written to the
# job summary). The job fails when an e2e test fails; the scoreboard reports
# the scenarios and does not fail the job by itself.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)" || exit 1

./scripts/test-e2e.sh
status=$?

PHASE="${PHASE:-1}" ./scripts/acceptance.sh
if [[ -n "${GITHUB_STEP_SUMMARY:-}" && -f build/acceptance/scoreboard.txt ]]; then
	{
		echo '## Acceptance scoreboard'
		echo '```'
		cat build/acceptance/scoreboard.txt
		echo '```'
	} >>"$GITHUB_STEP_SUMMARY"
fi

exit "$status"
