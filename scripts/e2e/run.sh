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
#       TOOLGATE_E2E_TOKENEXCHANGE_LISTEN  address the test process serves
#                                     the test token endpoint on over HTTPS,
#                                     with the resolver's certificate; its
#                                     /token path is the chart's only
#                                     allowed token URL
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

log() { echo "e2e: [$(date +%T)] $*" >&2; }

# mark records that a step finished, with the seconds since the script
# started; steps that run at the same time record their own marks. The
# summary line at the very end of the run shows where the time goes, also for
# a passing run. It is short and last because a CI check run keeps only the
# end of the log of a failing stage.
started_at=$SECONDS
marks="$work/marks"
mark() { echo "$((SECONDS - started_at)) $1" >>"$marks"; }
summary_line() {
	[[ -f "$marks" ]] || return 0
	sort -n "$marks" | awk '{ line = line (line == "" ? "" : ", ") $2 " " $1 } END { print "e2e: step finished at (s): " line }'
}

# slowest_tests prints the five top-level tests with the longest wall time from
# the go test -json events the CI stage writes. A
# parallel test's time includes the time it waited for the serial tests.
slowest_tests() {
	# The CI stage script sets GO_TEST_JSON for the test command only, so this
	# script reads the file CI uses when the variable is not set here.
	local events="${GO_TEST_JSON:-build/acceptance/results/go-e2e.json}"
	[[ -s "$events" ]] || return 0
	python3 -I - "$events" <<'PY' || true
import json
import sys

tests = []
for line in open(sys.argv[1]):
    try:
        event = json.loads(line)
    except ValueError:
        continue
    name = event.get("Test", "")
    if event.get("Action") in ("pass", "fail") and name and "/" not in name:
        tests.append((event.get("Elapsed", 0), name))
tests.sort(reverse=True)
if tests:
    print("e2e: slowest tests (s): " + ", ".join(f"{name} {elapsed}" for elapsed, name in tests[:5]))
PY
}

# Read by cleanup, which can run before the port-forwards start.
lgtm_forwards=()
flux_pid=

