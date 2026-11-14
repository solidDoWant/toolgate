#!/usr/bin/env bash
# Runs a command against a fresh kind cluster with toolgate installed, then
# deletes the cluster. `make test-e2e` and `make acceptance` use it.
#
#   scripts/e2e/run.sh COMMAND [ARGS...]
#
# Steps:
#  1. builds the gateway image from this checkout with the e2e build tag
#     (scripts/build-images.sh with GO_TAGS=e2e, so the gateway honours
#     TOOLGATE_TEST_TIME_SCALE) and the test MCP server image (test/mcpserver);
#  2. creates a kind cluster with a unique name (toolgate-<worktree>-<random>,
#     or E2E_CLUSTER) and loads both images into it;
#  3. installs Flux (`flux install`), then PostgreSQL, the S3 server and an
#     OCI registry (test/e2e/manifests/dependencies.yaml) in the namespace
#     toolgate-e2e, and pushes the test chart (test/e2e/charts/mcpserver) to
#     the registry;
#  4. installs the chart from deploy/charts/toolgate as release toolgate in
#     toolgate-system, with the fresh image and rbac.extraRules for Flux's
#     HelmRelease and OCIRepository, and waits until it is ready;
#  5. runs COMMAND with these variables set:
#       KUBECONFIG                    the cluster's kubeconfig
#       TOOLGATE_TEST_TIME_SCALE      the factor the e2e gateway image scales
#                                     its waits by (internal/testtime), also
#                                     set in the chart; tests derive their
#                                     deadlines from it
#       TOOLGATE_E2E_NAMESPACE        the release namespace
#       TOOLGATE_E2E_MCPSERVER_IMAGE  the test MCP server image
#       TOOLGATE_E2E_DATABASE_URL     PostgreSQL through a port-forward; the test
#                                     process gives each principal default
#                                     user_scopes rows through it
#       TOOLGATE_E2E_OIDC_LISTEN      address the test process serves the fake
#                                     identity provider on (test/e2e TestMain)
#       TOOLGATE_E2E_OIDC_ISSUER      its issuer URL, reachable from pods
#       TOOLGATE_E2E_OTLP_LISTEN      address the test process serves the test
#                                     OTLP/HTTP receiver on; the chart's
#                                     config.otlpEndpoint points at it
#       TOOLGATE_E2E_OTLP_FORWARD_TO  the telemetry test backend's OTLP/HTTP
#                                     port-forward; the test OTLP receiver
#                                     passes every export on to it
#       TOOLGATE_E2E_PROMETHEUS_URL   the test backend's Prometheus, to run
#                                     PromQL on
#       TOOLGATE_E2E_LOKI_URL         the test backend's Loki
#       TOOLGATE_E2E_TEMPO_URL        the test backend's Tempo
#       TOOLGATE_E2E_S3_ENDPOINT      the S3 server inside the cluster
#       TOOLGATE_E2E_WORKLOAD_NAMESPACE  a namespace whose pods' service
#                                     account tokens the gateway accepts
#       TOOLGATE_E2E_CHART_REPOSITORY the OCI repository of the test chart,
#                                     as Flux inside the cluster reaches it
#       TOOLGATE_E2E_UI_LISTEN        loopback address the web UI's public
#                                     URL (config.ui.publicURL, https) names;
#                                     a test serves a TLS proxy to a gateway
#                                     pod there. The chart makes the group
#                                     toolgate-admins the web UI admins.
#       TOOLGATE_E2E_RESOLVER_LISTEN  address the test process serves the test
#                                     resolver on over HTTPS; the chart's
#                                     resolver `test` points at it
#       TOOLGATE_E2E_RESOLVER_CERT    its certificate, which the gateway trusts
#       TOOLGATE_E2E_RESOLVER_KEY     its key
#       TOOLGATE_E2E_SPIFFE_LISTEN    address the test process serves the
#                                     SPIFFE bundle endpoint of allowed.test
#                                     on over HTTPS, with the resolver's
#                                     certificate
#  6. deletes the cluster, also when a step fails. E2E_KEEP_CLUSTER=true
#     keeps it for debugging.
#
# The exit status is COMMAND's, or the failing step's.
set -euo pipefail

