#!/usr/bin/env bash
# Checks the human-facing Markdown pages (scripts/docs/human_pages.sh):
#   - every relative link and anchor resolves (lychee, offline);
#   - no page carries the worst agent-prose tells;
#   - docs/README.md lists every page under docs/.
# Runs in CI (the docs job) and as `make docs-check`.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

mapfile -t pages < <(scripts/docs/human_pages.sh)
status=0

echo "== links"
lychee --offline --include-fragments --no-progress --root-dir "$PWD" "${pages[@]}" || status=1

echo "== agent prose"
# An em-dash alone in a table cell ("| — |") marks an empty cell; any other
# em-dash is agent prose.
prose=0
for page in "${pages[@]}"; do
	if sed -E 's/\|[[:space:]]*—[[:space:]]*\|/| |/g' "$page" |
		grep -n -E -e '—' -e '\(decision, #' -e '\*\*Why:\*\*' | sed "s|^|$page:|"; then
		prose=1
	fi
done
if [[ "$prose" == 1 ]]; then
	echo "agent prose in a human-facing page (em-dash, '(decision, #' or '**Why:**'): rewrite these lines" >&2
	status=1
fi

echo "== docs/README.md lists every page"
python3 scripts/docs/check_index.py docs/README.md "${pages[@]}" || status=1

exit "$status"
