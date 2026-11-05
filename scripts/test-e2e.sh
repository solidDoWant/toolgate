#!/usr/bin/env bash
# Runs the e2e-tagged Go tests against the cluster in KUBECONFIG. `make
# test-e2e` runs it inside scripts/e2e/run.sh, which creates the kind cluster
# with toolgate installed and deletes it afterwards.
#
# E2E_SHARD=i/n runs only shard i of n of the top-level tests
# (scripts/e2e/shard.py); unset, it runs them all.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

args=()
if [[ -n "${E2E_SHARD:-}" ]]; then
	listing="$(go test -tags e2e -list '.*' ./...)"
	pattern="$(./scripts/e2e/shard.py --shard "$E2E_SHARD" <<<"$listing")"
	if [[ -z "$pattern" ]]; then
		echo "test-e2e: shard $E2E_SHARD has no tests" >&2
		exit 0
	fi
	echo "test-e2e: shard $E2E_SHARD: go test -run '$pattern'" >&2
	args=(-run "$pattern")
fi

timeout 60m go test -count=1 -tags e2e -timeout 55m "${args[@]}" ./...
