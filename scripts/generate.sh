#!/usr/bin/env bash
# Regenerates the committed generated code (`make generate`), for each
# generator whose inputs exist:
#   - api/: CRD manifests (into the chart's crds/ directory) and deepcopy
#     methods, with controller-gen;
#   - sqlc.yaml: query code, with sqlc;
#   - internal/config: the gateway configuration reference in docs/admin/;
#   - the chart's values.yaml: the chart values reference in docs/admin/.
# Generated code is committed and never edited by hand.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

ran=0
if [[ -d api ]]; then
	controller-gen object paths=./api/...
	controller-gen crd paths=./api/... output:crd:artifacts:config=deploy/charts/toolgate/crds
	ran=1
fi
if [[ -f sqlc.yaml ]]; then
	sqlc generate
	ran=1
fi
if [[ -f internal/config/config.go ]]; then
	go run ./internal/config/docgen internal/config/config.go docs/admin/configuration.md
	ran=1
fi
if [[ -f deploy/charts/toolgate/values.yaml ]]; then
	./scripts/chart-values-docs.sh
	ran=1
fi
[[ "$ran" == 1 ]] || echo "generate: no generator inputs yet (api/, sqlc.yaml); nothing to generate"
