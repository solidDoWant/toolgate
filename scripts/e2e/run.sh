#!/usr/bin/env bash
# Runs a command against a fresh kind cluster with toolgate installed, then
# deletes the cluster. `make test-e2e` and `make acceptance` use it.
#
#   scripts/e2e/run.sh COMMAND [ARGS...]
#
# Steps:
#  1. builds the gateway image from this checkout (scripts/build-images.sh)
#     and the test MCP server image (test/mcpserver);
#  2. creates a kind cluster with a unique name (toolgate-<worktree>-<random>,
#     or E2E_CLUSTER) and loads both images into it;
#  3. installs Flux (`flux install`), then PostgreSQL and the S3 server
#     (test/e2e/manifests/dependencies.yaml) in the namespace toolgate-e2e;
#  4. installs the chart from deploy/charts/toolgate as release toolgate in
#     toolgate-system, with the fresh image, and waits until it is ready;
#  5. runs COMMAND with these variables set:
#       KUBECONFIG                    the cluster's kubeconfig
#       TOOLGATE_E2E_NAMESPACE        the release namespace
#       TOOLGATE_E2E_MCPSERVER_IMAGE  the test MCP server image
#       TOOLGATE_E2E_OIDC_LISTEN      address the test process serves the fake
#                                     identity provider on (test/e2e TestMain)
#       TOOLGATE_E2E_OIDC_ISSUER      its issuer URL, reachable from pods
#       TOOLGATE_E2E_OTLP_LISTEN      address the test process serves the test
#                                     OTLP/HTTP receiver on; the chart's
#                                     config.otlpEndpoint points at it
#       TOOLGATE_E2E_S3_ENDPOINT      the S3 server inside the cluster
#  6. deletes the cluster, also when a step fails. E2E_KEEP_CLUSTER=true
#     keeps it for debugging.
#
# The exit status is COMMAND's, or the failing step's.
set -euo pipefail

