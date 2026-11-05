#!/usr/bin/env python3
"""Prints the `go test -run` pattern that selects one shard of the tests.

Reads `go test -list '.*' ./...` output on standard input and keeps the
top-level test names (Test..., Fuzz..., Example...). A test belongs to shard
i of n (1-based) when the SHA-256 hash of its name, modulo n, is i - 1. So
every test is in exactly one shard, a new test lands in a shard without a
list to update, and the same name always lands in the same shard.

With `--match PATTERN`, only names that PATTERN (a `go test -run` pattern of
anchored alternatives, such as the acceptance pattern) matches are kept.

Prints `^(Name1|Name2|...)$`, or nothing when the shard has no tests.
"""

import argparse
import hashlib
import re
import sys

NAME = re.compile(r"^(Test|Fuzz|Example)\w*$")


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


def select(names, index, count, match=None):
    pattern = re.compile(match) if match else None
    return [name for name in names if shard_of(name, count) == index and (pattern is None or pattern.search(name))]


def run_pattern(names):
    return "^(" + "|".join(names) + ")$" if names else ""


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--shard", type=parse_shard, required=True, help="i/n: shard i of n, 1-based")
    parser.add_argument("--match", help="keep only the names this go test -run pattern matches")
    arguments = parser.parse_args()
    index, count = arguments.shard
    print(run_pattern(select(test_names(sys.stdin), index, count, arguments.match)))
    return 0


if __name__ == "__main__":
    sys.exit(main())
