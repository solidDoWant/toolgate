#!/usr/bin/env bash
# The CI e2e job, run inside scripts/e2e/run.sh so both steps share one
# cluster: the e2e tests, then the acceptance tests. The job fails when an e2e
# test fails; the acceptance results do not fail the job by themselves.
#
# With E2E_SHARD=i/n (the CI matrix), both steps run shard i of n only, and
# the e2e-scoreboard job scores the results of every shard together
# (scripts/ci/e2e-scoreboard.sh). Without it, the scoreboard of this run goes
# to the job summary.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)" || exit 1

./scripts/test-e2e.sh
status=$?

PHASE="${PHASE:-1}" ./scripts/acceptance.sh
if [[ -z "${E2E_SHARD:-}" && -n "${GITHUB_STEP_SUMMARY:-}" && -f build/acceptance/scoreboard.txt ]]; then
	{
		echo '## Acceptance scoreboard'
		echo '```'
		cat build/acceptance/scoreboard.txt
		echo '```'
	} >>"$GITHUB_STEP_SUMMARY"
fi

exit "$status"
