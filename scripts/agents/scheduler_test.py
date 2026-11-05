#!/usr/bin/env python3
"""Tests scripts/agents/scheduler.py with fake GitHub data, a fake `claude`
executable and a throwaway git repository (no network, no real agents).

  python3 scripts/agents/scheduler_test.py   (also run by `make test`)
"""

from __future__ import annotations

import datetime as dt
import io
import json
import os
import subprocess
import sys
import tempfile
import textwrap
import time
import unittest
from pathlib import Path

sys.dont_write_bytecode = True  # no __pycache__ in the checkout
sys.path.insert(0, str(Path(__file__).resolve().parent))
import scheduler as s  # noqa: E402

CFG = s.deep_merge(s.DEFAULT_CONFIG, {})
# The pr-reviewer stage is off by default; these tests cover its routing.
REVIEWED = s.deep_merge(CFG, {"reviews": True})
NOW = dt.datetime(2026, 10, 21, 5, 0, tzinfo=dt.timezone.utc)
MS1 = "Phase 1 — Core gateway"
MS2 = "Phase 2 — Security model and web UI"
MILESTONES = {MS1: 1, MS2: 2}


def issue(n, title=None, status="status:todo", milestone=MS1, labels=(), parent=None, blocked_by=(), author="solidDoWant",
          sub_issues=0):
    lab = set(labels) | ({status} if status else set())
    return s.Issue(n, title or f"feat(x): issue {n}", author, lab, milestone, parent, list(blocked_by), sub_issues)


def review(verdict, commit, url="https://example/r", headline=""):
    return s.Review(verdict, commit, url, "2026-10-21T00:00:00Z", headline or f"**Verdict: {verdict}**")


def pr(n, issue_n, head="h" * 40, check="success", draft=False, auto=False, mergeable="MERGEABLE", reviews=(),
       author="solidDoWant", branch=None, files=("internal/x.go",), failed=()):
    return s.PR(n, f"feat(x): pr {n}", author, draft, branch or f"feat/x-{issue_n}", head, mergeable, "CLEAN", auto,
                [issue_n] if issue_n else [], list(files), list(reviews), check, list(failed))


def snap(issues=(), prs=()):
    return s.Snapshot({i.number: i for i in issues}, {p.number: p for p in prs}, dict(MILESTONES))


def plan_for(sn, state=None, holds=None, cfg=CFG):
    st = {"sessions": {}, "running": {}, "failures": {}, "gave_up": {}, "ci_reruns": {}, "specials": {}}
    st.update(state or {})
    return s.build_plan(sn, st, cfg, holds or {}, NOW)


def keys(plan):
    return [j.key for j in plan.jobs]


class VerdictTest(unittest.TestCase):
    def test_real_headlines(self):
        cases = {
            "**Verdict: merge** (full review). No blocking findings.": "merge",
            "**Verdict: changes requested.** There is one blocking test gap": "changes",
            "**Verdict: no blocking findings; merging once CI is green on the updated branch.**": "merge",
            "**Verdict: request changes (rebase needed).** While CI ran": "changes",
            "**Verdict: approve and merge** (full review; no blocking findings).": "merge",
            "**Verdict: request changes** (one blocking finding, in the policy engine's handling": "changes",
            "**Verdict: reject.** The approach is out of scope.": "reject",
            "**Verdict: merge (light review).** Auto-merge armed at `9a5aa16f`.": "merge",
            # A "request changes" verdict that also says "no blocking".
            "**Verdict: request changes** (rebase needed; the content review of `9a230111` found no blocking "
            "defects)": "changes",
            "**Verdict: rebase needed** (no blocking findings in the content).": "changes",
            "**Verdict: merge (no blocking findings)**": "merge",
            "Thanks, addressed in abc123": None,
        }
        for body, want in cases.items():
            with self.subTest(body=body):
                self.assertEqual(s.parse_verdict(body)[0], want)


class DefaultsTest(unittest.TestCase):
    def test_toolgate_defaults(self):
        self.assertEqual(CFG["repo"], "solidDoWant/toolgate")
        self.assertFalse(CFG["reviews"])
        self.assertFalse(CFG["later_phase_work"])
        self.assertFalse(CFG["renovate_prs"])
        self.assertEqual(CFG["critical_parents"], [])
        self.assertEqual(CFG["role_models"]["merge-fixer"], {"model": "sonnet", "effort": "low"})


