#!/usr/bin/env bash
# Downloads htmx from the npm registry, checks the tarball against the
# registry's `integrity` value, and writes the minified file and its SHA-256
# to internal/ui/static (`make vendor-htmx`).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

version=2.0.7
static=internal/ui/static
out="$static/htmx-$version.min.js"

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

curl --fail --silent --show-error --location -o "$work/meta.json" "https://registry.npmjs.org/htmx.org/$version"
integrity=$(python3 -I -c 'import json, sys; print(json.load(open(sys.argv[1]))["dist"]["integrity"])' "$work/meta.json")
if [[ "$integrity" != sha512-* ]]; then
	echo "vendor-htmx: unexpected integrity value: $integrity" >&2
	exit 1
fi

curl --fail --silent --show-error --location -o "$work/htmx.tgz" "https://registry.npmjs.org/htmx.org/-/htmx.org-$version.tgz"
actual="sha512-$(python3 -I -c 'import base64, hashlib, sys; print(base64.b64encode(hashlib.sha512(open(sys.argv[1], "rb").read()).digest()).decode())' "$work/htmx.tgz")"
if [[ "$actual" != "$integrity" ]]; then
	echo "vendor-htmx: the tarball does not match the registry's integrity value" >&2
	exit 1
fi

tar -xzf "$work/htmx.tgz" -C "$work" package/dist/htmx.min.js
cp "$work/package/dist/htmx.min.js" "$out"
(cd "$static" && sha256sum "htmx-$version.min.js" > SHA256SUMS)
echo "vendor-htmx: wrote $out"