(($# > 0)) || { echo "usage: $0 COMMAND [ARGS...]" >&2; exit 2; }

root="$(git rev-parse --show-toplevel)"
cd "$root"

for tool in docker kind kubectl helm flux go yq curl; do
	command -v "$tool" >/dev/null || { echo "e2e: $tool is not on PATH (use the dev shell)" >&2; exit 1; }
done

suffix="$(od -An -N3 -tx1 /dev/urandom | tr -d ' \n')"
worktree="$(basename "$root" | tr -c 'a-z0-9\n' '-' | tr -d '\n')"
cluster="${E2E_CLUSTER:-toolgate-${worktree:0:24}-$suffix}"
release_namespace=toolgate-system
deps_namespace=toolgate-e2e
image_tag="e2e-$suffix"
# The e2e gateway image and the tests scale product waits by this factor.
export TOOLGATE_TEST_TIME_SCALE="${TOOLGATE_TEST_TIME_SCALE:-0.1}"
gateway_repository=ghcr.io/soliddowant/toolgate
# Kubernetes 1.35 (the chart needs it), through the Docker Hub mirror, pinned by digest.
kind_node_image=mirror.gcr.io/kindest/node:v1.35.8@sha256:07b2536e30b803ed61d1677a79df6115f798ce64c80f9e22f6ed45afd09323c0
mcpserver_image="localhost/toolgate-e2e-mcpserver:$image_tag"
embeddings_repository="localhost/toolgate-e2e-embeddings"
embeddings_image="$embeddings_repository:$image_tag"
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
	((${#lgtm_forwards[@]} == 0)) || kill "${lgtm_forwards[@]}" 2>/dev/null || true
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

log "building the e2e gateway image $gateway_repository:$image_tag"
GO_TAGS=e2e VERSION="$image_tag" IMAGE="$gateway_repository" PUSH_ALL=false OUT_DIR="$work/images" ./scripts/build-images.sh >&2

log "building the test MCP server image $mcpserver_image"
mkdir -p "$work/mcpserver"
CGO_ENABLED=0 go build -trimpath -o "$work/mcpserver/mcpserver" ./test/mcpserver
printf 'FROM scratch\nCOPY mcpserver /mcpserver\nUSER 65532:65532\nEXPOSE 8080\nENTRYPOINT ["/mcpserver"]\n' >"$work/mcpserver/Dockerfile"
docker build --quiet --tag "$mcpserver_image" "$work/mcpserver" >&2

# The embeddings server stands in for text-embeddings-inference. The same
# binary is also installed as uvx, so it passes as the model download Job.
log "building the test embeddings server image $embeddings_image"
mkdir -p "$work/embeddingsserver"
CGO_ENABLED=0 go build -trimpath -o "$work/embeddingsserver/embeddingsserver" ./test/embeddingsserver
printf 'FROM scratch\nCOPY embeddingsserver /embeddingsserver\nCOPY embeddingsserver /usr/local/bin/uvx\nUSER 1000:1000\nEXPOSE 80\nENTRYPOINT ["/embeddingsserver"]\n' >"$work/embeddingsserver/Dockerfile"
docker build --quiet --tag "$embeddings_image" "$work/embeddingsserver" >&2

log "creating kind cluster $cluster"
kind create cluster --image "$kind_node_image" --name "$cluster" --kubeconfig "$KUBECONFIG" --wait 3m >&2
kind load docker-image --name "$cluster" "$gateway_repository:$image_tag" "$mcpserver_image" "$embeddings_image" >&2

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
kubectl -n "$deps_namespace" apply -f test/e2e/manifests/otel-lgtm.yaml >&2
kubectl -n "$deps_namespace" rollout status deployment/postgres deployment/s3 deployment/registry deployment/otel-lgtm --timeout 5m >&2

# Port-forwards to the telemetry test backend, alive until the script exits.
free_port() { python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'; }
lgtm_forwards=()
for lgtm_port in 4318 9090 3100 3200; do
	local_port="$(free_port)"
	kubectl -n "$deps_namespace" port-forward --address 127.0.0.1 service/otel-lgtm "$local_port:$lgtm_port" >"$work/lgtm-forward-$lgtm_port.log" 2>&1 &
	lgtm_forwards+=("$!")
	case "$lgtm_port" in
		4318) export TOOLGATE_E2E_OTLP_FORWARD_TO="http://127.0.0.1:$local_port" ;;
		9090) export TOOLGATE_E2E_PROMETHEUS_URL="http://127.0.0.1:$local_port" ;;
		3100) export TOOLGATE_E2E_LOKI_URL="http://127.0.0.1:$local_port" ;;
		3200) export TOOLGATE_E2E_TEMPO_URL="http://127.0.0.1:$local_port" ;;
	esac
done

# A port-forward to PostgreSQL for the test process, which gives each test
# principal a user_scopes row with both defaults all through the store package.
postgres_port="$(free_port)"
kubectl -n "$deps_namespace" port-forward --address 127.0.0.1 service/postgres "$postgres_port:5432" >"$work/postgres-forward.log" 2>&1 &
lgtm_forwards+=("$!")
export TOOLGATE_E2E_DATABASE_URL="postgres://toolgate:$postgres_password@127.0.0.1:$postgres_port/toolgate?sslmode=disable"

log "pushing the test chart"
helm package test/e2e/charts/mcpserver --destination "$work/charts" >&2
registry_port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])')"
kubectl -n "$deps_namespace" port-forward service/registry "$registry_port:5000" >"$work/registry-forward.log" 2>&1 &
registry_forward=$!
for _ in $(seq 60); do
	curl --silent --fail "http://127.0.0.1:$registry_port/v2/" >/dev/null && break
	sleep 1
