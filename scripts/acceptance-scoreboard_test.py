#!/usr/bin/env python3
"""Tests scripts/acceptance-scoreboard.py, the scoreboard `make acceptance`
prints, against the real phase 1 checklist and fixture results and sources."""

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

# The phase 1 checklist's scenarios, written out here rather than read the way
# the script reads them: AC-1-01 to AC-1-31, with AC-1-05 and AC-1-12 manual.
SCENARIOS = [f"AC-1-{number:02d}" for number in range(1, 32)]
MANUAL = {"AC-1-05", "AC-1-12"}


class Scoreboard(unittest.TestCase):
    def setUp(self):
        self.work = pathlib.Path(tempfile.mkdtemp())
        self.results = self.work / "results"
        self.results.mkdir()
        self.sources = self.work / "sources"
        (self.sources / "test" / "e2e").mkdir(parents=True)

    def run_script(self, *arguments):
        completed = subprocess.run(
            [sys.executable, "-I", str(SCRIPT), "--sources", str(self.sources), *arguments],
            capture_output=True,
            text=True,
            check=True,
        )
        return completed.stdout

    def scoreboard(self):
        return self.run_script("--checklist", str(CHECKLIST), "--results", str(self.results))

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
        self.assertIn("passing 0/31", output)

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
        self.assertIn("passing 2/31", self.scoreboard())

    def test_a_tagged_test_that_did_not_run_is_not_run(self):
        self.write_source("x_test.go", 'package e2e\n\nfunc TestX(t *testing.T) {\n\tt.Run("[AC-1-09] variables", nil)\n}\n\nfunc TestAC1_10NoInjection(t *testing.T) {}\n')
        statuses = self.statuses(self.scoreboard())
        self.assertEqual(statuses["AC-1-09"], "not run")
        self.assertEqual(statuses["AC-1-10"], "not run")

    def test_manual_scenarios_never_pass(self):
        self.write_go(("TestAC1_05Plugin", "pass"))
        self.assertEqual(self.statuses(self.scoreboard())["AC-1-05"], "manual")

    def test_go_run_pattern(self):
        self.assertEqual(self.run_script("--go-run-pattern").strip(), "")
        self.write_source("x_test.go", 'package e2e\n\nfunc TestX(t *testing.T) {\n\tt.Run("[AC-1-09] variables", nil)\n}\n\nfunc TestY(t *testing.T) {}\n')
        self.assertEqual(self.run_script("--go-run-pattern").strip(), "^(TestX)$")
        self.write_source("y_test.go", "package e2e\n\nfunc TestAC1_10NoInjection(t *testing.T) {}\n")
        self.assertEqual(self.run_script("--go-run-pattern").strip(), "^(TestAC[0-9]+_[0-9]+.*|TestX)$")


if __name__ == "__main__":
    unittest.main()