class PlanTest(unittest.TestCase):
    def test_priority_order_across_tiers(self):
        sn = snap(
            [issue(1), issue(2, status="status:in-review"), issue(3, status="status:in-review"),
             issue(4, status="status:in-review"), issue(5, status="status:in-review")],
            [
                pr(10, 2, head="a" * 40, check="success"),  # T2 review
                pr(11, 3, head="b" * 40, reviews=[review("changes", "b" * 40)]),  # T3 worker
                pr(12, 4, auto=True, mergeable="CONFLICTING"),  # T1 reviewer
                pr(13, 5, check="pending"),  # waits for CI
            ],
        )
        plan = plan_for(sn, cfg=REVIEWED)
        self.assertEqual(keys(plan), ["reviewer:pr-12", "reviewer:pr-10", "worker:3", "worker:1"])
        self.assertIn(("PR #13", "CI pending"), plan.skipped)

    def test_armed_ci_failure_reruns_once_then_worker(self):
        failed = [{"name": "e2e", "url": "https://x/1", "run_id": 99}]
        sn = snap([issue(2, status="status:in-review")], [pr(10, 2, check="failure", auto=True, failed=failed)])
        plan = plan_for(sn)
        self.assertEqual(plan.reruns, [(10, [99], ["e2e"])])
        self.assertEqual(keys(plan), [])
        plan = plan_for(sn, {"ci_reruns": {"10": {"sha": "h" * 40, "jobs": ["e2e"]}}})
        self.assertEqual(plan.reruns, [])
        self.assertEqual(keys(plan), ["worker:2"])
        self.assertIn("e2e (https://x/1)", plan.jobs[0].trigger)

    def test_armed_lint_failure_goes_to_the_worker_without_a_rerun(self):
        failed = [{"name": "lint", "url": "https://x/1", "run_id": 99}, {"name": "unit", "url": "https://x/2", "run_id": 99}]
        plan = plan_for(snap([issue(2, status="status:in-review")], [pr(10, 2, check="failure", auto=True, failed=failed)]))
        self.assertEqual(plan.reruns, [])
        self.assertEqual(keys(plan), ["worker:2"])
        self.assertIn("lint (https://x/1)", plan.jobs[0].trigger)
        self.assertIn("reproduce the failing tests locally", plan.jobs[0].trigger)
        failed = [{"name": "integration", "url": "https://x/3", "run_id": 98}]
        plan = plan_for(snap([issue(2, status="status:in-review")], [pr(10, 2, check="failure", auto=True, failed=failed)]))
        self.assertEqual((plan.reruns, keys(plan)), ([], ["worker:2"]))

    def test_red_unarmed_pr_goes_to_reviewer(self):
        plan = plan_for(snap([issue(2, status="status:in-review")], [pr(10, 2, check="failure")]), cfg=REVIEWED)
        self.assertEqual(keys(plan), ["reviewer:pr-10"])

    def test_reviewed_head_and_moved_head(self):
        approved = pr(10, 2, head="a" * 40, reviews=[review("merge", "a" * 40)])
        self.assertEqual(keys(plan_for(snap([issue(2, status="status:in-review")], [approved]), cfg=REVIEWED)), [])
        # The worker pushed after a change request: back to a reviewer once CI finishes.
        moved = pr(10, 2, head="c" * 40, reviews=[review("changes", "a" * 40)])
        self.assertEqual(keys(plan_for(snap([issue(2, status="status:in-review")], [moved]), cfg=REVIEWED)), ["reviewer:pr-10"])

    def test_conflicting_unreviewed_pr_does_not_wait_for_ci(self):
        sn = snap([issue(2, status="status:in-review")], [pr(10, 2, check="missing", mergeable="CONFLICTING")])
        self.assertEqual(keys(plan_for(sn, cfg=REVIEWED)), ["reviewer:pr-10"])

    def test_skips_drafts_renovate_foreign_prs_and_holds(self):
        sn = snap([issue(2, status="status:in-review"), issue(3, status="status:in-review")], [
            pr(10, 2, draft=True),
            pr(11, None, author="renovate", branch="renovate/go"),
            pr(12, None, branch="renovate/npm"),
            pr(13, 3),
        ])
        self.assertEqual(keys(plan_for(sn, holds={"pr:13": "operator"})), [])
        self.assertEqual(keys(plan_for(sn, holds={"issue:3": "wait for bug hunt"})), [])
        self.assertEqual(keys(plan_for(sn, cfg=REVIEWED)), ["reviewer:pr-13"])

    def test_ready_issue_filters(self):
        sn = snap([
            issue(1),
            issue(2, author="mallory"),
            issue(3, labels=["needs-human"]),
            issue(4, blocked_by=[(1, "OPEN")]),
            issue(5, blocked_by=[(1, "CLOSED")]),
            issue(6, status="status:triage"),
            issue(7, sub_issues=3),  # a tracker
            issue(8, author="github-actions"),
            issue(9, status="status:blocked"),
        ])
        plan = plan_for(sn, holds={"issue:5": "x"})
        self.assertEqual(sorted(keys(plan)), ["worker:1", "worker:8"])
        self.assertIn(("#4", "blocked by #1"), plan.skipped)

    def test_ready_order_phase_critical_dependents(self):
        sn = snap([
            issue(1, labels=["severity:high"], milestone=MS2),  # later phase, even if critical
            issue(2),  # current phase, other
            issue(3, parent=77),  # child of a configured critical parent
            issue(4, parent=100),  # planned (parent is the phase epic)
            issue(100, labels=["epic"], status=None, sub_issues=5),
            issue(5, labels=["follow-up"], parent=100),  # follow-up: not planned
            issue(6),  # unblocks 7 and (transitively) 8
            issue(7, blocked_by=[(6, "OPEN")]),
            issue(8, blocked_by=[(7, "OPEN")]),
        ])
        cfg = s.deep_merge(CFG, {"critical_parents": [77], "later_phase_work": True})
        self.assertEqual(keys(plan_for(sn, cfg=cfg)), ["worker:3", "worker:4", "worker:6", "worker:2", "worker:5", "worker:1"])

    def test_in_progress_issue_without_pr_or_agent_is_resumed(self):
        sn = snap([issue(1, status="status:in-progress"), issue(2, status="status:in-progress")],
                  [pr(10, 2, check="pending")])
        plan = plan_for(sn)
        self.assertEqual(keys(plan), ["worker:1"])
        self.assertEqual(plan.jobs[0].tier, 4)

    def test_continuations_and_wait_pr(self):
        sessions = {
            "worker:1": {"role": "issue-worker", "issue": 1, "handoff": {"state": "incomplete"}},
            "worker:2": {"role": "issue-worker", "issue": 2, "interrupted": "usage limit"},
            "worker:3": {"role": "issue-worker", "issue": 3, "handoff": {"state": "wait-pr", "pr": 77}},
            "worker:4": {"role": "issue-worker", "issue": 4, "handoff": {"state": "wait-pr", "pr": 10}},
        }
        sn = snap([issue(n, status="status:in-progress") for n in (1, 2, 3, 4)], [pr(10, 99, check="pending")])
        plan = plan_for(sn, {"sessions": sessions})
        self.assertEqual(keys(plan), ["worker:1", "worker:2", "worker:3"])
        self.assertIn("PR #77", plan.jobs[2].trigger)
        # Each continuation run gets its own time box.
        self.assertIn("time box starts now", plan.jobs[0].trigger)
        self.assertIn("time box starts now", plan.jobs[1].trigger)

    def test_question_answered_resumes_with_note(self):
        sessions = {"worker:1": {"role": "issue-worker", "issue": 1, "handoff": {"state": "question"}}}
        self.assertEqual(keys(plan_for(snap([issue(1, status="status:in-progress")]), {"sessions": sessions})), [])
        plan = plan_for(snap([issue(1)]), {"sessions": sessions})
        self.assertIn("answered or unblocked", plan.jobs[0].trigger)

    def test_answered_question_resumes_despite_draft_pr(self):
        sessions = {"worker:1": {"role": "issue-worker", "issue": 1, "handoff": {"state": "question"}}}
        sn = snap([issue(1)], [pr(10, 1, draft=True)])
        plan = plan_for(sn, {"sessions": sessions})
        self.assertEqual(keys(plan), ["worker:1"])
        self.assertEqual(plan.jobs[0].pr, 10)
        self.assertIn("answered or unblocked", plan.jobs[0].trigger)
        # Without a hand-back, a todo issue whose open PR is a draft resumes on it (sent back for more work).
        self.assertEqual(keys(plan_for(sn, {"sessions": {}})), ["worker:1"])

    def test_todo_issue_with_only_a_draft_pr_resumes_on_it(self):
        sessions = {"worker:1": {"role": "issue-worker", "issue": 1, "handoff": {"state": "pr-ready"}}}
        plan = plan_for(snap([issue(1)], [pr(10, 1, draft=True)]), {"sessions": sessions})
        self.assertEqual(keys(plan), ["worker:1"])
        self.assertEqual(plan.jobs[0].pr, 10)
        self.assertIn("sent back for more work", plan.jobs[0].trigger)
        # A ready (non-draft) PR still waits for review instead.
        self.assertEqual(keys(plan_for(snap([issue(1)], [pr(10, 1, check="pending")]), {"sessions": sessions})), [])

    def test_reviews_off_routes_prs_without_a_reviewer(self):
        cfg = s.deep_merge(CFG, {"reviews": False})
        # A finished PR with green CI is armed by the scheduler, with no reviewer job.
        plan = plan_for(snap([issue(1, status="status:in-review")], [pr(10, 1)]), cfg=cfg)
        self.assertEqual(keys(plan), [])
        self.assertEqual(plan.arms, [(10, "h" * 40)])
        # ...but not while its worker is still running on it.
        running = {"running": {"x": {"key": "worker:1"}}}
        self.assertEqual(plan_for(snap([issue(1, status="status:in-review")], [pr(10, 1)]), running, cfg=cfg).arms, [])
        # A conflict goes to the cheap merge-fixer, armed or not.
        for auto in (False, True):
            plan = plan_for(snap([issue(1, status="status:in-review")], [pr(10, 1, mergeable="CONFLICTING", auto=auto)]),
                            cfg=cfg)
            self.assertEqual([(j.key, j.role) for j in plan.jobs], [("fixer:pr-10", "merge-fixer")])
        # A CI failure on an unarmed PR goes to its worker.
        plan = plan_for(snap([issue(1, status="status:in-review")], [pr(10, 1, check="failure")]), cfg=cfg)
        self.assertEqual([(j.key, j.role) for j in plan.jobs], [("worker:1", "issue-worker")])
        # A request for changes given before reviews were turned off still goes to the worker, unarmed.
        changed = pr(10, 1, reviews=[review("changes", "h" * 40)])
        plan = plan_for(snap([issue(1, status="status:in-review")], [changed]), cfg=cfg)
        self.assertEqual(([(j.key, j.role) for j in plan.jobs], plan.arms), ([("worker:1", "issue-worker")], []))
        # A rejected head is never armed.
        rejected = pr(10, 1, reviews=[review("reject", "h" * 40)])
        self.assertEqual(plan_for(snap([issue(1, status="status:in-review")], [rejected]), cfg=cfg).arms, [])
        # With reviews on, the same green PR gets a reviewer and is not armed.
        plan = plan_for(snap([issue(1, status="status:in-review")], [pr(10, 1)]), cfg=REVIEWED)
        self.assertEqual(keys(plan), ["reviewer:pr-10"])
        self.assertEqual(plan.arms, [])

    def test_renovate_prs_are_routed_only_when_enabled(self):
        bot = dict(author="infra-mk3-renovate-operator", branch="renovate/go-modules")
        # Off (the default): Renovate PRs are ignored entirely.
        plan = plan_for(snap([], [pr(20, None, check="failure", **bot)]))
        self.assertEqual((keys(plan), plan.arms), ([], []))
        cfg = s.deep_merge(CFG, {"renovate_prs": True})
        # Green: armed by the scheduler.
        self.assertEqual(plan_for(snap([], [pr(20, None, **bot)]), cfg=cfg).arms, [(20, "h" * 40)])
        # CI failure: a worker on the PR, with no issue.
        plan = plan_for(snap([], [pr(20, None, check="failure", **bot)]), cfg=cfg)
        self.assertEqual([(j.key, j.role, j.issue) for j in plan.jobs], [("worker:pr-20", "issue-worker", None)])
        # Conflict: the merge-fixer.
        plan = plan_for(snap([], [pr(20, None, mergeable="CONFLICTING", **bot)]), cfg=cfg)
        self.assertEqual(keys(plan), ["fixer:pr-20"])
        # Held, drafted, or armed and still running CI: left alone.
        self.assertEqual(keys(plan_for(snap([], [pr(20, None, check="failure", **bot)]), holds={"pr:20": "x"}, cfg=cfg)), [])
        self.assertEqual(keys(plan_for(snap([], [pr(20, None, check="failure", draft=True, **bot)]), cfg=cfg)), [])
        self.assertEqual(plan_for(snap([], [pr(20, None, auto=True, check="pending", **bot)]), cfg=cfg).arms, [])
        # A renovate/ branch from anyone else is never touched.
        other = pr(21, None, check="failure", author="someone", branch="renovate/x")
        self.assertEqual(keys(plan_for(snap([], [other]), cfg=cfg)), [])

    def test_a_running_special_job_is_not_launched_again(self):
        job = s.Job("special:audit", "security-auditor", 4, (0,), "r", "t", None, None, (), ())
        plan = s.Plan([job], [], [], [], [])
        state = {"running": {"20261030T0000Z-special-audit": {"key": "special:audit", "claims": []}}}
        chosen, queue = s.select_jobs(plan, state, CFG, 3, {}, NOW)
        self.assertEqual(chosen, [])
        self.assertEqual(queue[0][1], "running")

    def test_later_phase_work_off_starts_only_current_phase(self):
        cfg = s.deep_merge(CFG, {"later_phase_work": False})
        sessions = {"worker:4": {"role": "issue-worker", "issue": 4, "handoff": {"state": "question"}}}
        sn = snap([issue(1), issue(2, milestone=MS2), issue(3, milestone=MS2, status="status:in-progress"),
                   issue(4, milestone=MS2)])
        plan = plan_for(sn, {"sessions": sessions}, cfg=cfg)
        # Started later-phase work continues; a new later-phase issue does not start.
        self.assertEqual(sorted(keys(plan)), ["worker:1", "worker:3", "worker:4"])
        self.assertIn(("#2", "outside the current phase (later_phase_work off)"), plan.skipped)
        # On, a later-phase issue is filler work.
        self.assertIn("worker:2", keys(plan_for(sn, {"sessions": sessions}, cfg=s.deep_merge(CFG, {"later_phase_work": True}))))

    def test_time_box_note_gives_an_absolute_deadline(self):
        now = dt.datetime(2026, 10, 21, 18, 0, tzinfo=dt.timezone.utc)
        job = s.Job("worker:2", "issue-worker", 5, (0,), "r", "t", 2, None, ("issue:2",), ())
        self.assertIn("ends at 18:45 UTC", s.time_box_note(job, now))
        self.assertIn("ends at 19:30 UTC", s.time_box_note(job, now, "design(policy): interaction table"))
        self.assertIn("ends at 19:30 UTC", s.time_box_note(job, now, "design: phase 2"))
        self.assertIn("ends at 18:45 UTC", s.time_box_note(job, now, "fix(policy): design gap in rule order"))
        self.assertIn("ends at 18:45 UTC", s.time_box_note(job, now, "feat(ui): designer page"))

    def test_special_jobs_and_release_lock(self):
        specials = {"hunt-1": {"role": "bug-hunter", "brief": "policy under concurrent rule changes", "priority": 0},
                    "rel": {"role": "release-manager", "brief": "release 0.2.0", "priority": 1}}
        sessions = {"worker:2": {"role": "issue-worker", "issue": 2, "handoff": {"state": "incomplete"},
                                 "last_trigger": "Implement issue #2"}}
        plan = plan_for(snap([issue(1), issue(2, status="status:in-progress")]),
                        {"specials": specials, "sessions": sessions})
        self.assertEqual(keys(plan), ["worker:2", "special:hunt-1", "special:rel", "worker:1"])
        self.assertIn("Its trigger was: Implement issue #2", plan.jobs[0].trigger)
        self.assertEqual(plan.jobs[2].locks, ("release",))

    def test_review_rounds_give_up_and_planner_override(self):
        reviews = [review("changes", c * 40) for c in "abc"]
        sn = snap([issue(2, status="status:in-review")], [pr(10, 2, head="c" * 40, reviews=reviews)])
        plan = plan_for(sn)
        self.assertEqual(keys(plan), [])
        self.assertEqual([g[0] for g in plan.gave_up], ["worker:2"])
        record = {"kind": "rounds", "issue": 2, "pr": 10, "head": "c" * 40, "why": "x", "rounds": 3}
        blocked = snap([issue(2, status="status:blocked")], [pr(10, 2, head="c" * 40, reviews=reviews)])
        plan = plan_for(blocked, {"gave_up": {"worker:2": record}})
        self.assertEqual((keys(plan), plan.gave_up), ([], []))
        # The planner lifted status:blocked: the worker runs again.
        plan = plan_for(sn, {"gave_up": {"worker:2": record}})
        self.assertEqual((keys(plan), plan.cleared), (["worker:2"], ["worker:2"]))
        # ...and the acknowledged rounds no longer count.
        self.assertEqual(keys(plan_for(sn, {"rounds_ack": {"worker:2": 3}})), ["worker:2"])

    def test_light_review_hint(self):
        sn = snap([issue(2, status="status:in-review")], [pr(10, 2, files=["docs/a.md", "test/e2e/enable_test.go"])])
        self.assertIn("light review", plan_for(sn, cfg=REVIEWED).jobs[0].trigger)
        sn = snap([issue(2, status="status:in-review")], [pr(10, 2, files=["docs/a.md", "internal/x.go"])])
        self.assertNotIn("light review", plan_for(sn, cfg=REVIEWED).jobs[0].trigger)

    def test_pr_issue_mapping(self):
        self.assertEqual(s.pr_issue(pr(1, 611, branch="feat/policy-569")), 611)
        p = pr(1, None, branch="feat/policy-569")
        p.closing = [611, 569, 628]
        self.assertEqual(s.pr_issue(p), 569)
        p.closing = []
        self.assertEqual(s.pr_issue(p), 569)
        self.assertIsNone(s.pr_issue(pr(1, None, branch="docs/readme")))


