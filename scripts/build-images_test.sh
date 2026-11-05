#!/usr/bin/env bash
# Tests `make build-images` (scripts/build-images.sh):
#
#  - the image holds only the static toolgate binary and CA certificates,
#    runs as a non-root user, is loaded into Docker and runs;
#  - govulncheck -mode=binary runs on every binary, and a failing govulncheck
#    fails the target before any image is built or loaded;
#  - PUSH_ALL=true pushes an amd64 and arm64 index (to a local registry).
#
# Needs Docker and Nix; it skips when either is missing.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

if ! command -v nix >/dev/null || ! docker info >/dev/null 2>&1; then
	echo "build-images_test: SKIP (needs Nix and a running Docker)"
	exit 0
fi

work="$(mktemp -d)"
registry_pid=""
images=()
cleanup() {
	if [[ -n "$registry_pid" ]]; then kill "$registry_pid" 2>/dev/null || true; fi
	for image in "${images[@]}"; do docker rmi -f "$image" >/dev/null 2>&1 || true; done
	rm -rf "$work"
}
trap cleanup EXIT

failures=0
fail() {
	echo "FAIL: $*" >&2
	failures=$((failures + 1))
}

suffix="$$-$RANDOM"
version="0.0.0-test.$suffix"

echo "== the image holds only the binary and CA certificates, runs as non-root and loads into Docker"
image="toolgate-test-$suffix"
images+=("$image:$version")
VERSION="$version" IMAGE="$image" OUT_DIR="$work/out" ./scripts/build-images.sh >"$work/build.log" 2>&1 || {
	cat "$work/build.log" >&2
	fail "build-images failed"
}
if ! docker image inspect "$image:$version" >/dev/null 2>&1; then
	fail "image $image:$version is not loaded into Docker"
else
	user="$(docker image inspect --format '{{.Config.User}}' "$image:$version")"
	uid="${user%%:*}"
	if [[ -z "$uid" || "$uid" == 0 || "$uid" == root ]]; then
		fail "image user is '$user', want a non-root UID"
	fi
	output="$(docker run --rm "$image:$version" version 2>&1)" || fail "docker run ... version failed: $output"
	[[ "$output" == "$version" ]] || fail "toolgate version printed '$output', want '$version'"
fi

for arch in amd64 arm64; do
	archive="$work/out/image-$arch.tar"
	if [[ ! -e "$archive" ]]; then
		fail "no $arch image archive at $archive"
		continue
	fi
	config="$(tar -xOf "$archive" manifest.json | jq -r '.[0].Config')"
	got_arch="$(tar -xOf "$archive" "$config" | jq -r '.architecture')"
	[[ "$got_arch" == "$arch" ]] || fail "$arch image has architecture '$got_arch'"
	# Every regular file in every layer must be the binary or the CA bundle.
	mapfile -t layers < <(tar -xOf "$archive" manifest.json | jq -r '.[0].Layers[]')
	files=()
	for layer in "${layers[@]}"; do
		while IFS= read -r line; do files+=("$line"); done < <(tar -xOf "$archive" "$layer" | tar -tvf - 2>/dev/null | awk '$1 ~ /^-/ {print $NF}')
	done
	binaries=0
	certs=0
	for file in "${files[@]}"; do
		case "$file" in
		*/bin/toolgate) binaries=$((binaries + 1)) ;;
		*/etc/ssl/certs/ca-certificates.crt) certs=$((certs + 1)) ;;
		*) fail "$arch image holds an unexpected file: $file" ;;
		esac
	done
	[[ "$binaries" == 1 ]] || fail "$arch image holds $binaries toolgate binaries, want 1"
	[[ "$certs" == 1 ]] || fail "$arch image holds $certs CA bundles, want 1"
	binary="$work/out/linux_$arch/toolgate"
	if [[ -e "$binary" ]] && ! file "$binary" | grep -q 'statically linked'; then
		fail "$binary is not statically linked: $(file "$binary")"
	fi
done

echo "== a failing govulncheck -mode=binary fails the target and builds no image"
stub_dir="$work/stub"
mkdir -p "$stub_dir"
cat >"$stub_dir/govulncheck" <<EOF
#!/usr/bin/env bash
echo "\$*" >>"$work/govulncheck.calls"
exit 3
EOF
chmod +x "$stub_dir/govulncheck"
vuln_image="toolgate-test-vuln-$suffix"
images+=("$vuln_image:$version")
if PATH="$stub_dir:$PATH" VERSION="$version" IMAGE="$vuln_image" OUT_DIR="$work/vuln" \
	./scripts/build-images.sh >"$work/vuln.log" 2>&1; then
	fail "build-images succeeded although govulncheck reported vulnerabilities"
fi
if ! grep -qx -- "-mode=binary $work/vuln/linux_amd64/toolgate" "$work/govulncheck.calls" 2>/dev/null; then
	fail "govulncheck was not run as '-mode=binary' on the amd64 binary; calls: $(cat "$work/govulncheck.calls" 2>/dev/null)"
fi
if docker image inspect "$vuln_image:$version" >/dev/null 2>&1; then
	fail "an image was loaded although govulncheck failed"
fi
if [[ -e "$work/vuln/image-amd64.tar" || -e "$work/vuln/image-arm64.tar" ]]; then
	fail "an image archive was built although govulncheck failed"
fi

# A passing stub, so the push checks don't depend on the vulnerability database.
cat >"$stub_dir/govulncheck" <<EOF
#!/usr/bin/env bash
echo "\$*" >>"$work/govulncheck-pass.calls"
exit 0
EOF

echo "== PUSH_ALL=true pushes an amd64 and arm64 index"
port=$((20000 + RANDOM % 20000))
crane registry serve --address "127.0.0.1:$port" >"$work/registry.log" 2>&1 &
registry_pid=$!
for _ in $(seq 1 100); do
	if crane catalog "127.0.0.1:$port" >/dev/null 2>&1; then break; fi
	sleep 0.1
done
push_image="127.0.0.1:$port/toolgate"
images+=("$push_image:$version")
if ! PATH="$stub_dir:$PATH" PUSH_ALL=true VERSION="$version" IMAGE="$push_image" OUT_DIR="$work/push" \
	./scripts/build-images.sh >"$work/push.log" 2>&1; then
	cat "$work/push.log" >&2
	fail "build-images with PUSH_ALL=true failed"
fi
for arch in amd64 arm64; do
	grep -qx -- "-mode=binary $work/push/linux_$arch/toolgate" "$work/govulncheck-pass.calls" 2>/dev/null ||
		fail "govulncheck was not run on the $arch binary"
done
platforms="$(crane manifest "$push_image:$version" 2>/dev/null | jq -r '[.manifests[].platform | "\(.os)/\(.architecture)"] | sort | join(",")')" || true
[[ "$platforms" == "linux/amd64,linux/arm64" ]] || fail "pushed index has platforms '$platforms', want linux/amd64,linux/arm64"

echo "== without PUSH_ALL nothing is pushed"
noop_image="127.0.0.1:$port/toolgate-nopush"
images+=("$noop_image:$version")
PATH="$stub_dir:$PATH" VERSION="$version" IMAGE="$noop_image" OUT_DIR="$work/push" \
	./scripts/build-images.sh >"$work/nopush.log" 2>&1 || fail "build-images without PUSH_ALL failed"
if crane manifest "$noop_image:$version" >/dev/null 2>&1; then
	fail "build-images pushed although PUSH_ALL is not set"
fi

if ((failures > 0)); then
	echo "build-images_test: $failures failure(s)" >&2
	exit 1
fi
echo "build-images_test: PASS"
