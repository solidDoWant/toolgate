#!/usr/bin/env python3
"""Rebuilds test/e2e/durations.json from `go test -json` output.

Usage: refresh-durations.py [--output FILE] EVENTS.json...

Reads the `go test -json` event files of the e2e shards of one CI run (the
`go-e2e-shard-*.json` files in the `acceptance-results-*` artifacts). For
each top-level test that passed or failed, it records the elapsed seconds.
If a name appears more than once, it keeps the longest time. The output is a
JSON object that maps a test name to seconds, sorted by name, and
scripts/e2e/shard.py reads it to balance the shards.
"""

import argparse
import json
import pathlib
import sys

DEFAULT_OUTPUT = pathlib.Path(__file__).resolve().parents[2] / "test" / "e2e" / "durations.json"


def durations(paths):
    result = {}
    for path in paths:
        with open(path, encoding="utf-8") as events:
            for line in events:
                try:
                    event = json.loads(line)
                except ValueError:
                    continue
                name = event.get("Test", "")
                if event.get("Action") not in ("pass", "fail") or not name or "/" in name or "Elapsed" not in event:
                    continue
                result[name] = max(result.get(name, 0.0), round(float(event["Elapsed"]), 2))
    return dict(sorted(result.items()))


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--output", type=pathlib.Path, default=DEFAULT_OUTPUT)
    parser.add_argument("events", nargs="+", type=pathlib.Path)
    arguments = parser.parse_args()
    result = durations(arguments.events)
    if not result:
        print("no test durations found in the input", file=sys.stderr)
        return 1
    arguments.output.write_text(json.dumps(result, indent=2) + "\n", encoding="utf-8")
    print(f"wrote {len(result)} durations to {arguments.output}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