# A title lock, as .pm/scheduler.json can configure one.
LOCKED = s.deep_merge(CFG, {"locks": {"migrations": {"title_regex": r"^feat\(db\):"}}})
LOCKED_REVIEWED = s.deep_merge(LOCKED, {"reviews": True})


class SelectTest(unittest.TestCase):
    def test_no_title_locks_by_default(self):
        self.assertEqual(set(CFG["locks"]), {"release"})

    def test_locks_one_holder(self):
        step = r"feat(db): migration {}"
        sn = snap([issue(1, step.format(5)), issue(2, step.format(6)), issue(3)])
        plan = plan_for(sn, cfg=LOCKED)
        chosen, queue = s.select_jobs(plan, {"running": {}}, LOCKED, 5, {}, NOW)
        self.assertEqual([j.key for j in chosen], ["worker:1", "worker:3"])
        self.assertEqual(dict((j.key, st) for j, st in queue)["worker:2"], "lock migrations held by #1")

    def test_lock_held_by_in_flight_issue(self):
        step = "feat(db): migration {}"
        sn = snap([issue(1, step.format(5), status="status:in-review"), issue(2, step.format(6))],
                  [pr(10, 1, check="success")])
        locks = s.in_flight_locks(sn, {"running": {}}, LOCKED)
        self.assertEqual(locks, {"migrations": "#1"})
        chosen, _ = s.select_jobs(plan_for(sn, cfg=LOCKED_REVIEWED), {"running": {}}, LOCKED_REVIEWED, 5, locks, NOW)
        # The holder's own PR is reviewed; the next locked step waits.
        self.assertEqual([j.key for j in chosen], ["reviewer:pr-10"])

    def test_new_work_cap_keeps_slots_for_reviews(self):
        sn = snap([issue(n) for n in range(1, 6)], [])
        state = {"running": {"a": {"key": "worker:9", "claims": [], "locks": [], "tier": 5},
                             "b": {"key": "reviewer:pr-8", "claims": [], "locks": [], "tier": 2}}}
        chosen, queue = s.select_jobs(plan_for(sn), state, CFG, 3, {}, NOW)
        status = dict((j.key, st) for j, st in queue)
        # One new-work job is running, so only two more fit under the cap of 3.
        self.assertEqual([j.key for j in chosen], ["worker:1", "worker:2"])
        self.assertEqual(status["worker:3"], "new-work cap (3 slots)")

    def test_titles_outside_the_lock_regex_do_not_take_it(self):
        # Only matching titles serialize; bug fixes run alongside them and rebase.
        sn = snap([issue(1, "fix(db): a", status="status:in-progress"), issue(2, "feat(db): migration 7")], [])
        self.assertEqual(s.in_flight_locks(sn, {"running": {}}, LOCKED), {})

    def test_claims_backoff_and_free_slots(self):
        sn = snap([issue(1), issue(2), issue(3), issue(4, status="status:in-review")], [pr(10, 4)])
        state = {"running": {"j": {"key": "worker:9", "claims": ["issue:4", "pr:10"], "locks": []}},
                 "failures": {"worker:2": {"count": 1, "next_allowed": s.iso(NOW + dt.timedelta(minutes=5))}}}
        chosen, queue = s.select_jobs(plan_for(sn, cfg=REVIEWED), state, REVIEWED, 1, {}, NOW)
        status = dict((j.key, st) for j, st in queue)
        self.assertEqual([j.key for j in chosen], ["worker:1"])
        self.assertEqual(status["reviewer:pr-10"], "claimed by a running job")
        self.assertTrue(status["worker:2"].startswith("backoff until"))
        self.assertEqual(status["worker:3"], "ready")