cleanup() {
	local status=$?
	if [[ -n "$flux_pid" ]] && ! wait "$flux_pid"; then
		cat "$work/flux-install.log" >&2
		log "installing Flux failed"
		((status != 0)) || status=1
	fi
	mark tests-end
	local summary
	summary="$(summary_line)"
	local slowest
	slowest="$(slowest_tests)"
	if ((status != 0)) && ! grep -qx '[0-9]* kind' "$marks" 2>/dev/null; then
		# No cluster came up: kubectl has nothing to talk to, so show what kind
		# said and what the node logged. The check run keeps only the end of the
		# log, so this goes last.
		log "the kind cluster was not created; the end of the kind output:"
		tail -n 30 "$work/kind-create.log" >&2 2>/dev/null || true
		kind export logs "$work/kind-logs" --name "$cluster" >/dev/null 2>&1 || true
		local node_log
		for node_log in "$work"/kind-logs/*/journal.log "$work"/kind-logs/*/kubelet.log "$work"/kind-logs/*/containerd.log; do
			[[ -f "$node_log" ]] || continue
			log "end of $(basename "$(dirname "$node_log")")/$(basename "$node_log"):"
			tail -n 15 "$node_log" >&2
		done
	elif ((status != 0)); then
		log "a step failed; cluster state:"
		kubectl get pods --all-namespaces -o wide >&2 || true
		kubectl -n "$release_namespace" logs --all-containers --prefix --tail=100 -l app.kubernetes.io/name=toolgate >&2 || true
		# Crash-looping system pods (Flux, kube-proxy) log why only before they restart.
		kubectl -n flux-system logs --all-containers --prefix --tail=20 --previous -l app.kubernetes.io/part-of=flux >&2 || true
		kubectl -n kube-system logs --all-containers --prefix --tail=20 --previous -l k8s-app=kube-proxy >&2 || true
	fi
	((${#lgtm_forwards[@]} == 0)) || kill "${lgtm_forwards[@]}" 2>/dev/null || true
	if [[ "${E2E_KEEP_CLUSTER:-false}" == true ]]; then
		log "keeping cluster $cluster (KUBECONFIG=$KUBECONFIG); delete it with: kind delete cluster --name $cluster"
	else
		kind delete cluster --name "$cluster" >&2 || true
		mark deleted
		summary="$(summary_line)"
		# The images are tagged for this run only; without this they pile up
		# in the Docker storage (and in CI's cached volume).
		docker image rm "$gateway_repository:$image_tag" "$mcpserver_image" "$embeddings_image" >/dev/null 2>&1 || true
		rm -rf "$work"
	fi
	# The step times come last and the slowest tests before them: a CI check
	# run keeps only the end of a long log, and the step times matter most.
	[[ -z "$slowest" ]] || echo "$slowest" >&2
	[[ -z "$summary" ]] || echo "$summary" >&2
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

# Each image builds on its own and loads into the kind cluster as soon as
# both the image and the cluster exist ($work/cluster-ready appears when kind
# create is done).
load_image() {
	local image="$1"
	until [[ -f "$work/cluster-ready" ]]; do
		kill -0 $$ 2>/dev/null || return 1
		sleep 0.5
	done
	kind load docker-image --name "$cluster" "$image" >&2
}

build_gateway_image() {
	log "building the e2e gateway image $gateway_repository:$image_tag"
	ARCHS=host VULNCHECK=false GO_TAGS=e2e VERSION="$image_tag" IMAGE="$gateway_repository" PUSH_ALL=false OUT_DIR="$work/images" ./scripts/build-images.sh >&2
	mark gw-image
	load_image "$gateway_repository:$image_tag"
	mark gw-loaded
}

build_mcpserver_image() {
	log "building the test MCP server image $mcpserver_image"
	mkdir -p "$work/mcpserver"
	CGO_ENABLED=0 go build -trimpath -o "$work/mcpserver/mcpserver" ./test/mcpserver
	printf 'FROM scratch\nCOPY mcpserver /mcpserver\nUSER 65532:65532\nEXPOSE 8080\nENTRYPOINT ["/mcpserver"]\n' >"$work/mcpserver/Dockerfile"
	docker build --quiet --tag "$mcpserver_image" "$work/mcpserver" >&2
	mark mcp-image
	load_image "$mcpserver_image"
	mark mcp-loaded
}

# The embeddings server stands in for text-embeddings-inference. The same
# binary is also installed as uvx, so it passes as the model download Job.
build_embeddings_image() {
	log "building the test embeddings server image $embeddings_image"
	mkdir -p "$work/embeddingsserver"
	CGO_ENABLED=0 go build -trimpath -o "$work/embeddingsserver/embeddingsserver" ./test/embeddingsserver
	printf 'FROM scratch\nCOPY embeddingsserver /embeddingsserver\nCOPY embeddingsserver /usr/local/bin/uvx\nUSER 1000:1000\nEXPOSE 80\nENTRYPOINT ["/embeddingsserver"]\n' >"$work/embeddingsserver/Dockerfile"
	docker build --quiet --tag "$embeddings_image" "$work/embeddingsserver" >&2
	mark emb-image
	load_image "$embeddings_image"
	mark emb-loaded
}

# The e2e test binary and the helper programs compile in the background too,
# so the build cache is warm when the chart install and the tests need them.
{
	go test -tags e2e -c -o /dev/null ./test/e2e
	go build -o /dev/null ./test/resolver/cmd/resolver ./test/keyset/cmd/keyset
	mark compiled
} >"$work/warm-build.log" 2>&1 &
warm_pid=$!

# The images build in the background while the kind cluster comes up.
image_names=(gateway mcpserver embeddings)
image_pids=()
for image_name in "${image_names[@]}"; do
	"build_${image_name}_image" >"$work/build-$image_name.log" 2>&1 &
	image_pids+=("$!")
done

# etcd keeps its data in /tmp, which a kind node mounts as tmpfs. On a slow
# disk every API write waits for an etcd fsync, which makes the tests slow
# each other down. The cluster lives for one run, so losing etcd's data on a
# node restart does not matter.
etcd_patch='kind: ClusterConfiguration
etcd:
  local:
    dataDir: /tmp/etcd'

# The kind configuration: the CI one when ci-kind-config exists, else a plain
# single-node cluster, with etcd on tmpfs either way.
kind_config() {
	if command -v ci-kind-config >/dev/null; then
		# Tekton CI: the nodes pull through the CI cluster's registrygate and keep
		# their image cache between builds, keyed by the cluster's name (ci.yaml
		# gives the e2e stage a fixed one).
		ci-kind-config --name "$cluster" --image "$kind_node_image" |
			ETCD_PATCH="$etcd_patch" yq '.kubeadmConfigPatches += [strenv(ETCD_PATCH)]'
	else
		ETCD_PATCH="$etcd_patch" NODE_IMAGE="$kind_node_image" yq -n '
			.kind = "Cluster" | .apiVersion = "kind.x-k8s.io/v1alpha4" |
			.nodes = [{"role": "control-plane", "image": strenv(NODE_IMAGE)}] |
			.kubeadmConfigPatches = [strenv(ETCD_PATCH)]'
	fi
}

# One kind create attempt; its output goes to stderr and to $1. It does not
# pass --wait: kubeadm already returns once the API server answers, and every
# later step (rollout status, Flux and the chart's --wait) waits for the node
# and the pods it needs, so waiting for node readiness here only adds time.
create_cluster() {
	local output="$1"
	kind_config | kind create cluster --name "$cluster" --kubeconfig "$KUBECONFIG" --config=- 2>&1 | tee "$output" >&2
}

# kind can fail on a busy host (for example when the node's systemd is slow to
# boot). Delete the half-made cluster and try once more. The first error is
# kept in its own file and printed again if the retry fails too.
log "creating kind cluster $cluster"
if ! create_cluster "$work/kind-create.log"; then
	log "kind failed to create the cluster; deleting it and retrying once"
	cp "$work/kind-create.log" "$work/kind-create-first.log"
	kind export logs "$work/kind-logs" --name "$cluster" >/dev/null 2>&1 || true
	kind delete cluster --name "$cluster" --kubeconfig "$KUBECONFIG" >&2 || true
	if ! create_cluster "$work/kind-create.log"; then
		log "the first kind error was:"
		tail -n 30 "$work/kind-create-first.log" >&2
		exit 1
	fi
fi
mark kind
touch "$work/cluster-ready"
# PostgreSQL, S3 and the registry pull their own images, so they start as soon
# as the cluster is up, while the gateway images still build and then load.
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
kubectl apply -f test/e2e/manifests/storageclass.yaml >&2
for index in "${!image_names[@]}"; do
	if ! wait "${image_pids[$index]}"; then
		cat "$work/build-${image_names[$index]}.log" >&2
		log "building or loading the ${image_names[$index]} image failed"
		exit 1
	fi
	cat "$work/build-${image_names[$index]}.log" >&2
done
mark loaded
kubectl wait --for=condition=Ready node --all --timeout 5m >&2
mark node-ready
for deployment in postgres s3 registry; do
	kubectl -n "$deps_namespace" rollout status "deployment/$deployment" --timeout 5m >&2
	mark "$deployment"
done
mark deps
# Flux and the telemetry backend are big images and only the tests need them.
# They start once PostgreSQL, S3 and the registry are ready, so their pulls
# do not slow the pulls the gateway install waits for.
kubectl -n "$deps_namespace" apply -f test/e2e/manifests/otel-lgtm.yaml >&2
log "installing Flux"
{
	flux install --timeout 5m
	status=$?
	mark flux
	exit "$status"
} >"$work/flux-install.log" 2>&1 &
flux_pid=$!
# The telemetry backend is only needed by the tests; it finishes starting
# while the chart installs.
{
	kubectl -n "$deps_namespace" rollout status deployment/otel-lgtm --timeout 5m
	status=$?
	mark lgtm
	exit "$status"
} >"$work/lgtm-rollout.log" 2>&1 &
lgtm_rollout_pid=$!

free_port() { python3 -c 'import socket; s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1])'; }

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
mark chart-pushed

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
wait "$warm_pid" || { cat "$work/warm-build.log" >&2; log "compiling the test programs failed"; exit 1; }
mark compile-waited
mkdir -p "$work/resolver"
go run ./test/resolver/cmd/resolver -write-cert "$work/resolver" -host "$host_address" >&2
export TOOLGATE_E2E_RESOLVER_LISTEN="$host_address:$resolver_port"
export TOOLGATE_E2E_RESOLVER_CERT="$work/resolver/cert.pem"
export TOOLGATE_E2E_RESOLVER_KEY="$work/resolver/key.pem"

# The test SPIFFE bundle endpoint for the trust domain allowed.test runs in
# the test process too, with the resolver's certificate.
spiffe_port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])')"
export TOOLGATE_E2E_SPIFFE_LISTEN="$host_address:$spiffe_port"

# The test token endpoint for the tests' TokenExchange resources runs in the
# test process too, with the resolver's certificate.
tokenexchange_port="$(python3 -c 'import socket; s = socket.socket(); s.bind(("", 0)); print(s.getsockname()[1])')"
export TOOLGATE_E2E_TOKENEXCHANGE_LISTEN="$host_address:$tokenexchange_port"

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
kubectl -n "$template_secrets_namespace" create secret generic toolgate-exchange-client \
	--from-literal=client-secret=e2e-exchange-client-secret >&2
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
	--set config.persistentVolumes.enabled=true \
	--set config.persistentVolumes.unusedRetention=1h \
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
	--set "config.tokenExchange.allowedTokenURLs={https://$TOOLGATE_E2E_TOKENEXCHANGE_LISTEN/token}" \
	--set config.tokenExchange.caFile=/etc/toolgate/resolver-ca/ca.pem \
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

mark gateway
wait "$lgtm_rollout_pid" || { cat "$work/lgtm-rollout.log" >&2; log "the telemetry backend did not start"; exit 1; }

# Port-forwards to the telemetry test backend, alive until the script exits.
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

export TOOLGATE_E2E_NAMESPACE="$release_namespace"
export TOOLGATE_E2E_MCPSERVER_IMAGE="$mcpserver_image"
export TOOLGATE_E2E_WORKLOAD_NAMESPACE="$workload_namespace"
export TOOLGATE_E2E_TEMPLATE_SECRETS_NAMESPACE="$template_secrets_namespace"
export TOOLGATE_E2E_S3_ENDPOINT="http://s3.$deps_namespace.svc:7070"
export TOOLGATE_E2E_CHART_REPOSITORY="oci://registry.$deps_namespace.svc:5000/charts/mcpserver"

# Flux keeps installing while the tests run: only the tests that create
# HelmReleases need it, and they wait for it themselves. cleanup fails the
# run when the install failed.
mark tests-start
log "running: $*"
"$@"