done
helm push "$work/charts/mcpserver-0.1.0.tgz" "oci://127.0.0.1:$registry_port/charts" --plain-http >&2
kill "$registry_forward"

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

# The web UI's public URL is a loopback port on the host, where a browser
# test serves a TLS proxy to a gateway pod. The fake identity provider allows
# its callback.
ui_port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])')"
export TOOLGATE_E2E_UI_LISTEN="127.0.0.1:$ui_port"

# The test resolver runs in the test process too, over HTTPS with a
# self-signed certificate the gateway trusts through the resolver's caFile.
resolver_port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])')"
mkdir -p "$work/resolver"
go run ./test/resolver/cmd/resolver -write-cert "$work/resolver" -host "$host_address" >&2
export TOOLGATE_E2E_RESOLVER_LISTEN="$host_address:$resolver_port"
export TOOLGATE_E2E_RESOLVER_CERT="$work/resolver/cert.pem"
export TOOLGATE_E2E_RESOLVER_KEY="$work/resolver/key.pem"

# The test SPIFFE bundle endpoint for the trust domain allowed.test runs in
# the test process too, with the resolver's certificate.
spiffe_port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])')"
export TOOLGATE_E2E_SPIFFE_LISTEN="$host_address:$spiffe_port"

log "installing the chart (issuer $TOOLGATE_E2E_OIDC_ISSUER)"
kubectl create namespace "$release_namespace" >&2
workload_namespace=toolgate-workloads
kubectl create namespace "$workload_namespace" >&2
template_secrets_namespace=toolgate-template-secrets
kubectl create namespace "$template_secrets_namespace" >&2
kubectl -n "$release_namespace" create secret generic toolgate-database \
	--from-literal=url="postgres://toolgate:$postgres_password@postgres.$deps_namespace.svc:5432/toolgate?sslmode=disable" >&2
mkdir -p "$work/keyset"
go run ./test/keyset/cmd/keyset "$work/keyset/keyset.json" >&2
kubectl -n "$release_namespace" create secret generic toolgate-secrets-keyset \
	--from-file=keyset.json="$work/keyset/keyset.json" >&2
kubectl -n "$release_namespace" create secret generic toolgate-resolver-ca \
	--from-file=ca.pem="$TOOLGATE_E2E_RESOLVER_CERT" >&2
