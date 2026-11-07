# How toolgate gets the Claude Code session ID

For: developers who work on the `toolgate` client commands (`toolgate headers` and `toolgate hook session-start`).

## The answer

Claude Code does not give the session ID to a `headersHelper`. The `CLAUDE_CODE_SESSION_ID` variable is not in the environment of the helper. Claude Code gives the session ID to a SessionStart hook, on standard input and in `CLAUDE_CODE_SESSION_ID`.

For this reason, `toolgate headers` uses the fallback in the "Client: CLI and Claude Code plugin" section of [`SPEC.md`](../../SPEC.md):

1. The SessionStart hook (`toolgate hook session-start`) writes the session ID to the file `$XDG_RUNTIME_DIR/toolgate/<claude pid>`.
2. `toolgate headers` walks up its parent processes, finds the Claude Code process, and reads the file for that process ID.

## The test

We ran the test on 2026-11-05 with Claude Code 2.1.318 on the agent VM. The setup was:

- An HTTP MCP endpoint on `127.0.0.1` that logged the headers of each request.
- A `headersHelper` shell script. It wrote its full environment and its parent process ID to a log. It returned the header `X-Spike-Session: ${CLAUDE_CODE_SESSION_ID:-MISSING}`.
- A SessionStart hook script. It wrote its standard input, its `CLAUDE*` variables, its parent process ID and timestamps to a log.

The command was:

```
env -i HOME="$HOME" PATH="$PATH" USER="$USER" \
  claude -p --dangerously-skip-permissions --settings settings.json \
  --strict-mcp-config --mcp-config mcp.json --output-format json "Call the echo tool."
```

`env -i` removes all inherited variables. Without it, the helper received the `CLAUDE_CODE_SESSION_ID` of the outer agent session that started the command. That value was the ID of a different session. So a test must start Claude Code with a clean environment.

The nested `claude -p` was not signed in, so it made no model call. This does not change the result. Claude Code connects to the MCP servers and runs the helper before the first model call. The endpoint received `initialize`, `notifications/initialized` and `tools/list`.

## The evidence

The JSON output of `claude -p` reported the session ID `a7ed7512-c8ff-4c51-b231-a2590cb31683`. The transcript file was `~/.claude/projects/-tmp-spike5/a7ed7512-c8ff-4c51-b231-a2590cb31683.jsonl`.

The environment of the `headersHelper` had these `CLAUDE*` variables only:

```
CLAUDE_CODE_ENTRYPOINT=sdk-cli
CLAUDE_CODE_MCP_SERVER_NAME=spike
CLAUDE_CODE_MCP_SERVER_URL=http://127.0.0.1:18765/mcp
CLAUDE_CODE_MESSAGING_SOCKET=/tmp/cc-socks/296970.sock
CLAUDE_CODE_MESSAGING_TOKEN=(removed)
```

`CLAUDE_CODE_SESSION_ID` and `CLAUDE_PID` were not set. All requests carried `X-Spike-Session: MISSING`.

The SessionStart hook received this on standard input:

```
{"session_id":"a7ed7512-c8ff-4c51-b231-a2590cb31683","transcript_path":"/home/coder/.claude/projects/-tmp-spike5/a7ed7512-c8ff-4c51-b231-a2590cb31683.jsonl","cwd":"/tmp/spike5","hook_event_name":"SessionStart","source":"startup"}
```

The hook also had `CLAUDE_CODE_SESSION_ID=a7ed7512-c8ff-4c51-b231-a2590cb31683` and `CLAUDE_PID=296970`. The session ID in the hook is equal to the session ID in the transcript.

The parent process of the helper and the parent process of the hook were the same process, 296970. That process is the Claude Code process: it is the value of `CLAUDE_PID` in the hook, and it is the number in the name of the messaging socket. So `toolgate headers` finds the Claude Code process as its direct parent when Claude Code starts it without a shell in between.

## Order of the hook and the helper

Claude Code does not wait for the SessionStart hook to finish before it runs the `headersHelper`. In a second run, the hook slept for 3 seconds. The helper started 4 milliseconds after the hook started, and 3 seconds before the hook finished.

Claude Code ran the helper one time for the connection. It sent the same headers on all four requests.

Thus `toolgate headers` cannot read the file at once. The file is usually not there yet. `toolgate headers` must wait for the file, up to a limit. The `toolgate hook session-start` command must write the file first, before it does slower work such as the network calls for sign-in and settings.

## What the test did not cover

- Interactive Claude Code. The test used `claude -p` only.
- A new session ID during a run, for example after `/clear` or `--resume`. The hook gets `source` `clear` or `resume` in that case. The test did not check if Claude Code runs the helper again.
- macOS and Windows. The test ran on Linux only.
