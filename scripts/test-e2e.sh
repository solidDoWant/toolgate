#!/usr/bin/env bash
# Runs the e2e-tagged Go tests (`make test-e2e`). They create their own kind
# cluster (named after the worktree, so agents don't collide), install Flux,
# the gateway image built from this checkout, the fake identity provider and
# the test MCP server, and delete the cluster when done.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

mapfile -t tests < <(git ls-files --cached --others --exclude-standard -- '*_test.go')
if ((${#tests[@]} == 0)) || ! grep -lq '^//go:build.*\be2e\b' "${tests[@]}"; then
	echo "test-e2e: no e2e tests yet; nothing to run"
	exit 0
fi
timeout 60m go test -count=1 -tags e2e -timeout 55m ./...
