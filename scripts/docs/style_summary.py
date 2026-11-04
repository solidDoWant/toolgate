#!/usr/bin/env python3
"""Turns one ste_lint.py JSON report (on stdin) into a short Markdown summary."""
import json
import sys

report = json.load(sys.stdin)
print(f"{report['words']} words, {report['violations_total']} violations.")
print()
for name, count in sorted(report["violations"].items()):
    if count:
        print(f"- {name}: {count}")
