#!/usr/bin/env python3
"""Tests scripts/acceptance-scoreboard.py, the scoreboard `make acceptance`
prints, against a fixture checklist, results and sources. The real phase 1
checklist is only checked for being read whole, so the owner can add or remove
scenarios without breaking these tests."""

import json
import pathlib
import re
import subprocess
import sys
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parent.parent
SCRIPT = ROOT / "scripts" / "acceptance-scoreboard.py"
CHECKLIST = ROOT / "docs" / "acceptance" / "phase-1.md"

# The fixture checklist: sections, prose and scenarios out of numeric order,
# with AC-1-05 and AC-1-12 manual, the way docs/acceptance/phase-N.md writes them.
FIXTURE = """# Phase 1 acceptance checklist

Each automated scenario has a test tagged `[AC-1-nn]`. Scenarios marked **manual** are checked by the owner.

## First section

- **AC-1-01 One.** Given this, then that.
- **AC-1-05 Plugin (manual).** Checked in a real session.
- **AC-1-07 Enable.** Given this, then that.
- **AC-1-08 Call.** Given this, then that.
- **AC-1-31 Added later.** Listed out of numeric order.

## Second section

Not a scenario: - **AC-1-99 Inline.** text.
- **AC-1-09 Variables.** Given this, then that.
- **AC-1-10 No injection.** Given this, then that.
- **AC-1-12 Real session (manual).** Checked in a real session.
- **AC-1-25 Block a tool.** Given this, then that.
- **AC-1-26 Block by workspace.** Given this, then that.
"""
SCENARIOS = ["AC-1-01", "AC-1-05", "AC-1-07", "AC-1-08", "AC-1-31", "AC-1-09", "AC-1-10", "AC-1-12", "AC-1-25", "AC-1-26"]
MANUAL = {"AC-1-05", "AC-1-12"}


class Scoreboard(unittest.TestCase):
    def setUp(self):
        self.work = pathlib.Path(tempfile.mkdtemp())
        self.results = self.work / "results"
        self.results.mkdir()
        self.sources = self.work / "sources"
        (self.sources / "test" / "e2e").mkdir(parents=True)
        self.checklist = self.work / "phase-1.md"
        self.checklist.write_text(FIXTURE, encoding="utf-8")

    def run_script(self, *arguments):
        completed = subprocess.run(
            [sys.executable, "-I", str(SCRIPT), "--sources", str(self.sources), *arguments],
            capture_output=True,
            text=True,
            check=True,
        )
        return completed.stdout

    def scoreboard(self, checklist=None):
        return self.run_script("--checklist", str(checklist or self.checklist), "--results", str(self.results))

    def statuses(self, output):
        lines = {}
        for line in output.splitlines():
            match = re.match(r"^(AC-\d+-\d\d)\s+(.+?)\s*$", line)
            if match:
                lines[match.group(1)] = match.group(2)
        return lines

    def write_go(self, *events):
        with open(self.results / "go-e2e.json", "w", encoding="utf-8") as file:
            for name, action in events:
                file.write(json.dumps({"Action": action, "Test": name}) + "\n")

    def write_source(self, name, text):
        (self.sources / "test" / "e2e" / name).write_text(text, encoding="utf-8")

    def test_lists_every_scenario_with_nothing_run(self):
        output = self.scoreboard()
        statuses = self.statuses(output)
        self.assertEqual(list(statuses), SCENARIOS)
        self.assertEqual({scenario for scenario, value in statuses.items() if value == "manual"}, MANUAL)
        self.assertTrue(all(value == "no test" for scenario, value in statuses.items() if scenario not in MANUAL))
        self.assertIn("passing 0/10", output)

    def test_subtest_and_function_results(self):
        self.write_go(
            ("TestEnable/[AC-1-07]_enable_creates_a_namespace", "pass"),
            ("TestEnable/[AC-1-07]_enable_creates_a_namespace/inner", "fail"),  # its own subtest: not the scenario's result
            ("TestEnable/[AC-1-08]_call", "pass"),
            ("TestEnable/[AC-1-08]_call_audited", "fail"),
            ("TestAC1_25BlockATool", "pass"),
            ("TestAC1_26BlockByWorkspace", "skip"),
            ("TestUnrelated", "fail"),
        )
        statuses = self.statuses(self.scoreboard())
        self.assertEqual(statuses["AC-1-07"], "pass")
        self.assertEqual(statuses["AC-1-08"], "fail")
        self.assertEqual(statuses["AC-1-25"], "pass")
        self.assertEqual(statuses["AC-1-26"], "not run")
        self.assertIn("passing 2/10", self.scoreboard())

    def test_a_tagged_test_that_did_not_run_is_not_run(self):
        self.write_source("x_test.go", 'package e2e\n\nfunc TestX(t *testing.T) {\n\tt.Run("[AC-1-09] variables", nil)\n}\n\nfunc TestAC1_10NoInjection(t *testing.T) {}\n')
        statuses = self.statuses(self.scoreboard())
        self.assertEqual(statuses["AC-1-09"], "not run")
        self.assertEqual(statuses["AC-1-10"], "not run")

    def test_manual_scenarios_never_pass(self):
        self.write_go(("TestAC1_05Plugin", "pass"))
        self.assertEqual(self.statuses(self.scoreboard())["AC-1-05"], "manual")

    def test_reads_every_scenario_of_the_real_checklist(self):
        # Counted by plain prefix matching, not the script's pattern, so the
        # test holds whatever scenarios the owner lists.
        listed = [line[len("- **"):].split()[0] for line in CHECKLIST.read_text(encoding="utf-8").splitlines() if line.startswith("- **AC-1-")]
        output = self.scoreboard(CHECKLIST)
        self.assertTrue(listed)
        self.assertEqual(list(self.statuses(output)), listed)
        self.assertIn(f"passing 0/{len(listed)}", output)

    def test_go_run_pattern(self):
        self.assertEqual(self.run_script("--go-run-pattern").strip(), "")
        self.write_source("x_test.go", 'package e2e\n\nfunc TestX(t *testing.T) {\n\tt.Run("[AC-1-09] variables", nil)\n}\n\nfunc TestY(t *testing.T) {}\n')
        self.assertEqual(self.run_script("--go-run-pattern").strip(), "^(TestX)$")
        self.write_source("y_test.go", "package e2e\n\nfunc TestAC1_10NoInjection(t *testing.T) {}\n")
        self.assertEqual(self.run_script("--go-run-pattern").strip(), "^(TestAC[0-9]+_[0-9]+.*|TestX)$")


if __name__ == "__main__":
    unittest.main()
