#!/usr/bin/env bash
# Runs the e2e-tagged Go tests against the cluster in KUBECONFIG. `make
# test-e2e` runs it inside scripts/e2e/run.sh, which creates the kind cluster
# with toolgate installed and deletes it afterwards.
#
# E2E_SHARD=i/n runs only shard i of n of the top-level tests
# (scripts/e2e/shard.py); unset, it runs them all. E2E_SKIP_ACCEPTANCE=true
# leaves out the acceptance tests, for a caller that runs them next with
# scripts/acceptance.sh, so they do not run twice. GO_TEST_JSON=file also
# writes the go test -json events there.
#
# A shard (about a third of the tests) gets a 20-minute go test timeout, so a
# hung test fails with a goroutine dump well before CI's job limit; a full
# run keeps 55 minutes.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

args=()
go_timeout=55m
if [[ -n "${E2E_SHARD:-}" || "${E2E_SKIP_ACCEPTANCE:-false}" == true ]]; then
	spec="${E2E_SHARD:-1/1}"
	[[ -z "${E2E_SHARD:-}" ]] || go_timeout=20m
	select=(--shard "$spec")
	if [[ "${E2E_SKIP_ACCEPTANCE:-false}" == true ]]; then
		acceptance="$(./scripts/acceptance-scoreboard.py --go-run-pattern --sources .)"
		[[ -z "$acceptance" ]] || select+=(--exclude "$acceptance")
	fi
	listing="$(go test -tags e2e -list '.*' ./...)"
	pattern="$(./scripts/e2e/shard.py "${select[@]}" <<<"$listing")"
	if [[ -z "$pattern" ]]; then
		echo "test-e2e: shard $spec has no tests to run" >&2
		exit 0
	fi
	echo "test-e2e: shard $spec: go test -run '$pattern'" >&2
	args=(-run "$pattern")
fi

timeout 60m ./scripts/ci/go-test-json.sh -count=1 -tags e2e -timeout "$go_timeout" "${args[@]}" ./...
