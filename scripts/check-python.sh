#!/usr/bin/env bash
# Checks that every tracked Python script parses (no bytecode is written).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
mapfile -t files < <(git ls-files --cached --others --exclude-standard -- '*.py')
((${#files[@]} == 0)) && exit 0
python3 - "${files[@]}" <<'PY'
import ast
import sys

failed = False
for path in sys.argv[1:]:
    try:
        ast.parse(open(path, encoding="utf-8").read(), path)
    except SyntaxError as error:
        print(f"{path}:{error.lineno}: {error.msg}", file=sys.stderr)
        failed = True
sys.exit(1 if failed else 0)
PY
