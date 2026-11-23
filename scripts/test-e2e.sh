#!/usr/bin/env bash
# Runs the e2e-tagged Go tests against the cluster in KUBECONFIG. `make
# test-e2e` runs it inside scripts/e2e/run.sh, which creates the kind cluster
# with toolgate installed and deletes it afterwards.
#
# E2E_SKIP_ACCEPTANCE=true leaves out the acceptance tests, for a caller that
# runs them next with scripts/acceptance.sh, so they do not run twice.
# GO_TEST_JSON=file also writes the go test -json events there.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

args=()
if [[ "${E2E_SKIP_ACCEPTANCE:-false}" == true ]]; then
	acceptance="$(./scripts/acceptance-scoreboard.py --go-run-pattern --sources .)"
	[[ -z "$acceptance" ]] || args=(-skip "$acceptance")
fi

# Tests that call t.Parallel() run together once the serial ones are done, up
# to E2E_PARALLEL (default 24) at a time: go test's own default, the CPU count,
# leaves the longest tests waiting for a slot.
# A hung test must fail in minutes, with go test's goroutine dump, not hold the
# CI slot for the pipeline's hour. The limit is a small multiple of the suite's
# run time: lower it as the suite gets faster. E2E_TEST_TIMEOUT overrides it.
test_timeout="${E2E_TEST_TIMEOUT:-25m}"
timeout "$((${test_timeout%m} + 3))m" ./scripts/ci/go-test-json.sh -count=1 -tags e2e -timeout "$test_timeout" -parallel "${E2E_PARALLEL:-24}" "${args[@]}" ./test/e2e/... ./api/... ./internal/testtime/...
