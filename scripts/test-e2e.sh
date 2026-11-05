#!/usr/bin/env bash
# Runs the e2e-tagged Go tests against the cluster in KUBECONFIG. `make
# test-e2e` runs it inside scripts/e2e/run.sh, which creates the kind cluster
# with toolgate installed and deletes it afterwards.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

timeout 60m go test -count=1 -tags e2e -timeout 55m ./...
