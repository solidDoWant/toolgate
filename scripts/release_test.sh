#!/usr/bin/env bash
# Tests `make release` (scripts/release.sh) in a throwaway copy of the
# repository whose origin is a local bare repository, with `gh` replaced by a
# stub that records its calls:
#
#  - a dry run builds the chart package and the six CLI binaries, runs the
#    image build without pushing, reports what it would publish, and tags,
#    pushes and calls gh for nothing;
#  - without a CHANGELOG section for the version, the release refuses before
#    it builds or publishes anything, with and without PUSH_ALL=true;
#  - PUSH_ALL=true pushes the tag and creates the GitHub release with the
#    CHANGELOG section as its notes, never GitHub's generated notes, and the
#    binaries attached; a second publish of the same version is refused.
#
# The image build is replaced by a stub that records how it was called;
# scripts/build-images_test.sh tests the real one.
set -euo pipefail
source_dir="$(git rev-parse --show-toplevel)"

work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

failures=0
fail() {
	echo "FAIL: $*" >&2
	failures=$((failures + 1))
}

repo="$work/repo"
mkdir -p "$repo" "$work/stub"
(cd "$source_dir" && git ls-files -z --cached --others --exclude-standard | tar --null -T - -cf - 2>/dev/null) |
	tar -xf - -C "$repo"

cat >"$repo/scripts/build-images.sh" <<EOF
#!/usr/bin/env bash
echo "VERSION=\$VERSION PUSH_ALL=\$PUSH_ALL" >>"$work/build-images.calls"
EOF
cat >"$work/stub/gh" <<EOF
#!/usr/bin/env bash
printf '%s\n' "\$*" >>"$work/gh.calls"
for argument in "\$@"; do
	case "\$argument" in
	--notes-file) next_is_notes=1 ;;
	*) if [[ -n "\${next_is_notes:-}" ]]; then cp "\$argument" "$work/gh.notes"; next_is_notes=; fi ;;
	esac
done
EOF
chmod +x "$repo/scripts/build-images.sh" "$work/stub/gh"

cat >"$repo/CHANGELOG.md" <<'EOF'
# Changelog

## 0.0.1-rc.1

The first candidate.

- It has notes.

## 0.0.0

Older notes.
EOF

git init --quiet --bare "$work/origin.git"
(
	cd "$repo"
	git init --quiet --initial-branch=master
	# The release tags HEAD with an annotated tag, which needs an identity;
	# CI runners have none.
	git config user.name test
	git config user.email test@example.com
	git add --all
	git commit --quiet -m "test tree"
	git remote add origin "$work/origin.git"
	git push --quiet origin master
)

run_release() {
	(cd "$repo" && PATH="$work/stub:$PATH" make --no-print-directory release "$@")
}

echo "== a dry run builds every artifact and reports what it would publish"
version=0.0.1-rc.1
if ! run_release VERSION="$version" >"$work/dry.log" 2>&1; then
	cat "$work/dry.log" >&2
	fail "the dry run failed"
fi
out="$repo/build/release/$version"
for name in toolgate-linux-amd64 toolgate-linux-arm64 toolgate-darwin-amd64 toolgate-darwin-arm64 \
	toolgate-windows-amd64.exe toolgate-windows-arm64.exe; do
	[[ -s "$out/bin/$name" ]] || fail "no CLI binary $name"
	grep -qF "build/release/$version/bin/$name" "$work/dry.log" || fail "the dry run does not report $name"
	grep -q " $name\$" "$out/bin/checksums.txt" 2>/dev/null || fail "checksums.txt has no entry for $name"
done
case "$(file "$out/bin/toolgate-darwin-arm64")" in *Mach-O*arm64*) ;; *) fail "toolgate-darwin-arm64 is not a macOS arm64 binary" ;; esac
case "$(file "$out/bin/toolgate-windows-amd64.exe")" in *PE32+*x86-64*) ;; *) fail "toolgate-windows-amd64.exe is not a Windows amd64 binary" ;; esac
if [[ "$(uname -s)/$(uname -m)" == Linux/x86_64 ]]; then
	printed="$("$out/bin/toolgate-linux-amd64" version 2>&1)" || true
	[[ "$printed" == "$version" ]] || fail "toolgate-linux-amd64 version printed '$printed', want '$version'"
