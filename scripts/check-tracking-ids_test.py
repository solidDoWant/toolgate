#!/usr/bin/env python3
"""Tests for check-tracking-ids.py: one hit and one allowed case per pattern."""

import importlib.util
import pathlib
import unittest

SPEC = importlib.util.spec_from_file_location(
    "check_tracking_ids", pathlib.Path(__file__).with_name("check-tracking-ids.py")
)
check = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(check)

CASES = [
    ("scenario ID", "// Covers AC-4-16.", "// Covers the approval timeout."),
    ("test named after a scenario", "func TestAC416(t *testing.T) {", "func TestApprovalTimesOut(t *testing.T) {"),
    ("phase number", "// Added in phase 3.", "// Added in the second phase of the rollout."),
    ("design row number", "// See row 247.", "// Row 7 of the table is a header."),
    ("design section number", "// Per §5 of the design.", "// Per the section on leases."),
    ("issue or PR number", "// Fixes #354.", "// Use the #1 sort key; see https://example.com/page#top."),
]


class CheckTrackingIDs(unittest.TestCase):
    def test_hit_and_allowed_case_for_each_pattern(self):
        for label, hit, allowed in CASES:
            with self.subTest(label=label):
                found = check.check_text("internal/x/x.go", hit)
                self.assertEqual(1, len(found), found)
                self.assertTrue(found[0].startswith("internal/x/x.go:1: " + label), found)
                self.assertEqual([], check.check_text("internal/x/x.go", allowed))

    def test_reports_every_hit_with_its_line(self):
        text = "ok\n// AC-1-01\nok\n// phase 2 and #123\n"
        found = check.check_text("a.go", text)
        self.assertEqual(
            ["a.go:2: scenario ID 'AC-1-01'", "a.go:4: phase number 'phase 2'", "a.go:4: issue or PR number '#123'"],
            found,
        )

    def test_exceptions_are_allowed(self):
        for path in ["scripts/acceptance-scoreboard.py", "docs/acceptance/phase-1.md", ".claude/agents/x.md", "CHANGELOG.md"]:
            with self.subTest(path=path):
                self.assertEqual([], check.check_text(path, "AC-1-01 phase 2 row 123 §4 #456"))

    def test_other_paths_are_not_excepted(self):
        self.assertEqual(1, len(check.check_text("docs/user/x.md", "AC-1-01")))

    def test_every_exception_has_a_reason(self):
        for path, reason in check.EXCEPTIONS.items():
            self.assertTrue(reason.strip(), path)


if __name__ == "__main__":
    unittest.main()
