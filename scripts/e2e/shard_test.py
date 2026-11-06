#!/usr/bin/env python3
"""Tests scripts/e2e/shard.py, the e2e shard selection, through its command
line: the shards are disjoint, together they hold every test, and a new test
lands in one shard with no list to update."""

import json
import pathlib
import re
import subprocess
import sys
import tempfile
import unittest

SCRIPT = pathlib.Path(__file__).resolve().parent / "shard.py"

NAMES = [f"TestScenario{number}" for number in range(40)] + [
    "TestAC1_07ToolCallDenied",
    "TestAC2_03AuditPayload",
    "FuzzTemplate",
    "ExampleRender",
]

# What `go test -list '.*' ./...` prints: names, then a status line per
# package, and packages without tests.
LISTING = (
    "\n".join(NAMES[:20])
    + "\nok  \tgithub.com/solidDoWant/toolgate/test/e2e\t0.012s\n"
    + "?   \tgithub.com/solidDoWant/toolgate/cmd/toolgate\t[no test files]\n"
    + "\n".join(NAMES[20:])
    # The same name in a second package.
    + "\nTestScenario3\nok  \tgithub.com/solidDoWant/toolgate/api/v1alpha1\t0.010s\n"
)


def shard(spec, listing=LISTING, *extra, check=True):
    # An absent durations file keeps these tests on the hash fallback.
    return subprocess.run(
        [sys.executable, str(SCRIPT), "--shard", spec, "--durations", "/nonexistent/durations.json", *extra],
        input=listing,
        capture_output=True,
        text=True,
        check=check,
    )


def selected(output):
    """The test names a printed `^(A|B)$` pattern selects."""
    pattern = output.strip()
    if not pattern:
        return []
    match = re.fullmatch(r"\^\((.*)\)\$", pattern)
    assert match, pattern
    return match.group(1).split("|")


class Shard(unittest.TestCase):
    def test_shards_are_disjoint_and_complete(self):
        for count in (1, 2, 3, 5):
            with self.subTest(count=count):
                seen = []
                for index in range(1, count + 1):
                    seen.extend(selected(shard(f"{index}/{count}").stdout))
                self.assertEqual(sorted(seen), sorted(NAMES), "every test in exactly one shard")

    def test_every_shard_gets_tests(self):
        for index in (1, 2, 3):
            with self.subTest(index=index):
                self.assertTrue(selected(shard(f"{index}/3").stdout))

    def test_selection_is_deterministic(self):
        self.assertEqual(shard("2/3").stdout, shard("2/3").stdout)

    def test_a_new_test_lands_in_one_shard(self):
        listing = LISTING + "TestBrandNewScenario\n"
        holders = [index for index in (1, 2, 3) if "TestBrandNewScenario" in selected(shard(f"{index}/3", listing).stdout)]
        self.assertEqual(len(holders), 1)

    def test_selected_pattern_matches_only_its_names(self):
        names = selected(shard("1/3").stdout)
        pattern = re.compile(shard("1/3").stdout.strip())
        for name in NAMES:
            self.assertEqual(bool(pattern.search(name)), name in names, name)

    def test_match_keeps_only_matching_names(self):
        acceptance = "^(TestAC[0-9]+_[0-9]+.*|TestScenario7)$"
        seen = []
        for index in (1, 2, 3):
            seen.extend(selected(shard(f"{index}/3", LISTING, "--match", acceptance).stdout))
        self.assertEqual(sorted(seen), ["TestAC1_07ToolCallDenied", "TestAC2_03AuditPayload", "TestScenario7"])

    def test_exclude_drops_matching_names(self):
        acceptance = "^(TestAC[0-9]+_[0-9]+.*|TestScenario7)$"
        seen = []
        for index in (1, 2, 3):
            seen.extend(selected(shard(f"{index}/3", LISTING, "--exclude", acceptance).stdout))
        self.assertEqual(sorted(seen), sorted(set(NAMES) - {"TestAC1_07ToolCallDenied", "TestAC2_03AuditPayload", "TestScenario7"}))

    def test_empty_shard_prints_nothing(self):
        self.assertEqual(shard("1/3", "ok  \tgithub.com/x\t0.01s\n").stdout.strip(), "")

    def test_invalid_shard_is_refused(self):
        for spec in ("0/3", "4/3", "1/0", "3", "a/b"):
            with self.subTest(spec=spec):
                result = shard(spec, check=False)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(result.stdout, "")


class Packing(unittest.TestCase):
    DURATIONS = {"TestA": 100, "TestB": 90, "TestC": 50, "TestD": 40, "TestE": 30, "TestF": 20, "TestG": 10}
    LISTING = "\n".join(DURATIONS) + "\nTestUntimed\n"

    def packed(self, spec, durations=None, listing=None):
        with tempfile.TemporaryDirectory() as directory:
            path = pathlib.Path(directory) / "durations.json"
            path.write_text(json.dumps(self.DURATIONS if durations is None else durations))
            result = subprocess.run(
                [sys.executable, str(SCRIPT), "--shard", spec, "--durations", str(path)],
                input=listing or self.LISTING,
                capture_output=True,
                text=True,
                check=True,
            )
        return selected(result.stdout)

    def test_packing_balances_a_known_input(self):
        # Longest first into the lightest shard: A->1, B->2, C->3, D->3(50+40=90),
        # E->2? totals 100,90,90 -> E goes to shard 2 (first minimum): 120;
        # F->3: 110; G->1: 110.
        expected = {1: {"TestA", "TestG"}, 2: {"TestB", "TestE"}, 3: {"TestC", "TestD", "TestF"}}
        for index, names in expected.items():
            with self.subTest(index=index):
                got = {name for name in self.packed(f"{index}/3") if name != "TestUntimed"}
                self.assertEqual(got, names)

    def test_every_test_runs_once_with_durations(self):
        seen = []
        for index in (1, 2, 3):
            seen.extend(self.packed(f"{index}/3"))
        self.assertEqual(sorted(seen), sorted([*self.DURATIONS, "TestUntimed"]))

    def test_untimed_test_falls_back_to_the_hash(self):
        holders = [index for index in (1, 2, 3) if "TestUntimed" in self.packed(f"{index}/3")]
        hashed = [index for index in (1, 2, 3) if "TestUntimed" in selected(shard(f"{index}/3", "TestUntimed\n").stdout)]
        self.assertEqual(holders, hashed)


if __name__ == "__main__":
    unittest.main()
