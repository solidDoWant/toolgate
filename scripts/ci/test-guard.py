"""Fails a pull request that deletes or skips a checklist scenario test
(`make test-guard`, the `test-guard` CI job).

It compares HEAD with its merge base with `--base` (default `origin/master`).

Scenario tests are the Go tests named on the `Tests:` lines of the checklists
(docs/acceptance/phase-N.md; the format is in
scripts/acceptance-scoreboard.py): a package directory and a test name, with
a subtest path after a top-level test function. Every test named at the base
must still be named for the same scenario at HEAD and still exist: its
top-level function is declared in the package, and each subtest title is a
string literal in the package's test files. Moving a test between files of
its package is fine. None may gain skip calls (`t.Skip`, `t.Skipf`,
`t.SkipNow`) in its body, or for a subtest in the top-level function before
it. Commented-out code does not count.

The label `test-removal-approved` (TEST_GUARD_PR_LABELS, comma-separated),
which only the owner applies, turns the check off.

Threat model: it stops a self-merging agent from making the checklist pass
by deleting, renaming, unlisting or skipping a scenario test. It does not
judge whether a test still checks its scenario; weakened assertions are out
of its reach.
"""

import argparse
import collections
import importlib.util
import os
import pathlib
import re
import subprocess
import sys

APPROVAL_LABEL = "test-removal-approved"
CHECKLISTS = "docs/acceptance"

GO_SUBTEST = re.compile(r"\b\w+\.Run\s*\(")
GO_TOP_LEVEL = re.compile(r"^func ", re.MULTILINE)
SKIP_CALL = re.compile(r"(?:\b\w+|\))\.(?:Skip|Skipf|SkipNow)\s*\(")
STRING = re.compile(r"\"(?:[^\"\\\n]|\\.)*\"|'(?:[^'\\\n]|\\.)*'|`[^`]*`")
TITLE = re.compile(r"(?:\"((?:[^\"\\\n]|\\.)*)\"|`([^`]*)`)")

Test = collections.namedtuple("Test", "scenario directory name")
Found = collections.namedtuple("Found", "skip_calls")


def load_scoreboard():
    path = pathlib.Path(__file__).resolve().parent.parent / "acceptance-scoreboard.py"
    spec = importlib.util.spec_from_file_location("acceptance_scoreboard", path)
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


scoreboard = load_scoreboard()


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


def unquote(literal):
    return literal[1:-1].replace('\\"', '"').replace("\\\\", "\\")


def listed_tests(ref):
    """The tests the checklists at ref name, as Test tuples."""
    files = git("ls-tree", "--name-only", ref, f"{CHECKLISTS}/", check=False).stdout.split()
    tests = []
    for file in sorted(files):
        if not re.search(r"phase-\d+\.md$", file):
            continue
        for scenario in scoreboard.parse_scenarios(git("show", f"{ref}:{file}").stdout):
            tests.extend(Test(scenario.identifier, directory, written) for directory, _, written in scenario.tests)
    return tests


def package_texts(ref, directory):
    """The comment-masked text of each test file of a package directory at ref."""
    names = git("ls-tree", "--name-only", ref, f"{directory}/", check=False).stdout.split()
    return [mask_comments(git("show", f"{ref}:{name}").stdout) for name in sorted(names) if name.endswith("_test.go")]


def locate(texts, name):
    """Where the test `name` (Function or Function/subtest/...) is declared in
    the package's texts: its skip call count, or None when it is not there."""
    elements = name.split("/")
    for text in texts:
        match = re.search(rf"^func {re.escape(elements[0])}\s*\(", text, re.MULTILINE)
        if not match:
            continue
        body = text.find("{", closing(text, match.end() - 1))
        end = closing(text, body) if body >= 0 else len(text)
        literals = {scoreboard.normalize(unquote(found.group(0))) for found in STRING.finditer(text)}
        if any(scoreboard.normalize(element) not in literals for element in elements[1:]):
            return None
        if len(elements) == 1:
            return Found(len(SKIP_CALL.findall(text, match.start(), end)))
        for run in GO_SUBTEST.finditer(text, match.start(), end):
            start = run.end() - 1
            index = start + 1
            while text[index].isspace():
                index += 1
            title = TITLE.match(text, index)
            if title and scoreboard.normalize(unquote(title.group(0))) == scoreboard.normalize(elements[-1]):
                return Found(len(SKIP_CALL.findall(text, match.start(), run.start())) + len(SKIP_CALL.findall(text, start, closing(text, start))))
        return Found(len(SKIP_CALL.findall(text, match.start(), end)))
    return None


def test_problems(base, head):
    problems = []
    base_texts = {}
    head_texts = {}
    listed_at_head = set(listed_tests(head))
    for test in listed_tests(base):
        before = locate(base_texts.setdefault(test.directory, package_texts(base, test.directory)), test.name)
        if before is None:
            continue
        label = f"{test.scenario} {test.directory}: {test.name}"
        if test not in listed_at_head:
            problems.append(f"removed from the Tests line: {label}")
            continue
        after = locate(head_texts.setdefault(test.directory, package_texts(head, test.directory)), test.name)
        if after is None:
            problems.append(f"deleted or renamed: {label}")
        elif after.skip_calls > before.skip_calls:
            problems.append(f"new skip call: {label}")
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