class SessionTest(unittest.TestCase):
    def test_decide_session(self):
        self.assertEqual(s.decide_session(None, 150000)[0], "fresh")
        self.assertEqual(s.decide_session({"session_id": "x"}, 150000)[0], "fresh")
        self.assertEqual(s.decide_session({"session_id": "x", "context_tokens": 149999}, 150000)[0], "resume")
        self.assertEqual(s.decide_session({"session_id": "x", "context_tokens": 150000}, 150000)[0], "fresh")

    def test_fresh_prompt_seeds_previous_report(self):
        job = s.Job("worker:5", "issue-worker", 3, (), "r", "Address the review", 5, 9)
        text = s.fresh_prompt(job, Path("/wt"), "note", {"last_report": "did A\nHANDOFF: pr-ready"}, "context 2 >= 1")
        self.assertIn("> did A", text)
        self.assertIn("Work only on issue #5", text)
        self.assertIn("issue #5 (PR #9)", text)


class ParseTest(unittest.TestCase):
    def test_limit_reset(self):
        cases = [
            ("You've hit your session limit · resets 3am (UTC)", NOW.replace(hour=3), True),
            ("You've hit your session limit · resets 10pm (UTC)", NOW.replace(hour=22), False),
            ("You've hit your session limit · resets 5:30am (UTC)", NOW.replace(hour=5, minute=30), False),
            ("You've hit your weekly limit · resets Oct 23, 9am (UTC)", NOW.replace(day=23, hour=9), False),
            ("You've hit your weekly limit · resets Mon 9am (UTC)", NOW.replace(day=26, hour=9), False),
        ]
        for text, want, next_day in cases:
            with self.subTest(text=text):
                if next_day:
                    want += dt.timedelta(days=1)
                self.assertEqual(s.parse_limit_reset(text, NOW), want)
        berlin = s.parse_limit_reset("resets 9am (Europe/Berlin)", NOW)
        self.assertEqual(berlin, NOW.replace(hour=7))
        self.assertIsNone(s.parse_limit_reset("You've hit your session limit", NOW))

    def test_handoff(self):
        self.assertEqual(s.parse_handoff("report\nHANDOFF: pr-ready"), {"state": "pr-ready", "pr": None})
        self.assertEqual(s.parse_handoff("x\nHANDOFF: wait-pr #123\n"), {"state": "wait-pr", "pr": 123})
        self.assertIsNone(s.parse_handoff("HANDOFF: maybe"))
        self.assertIsNone(s.parse_handoff(None))

    def test_classify_exit(self):
        limit = {"result": "You've hit your session limit · resets 3am (UTC)", "is_error": True,
                 "api_error": "usage_limit_reached"}
        self.assertEqual(s.classify_exit(1, limit, ""), "limit")
        self.assertEqual(s.classify_exit(0, {"result": "ok", "is_error": False}, ""), "ok")
        self.assertEqual(s.classify_exit(1, {"result": None}, "boom"), "failed")
        self.assertEqual(s.classify_exit(0, {"result": "x", "is_error": True}, ""), "failed")
        self.assertEqual(s.classify_exit(1, {}, "No conversation found with session ID: x"), "resume-missing")

    def test_parse_stream_context_from_last_main_turn(self):
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp) / "t.jsonl"
            events = [
                {"type": "system", "subtype": "init", "session_id": "S"},
                {"type": "assistant", "session_id": "S", "parent_tool_use_id": None, "message": {"usage": {
                    "input_tokens": 1, "cache_read_input_tokens": 100, "cache_creation_input_tokens": 10,
                    "output_tokens": 5}}},
                {"type": "assistant", "session_id": "S", "parent_tool_use_id": "sub", "message": {"usage": {
                    "input_tokens": 1, "cache_read_input_tokens": 99999}}},
                {"type": "result", "session_id": "S", "result": "done", "is_error": False, "total_cost_usd": 0.5,
                 "num_turns": 2, "usage": {"input_tokens": 2}},
            ]
            path.write_text("\n".join(json.dumps(e) for e in events) + "\nnot json\n")
            info = s.parse_stream(path)
            self.assertEqual((info["session_id"], info["context_tokens"], info["cost_usd"], info["result"]),
                             ("S", 116, 0.5, "done"))


