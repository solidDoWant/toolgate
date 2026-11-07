#!/usr/bin/env python3
"""Tests scripts/acceptance-scoreboard.py, the scoreboard `make acceptance`
prints, against fixture checklists, results and sources, and checks the real
checklists against the real test sources."""

import json
import pathlib
import re
import subprocess
import sys
import tempfile
import unittest

ROOT = pathlib.Path(__file__).resolve().parent.parent
SCRIPT = ROOT / "scripts" / "acceptance-scoreboard.py"

CHECKLIST = """# Phase 1

- **AC-1-01 Install.** Installs.
  Tests: `test/e2e: TestInstall`
- **AC-1-02 Enable.** Enables.
  Tests: `test/e2e: TestEnable/enable creates a namespace`, `internal/gateway: TestEnableServer`
- **AC-1-03 Call.** Calls.
  Tests: `test/e2e: TestEnable/call is audited`
- **AC-1-04 Plugin (manual).** Checked by hand.
- **AC-1-05 Rules.** Rules.
  Tests: `internal/policy: TestRules`
- **AC-1-06 Release.** Released.
"""
SCENARIOS = [f"AC-1-{number:02d}" for number in range(1, 7)]

E2E_SOURCE = """package e2e

func TestInstall(t *testing.T) {}

func TestEnable(t *testing.T) {
	t.Run("enable creates a namespace", nil)
	t.Run("call is audited", nil)
}
"""
GATEWAY_SOURCE = "package gateway\n\nfunc TestEnableServer(t *testing.T) {}\n"
POLICY_SOURCE = "package policy\n\nfunc TestRules(t *testing.T) {}\n"


