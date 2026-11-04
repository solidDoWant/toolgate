#!/usr/bin/env python3
"""Prints the acceptance scoreboard (`make acceptance`): one line per scenario
of a checklist (docs/acceptance/phase-N.md) with the result of its tests, then
the number passing.

The checklist lists its scenarios as `- **AC-N-nn Title.** ...` items; an
item whose bold title says `(manual)` is checked by the owner in a real
Claude Code session. A test belongs to a scenario by its name: a Go subtest
(`t.Run`) titled `[AC-N-nn] ...`, or a top-level Go test `TestACN_nn...` (Go
function names cannot hold brackets). The results directory holds
`go test -json` output in `go-*.json` files.

A scenario is `fail` if any of its tests failed, `pass` if its tests ran and
all passed, `not run` if it has tagged tests (in the sources or skipped in the
results) of which none failed and not all passed, `no test` otherwise, and
`manual` when the checklist marks it so: it counts in the total but never as
passing here.

With `--go-run-pattern`, it prints instead the `go test -run` pattern that
selects the acceptance tests: the `TestACN_nn...` tests and every top-level
test that holds an `[AC-N-nn] ...` subtest. It prints nothing when there are
none.
"""

import argparse
import json
import pathlib
import re
import sys

SCENARIO = re.compile(r"^- \*\*(AC-\d+-\d+)\b([^*]*)\*\*")
TAG = re.compile(r"\[(AC-\d+-\d+)\]")
GO_TEST = re.compile(r"^TestAC(\d+)_(\d+)")
GO_DECLARATION = re.compile(r"^func TestAC(\d+)_(\d+)\w*\(", re.MULTILINE)
GO_FUNCTION = re.compile(r"^func (Test\w+)\(", re.MULTILINE)
GO_SUBTEST = re.compile(r"""\b\w+\.Run\(\s*["`]\[(AC-\d+-\d+)\]""")
SKIPPED_DIRECTORIES = {".git", ".claude", "node_modules", "build", "bin"}


def scenarios(checklist):
    """The checklist's scenarios: (id, manual) pairs, in order."""
    found = []
    for line in checklist.read_text(encoding="utf-8").splitlines():
        match = SCENARIO.match(line)
        if match:
            found.append((match.group(1), "(manual)" in match.group(2)))
    return found


def scenario_id(phase, number):
    return f"AC-{int(phase)}-{int(number):02d}"


def title_id(title):
    match = TAG.match(title.strip())
    return match.group(1) if match else None


def go_id(name):
    """The scenario of a `go test` result: a top-level `TestACN_nn...` test,
    or a subtest named `[AC-N-nn] ...` (its own result, not its subtests')."""
    elements = name.split("/")
    if len(elements) == 1:
        match = GO_TEST.match(name)
        return scenario_id(*match.groups()) if match else None
    return title_id(elements[-1])


def results(directory):
    """(scenario id, outcome) pairs, outcome one of pass, fail, skip."""
    found = []
    for path in sorted(directory.glob("go-*.json")):
        for line in path.read_text(encoding="utf-8").splitlines():
            if not line.startswith("{"):
                continue
            event = json.loads(line)
            action = event.get("Action")
            if action not in ("pass", "fail", "skip"):
                continue
            scenario = go_id(event.get("Test", ""))
            if scenario:
                found.append((scenario, action))
    return found


def go_sources(root):
    for path in root.rglob("*_test.go"):
        if not SKIPPED_DIRECTORIES & set(path.relative_to(root).parts):
            yield path


def tagged_in_sources(root):
    """Scenario ids that have a test in the sources, run or not."""
    ids = set()
    for path in go_sources(root):
        source = path.read_text(encoding="utf-8")
        for match in GO_DECLARATION.finditer(source):
            ids.add(scenario_id(*match.groups()))
        for match in GO_SUBTEST.finditer(source):
            ids.add(match.group(1))
    return ids


def go_run_pattern(root):
    names = set()
    has_functions = False
    for path in go_sources(root):
        source = path.read_text(encoding="utf-8")
        has_functions = has_functions or bool(GO_DECLARATION.search(source))
        functions = list(GO_FUNCTION.finditer(source))
        for subtest in GO_SUBTEST.finditer(source):
            enclosing = [function for function in functions if function.start() < subtest.start()]
            if enclosing:
                names.add(enclosing[-1].group(1))
    alternatives = (["TestAC[0-9]+_[0-9]+.*"] if has_functions else []) + sorted(names)
    return "^(" + "|".join(alternatives) + ")$" if alternatives else ""


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
    parser.add_argument("--checklist", type=pathlib.Path)
    parser.add_argument("--results", type=pathlib.Path)
    parser.add_argument("--sources", type=pathlib.Path, required=True)
    parser.add_argument("--go-run-pattern", action="store_true", help="print the go test -run pattern of the acceptance tests")
    arguments = parser.parse_args()
    if arguments.go_run_pattern:
        print(go_run_pattern(arguments.sources))
        return 0
    if arguments.checklist is None or arguments.results is None:
        parser.error("--checklist and --results are required to print the scoreboard")

    listed = scenarios(arguments.checklist)
    outcomes = {scenario: [] for scenario, _ in listed}
    for scenario, outcome in results(arguments.results):
        if scenario in outcomes:
            outcomes[scenario].append(outcome)
    tagged = tagged_in_sources(arguments.sources)

    passing = 0
    for scenario, manual in listed:
        line_status = "manual" if manual else status(outcomes[scenario], scenario in tagged)
        passing += line_status == "pass"
        print(f"{scenario}  {line_status}")
    print(f"passing {passing}/{len(listed)}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
