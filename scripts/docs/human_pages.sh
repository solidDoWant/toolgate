#!/usr/bin/env bash
# Prints the human-facing Markdown pages, one per line: the front door, the
# release notes (once they exist), SPEC.md and every page under docs/ except
# docs/agents/ (agent instructions, exempt from the style rules).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
for page in README.md CHANGELOG.md SPEC.md; do
	[[ -f "$page" ]] && echo "$page"
done
git ls-files --cached --others --exclude-standard -- 'docs/*.md' ':!docs/agents/' | sort
