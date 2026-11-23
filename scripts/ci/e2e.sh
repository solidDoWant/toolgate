#!/usr/bin/env bash
# A CI e2e stage (ci.yaml e2e-1 to e2e-3), run inside scripts/e2e/run.sh: the
# e2e tests of one shard, acceptance tests included, so each test runs once.
# Their `go test -json` events go to build/acceptance/results/go-e2e-shard-i.json
# (scripts/e2e/refresh-durations.py reads them). The stage fails when any test
# fails.
#
# E2E_SHARD=i/n selects the shard (default 1/1).
set -uo pipefail
cd "$(git rev-parse --show-toplevel)" || exit 1

shard="${E2E_SHARD:-1/1}"
export E2E_SHARD="$shard"
export GO_TEST_JSON="build/acceptance/results/go-e2e-shard-${shard%%/*}.json"
exec ./scripts/test-e2e.sh
