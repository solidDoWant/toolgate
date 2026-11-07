# The OTLP fixture

For: people who change the telemetry ingest or its tests.

`test/fixtures/otlp/` holds the OTLP data of two real Claude Code sessions. The telemetry tests replay it. It was not written by hand.

## How it was recorded

- Claude Code 2.1.318, started with `claude -p` and the model `sonnet`.
- The prompt asked it to run `echo toolgate-fixture` with the Bash tool and to reply with the output. The session made one built-in tool call. It made no toolgate tool call, because the ingest, content removal and usage paths are the same for every tool. The link from a Claude Code tool span to the gateway's spans is checked by hand.
- The settings were the `SettingsProfile` of [Telemetry](../admin/telemetry.md), with `OTEL_EXPORTER_OTLP_PROTOCOL=http/json` so that the files can be read as text, and a 2 s export interval. A local OTLP sink wrote each request body to a file, in arrival order.
- `content-on/` has `OTEL_LOG_USER_PROMPTS`, `OTEL_LOG_TOOL_DETAILS` and `OTEL_LOG_TOOL_CONTENT` set to `1`. `content-off/` has none of them.
- The only edit: the `user.id` value, a hash of the account, is zeros. The files have no email, account ID, organization ID or local path.

The two metric files (`001` and `004`) are cumulative points of the same series. The tests rely on that: the growth between them is what the usage queries must report.

## The content attribute list

The list of removed attributes in the ingest comes from `content-on/` compared with `content-off/`, and from the monitoring page of Claude Code. In this version the recording with the variables on adds these attributes:

- `claude_code.user_prompt` log records: `prompt`, `prompt_text` (the off recording has them too, with the text redacted);
- `claude_code.tool_decision` records: `tool_parameters`;
- `claude_code.tool_result` records: `tool_input`, `tool_parameters`;
- the `claude_code.tool` span: `full_command`, and a `tool.output` event with `bash_command` and `output`.

All are in the removed list. `response` is on `claude_code.assistant_response` in both recordings and is in the list as well.

To record again, repeat these steps with the new Claude Code version, replace the files, and check the list again.
