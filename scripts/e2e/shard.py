#!/usr/bin/env python3
"""Prints the `go test -run` pattern that selects one shard of the tests.

Reads `go test -list '.*' ./...` output on standard input and keeps the
top-level test names (Test..., Fuzz..., Example...). Tests with a duration in
the durations file (test/e2e/durations.json, a map of name to seconds) are
packed greedily: longest first, each into the shard with the least total so
far. A test with no recorded duration belongs to shard i of n (1-based) when
the SHA-256 hash of its name, modulo n, is i - 1. So every test is in exactly
one shard, a new test lands in a shard without a list to update, and the same
input always gives the same shards.

With `--match PATTERN`, only names that PATTERN (a `go test -run` pattern of
anchored alternatives, such as the acceptance pattern) matches are kept;
with `--exclude PATTERN`, the names it matches are dropped.

Prints `^(Name1|Name2|...)$`, or nothing when the shard has no tests.
"""

import argparse
import hashlib
import json
import pathlib
import re
import sys

NAME = re.compile(r"^(Test|Fuzz|Example)\w*$")
DEFAULT_DURATIONS = pathlib.Path(__file__).resolve().parents[2] / "test" / "e2e" / "durations.json"


def parse_shard(value):
    """(index, count) from `i/n`, 1 <= i <= n."""
    match = re.fullmatch(r"(\d+)/(\d+)", value)
    if not match:
        raise argparse.ArgumentTypeError(f"shard {value!r} is not i/n")
    index, count = int(match.group(1)), int(match.group(2))
    if count < 1 or not 1 <= index <= count:
        raise argparse.ArgumentTypeError(f"shard {value!r} needs 1 <= i <= n")
    return index, count


def shard_of(name, count):
    """The 1-based shard of a test name among count shards."""
    digest = hashlib.sha256(name.encode("utf-8")).digest()
    return int.from_bytes(digest[:8], "big") % count + 1


def test_names(lines):
    return sorted({line.strip() for line in lines if NAME.match(line.strip())})


def load_durations(path):
    """The name -> seconds map in path; empty when the file is missing."""
    try:
        with open(path, encoding="utf-8") as handle:
            return {name: float(seconds) for name, seconds in json.load(handle).items()}
    except FileNotFoundError:
        return {}


def assign(names, count, durations):
    """Maps each name to its 1-based shard: greedy bin packing for the names
    with a recorded duration, the name hash for the others."""
    assigned = {}
    totals = [0.0] * count
    timed = [name for name in names if name in durations]
    for name in sorted(timed, key=lambda name: (-durations[name], name)):
        shard = min(range(count), key=lambda shard: (totals[shard], shard))
        totals[shard] += durations[name]
        assigned[name] = shard + 1
    for name in names:
        assigned.setdefault(name, shard_of(name, count))
    return assigned


def select(names, index, count, match=None, exclude=None, durations=None):
    assigned = assign(names, count, durations or {})
    keep = re.compile(match) if match else None
    drop = re.compile(exclude) if exclude else None
    return [
        name
        for name in names
        if assigned[name] == index
        and (keep is None or keep.search(name))
        and (drop is None or not drop.search(name))
    ]


def run_pattern(names):
    return "^(" + "|".join(names) + ")$" if names else ""


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--shard", type=parse_shard, required=True, help="i/n: shard i of n, 1-based")
    parser.add_argument("--match", help="keep only the names this go test -run pattern matches")
    parser.add_argument("--exclude", help="drop the names this go test -run pattern matches")
    parser.add_argument("--durations", type=pathlib.Path, default=DEFAULT_DURATIONS, help="JSON map of test name to seconds")
    arguments = parser.parse_args()
    index, count = arguments.shard
    durations = load_durations(arguments.durations)
    print(run_pattern(select(test_names(sys.stdin), index, count, arguments.match, arguments.exclude, durations)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