# --------------------------------------------------------------------------
# Scheduler with a fake claude and a throwaway repository

FAKE_CLAUDE = textwrap.dedent("""\
    #!/usr/bin/env python3
    # Fake claude: appends its argv to $FAKE_CLAUDE_CALLS and behaves per
    # $FAKE_CLAUDE_MODE: ok | limit | crash | sleep.
    import json, os, sys, time
    args = sys.argv[1:]
    with open(os.environ["FAKE_CLAUDE_CALLS"], "a") as f:
        f.write(json.dumps({"argv": args, "cwd": os.getcwd()}) + "\\n")
    mode = os.environ.get("FAKE_CLAUDE_MODE", "ok")
    sid = args[args.index("--resume") + 1] if "--resume" in args else args[args.index("--session-id") + 1]
    def emit(ev):
        ev.setdefault("session_id", sid)
        print(json.dumps(ev), flush=True)
    if mode == "crash":
        print("boom", file=sys.stderr)
        sys.exit(1)
    if mode == "sleep":
        time.sleep(float(os.environ.get("FAKE_CLAUDE_SLEEP", "30")))
    emit({"type": "system", "subtype": "init"})
    if mode == "limit":
        emit({"type": "result", "subtype": "success", "is_error": True, "api_error": "usage_limit_reached",
              "result": "You've hit your session limit · resets 3am (UTC)", "total_cost_usd": 0})
        sys.exit(1)
    ctx = int(os.environ.get("FAKE_CLAUDE_CONTEXT", "1000"))
    cost = 0.5 if "--resume" in args else 0.25  # cumulative over the session
    emit({"type": "assistant", "parent_tool_use_id": None,
          "message": {"usage": {"input_tokens": 10, "cache_read_input_tokens": ctx - 10, "output_tokens": 0}}})
    emit({"type": "result", "subtype": "success", "is_error": False, "total_cost_usd": cost, "num_turns": 1,
          "result": os.environ.get("FAKE_CLAUDE_RESULT", "Did the thing.\\nHANDOFF: pr-ready"),
          "usage": {"input_tokens": 10}})
""")


class FakeGitHub:
    def __init__(self, snapshot):
        self.snap = snapshot
        self.writes = []
        self.comments = []  # what GitHub.pm_comments returns: /pm comments by the human
        self.comment_queries = []

    def snapshot(self):
        return self.snap

    def pm_comments(self, since):
        self.comment_queries.append(since)
        return [c for c in self.comments if c["updated_at"] >= since]

    def rerun_failed(self, run_id):
        self.writes.append(("rerun", run_id))

    def set_status(self, issue, status, remove):
        self.writes.append(("status", issue, status))

    def comment(self, number, body):
        self.writes.append(("comment", number, body))


