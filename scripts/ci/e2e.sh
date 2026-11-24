#!/usr/bin/env bash
# The CI e2e stage (ci.yaml e2e), run inside scripts/e2e/run.sh: all e2e
# tests, acceptance tests included, so each test runs once. Their `go test
# -json` events go to build/acceptance/results/go-e2e.json. The stage fails
# when any test fails.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)" || exit 1

export GO_TEST_JSON="build/acceptance/results/go-e2e.json"
echo "e2e timing probe: failing on purpose so the check run shows the step times"; exit 1
