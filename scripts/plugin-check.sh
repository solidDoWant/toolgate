#!/usr/bin/env bash
# Checks the Claude Code plugin and its marketplace (`make plugin-check`):
# `claude plugin validate --strict` on both manifests, then an install from
# this repository's marketplace into a throwaway Claude Code config directory,
# and checks that the installed plugin asks for the gateway URL and configures
# the toolgate MCP server, the SessionStart hook and the skill.
# Needs the `claude` CLI on PATH (CI installs a pinned version).
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"

if ! command -v claude >/dev/null; then
	echo "plugin-check: the claude CLI is not on PATH" >&2
	exit 1
fi

# A throwaway Claude Code config directory: no sign-in, no other plugins.
config_dir="$(mktemp -d)"
trap 'rm -rf "$config_dir"' EXIT
export CLAUDE_CONFIG_DIR="$config_dir"

failed=""
fail() {
	echo "plugin-check: $*" >&2
	failed="$failed
  $*"
}

claude plugin validate --strict . || fail "marketplace manifest is not valid"
claude plugin validate --strict plugin || fail "plugin manifest is not valid"

plugin="toolgate@toolgate"

claude plugin marketplace add ./
claude plugin install "$plugin"

# Before configuration, the plugin asks for the gateway URL, and requires it.
options="$(claude plugin configure "$plugin" --json)"
unconfigured="$(jq -r '.unconfigured | join(",")' <<<"$options")"
[[ "$unconfigured" == "gateway_url" ]] ||
	fail "before configuration, unset options are '$unconfigured', want 'gateway_url'"
[[ "$(jq -r '.schema.gateway_url.required' <<<"$options")" == "true" ]] ||
	fail "the gateway_url option is not required"

gateway="https://gateway.invalid"
jq -n --arg url "$gateway" '{gateway_url: $url}' | claude plugin configure "$plugin" --values-stdin

install_path="$(jq -r --arg id "$plugin" '.plugins[$id][0].installPath' "$config_dir/plugins/installed_plugins.json")"

# The MCP server: HTTP, at the configured URL, with the headers helper.
server="$(jq -c '.mcpServers.toolgate' "$install_path/.mcp.json")"
[[ "$(jq -r '.type' <<<"$server")" == "http" ]] || fail "MCP server toolgate is not type http: $server"
[[ "$(jq -r '.headersHelper' <<<"$server")" == "toolgate headers" ]] ||
	fail "MCP server toolgate does not use headersHelper 'toolgate headers': $server"
[[ "$(jq -r '.mcpServers | keys | join(",")' "$install_path/.mcp.json")" == "toolgate" ]] ||
	fail "the plugin must declare exactly one MCP server, named toolgate"
mcp_get="$(timeout 60 claude mcp get plugin:toolgate:toolgate 2>&1 || true)"
grep -qF "URL: $gateway/mcp" <<<"$mcp_get" ||
	fail "Claude Code does not resolve the MCP server URL to $gateway/mcp: $mcp_get"

# The SessionStart hook.
hook_commands="$(jq -r '[.hooks.SessionStart[].hooks[] | select(.type == "command") | .command] | join(",")' "$install_path/hooks/hooks.json")"
[[ "$hook_commands" == "toolgate hook session-start" ]] ||
	fail "SessionStart hook commands are '$hook_commands', want 'toolgate hook session-start'"

# Claude Code loads the skill, the hook and the MCP server.
details="$(claude plugin details "$plugin")"
for component in "Skills (1)" "Hooks (1)  SessionStart" "MCP servers (1)  toolgate"; do
	grep -qF "$component" <<<"$details" || fail "plugin details do not list '$component'"
done

# The skill tells the model to list first, enable what it needs, disable when done.
skill="$install_path/skills/toolgate/SKILL.md"
for tool in toolgate_list_servers toolgate_enable_server toolgate_disable_server; do
	grep -qF "$tool" "$skill" || fail "the skill does not name $tool"
done
for rule in "List first" "Enable only what the task needs" "Disable when done"; do
	grep -qF "$rule" "$skill" || fail "the skill does not say '$rule'"
done

if [[ -n "$failed" ]]; then
	echo "plugin-check failed:$failed" >&2
	exit 1
fi
echo "plugin-check: ok"