./scripts/chart-deps.sh >&2
helm install toolgate deploy/charts/toolgate --namespace "$release_namespace" \
	--set config.image.repository="$gateway_repository" \
	--set config.image.tag="$image_tag" \
	--set config.image.pullPolicy=Never \
	--set config.database.existingSecret=toolgate-database \
	--set config.oidc.issuerURL="$TOOLGATE_E2E_OIDC_ISSUER" \
	--set 'config.oidc.audience={toolgate,toolgate-connector}' \
	--set config.ui.clientID=toolgate-ui \
	--set config.ui.adminGroup=toolgate-admins \
	--set config.ui.publicURL="https://$TOOLGATE_E2E_UI_LISTEN" \
	--set config.ui.approvalTimeout=1m \
	--set config.workspaceKeys.enabled=true \
	--set 'config.extraEnv[1].name=TOOLGATE_OTLP_INGEST' \
	--set 'config.extraEnv[1].value=true' \
	--set 'config.extraEnv[2].name=TOOLGATE_SECRETS_KEYSET_FILE' \
	--set 'config.extraEnv[2].value=/etc/toolgate/secrets-keyset/keyset.json' \
	--set config.otlpEndpoint="http://$TOOLGATE_E2E_OTLP_LISTEN" \
	--set config.sharedNamespace.name=toolgate-shared \
	--set config.templateSecretsNamespace="$template_secrets_namespace" \
	--set 'config.extraEnv[0].name=TOOLGATE_SERVICE_ACCOUNT_NAMESPACES' \
	--set "config.extraEnv[0].value=$workload_namespace" \
	--set 'config.extraEnv[3].name=TOOLGATE_TEST_TIME_SCALE' \
	--set-string "config.extraEnv[3].value=$TOOLGATE_TEST_TIME_SCALE" \
	--set-json "config.resolvers=[{\"name\":\"test\",\"url\":\"https://$TOOLGATE_E2E_RESOLVER_LISTEN/resolve\",\"audience\":\"toolgate-resolver-test\",\"caFile\":\"/etc/toolgate/resolver-ca/ca.pem\"}]" \
	--set-json "config.workspaceAttestation={\"namespaces\":[\"$workload_namespace\"],\"match\":{\"by\":\"claim\",\"claim\":\"sub\",\"key\":\"username\"},\"workspaceID\":\"coder/{username}/{workspace}\",\"spiffeIDPattern\":\"spiffe://allowed.test/coder/{username}/{workspace}\"}" \
	--set-json "config.spiffe.trustDomains=[{\"name\":\"allowed.test\",\"bundleEndpointURL\":\"https://$TOOLGATE_E2E_SPIFFE_LISTEN\",\"caFile\":\"/etc/toolgate/resolver-ca/ca.pem\"}]" \
	--set-json 'resources.persistence.resolver-ca={"type":"secret","name":"toolgate-resolver-ca","advancedMounts":{"gateway":{"gateway":[{"path":"/etc/toolgate/resolver-ca","readOnly":true}]}}}' \
	--set-json 'resources.persistence.secrets-keyset={"type":"secret","name":"toolgate-secrets-keyset","advancedMounts":{"gateway":{"gateway":[{"path":"/etc/toolgate/secrets-keyset","readOnly":true}]}}}' \
	--set-json 'rbac.extraRules=[{"apiGroups":["helm.toolkit.fluxcd.io"],"resources":["helmreleases"],"verbs":["create","patch","delete","get","list","watch"]},{"apiGroups":["source.toolkit.fluxcd.io"],"resources":["ocirepositories"],"verbs":["create","patch","delete","get","list","watch"]}]' \
	--set embeddings.server.enabled=true \
	--set embeddings.server.scaleToZero.enabled=true \
	--set embeddings.server.scaleToZero.idleTimeout=1m \
	--set embeddings.server.image.repository="$embeddings_repository" \
	--set embeddings.server.image.tag="$image_tag" \
	--set embeddings.server.image.digest= \
	--set embeddings.downloader.image.repository="$embeddings_repository" \
	--set embeddings.downloader.image.tag="$image_tag" \
	--set embeddings.downloader.image.digest= \
	--set embeddings.server.cache.accessModes='{ReadWriteOnce}' \
	--wait --timeout 5m >&2

export TOOLGATE_E2E_NAMESPACE="$release_namespace"
export TOOLGATE_E2E_MCPSERVER_IMAGE="$mcpserver_image"
export TOOLGATE_E2E_WORKLOAD_NAMESPACE="$workload_namespace"
export TOOLGATE_E2E_TEMPLATE_SECRETS_NAMESPACE="$template_secrets_namespace"
export TOOLGATE_E2E_S3_ENDPOINT="http://s3.$deps_namespace.svc:7070"
export TOOLGATE_E2E_CHART_REPOSITORY="oci://registry.$deps_namespace.svc:5000/charts/mcpserver"

log "running: $*"
"$@"
