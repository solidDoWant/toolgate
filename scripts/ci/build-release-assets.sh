#!/usr/bin/env bash
# Builds the files attached to a GitHub release into the directory given: the
# toolgate CLI for linux, darwin and windows on amd64 and arm64
# (toolgate-<os>-<arch>, .exe on windows), static, and checksums.txt with their
# SHA-256 sums. The Tekton release run (ci.yaml `release.assets`) uploads them;
# `make release` builds them too, for its dry run.
#
#   scripts/ci/build-release-assets.sh <version> <output-dir>
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

version="${1:?usage: build-release-assets.sh <version> <output-dir>}"
version="${version#v}"
out="${2:?usage: build-release-assets.sh <version> <output-dir>}"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]]; then
	echo "build-release-assets: '$version' is not a release version (x.y.z or x.y.z-rc.K)" >&2
	exit 1
fi

mkdir -p "$out"
for platform in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64; do
	os="${platform%/*}"
	arch="${platform#*/}"
	name="toolgate-$os-$arch"
	[[ "$os" == windows ]] && name="$name.exe"
	echo "build-release-assets: building $out/$name"
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
		-ldflags="-s -w -X github.com/solidDoWant/toolgate/internal/version.version=$version" \
		-o "$out/$name" ./cmd/toolgate
done
(cd "$out" && sha256sum -- toolgate-*) >"$out/checksums.txt"