(($# > 0)) || { echo "usage: $0 COMMAND [ARGS...]" >&2; exit 2; }

root="$(git rev-parse --show-toplevel)"
cd "$root"

for tool in docker kind kubectl helm flux go yq; do
	command -v "$tool" >/dev/null || { echo "e2e: $tool is not on PATH (use the dev shell)" >&2; exit 1; }
done

suffix="$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')"
worktree="$(basename "$root" | tr -c 'a-z0-9\n' '-' | tr -d '\n')"
cluster="${E2E_CLUSTER:-toolgate-${worktree:0:24}-$suffix}"
release_namespace=toolgate-system
deps_namespace=toolgate-e2e
image_tag="e2e-$suffix"
gateway_repository=ghcr.io/soliddowant/toolgate
mcpserver_image="localhost/toolgate-e2e-mcpserver:$image_tag"
work="$(mktemp -d)"
export KUBECONFIG="$work/kubeconfig"

log() { echo "e2e: $*" >&2; }

cleanup() {
	local status=$?
	if ((status != 0)); then
		log "a step failed; cluster state:"
		kubectl get pods --all-namespaces -o wide >&2 || true
		kubectl -n "$release_namespace" logs --all-containers --prefix --tail=100 -l app.kubernetes.io/name=toolgate >&2 || true
	fi
	if [[ "${E2E_KEEP_CLUSTER:-false}" == true ]]; then
		log "keeping cluster $cluster (KUBECONFIG=$KUBECONFIG); delete it with: kind delete cluster --name $cluster"
	else
		kind delete cluster --name "$cluster" >&2 || true
		rm -rf "$work"
	fi
	exit "$status"
}
trap cleanup EXIT

# The constant line `const <name> = "<image>"` in internal/testutil.
pinned_image() {
	sed -n "s/^const $1 = \"\\(.*\\)\"$/\\1/p" "$2"
}
postgres_image="$(pinned_image PostgresImage internal/testutil/postgres.go)"
s3_image="$(pinned_image S3Image internal/testutil/s3.go)"
[[ -n "$postgres_image" && -n "$s3_image" ]] || { log "cannot read the pinned images in internal/testutil"; exit 1; }

log "building the gateway image $gateway_repository:$image_tag"
VERSION="$image_tag" IMAGE="$gateway_repository" PUSH_ALL=false OUT_DIR="$work/images" ./scripts/build-images.sh >&2

log "building the test MCP server image $mcpserver_image"
mkdir -p "$work/mcpserver"
CGO_ENABLED=0 go build -trimpath -o "$work/mcpserver/mcpserver" ./test/mcpserver
printf 'FROM scratch\nCOPY mcpserver /mcpserver\nUSER 65532:65532\nEXPOSE 8080\nENTRYPOINT ["/mcpserver"]\n' >"$work/mcpserver/Dockerfile"
docker build --quiet --tag "$mcpserver_image" "$work/mcpserver" >&2

log "creating kind cluster $cluster"
kind create cluster --name "$cluster" --kubeconfig "$KUBECONFIG" --wait 3m >&2
kind load docker-image --name "$cluster" "$gateway_repository:$image_tag" "$mcpserver_image" >&2

log "installing Flux"
flux install --timeout 5m >&2

log "installing PostgreSQL and S3"
postgres_password="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
s3_access_key="e2e$suffix"
s3_secret_key="$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')"
kubectl create namespace "$deps_namespace" >&2
kubectl -n "$deps_namespace" create secret generic dependencies \
	--from-literal=postgres-password="$postgres_password" \
	--from-literal=s3-access-key="$s3_access_key" \
	--from-literal=s3-secret-key="$s3_secret_key" >&2
POSTGRES_IMAGE="$postgres_image" S3_IMAGE="$s3_image" yq '
	(select(.kind == "Deployment" and .metadata.name == "postgres") | .spec.template.spec.containers[0].image) = strenv(POSTGRES_IMAGE) |
	(select(.kind == "Deployment" and .metadata.name == "s3") | .spec.template.spec.containers[0].image) = strenv(S3_IMAGE)
' test/e2e/manifests/dependencies.yaml | kubectl -n "$deps_namespace" apply -f - >&2
kubectl -n "$deps_namespace" rollout status deployment/postgres deployment/s3 --timeout 5m >&2

# The fake identity provider runs in the test process, on the host's address
# in the kind Docker network, so both the tests and the gateway pods reach it
# at the same issuer URL.
host_address="$(docker network inspect kind --format '{{range .IPAM.Config}}{{.Gateway}} {{end}}' | tr ' ' '\n' | grep -m1 '^[0-9.]*$')"
oidc_port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])')"
export TOOLGATE_E2E_OIDC_LISTEN="$host_address:$oidc_port"
export TOOLGATE_E2E_OIDC_ISSUER="http://$host_address:$oidc_port"

# The test OTLP receiver runs there too, so the gateway's spans reach the
# tests.
otlp_port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])')"
export TOOLGATE_E2E_OTLP_LISTEN="$host_address:$otlp_port"

log "installing the chart (issuer $TOOLGATE_E2E_OIDC_ISSUER)"
kubectl create namespace "$release_namespace" >&2
kubectl -n "$release_namespace" create secret generic toolgate-database \
	--from-literal=url="postgres://toolgate:$postgres_password@postgres.$deps_namespace.svc:5432/toolgate?sslmode=disable" >&2
./scripts/chart-deps.sh >&2
helm install toolgate deploy/charts/toolgate --namespace "$release_namespace" \
	--set config.image.repository="$gateway_repository" \
	--set config.image.tag="$image_tag" \
	--set config.image.pullPolicy=Never \
	--set config.database.existingSecret=toolgate-database \
	--set config.oidc.issuerURL="$TOOLGATE_E2E_OIDC_ISSUER" \
	--set config.oidc.audience=toolgate \
	--set config.otlpEndpoint="http://$TOOLGATE_E2E_OTLP_LISTEN" \
	--wait --timeout 5m >&2

export TOOLGATE_E2E_NAMESPACE="$release_namespace"
export TOOLGATE_E2E_MCPSERVER_IMAGE="$mcpserver_image"
export TOOLGATE_E2E_S3_ENDPOINT="http://s3.$deps_namespace.svc:7070"

log "running: $*"
"$@"
