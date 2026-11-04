#!/usr/bin/env bash
# Runs the agent scheduler (scripts/agents/scheduler.py) and restarts it when
# it exits: at once when it asks to (exit 75: scheduler.py changed on disk,
# e.g. master was fast-forwarded), after 30 s when it crashed. Agents run in
# their own process sessions, so a scheduler restart does not touch them; the
# new scheduler re-adopts them from .pm/scheduler/state.json. Start it
# in the `pm` tmux session from `coder ssh` (agents need
# that session's CODER_AGENT_*, GIT_ASKPASS and Claude credentials).
#
# Control files (in .pm/):
#   PAUSE - launch nothing new; running jobs finish.
#   STOP  - launch nothing new; the scheduler exits once no job runs, and
#           this loop exits with it.
#
# Logs: $PM_LOG_DIR (default ~/.pm-logs): scheduler.log (JSON lines),
# jobs/*.jsonl (one stream-json transcript per job), scheduler-loop.log.
set -uo pipefail

REPO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
LOG_DIR="${PM_LOG_DIR:-$HOME/.pm-logs}"
ENV_FILE="$REPO_DIR/.pm/devenv.sh"
mkdir -p "$LOG_DIR" "$REPO_DIR/.pm"
cd "$REPO_DIR" || exit 1

# ~/.local/bin first so the coder gh wrapper (fresh GitHub token per call)
# wins, for the scheduler's own gh calls and the agents it starts.
export PATH="$HOME/.local/bin:$PATH"
export PM_LOG_DIR="$LOG_DIR"

log() { echo "$(date -Is) $*" | tee -a "$LOG_DIR/scheduler-loop.log"; }

# python3 lives only in the dev shell. Take the interpreter from it without
# loading the whole dev shell into the scheduler (agents get the dev shell
# through CLAUDE_ENV_FILE, which the scheduler keeps fresh).
python_bin() {
  if [ ! -s "$ENV_FILE" ]; then
    # The PATH line is written literally, to expand when agents source it.
    # shellcheck disable=SC2016
    nix print-dev-env "$REPO_DIR" >"$ENV_FILE.tmp" 2>>"$LOG_DIR/devenv.stderr" &&
      printf 'export PATH="$HOME/.local/bin:$PATH"\n' >>"$ENV_FILE.tmp" &&
      mv "$ENV_FILE.tmp" "$ENV_FILE"
  fi
  # shellcheck disable=SC1090
  (source "$ENV_FILE" >/dev/null 2>&1 && command -v python3)
}

while true; do
  if [ -e .pm/STOP ]; then
    log "STOP file present; exiting"
    exit 0
  fi
  py="$(python_bin)"
  if [ -z "$py" ]; then
    log "no python3 from the dev shell ($ENV_FILE); retrying in 60 s"
    sleep 60
    continue
  fi
  log "starting scheduler ($py)"
  "$py" scripts/agents/scheduler.py run "$@"
  rc=$?
  case "$rc" in
    0) log "scheduler exited (rc 0)"; [ -e .pm/STOP ] && exit 0; sleep 5 ;;
    75) log "scheduler.py changed; restarting" ;;
    130 | 143) log "scheduler stopped by signal (rc $rc); exiting (running agents continue)"; exit 0 ;;
    *) log "scheduler crashed (rc $rc); restarting in 30 s"; sleep 30 ;;
  esac
done