class Scoreboard(unittest.TestCase):
    def setUp(self):
        self.work = pathlib.Path(tempfile.mkdtemp())
        self.results = self.work / "results"
        self.results.mkdir()
        self.sources = self.work / "sources"
        self.checklist = self.work / "phase-1.md"
        self.checklist.write_text(CHECKLIST, encoding="utf-8")
        self.write_source("test/e2e/e2e_test.go", E2E_SOURCE)
        self.write_source("internal/gateway/gateway_test.go", GATEWAY_SOURCE)
        self.write_source("internal/policy/policy_test.go", POLICY_SOURCE)

    def run_script(self, *arguments):
        return subprocess.run([sys.executable, "-I", str(SCRIPT), *arguments], capture_output=True, text=True)

    def scoreboard(self, expected_status=0):
        completed = self.run_script("--checklist", str(self.checklist), "--results", str(self.results), "--sources", str(self.sources))
        self.assertEqual(completed.returncode, expected_status, completed.stdout + completed.stderr)
        self.stderr = completed.stderr
        return completed.stdout

    def statuses(self, output):
        lines = {}
        for line in output.splitlines():
            match = re.match(r"^(AC-\d+-\d\d)\s+(.+?)\s*$", line)
            if match:
                lines[match.group(1)] = match.group(2)
        return lines

    def write_go(self, *events, file="go-e2e.json", package="example.com/m/test/e2e"):
        with open(self.results / file, "w", encoding="utf-8") as handle:
            for name, action in events:
                handle.write(json.dumps({"Action": action, "Package": package, "Test": name}) + "\n")

    def write_source(self, name, text):
        target = self.sources / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_text(text, encoding="utf-8")

    def test_lists_every_scenario_with_nothing_run(self):
        output = self.scoreboard()
        statuses = self.statuses(output)
        self.assertEqual(list(statuses), SCENARIOS)
        self.assertEqual(statuses["AC-1-04"], "manual")
        self.assertEqual(statuses["AC-1-06"], "no test")
        self.assertTrue(all(statuses[scenario] == "not run" for scenario in ("AC-1-01", "AC-1-02", "AC-1-03", "AC-1-05")))
        self.assertIn("passing 0/5 (1 manual, not counted)", output)

    def test_results_come_from_the_named_tests_only(self):
        self.write_go(
            ("TestEnable/enable_creates_a_namespace", "pass"),
            ("TestEnable/enable_creates_a_namespace/inner", "fail"),  # its own subtest: not the scenario's result
            ("TestEnable/call_is_audited", "fail"),
            ("TestInstall", "pass"),
            ("TestUnrelated", "fail"),
        )
        self.write_go(("TestEnableServer", "pass"), file="go-integration.json", package="example.com/m/internal/gateway")
        statuses = self.statuses(self.scoreboard())
        self.assertEqual(statuses["AC-1-01"], "pass")
        self.assertEqual(statuses["AC-1-02"], "pass")
        self.assertEqual(statuses["AC-1-03"], "fail")

    def test_a_scenario_with_several_tests_needs_all_to_pass(self):
        self.write_go(("TestEnable/enable_creates_a_namespace", "pass"))
        self.write_go(("TestEnableServer", "fail"), file="go-integration.json", package="example.com/m/internal/gateway")
        self.assertEqual(self.statuses(self.scoreboard())["AC-1-02"], "fail")

    def test_a_test_with_the_name_in_another_package_does_not_count(self):
        self.write_go(("TestRules", "pass"), package="example.com/m/internal/other")
        self.assertEqual(self.statuses(self.scoreboard())["AC-1-05"], "not run")

    def test_one_test_in_several_result_files_counts_once(self):
        self.write_go(("TestInstall", "skip"), ("TestEnable/call_is_audited", "pass"), file="go-unit.json")
        self.write_go(("TestInstall", "pass"), ("TestEnable/call_is_audited", "skip"), file="go-e2e.json")
        statuses = self.statuses(self.scoreboard())
        self.assertEqual(statuses["AC-1-01"], "pass")  # skipped in one run, passed in the other
        self.assertEqual(statuses["AC-1-03"], "pass")

    def test_a_skipped_test_is_not_run(self):
        self.write_go(("TestInstall", "skip"))
        self.assertEqual(self.statuses(self.scoreboard())["AC-1-01"], "not run")

    def test_the_tag_in_a_test_name_ties_nothing(self):
        self.write_go(("TestAC1_05Rules", "pass"), ("TestEnable/[AC-1-05]_rules", "pass"))
        self.assertEqual(self.statuses(self.scoreboard())["AC-1-05"], "not run")

    def test_manual_scenarios_never_pass(self):
        self.write_go(("TestAC1_04Plugin", "pass"))
        self.assertEqual(self.statuses(self.scoreboard())["AC-1-04"], "manual")

    def test_a_named_test_that_does_not_exist_fails_the_scoreboard(self):
        self.write_source("test/e2e/e2e_test.go", E2E_SOURCE.replace("TestInstall", "TestSetup"))
        output = self.scoreboard(expected_status=1)
        self.assertIn("AC-1-01", output)  # the scoreboard is still printed
        self.assertIn("AC-1-01: no test TestInstall in test/e2e", self.stderr)

    def test_a_named_subtest_that_does_not_exist_fails_the_scoreboard(self):
        self.write_source("test/e2e/e2e_test.go", E2E_SOURCE.replace("call is audited", "call is logged"))
        self.scoreboard(expected_status=1)
        self.assertIn("AC-1-03: no test TestEnable/call is audited in test/e2e", self.stderr)

    def test_a_named_test_that_ran_exists_even_when_its_name_is_not_in_the_sources(self):
        self.write_source("test/e2e/e2e_test.go", E2E_SOURCE.replace("call is audited", "call is logged"))
        self.write_go(("TestEnable/call_is_audited", "pass"))
        self.assertEqual(self.statuses(self.scoreboard())["AC-1-03"], "pass")

    def test_a_scenario_that_is_not_manual_needs_tests(self):
        self.checklist.write_text(CHECKLIST.replace("  Tests: `internal/policy: TestRules`\n", ""), encoding="utf-8")
        self.assertEqual(self.statuses(self.scoreboard(expected_status=1))["AC-1-05"], "no test")
        self.assertIn("AC-1-05: not manual and has no Tests line", self.stderr)

    def test_a_malformed_entry_fails_the_scoreboard(self):
        self.checklist.write_text(CHECKLIST.replace("`internal/policy: TestRules`", "`TestRules`"), encoding="utf-8")
        self.scoreboard(expected_status=1)
        self.assertIn("malformed Tests entry `TestRules`", self.stderr)

    def test_go_run_pattern(self):
        completed = self.run_script("--go-run-pattern", "--checklist", str(self.checklist))
        self.assertEqual(completed.stdout.strip(), "^(TestEnable|TestEnableServer|TestInstall|TestRules)$")
        empty = self.work / "empty.md"
        empty.write_text("- **AC-1-01 Release.** Released.\n", encoding="utf-8")
        self.assertEqual(self.run_script("--go-run-pattern", "--checklist", str(empty)).stdout.strip(), "")


class RealChecklists(unittest.TestCase):
    def test_every_checklist_matches_the_test_sources(self):
        empty = pathlib.Path(tempfile.mkdtemp())
        for checklist in sorted((ROOT / "docs" / "acceptance").glob("phase-*.md")):
            completed = subprocess.run(
                [sys.executable, "-I", str(SCRIPT), "--checklist", str(checklist), "--results", str(empty), "--sources", str(ROOT)],
                capture_output=True,
                text=True,
            )
            self.assertEqual(completed.returncode, 0, f"{checklist.name}: {completed.stderr}")


if __name__ == "__main__":
    unittest.main()