fi
chart_version="$(tar -xOzf "$out/toolgate-$version.tgz" toolgate/Chart.yaml 2>/dev/null | sed -n 's/^version: //p')"
[[ "$chart_version" == "$version" ]] || fail "the chart package has version '$chart_version', want '$version'"
grep -qx "VERSION=$version PUSH_ALL=false" "$work/build-images.calls" 2>/dev/null ||
	fail "the image was not built without pushing; calls: $(cat "$work/build-images.calls" 2>/dev/null)"
for want in "tag:     $version" "image:   ghcr.io/soliddowant/toolgate:$version" \
	"chart:   oci://ghcr.io/soliddowant/charts/toolgate:$version" "--prerelease" "| The first candidate." \
	"nothing was published"; do
	grep -qF -- "$want" "$work/dry.log" || fail "the dry run does not report '$want'"
done
grep -qF "Older notes." "$work/dry.log" && fail "the dry run notes include the next section"
[[ -e "$work/gh.calls" ]] && fail "the dry run called gh: $(cat "$work/gh.calls")"
[[ -n "$(git -C "$repo" tag)" ]] && fail "the dry run created a tag"
[[ -n "$(git -C "$work/origin.git" tag)" ]] && fail "the dry run pushed a tag"

echo "== a missing CHANGELOG section refuses the release"
rm -f "$work/build-images.calls"
for push_all in false true; do
	if run_release VERSION=0.0.2 PUSH_ALL="$push_all" >"$work/missing.log" 2>&1; then
		fail "the release of 0.0.2 (PUSH_ALL=$push_all) succeeded without a CHANGELOG section"
	fi
	grep -qF "no '## 0.0.2' section" "$work/missing.log" || fail "the refusal does not name the missing section: $(cat "$work/missing.log")"
done
[[ -e "$work/build-images.calls" ]] && fail "an image was built without a CHANGELOG section"
[[ -e "$repo/build/release/0.0.2/toolgate-0.0.2.tgz" ]] && fail "a chart was packaged without a CHANGELOG section"
[[ -e "$work/gh.calls" ]] && fail "gh was called without a CHANGELOG section: $(cat "$work/gh.calls")"
[[ -n "$(git -C "$work/origin.git" tag)" ]] && fail "a tag was pushed without a CHANGELOG section"

echo "== PUSH_ALL=true tags, pushes and creates the release with the CHANGELOG section as notes"
if ! run_release VERSION="$version" PUSH_ALL=true >"$work/publish.log" 2>&1; then
	cat "$work/publish.log" >&2
	fail "the publish failed"
fi
[[ "$(git -C "$work/origin.git" tag)" == "$version" ]] || fail "tag $version was not pushed"
[[ "$(git -C "$work/origin.git" rev-parse "$version^{commit}" 2>/dev/null)" == "$(git -C "$repo" rev-parse HEAD)" ]] ||
	fail "tag $version does not point at HEAD"
calls="$(cat "$work/gh.calls" 2>/dev/null)"
[[ "$(wc -l <<<"$calls")" == 1 && "$calls" == "release create $version --verify-tag "* ]] ||
	fail "gh was not called once as 'release create $version --verify-tag ...': $calls"
[[ "$calls" == *--generate-notes* ]] && fail "the release uses GitHub's generated notes"
[[ "$calls" == *--prerelease* ]] || fail "the -rc release is not marked as a pre-release"
for name in toolgate-linux-amd64 toolgate-darwin-arm64 toolgate-windows-arm64.exe checksums.txt; do
	[[ "$calls" == *"build/release/$version/bin/$name"* ]] || fail "the release does not attach $name"
done
printf '\nThe first candidate.\n\n- It has notes.\n\n' >"$work/want.notes"
diff -u "$work/want.notes" "$work/gh.notes" || fail "the release notes are not the CHANGELOG section"

echo "== an existing tag refuses the release"
rm -f "$work/gh.calls"
if run_release VERSION="$version" PUSH_ALL=true >"$work/again.log" 2>&1; then
	fail "a second publish of $version succeeded"
fi
grep -qF "already exists" "$work/again.log" || fail "the refusal does not say the tag exists: $(cat "$work/again.log")"
[[ -e "$work/gh.calls" ]] && fail "gh was called for an existing tag"

if ((failures > 0)); then
	echo "release_test: $failures failure(s)" >&2
	exit 1
fi
echo "release_test: PASS"
