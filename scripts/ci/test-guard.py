#!/usr/bin/env python3
"""Fails a pull request that deletes or skips a checklist scenario test
(`make test-guard`, the `test-guard` CI job).

It compares HEAD with its merge base with `--base` (default `origin/master`).

Scenario tests are the Go tests tagged with a checklist id
(docs/acceptance/phase-N.md): `t.Run` subtests (any `x.Run(...)` call) whose
title starts with `[AC-N-nn]`, and top-level functions named
`TestACN_nn...` (Go function names cannot hold brackets). A test is
identified by its title (or function name), wherever it lives, so moving one
between files is fine. Every title at the base must still exist at HEAD as
often as before, and none may gain skip calls (`t.Skip`, `t.Skipf`,
`t.SkipNow`) in its body, or for a subtest in its enclosing top-level
function before it. Commented-out code does not count.

The label `test-removal-approved` (TEST_GUARD_PR_LABELS, comma-separated),
which only the owner applies, turns the check off.

Threat model: it stops a self-merging agent from making the checklist pass
by deleting, renaming or skipping a scenario test. It does not judge whether
a test still checks its scenario; weakened assertions are out of its reach.
"""

import argparse
import collections
import os
import re
import subprocess
import sys

TAG = r"\[AC-\d+-\d+\]"
APPROVAL_LABEL = "test-removal-approved"

GO_SUBTEST = re.compile(r"\b\w+\.Run\s*\(")
GO_FUNCTION = re.compile(r"^func (TestAC\d+_\d+\w*)\s*\(", re.MULTILINE)
GO_TOP_LEVEL = re.compile(r"^func ", re.MULTILINE)
SKIP_CALL = re.compile(r"(?:\b\w+|\))\.(?:Skip|Skipf|SkipNow)\s*\(")
STRING = re.compile(r"\"(?:[^\"\\\n]|\\.)*\"|'(?:[^'\\\n]|\\.)*'|`[^`]*`")
TITLE = re.compile(r"(?:\"((?:[^\"\\\n]|\\.)*)\"|`([^`]*)`)")


Test = collections.namedtuple("Test", "title path skip_calls")


def git(*arguments, check=True):
    completed = subprocess.run(["git", *arguments], capture_output=True, text=True, encoding="utf-8")
    if check and completed.returncode != 0:
        raise SystemExit(f"test-guard: git {' '.join(arguments)} failed: {completed.stderr.strip()}")
    return completed


def mask_comments(text):
    """The text with `//` and `/* */` comments blanked out (newlines kept), so
    commented-out tests and skips don't count. Strings are kept."""
    out = []
    index = 0
    length = len(text)
    while index < length:
        char = text[index]
        if char in "\"'`":
            match = STRING.match(text, index)
            end = match.end() if match else index + 1
            out.append(text[index:end])
            index = end
        elif text.startswith("//", index):
            end = text.find("\n", index)
            end = length if end < 0 else end
            out.append(" " * (end - index))
            index = end
        elif text.startswith("/*", index):
            end = text.find("*/", index + 2)
            end = length if end < 0 else end + 2
            out.append(re.sub(r"[^\n]", " ", text[index:end]))
            index = end
        else:
            out.append(char)
            index += 1
    return "".join(out)


def closing(text, open_index):
    """The index just past the bracket that closes the one at open_index
    (comments already masked; strings skipped), or the end of the text."""
    depth = 0
    index = open_index
    while index < len(text):
        char = text[index]
        if char in "\"'`":
            match = STRING.match(text, index)
            index = match.end() if match else index + 1
            continue
        if char in "([{":
            depth += 1
        elif char in ")]}":
            depth -= 1
            if depth == 0:
                return index + 1
        index += 1
    return len(text)


def tagged_title(text, open_index):
    """The first argument of the call whose `(` is at open_index, if it is a
    string literal starting with a checklist id."""
    index = open_index + 1
    while index < len(text) and text[index].isspace():
        index += 1
    match = TITLE.match(text, index)
    if not match:
        return None
    title = next(group for group in match.groups() if group is not None)
    return " ".join(title.split()) if re.match(TAG, title) else None


def go_tests(path, text):
    tests = []
    for match in GO_FUNCTION.finditer(text):
        body = text.find("{", closing(text, match.end() - 1))
        end = closing(text, body) if body >= 0 else len(text)
        tests.append(Test(f"func {match.group(1)}", path, len(SKIP_CALL.findall(text, match.start(), end))))
    tops = [match.start() for match in GO_TOP_LEVEL.finditer(text)]
    for match in GO_SUBTEST.finditer(text):
        start = match.end() - 1
        title = tagged_title(text, start)
        if title is None:
            continue
        parent = max((top for top in tops if top < match.start()), default=0)
        skip_calls = len(SKIP_CALL.findall(text, parent, match.start())) + len(SKIP_CALL.findall(text, start, closing(text, start)))
        tests.append(Test(title, path, skip_calls))
    return tests


def scenario_tests(ref):
    """The scenario tests at ref."""
    listing = git("grep", "-lE", r"\[AC-[0-9]+-[0-9]+\]|func TestAC[0-9]+_[0-9]", ref, "--", "*.go", check=False)
    if listing.returncode not in (0, 1):
        raise SystemExit(f"test-guard: git grep failed: {listing.stderr.strip()}")
    tests = []
    for line in listing.stdout.splitlines():
        path = line.split(":", 1)[1]
        tests.extend(go_tests(path, mask_comments(git("show", f"{ref}:{path}").stdout)))
    return tests


def test_problems(base, head):
    problems = []
    by_title = collections.defaultdict(lambda: ([], []))
    for test in scenario_tests(base):
        by_title[test.title][0].append(test)
    for test in scenario_tests(head):
        by_title[test.title][1].append(test)
    for title, (before, after) in sorted(by_title.items()):
        if not before:
            continue
        where = ", ".join(sorted({test.path for test in before}))
        if len(after) < len(before):
            problems.append(f"deleted or renamed: {title} (was in {where})")
        elif sum(test.skip_calls for test in after) > sum(test.skip_calls for test in before):
            problems.append(f"new skip call: {title} (in {', '.join(sorted({test.path for test in after}))})")
    return problems


def main():
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--base", default="origin/master", help="the branch the pull request merges into")
    parser.add_argument("--head", default="HEAD")
    arguments = parser.parse_args()

    labels = {label.strip() for label in os.environ.get("TEST_GUARD_PR_LABELS", "").split(",")}
    if APPROVAL_LABEL in labels:
        print(f"test-guard: skipped, the pull request has the {APPROVAL_LABEL} label")
        return 0
    base = git("merge-base", arguments.base, arguments.head).stdout.strip()
    problems = test_problems(base, arguments.head)
    if not problems:
        print(f"test-guard: no scenario test removed or skipped since {base[:12]}")
        return 0
    print(f"test-guard: since the merge base {base[:12]}:")
    for problem in problems:
        print(f"  {problem}")
    print(f"Restore them. Removing a scenario test needs the owner's {APPROVAL_LABEL} label, then a re-run of this job.")
    return 1


if __name__ == "__main__":
    sys.exit(main())
