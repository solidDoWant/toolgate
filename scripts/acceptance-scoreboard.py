#!/usr/bin/env python3
"""Prints the acceptance scoreboard (`make acceptance`): one line per scenario
of a checklist (docs/acceptance/phase-N.md) with the result of its tests, then
the number passing.

The checklist lists its scenarios as `- **AC-N-nn Title.** ...` items. An
item whose bold title says `(manual)` is a manual check that no one runs. A
scenario titled `Release` is checked by the release manager and has no tests.
Every other scenario has a continuation line
`  Tests: `package/dir: TestName/subtest name`, ...` that names its Go tests:
the package directory, a colon, and the test's name as `go test -json` reports
it, with a subtest path joined by `/` (spaces may stand for the `_` Go uses).
A test is tied to a scenario only by this line. The results directory holds
`go test -json` output in `go-*.json` files.

A scenario is `fail` if any of its tests failed, `pass` if its tests ran and
all passed, `not run` if it has tests of which none failed and not all passed,
and `no test` when it has none (a release scenario). Manual scenarios are still
printed, but nobody runs them and they are left out of the total.

The scoreboard still prints, but the exit status is 1, when a `Tests:` line
names a test that doesn't exist in the sources or the results, or when a
scenario that is neither manual nor a release has no `Tests:` line.

With `--go-run-pattern`, it prints instead the `go test -run` pattern that
selects the top-level tests of every given checklist. It prints nothing when
there are none.
"""

import argparse
import json
import pathlib
import re
import sys

SCENARIO = re.compile(r"^- \*\*(AC-\d+-\d+)\b([^*]*)\*\*")
TESTS_LINE = re.compile(r"^\s+Tests:(.*)$")
TEST_ENTRY = re.compile(r"`([^`]*)`")
ENTRY_PARTS = re.compile(r"^(\S+): (\S.*)$")
GO_FUNCTION = re.compile(r"^func (Test\w+)\(", re.MULTILINE)
PRECEDENCE = ["skip", "pass", "fail"]


class Scenario:
    def __init__(self, identifier, title):
        self.identifier = identifier
        self.manual = "(manual)" in title
        self.release = title.strip().startswith("Release")
        self.tests = []  # (package directory, normalized test name, test name as written)
        self.malformed = []


def normalize(name):
    """The name as `go test` reports it: whitespace becomes `_`."""
    return re.sub(r"\s", "_", name)


def scenarios(checklist):
    """The checklist's scenarios, in order, with the tests they name."""
    return parse_scenarios(checklist.read_text(encoding="utf-8"))


def parse_scenarios(text):
    found = []
    for line in text.splitlines():
        match = SCENARIO.match(line)
        if match:
            found.append(Scenario(match.group(1), match.group(2)))
            continue
        tests = TESTS_LINE.match(line)
        if tests and found:
            for entry in TEST_ENTRY.findall(tests.group(1)):
                parts = ENTRY_PARTS.match(entry)
                if parts:
                    found[-1].tests.append((parts.group(1), normalize(parts.group(2)), parts.group(2)))
                else:
                    found[-1].malformed.append(entry)
    return found


def in_package(package, directory):
    return package == directory or package.endswith("/" + directory)


def results(directory):
    """{(package, test name): outcome}, outcome one of pass, fail, skip.

    The same test can appear in several files, since the unit, integration and
    e2e runs each include the tests without build tags. Each (package, test)
    counts once: fail if it failed anywhere, else pass if it passed anywhere
    (a run that skipped it, for lack of a cluster, say, does not undo that),
    else skip."""
    merged = {}
    for path in sorted(directory.glob("go-*.json")):
        for line in path.read_text(encoding="utf-8").splitlines():
            if not line.startswith("{"):
                continue
            event = json.loads(line)
            action = event.get("Action")
            if action not in ("pass", "fail", "skip") or not event.get("Test"):
                continue
            key = (event.get("Package", ""), event["Test"])
            merged[key] = max(merged.get(key, action), action, key=PRECEDENCE.index)
    return merged


def package_sources(root, directory):
    """The text of the test files in a package directory, or None."""
    package = root / directory
    if not package.is_dir():
        return None
    return "\n".join(path.read_text(encoding="utf-8") for path in sorted(package.glob("*_test.go")))


def exists_in_sources(root, directory, name):
    """Whether the test is declared: its top-level function is, and every
    subtest title after it appears in the package's test files as written."""
    source = package_sources(root, directory)
    if source is None:
        return False
    elements = name.split("/")
    if elements[0] not in {match.group(1) for match in GO_FUNCTION.finditer(source)}:
        return False
    flat = normalize(source)
    return all(normalize(element.replace("_", " ")) in flat or element in flat for element in elements[1:])


def top_level_names(listed):
    return sorted({normalized.split("/")[0] for scenario in listed for _, normalized, _ in scenario.tests})


def go_run_pattern(listed):
    names = top_level_names(listed)
    return "^(" + "|".join(names) + ")$" if names else ""


def status(outcomes, tagged):
    if "fail" in outcomes:
        return "fail"
    if outcomes and all(outcome == "pass" for outcome in outcomes):
        return "pass"
    if outcomes or tagged:
        return "not run"
    return "no test"


def main():
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--checklist", type=pathlib.Path, action="append", default=[])
    parser.add_argument("--results", type=pathlib.Path)
    parser.add_argument("--sources", type=pathlib.Path)
    parser.add_argument("--go-run-pattern", action="store_true", help="print the go test -run pattern of the checklists' tests")
    arguments = parser.parse_args()
    if arguments.go_run_pattern:
        if not arguments.checklist:
            parser.error("--checklist is required with --go-run-pattern")
        print(go_run_pattern([scenario for checklist in arguments.checklist for scenario in scenarios(checklist)]))
        return 0
    if len(arguments.checklist) != 1 or arguments.results is None or arguments.sources is None:
        parser.error("one --checklist, --results and --sources are required to print the scoreboard")

    listed = scenarios(arguments.checklist[0])
    outcomes = results(arguments.results)
    problems = []
    passing = 0
    automated = 0
    for scenario in listed:
        found = []
        for directory, normalized, written in scenario.tests:
            matching = [action for (package, test), action in outcomes.items() if test == normalized and in_package(package, directory)]
            found.extend(matching)
            if not matching and not exists_in_sources(arguments.sources, directory, written):
                problems.append(f"{scenario.identifier}: no test {written} in {directory}")
        problems.extend(f"{scenario.identifier}: malformed Tests entry `{entry}` (want `directory: TestName`)" for entry in scenario.malformed)
        if not scenario.tests and not scenario.manual and not scenario.release:
            problems.append(f"{scenario.identifier}: not manual and has no Tests line")
        line_status = "manual" if scenario.manual else status(found, bool(scenario.tests))
        passing += line_status == "pass"
        automated += not scenario.manual
        print(f"{scenario.identifier}  {line_status}")
    left_out = len(listed) - automated
    suffix = f" ({left_out} manual, not counted)" if left_out else ""
    print(f"passing {passing}/{automated}{suffix}")
    for problem in problems:
        print(f"acceptance-scoreboard: {problem}", file=sys.stderr)
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
