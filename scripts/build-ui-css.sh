#!/usr/bin/env bash
# Builds the web UI's stylesheet (`make ui-css`): the Tailwind CSS v4
# standalone CLI from the dev shell compiles internal/ui/styles/app.css, with
# the classes the templates and ui.js use, into internal/ui/static/toolgate.css,
# which the binary embeds. No Node is needed. `make generate` runs this, so
# `make check-generate` fails when the committed stylesheet is stale.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

if ! command -v tailwindcss >/dev/null; then
	echo "build-ui-css: tailwindcss is not on PATH: enter the dev shell (nix develop)" >&2
	exit 1
fi

output=internal/ui/static/toolgate.css
tailwindcss --input internal/ui/styles/app.css --output "$output" --minify 2>/dev/null
# The pages link only same-origin stylesheets with no URLs in them; keep the
# licence banner but drop its link.
sed -i '1s#| https://tailwindcss.com ##' "$output"