def git(*args, cwd):
    subprocess.run(["git", *args], cwd=cwd, check=True, capture_output=True, text=True)


class SchedulerTest(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        root = Path(self.tmp.name)
        self.repo = root / "repo"
        git("init", "-q", "--bare", "-b", "master", str(root / "origin.git"), cwd=root)
        git("clone", "-q", str(root / "origin.git"), str(self.repo), cwd=root)
        for args in (("config", "user.email", "t@example.com"), ("config", "user.name", "t")):
            git(*args, cwd=self.repo)
        (self.repo / ".claude" / "agents").mkdir(parents=True)
        (self.repo / ".claude" / "agents" / "issue-worker.md").write_text(
            "---\nname: issue-worker\ndescription: Works one issue.\n---\n\nYou implement one issue.\n")
        (self.repo / ".gitignore").write_text(".pm/\n.claude/worktrees/\n")
        git("add", ".gitignore", ".claude/agents/issue-worker.md", cwd=self.repo)
        git("commit", "-q", "-m", "init", cwd=self.repo)
        git("push", "-q", "origin", "HEAD:master", cwd=self.repo)
        git("switch", "-q", "-c", "feat/x-2", cwd=self.repo)
        git("commit", "-q", "--allow-empty", "-m", "pr", cwd=self.repo)
        git("push", "-q", "origin", "feat/x-2", cwd=self.repo)
        git("switch", "-q", "master", cwd=self.repo)
        git("branch", "-q", "-D", "feat/x-2", cwd=self.repo)
        fake = root / "claude"
        fake.write_text(FAKE_CLAUDE)
        fake.chmod(0o755)
        self.calls = root / "calls.jsonl"
        self.env = {"FAKE_CLAUDE_CALLS": str(self.calls), "FAKE_CLAUDE_MODE": "ok"}
        self.old_env = {k: os.environ.get(k) for k in list(self.env) + ["FAKE_CLAUDE_CONTEXT", "FAKE_CLAUDE_RESULT", "FAKE_CLAUDE_SLEEP"]}
        os.environ.update(self.env)
        self.gh = FakeGitHub(snap([issue(1)]))
        self.sched = s.Scheduler(self.repo, github=self.gh, claude_bin=str(fake), log_dir=root / "logs",
                                 out=io.StringIO())
        self.sched.housekeeping = lambda: None

    def tearDown(self):
        for k, v in self.old_env.items():
            if v is None:
                os.environ.pop(k, None)
            else:
                os.environ[k] = v
        for proc in self.sched.procs.values():
            proc.kill()
            proc.wait()
        self.tmp.cleanup()

    def calls_made(self):
        if not self.calls.exists():
            return []
        return [json.loads(line) for line in self.calls.read_text().splitlines()]

    def wait_idle(self):
        deadline = time.time() + 20
        while self.sched.state["running"] and time.time() < deadline:
            time.sleep(0.05)
            self.sched.reap()
        self.assertEqual(self.sched.state["running"], {})

    def events(self, kind):
        log = self.sched.event_log
        return [e for e in map(json.loads, log.read_text().splitlines()) if e["event"] == kind] if log.exists() else []

    def disable_planner(self):
        # Pending reasons launch the planner whatever its last start was, so also hold it in
        # failure backoff: a planner that ran and exited between two polls would clear them.
        self.sched.state["planner"]["last_start"] = s.iso(s.now_utc())
        self.sched.state["failures"]["planner"] = {"next_allowed": "9999-01-01T00:00:00Z"}

    def test_launch_records_session_then_resumes_in_same_worktree(self):
        self.disable_planner()
        self.sched.poll()
        self.wait_idle()
        call = self.calls_made()[0]
        argv = call["argv"]
        self.assertEqual(argv[argv.index("--agent") + 1], "issue-worker")
        self.assertIn("--session-id", argv)
        self.assertNotIn("--resume", argv)
        self.assertEqual(Path(call["cwd"]).name, "issue-1")
        self.assertIn(("status", 1, "status:in-progress"), self.gh.writes)
        agents = json.loads(Path(argv[argv.index("--agents") + 1]).read_text())
        self.assertEqual(agents["issue-worker"]["prompt"], "You implement one issue.")
        sess = self.sched.state["sessions"]["worker:1"]
        first_sid = argv[argv.index("--session-id") + 1]
        self.assertEqual((sess["session_id"], sess["context_tokens"], sess["handoff"]["state"]),
                         (first_sid, 1000, "pr-ready"))
        self.assertTrue(json.loads(self.sched.state_path.read_text())["sessions"]["worker:1"])

        # Review requested changes: the same session resumes in the same worktree.
        self.gh.snap = snap([issue(1, status="status:in-review")],
                            [pr(10, 1, head="a" * 40, reviews=[review("changes", "a" * 40)])])
        self.sched.poll()
        self.wait_idle()
        call = self.calls_made()[1]
        self.assertEqual(call["argv"][call["argv"].index("--resume") + 1], first_sid)
        self.assertEqual(Path(call["cwd"]).name, "issue-1")
        self.assertIn("requested changes", call["argv"][call["argv"].index("-p") + 1])
        self.assertEqual([e["event"] for e in map(json.loads, self.sched.event_log.read_text().splitlines())
                          if e["event"] in ("fresh", "resume")], ["fresh", "resume"])
        # The fake reports a cumulative session cost like claude does.
        self.assertEqual([e["cost_usd"] for e in self.events("exit")], [0.25, 0.25])
        self.assertEqual(self.sched.state["sessions"]["worker:1"]["cost_usd"], 0.5)

    def test_large_context_starts_fresh_seeded_with_report(self):
        self.disable_planner()
        os.environ["FAKE_CLAUDE_CONTEXT"] = "200000"
        os.environ["FAKE_CLAUDE_RESULT"] = "Implemented X; PR #10.\nHANDOFF: pr-ready"
        self.sched.poll()
        self.wait_idle()
        self.gh.snap = snap([issue(1, status="status:in-review")],
                            [pr(10, 1, head="a" * 40, reviews=[review("changes", "a" * 40)])])
        self.sched.poll()
        self.wait_idle()
        argv = self.calls_made()[1]["argv"]
        self.assertNotIn("--resume", argv)
        self.assertIn("> Implemented X; PR #10.", argv[argv.index("-p") + 1])
        self.assertIn("200000 >= 150000", self.events("fresh")[-1]["why"])

    def test_worker_on_existing_pr_branch(self):
        self.disable_planner()
        self.gh.snap = snap([issue(2, status="status:in-review")],
                            [pr(10, 2, head="a" * 40, reviews=[review("changes", "a" * 40)])])
        self.sched.poll()
        self.wait_idle()
        wt = self.repo / ".claude" / "worktrees" / "issue-2"
        head = subprocess.run(["git", "symbolic-ref", "--short", "HEAD"], cwd=wt, capture_output=True, text=True)
        self.assertEqual(head.stdout.strip(), "feat/x-2")

    def test_usage_limit_stops_launches_and_keeps_session(self):
        self.disable_planner()
        self.sched.poll()
        self.wait_idle()  # first run ok: session has context
        os.environ["FAKE_CLAUDE_MODE"] = "limit"
        self.gh.snap = snap([issue(1, status="status:in-review")],
                            [pr(10, 1, head="a" * 40, reviews=[review("changes", "a" * 40)])])
        self.sched.poll()
        self.wait_idle()
        until = s.parse_iso(self.sched.state["limit_until"])
        self.assertGreater(until, s.now_utc())
        self.assertEqual(until.minute, 2)  # 3am + the 2-minute margin
        sess = self.sched.state["sessions"]["worker:1"]
        self.assertEqual(sess["interrupted"], "usage limit")
        self.assertTrue(sess["session_id"])
        n = len(self.calls_made())
        self.sched.poll()
        self.assertEqual(len(self.calls_made()), n)
        self.assertIn("usage limit", self.sched.state["blocked_by"])
        # After the reset the interrupted session resumes with a note.
        os.environ["FAKE_CLAUDE_MODE"] = "ok"
        self.sched.state["limit_until"] = s.iso(s.now_utc() - dt.timedelta(seconds=1))
        self.sched.poll()
        self.wait_idle()
        argv = self.calls_made()[-1]["argv"]
        self.assertIn("--resume", argv)
        self.assertIn("interrupted (usage limit)", argv[argv.index("-p") + 1])
        self.assertNotIn("interrupted", self.sched.state["sessions"]["worker:1"])

    def test_three_failures_back_off_then_block_the_issue(self):
        self.disable_planner()
        os.environ["FAKE_CLAUDE_MODE"] = "crash"
        for attempt in range(1, 4):
            self.sched.state["failures"].get("worker:1", {}).pop("next_allowed", None)
            self.sched.poll()
            self.wait_idle()
            self.assertEqual(self.sched.state["failures"]["worker:1"]["count"], attempt)
            if attempt == 1:
                n = len(self.calls_made())
                self.sched.poll()  # backing off: no relaunch
                self.assertEqual(len(self.calls_made()), n)
        self.assertIn(("status", 1, "status:blocked"), self.gh.writes)
        self.assertTrue(any(w[0] == "comment" and w[1] == 1 for w in self.gh.writes))
        self.assertIn("worker:1", self.sched.state["gave_up"])
        self.assertTrue(any("gave up on worker:1" in r for r in self.sched.state["planner"]["pending"]))

    def test_reconcile_adopts_live_jobs_and_finishes_dead_ones(self):
        marker = "sched-20261021T000000Z-worker-7"
        proc = subprocess.Popen([sys.executable, "-c", "import time; time.sleep(30)", marker],
                                start_new_session=True)
        try:
            log = Path(self.tmp.name) / "dead.jsonl"
            log.write_text(json.dumps({"type": "result", "result": "x\nHANDOFF: done", "is_error": False,
                                       "session_id": "D"}) + "\n")
            base = {"role": "issue-worker", "started": s.iso(s.now_utc()), "mode": "fresh", "claims": [],
                    "locks": [], "trigger": "t"}
            self.sched.state["running"] = {
                "live": dict(base, key="worker:7", marker=marker, pid=proc.pid, issue=7, log=str(log)),
                "launching": dict(base, key="worker:7b", marker=marker, pid=None, issue=7, log=str(log)),
                "dead": dict(base, key="worker:8", marker="sched-gone", pid=proc.pid, issue=8, log=str(log)),
            }
            self.sched.reconcile()
            running = self.sched.state["running"]
            self.assertEqual(sorted(running), ["launching", "live"])
            self.assertEqual(running["launching"]["pid"], proc.pid)  # found by its marker
            self.assertEqual(self.sched.state["sessions"]["worker:8"]["handoff"]["state"], "done")
            # A live job is never relaunched: its claims hold.
            running["live"]["claims"] = ["issue:7"]
            self.gh.snap = snap([issue(7, status="status:in-progress")])
            self.disable_planner()
            self.sched.poll()
            self.assertEqual(self.calls_made(), [])
        finally:
            proc.kill()
            proc.wait()

    def test_cleanup_waits_for_running_job_and_complete_snapshot(self):
        self.disable_planner()
        self.sched.poll()
        self.wait_idle()
        wt = self.repo / ".claude" / "worktrees" / "issue-1"
        self.assertTrue(wt.is_dir())
        # Snapshot without issue 1 but marked incomplete: nothing is collected.
        partial = snap([])
        partial.complete = False
        self.sched.cleanup_closed(partial)
        self.assertTrue(wt.is_dir())
        # A job still running on it: kept.
        self.sched.state["running"]["x"] = {"key": "worker:1", "worktree": str(wt)}
        self.sched.cleanup_closed(snap([]))
        self.assertTrue(wt.is_dir())
        del self.sched.state["running"]["x"]
        # Closed (e.g. while the scheduler was down): worktree and session go.
        self.sched.cleanup_closed(snap([]))
        self.assertFalse(wt.exists())
        self.assertNotIn("worker:1", self.sched.state["sessions"])

    def test_gocache_trim_runs_beside_jobs_without_blocking(self):
        sched = self.sched
        del sched.housekeeping  # use the real one, with its side effects stubbed
        cmds = []
        sched.sync_main_checkout = lambda: None
        sched.refresh_env_file = lambda: None
        sched.remove_orphan_stacks = lambda: None
        sched.gocache_size = lambda: 50 * 1024 ** 3
        sched.sh = lambda cmd, timeout=600: cmds.append(cmd) or subprocess.CompletedProcess(cmd, 0, "", "")
        sched.state["running"]["x"] = {"key": "worker:9", "claims": [], "locks": [], "marker": "m", "pid": None,
                                       "log": "/nonexistent", "started": s.iso(s.now_utc())}
        sched.state["maintenance"] = "gocache"  # left by the old blocking clear
        sched.housekeeping()
        self.assertTrue(any("-mmin +360 -delete" in c for c in cmds))
        self.assertFalse(any("go clean" in c for c in cmds))
        self.assertIsNone(sched.state["maintenance"])

    def test_pause_and_stop_files(self):
        self.disable_planner()
        (self.repo / ".pm").mkdir(exist_ok=True)
        (self.repo / ".pm" / "PAUSE").touch()
        self.sched.poll()
        self.assertEqual(self.calls_made(), [])
        (self.repo / ".pm" / "PAUSE").unlink()
        (self.repo / ".pm" / "STOP").touch()
        self.assertEqual(self.sched.run(), 0)  # nothing running: exits
        self.assertEqual(self.calls_made(), [])

    def test_queue_files_become_special_jobs(self):
        self.disable_planner()
        qdir = self.repo / ".pm" / "queue"
        qdir.mkdir(parents=True)
        (qdir / "hunt-policy.json").write_text(json.dumps({"role": "bug-hunter", "brief": "Focus: policy under concurrent rule changes"}))
        (qdir / "bad.json").write_text(json.dumps({"role": "issue-worker", "brief": "x"}))
        self.gh.snap = snap([])
        self.sched.poll()
        self.wait_idle()
        self.assertTrue((qdir / "bad.invalid").exists())
        self.assertFalse((qdir / "hunt-policy.json").exists())
        argv = self.calls_made()[0]["argv"]
        self.assertEqual(argv[argv.index("--agent") + 1], "bug-hunter")
        self.assertIn("Focus: policy under concurrent rule changes", argv[argv.index("-p") + 1])
        # HANDOFF pr-ready (not incomplete/wait-pr): the special is done.
        self.assertEqual(self.sched.state["specials"], {})

    def test_planner_runs_on_triggers_in_reserved_slot(self):
        self.sched.cfg["max_new_work_slots"] = 5  # fill every slot with new work
        self.gh.snap = snap([issue(n) for n in range(1, 8)] + [issue(50, status="status:triage")])
        self.sched.poll()
        self.wait_idle()
        roles = [c["argv"][c["argv"].index("--agent") + 1] if "--agent" in c["argv"] else "planner"
                 for c in self.calls_made()]
        self.assertEqual(roles.count("issue-worker"), 5)
        self.assertEqual(roles.count("planner"), 1)
        planner = [c for c in self.calls_made() if "--agent" not in c["argv"]][0]
        prompt = planner["argv"][planner["argv"].index("-p") + 1]
        self.assertIn("triage #50", prompt)
        self.assertEqual(Path(planner["cwd"]), self.repo)
        self.assertEqual(self.sched.state["planner"]["pending"], [])
        # Seen triage issues do not re-trigger it.
        self.sched.poll()
        self.assertEqual(len([c for c in self.calls_made() if "--agent" not in c["argv"]]), 1)

    def pm_comment(self, cid, updated_at):
        return {"id": cid, "url": f"https://github.com/o/r/issues/51#issuecomment-{cid}",
                "updated_at": updated_at, "line": f"/pm do thing {cid}"}

    def test_pm_comment_while_planner_idle_becomes_a_reason(self):
        self.disable_planner()
        self.gh.comments = [self.pm_comment(7, "2099-01-01T00:00:00Z")]
        self.sched.poll()
        p = self.sched.state["planner"]
        reason = "/pm instruction https://github.com/o/r/issues/51#issuecomment-7: /pm do thing 7"
        self.assertIn(reason, p["pending"])
        self.assertEqual(p["seen_comments"], [7])
        self.assertEqual(p["comments_since"], "2099-01-01T00:00:00Z")
        self.assertEqual(json.loads(self.sched.state_path.read_text())["planner"]["pending"], p["pending"])
        # The next poll asks from the advanced cursor and does not repeat the reason.
        self.sched.poll()
        self.assertEqual(self.gh.comment_queries[-1], "2099-01-01T00:00:00Z")
        self.assertEqual(p["pending"].count(reason), 1)
        self.wait_idle()

    def test_pm_comment_during_planner_run_survives_its_exit(self):
        os.environ["FAKE_CLAUDE_MODE"] = "sleep"
        os.environ["FAKE_CLAUDE_SLEEP"] = "3"  # outlives the second poll
        os.environ["FAKE_CLAUDE_RESULT"] = "Handled.\nPLANNER-IDLE"
        self.gh.snap = snap([issue(50, status="status:triage")])
        self.sched.poll()
        running = [r for r in self.sched.state["running"].values() if r["key"] == "planner"]
        self.assertEqual(len(running), 1)
        self.gh.comments = [self.pm_comment(8, "2099-01-01T00:00:00Z")]
        self.sched.poll()  # planner still running: the reason is only recorded
        reason = "/pm instruction https://github.com/o/r/issues/51#issuecomment-8: /pm do thing 8"
        self.assertIn(reason, self.sched.state["planner"]["pending"])
        self.wait_idle()
        self.assertEqual(self.sched.state["planner"]["last_outcome"], "ok")
        self.assertEqual(self.sched.state["planner"]["pending"], [reason])
        os.environ["FAKE_CLAUDE_MODE"] = "ok"
        self.sched.poll()
        self.wait_idle()
        planners = [c for c in self.calls_made() if "--agent" not in c["argv"]]
        self.assertEqual(len(planners), 2)
        prompt = planners[1]["argv"][planners[1]["argv"].index("-p") + 1]
        self.assertIn(reason, prompt)
        self.assertNotIn("triage #50", prompt)
        self.assertEqual(self.sched.state["planner"]["pending"], [])


class GitHubPmCommentsTest(unittest.TestCase):
    def comment(self, cid, login, body, updated_at):
        return {"id": cid, "user": {"login": login}, "body": body,
                "html_url": f"https://github.com/o/r/issues/1#issuecomment-{cid}", "updated_at": updated_at}

    def test_paginates_and_encodes_since(self):
        gh = s.GitHub(CFG)
        human = CFG["human"]
        pages = [[self.comment(1, "bot", "/pm no", "2026-10-28T09:01:00Z")] * 100,
                 [self.comment(2, human, "  /pm go\nmore", "2026-10-28T09:27:29Z"),
                  self.comment(3, human, "not pm", "2026-10-28T09:30:00Z")]]
        calls = []

        def fake_gh(*args, **kwargs):
            calls.append(args)
            return json.dumps(pages)
        gh._gh = fake_gh
        found = gh.pm_comments("2026-10-28T09:00:00+00:00")
        self.assertEqual([c["id"] for c in found], [2])
        self.assertEqual(found[0]["line"], "/pm go")
        args = calls[0]
        self.assertIn("--paginate", args)
        url = next(a for a in args if a.startswith("repos/"))
        self.assertIn("since=2026-10-28T09%3A00%3A00Z", url)
        self.assertNotIn("+", url)


if __name__ == "__main__":
    unittest.main(verbosity=1)
