#!/usr/bin/env bash
# Prints a Markdown report of scripts/docs/ste_lint.py's counts for each
# human-facing page (scripts/docs/human_pages.sh) that changed since BASE
# (default origin/master). Advisory only: it always exits 0. The
# simple-english skill and the reader are the standard, not this score.
# CI appends the report to the job summary.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)" || exit 1
base="${1:-origin/master}"

mapfile -t pages < <(scripts/docs/human_pages.sh)
mapfile -t changed < <(git diff --name-only --diff-filter=d "$(git merge-base "$base" HEAD)" -- '*.md')

echo "## Style report (advisory)"
echo
echo "Counts from \`scripts/docs/ste_lint.py\` for the human-facing pages this change touches. They never fail the build."
echo
found=0
for page in "${changed[@]}"; do
	printf '%s\n' "${pages[@]}" | grep -qxF -- "$page" || continue
	found=1
	echo "### \`$page\`"
	echo
	python3 scripts/docs/ste_lint.py --type descriptive "$page" |
		python3 scripts/docs/style_summary.py || echo "ste_lint.py failed on this page."
	echo
done
[[ "$found" == 1 ]] || echo "No human-facing page changed."
exit 0
