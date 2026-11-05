#!/usr/bin/env bash
# Lints the Helm chart without a cluster (`make chart-lint`): helm lint with the
# quick-start values, the Go tests that check the rendered manifests, and a
# check that the generated values reference in docs/admin/ is current.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

chart="deploy/charts/toolgate"
# The values docs/admin/quick-start.md installs with.
quick_start=(--set database.existingSecret=toolgate-database
	--set oidc.issuerURL=https://idp.example.com/realms/main --set oidc.audience=toolgate)

failed=""
helm lint --strict "$chart" "${quick_start[@]}" || failed="$failed helm-lint"
helm template toolgate "$chart" --namespace toolgate-system --include-crds "${quick_start[@]}" >/dev/null ||
	failed="$failed helm-template"
go test -count=1 ./test/chart/ || failed="$failed rendered-manifests"

generated="$(mktemp)"
trap 'rm -f "$generated"' EXIT
./scripts/chart-values-docs.sh "$generated"
if ! diff -u docs/admin/chart-values.md "$generated"; then
	echo "chart-lint: docs/admin/chart-values.md is stale; run make generate-docs" >&2
	failed="$failed values-reference"
fi

if [[ -n "$failed" ]]; then
	echo "chart-lint failed:$failed" >&2
	exit 1
fi
