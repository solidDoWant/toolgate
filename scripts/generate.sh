#!/usr/bin/env bash
# Regenerates the committed generated code (`make generate`), for each
# generator whose inputs exist:
#   - api/: CRD manifests (into config/crd/bases, which the chart reads
#     through its rawCRDs link) and deepcopy methods, with controller-gen;
#   - sqlc.yaml: query code, with sqlc;
#   - internal/config: the gateway configuration reference in docs/admin/;
#   - internal/apidoc: the OpenAPI document of the /api/v1/ routes, docs/dev/openapi.yaml;
#   - the chart's values.yaml: the chart values reference in docs/admin/;
#   - internal/ui/styles: the web UI's stylesheet, with Tailwind CSS.
# Generated code is committed and never edited by hand.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

ran=0
if [[ -d api ]]; then
	controller-gen object paths=./api/...
	controller-gen crd paths=./api/... output:crd:artifacts:config=config/crd/bases
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
if [[ -d internal/apidoc/docgen ]]; then
	go run ./internal/apidoc/docgen docs/dev/openapi.yaml
	ran=1
fi
if [[ -f deploy/charts/toolgate/values.yaml ]]; then
	./scripts/chart-values-docs.sh
	ran=1
fi
if [[ -f internal/ui/styles/app.css ]]; then
	./scripts/build-ui-css.sh
	ran=1
fi
if [[ -f internal/ui/static/SHA256SUMS ]]; then
	# The vendored htmx is not regenerated; a file that differs from its
	# recorded hash fails here, so `make check-generate` fails too.
	(cd internal/ui/static && sha256sum --check --quiet SHA256SUMS)
	ran=1
fi
[[ "$ran" == 1 ]] || echo "generate: no generator inputs yet (api/, sqlc.yaml); nothing to generate"
