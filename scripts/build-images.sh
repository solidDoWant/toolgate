#!/usr/bin/env bash
# Builds the gateway image (`make build-images`) for linux/amd64 and
# linux/arm64:
#
#  1. builds the static toolgate binary for each architecture;
#  2. runs `govulncheck -mode=binary` on each binary and stops if it reports
#     a vulnerability (or fails to run);
#  3. wraps each binary into an image with the flake's dockerTools image
#     (the binary and CA certificates only, non-root user);
#  4. loads the image for this machine's architecture into Docker as
#     $IMAGE:$VERSION;
#  5. with PUSH_ALL=true, also pushes both images and a multi-arch index to
#     $IMAGE:$VERSION (releases only).
#
# Environment: VERSION (default 0.0.0-dev), IMAGE (default
# ghcr.io/soliddowant/toolgate), PUSH_ALL (default false), OUT_DIR (default
# build/images), GO_TAGS (Go build tags, default none; scripts/e2e/run.sh sets
# e2e for the test-only gateway image, releases never set it), ARCHS (all, the
# default, or host for this machine's architecture only) and VULNCHECK (true,
# the default, or false to skip govulncheck). scripts/e2e/run.sh sets
# ARCHS=host and VULNCHECK=false for the test image; PUSH_ALL=true refuses
# both.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

version="${VERSION:-0.0.0-dev}"
image="${IMAGE:-ghcr.io/soliddowant/toolgate}"
push_all="${PUSH_ALL:-false}"
out_dir="${OUT_DIR:-build/images}"
go_tags="${GO_TAGS:-}"
archs="${ARCHS:-all}"
vulncheck="${VULNCHECK:-true}"

for setting in "VULNCHECK=$vulncheck" "PUSH_ALL=$push_all"; do
	case "${setting#*=}" in
	true | false) ;;
	*)
		echo "build-images: ${setting%%=*} must be true or false, not '${setting#*=}'" >&2
		exit 2
		;;
	esac
done
case "$archs" in
all | host) ;;
*)
	echo "build-images: ARCHS must be all or host, not '$archs'" >&2
	exit 2
	;;
esac
if [[ "$push_all" == true && ( "$archs" != all || "$vulncheck" != true ) ]]; then
	echo "build-images: PUSH_ALL=true needs ARCHS=all and VULNCHECK=true" >&2
	exit 2
fi

case "$push_all" in
true | false) ;;
*)
	echo "build-images: PUSH_ALL must be true or false, not '$push_all'" >&2
	exit 2
	;;
esac

case "$(uname -m)" in
x86_64 | amd64) host_arch=amd64 ;;
aarch64 | arm64) host_arch=arm64 ;;
*)
	echo "build-images: no image for this machine's architecture ($(uname -m))" >&2
	exit 1
	;;
esac

if [[ "$archs" == host ]]; then
	architectures=("$host_arch")
else
	architectures=(amd64 arm64)
fi

mkdir -p "$out_dir"
out_dir="$(cd "$out_dir" && pwd)"

for arch in "${architectures[@]}"; do
	binary="$out_dir/linux_$arch/toolgate"
	mkdir -p "$(dirname "$binary")"
	echo "build-images: building $binary"
	CGO_ENABLED=0 GOOS=linux GOARCH="$arch" go build -trimpath -tags "$go_tags" \
		-ldflags="-s -w -X github.com/solidDoWant/toolgate/internal/version.version=$version" \
		-o "$binary" ./cmd/toolgate
done

if [[ "$vulncheck" == true ]]; then
	for arch in "${architectures[@]}"; do
		binary="$out_dir/linux_$arch/toolgate"
		echo "build-images: govulncheck -mode=binary $binary"
		if ! govulncheck -mode=binary "$binary"; then
			echo "build-images: govulncheck failed for $binary; no image was built" >&2
			exit 1
		fi
	done
fi

for arch in "${architectures[@]}"; do
	echo "build-images: building the linux/$arch image"
	TOOLGATE_BINARY="$out_dir/linux_$arch/toolgate" TOOLGATE_VERSION="$version" \
		nix build --impure --no-link --print-out-paths \
		".#packages.$(nix eval --impure --raw --expr builtins.currentSystem).image-$arch" \
		>"$out_dir/image-$arch.path"
	ln -sfn "$(cat "$out_dir/image-$arch.path")" "$out_dir/image-$arch.tar"
done

# The archive is tagged ghcr.io/soliddowant/toolgate:$version; retag it when
# IMAGE names another repository.
loaded="$(docker load --input "$out_dir/image-$host_arch.tar" | sed -n 's/^Loaded image: //p' | tail -n 1)"
if [[ "$loaded" != "$image:$version" ]]; then
	docker tag "$loaded" "$image:$version"
	docker rmi "$loaded" >/dev/null
fi
echo "build-images: loaded $image:$version (linux/$host_arch) into Docker"

if [[ "$push_all" != true ]]; then
	exit 0
fi

manifests=()
for arch in "${architectures[@]}"; do
	echo "build-images: pushing the linux/$arch image"
	crane push "$out_dir/image-$arch.tar" "$image:$version-$arch"
	manifests+=(-m "$image:$version-$arch")
done
crane index append "${manifests[@]}" -t "$image:$version"
echo "build-images: pushed $image:$version (linux/amd64, linux/arm64)"
