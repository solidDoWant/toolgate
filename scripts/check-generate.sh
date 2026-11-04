#!/usr/bin/env bash
# Fails when `make generate` changes the working tree, meaning committed
# generated code is stale (`make check-generate`, run by CI). It compares the
# tree's state before and after, so it also works in a dirty checkout.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

snapshot() {
	{ git status --porcelain=v1 --untracked-files=all; git diff; } | sha256sum
}

before=$(snapshot)
./scripts/generate.sh
after=$(snapshot)
if [[ "$before" != "$after" ]]; then
	git status --short >&2
	echo "check-generate: generated code is stale: run 'make generate' and commit the result" >&2
	exit 1
fi
echo "check-generate: generated code is current"
