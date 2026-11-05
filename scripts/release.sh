#!/usr/bin/env bash
# Releases VERSION (`make release`). A dry run unless PUSH_ALL=true.
#
#  1. checks VERSION (x.y.z or x.y.z-rc.K) and reads the `## VERSION`
#     section of CHANGELOG.md; without a non-empty section it stops before
#     anything is built or published;
#  2. builds every artifact into build/release/VERSION: the gateway image
#     (scripts/build-images.sh, loaded into Docker, not pushed), the Helm
#     chart package, and the CLI binaries for linux, darwin and windows on
#     amd64 and arm64 with their SHA-256 checksums;
#  3. dry run: prints what a publish would do and stops;
#  4. PUSH_ALL=true: refuses unless the tree is clean, HEAD is on
#     origin/master and the tag is new; then tags HEAD, pushes the tag and
#     creates the GitHub release with the CHANGELOG section as its notes and
#     the CLI binaries attached. The tag's release workflow
#     (.github/workflows/release.yml) pushes the image and the chart.
#
# Environment: VERSION (required), PUSH_ALL (default false).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

version="${VERSION:-}"
push_all="${PUSH_ALL:-false}"
image="ghcr.io/soliddowant/toolgate"
chart_repository="oci://ghcr.io/soliddowant/charts"
platforms=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64)

fail() {
	echo "release: $*" >&2
	exit 1
}

case "$push_all" in
true | false) ;;
*) fail "PUSH_ALL must be true or false, not '$push_all'" ;;
esac

if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$ ]]; then
	fail "VERSION must be x.y.z or x.y.z-rc.K, not '$version'"
fi

out_dir="build/release/$version"
rm -rf "$out_dir"
mkdir -p "$out_dir/bin"
notes="$out_dir/notes.md"

# The notes are the lines after `## VERSION` (optionally followed by a space
# and more text, such as a date) up to the next `## ` heading.
if [[ -f CHANGELOG.md ]]; then
	awk -v heading="## $version" '
		/^## / { if (found) exit; if ($0 == heading || index($0, heading " ") == 1) { found = 1; next } }
		found { print }
	' CHANGELOG.md >"$notes"
else
	: >"$notes"
fi
if ! grep -q '[^[:space:]]' "$notes"; then
	fail "CHANGELOG.md has no '## $version' section with release notes; add it before releasing. Nothing was built or published."
fi

if [[ "$push_all" == true ]]; then
	echo "release: checking that $version can be published"
	[[ -z "$(git status --porcelain)" ]] || fail "the working tree is not clean; release only from a clean checkout"
	git fetch --quiet origin master
	git merge-base --is-ancestor HEAD origin/master || fail "HEAD is not on origin/master; release only from a commit on master"
	if git rev-parse --quiet --verify "refs/tags/$version" >/dev/null ||
		[[ -n "$(git ls-remote --tags origin "refs/tags/$version")" ]]; then
		fail "tag $version already exists; a release is never replaced"
	fi
fi

echo "release: building the gateway image $image:$version"
VERSION="$version" PUSH_ALL=false ./scripts/build-images.sh

echo "release: packaging the Helm chart"
./scripts/chart-deps.sh
helm package deploy/charts/toolgate --version "$version" --app-version "$version" --destination "$out_dir"
chart="$out_dir/toolgate-$version.tgz"
[[ -f "$chart" ]] || fail "helm package wrote no $chart"

assets=()
for platform in "${platforms[@]}"; do
	os="${platform%/*}"
	arch="${platform#*/}"
	name="toolgate-$os-$arch"
	[[ "$os" == windows ]] && name="$name.exe"
	echo "release: building $out_dir/bin/$name"
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
		-ldflags="-s -w -X github.com/solidDoWant/toolgate/internal/version.version=$version" \
		-o "$out_dir/bin/$name" ./cmd/toolgate
	assets+=("$out_dir/bin/$name")
done
(cd "$out_dir/bin" && sha256sum -- toolgate-*) >"$out_dir/bin/checksums.txt"
assets+=("$out_dir/bin/checksums.txt")

release_flags=(--latest)
if [[ "$version" == *-* ]]; then
	release_flags=(--prerelease --latest=false)
fi

echo
echo "release: $version would publish:"
echo "  tag:     $version on $(git rev-parse HEAD)"
echo "  image:   $image:$version (linux/amd64, linux/arm64)$([[ "$version" == *-* ]] || echo ", and :latest") (by the release workflow)"
echo "  chart:   $chart_repository/toolgate:$version (by the release workflow)"
echo "  release: $version (${release_flags[*]}) with notes from $notes:"
sed 's/^/           | /' "$notes"
echo "  assets:"
for asset in "${assets[@]}"; do echo "           $asset"; done

if [[ "$push_all" != true ]]; then
	echo
	echo "release: dry run; nothing was published. Run with PUSH_ALL=true to publish."
	exit 0
fi

git tag --annotate "$version" --message "toolgate $version"
git push origin "refs/tags/$version"
gh release create "$version" --verify-tag --title "$version" --notes-file "$notes" "${release_flags[@]}" "${assets[@]}"
echo "release: published $version; the release workflow pushes the image and the chart"
