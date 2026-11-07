# toolgate for users

For: people who use Claude Code with a toolgate gateway.

toolgate is a gateway between Claude Code and MCP servers. An MCP server gives Claude tools, for example to read GitHub issues. Your admin decides which servers you can use. toolgate starts a server when you need it, checks each tool call against the rules of your admin, and records the call.

This page tells you how to:

- install the toolgate plugin for Claude Code and set the address of your gateway;
- sign in with `toolgate login`;
- list, turn on and turn off MCP servers in a session;
- set the secrets that your servers need with `toolgate secret set`;
- read a refusal, and find out which rule refused a call.

## Install the plugin

The toolgate plugin connects Claude Code to your gateway. A plugin marketplace is a list of plugins that Claude Code can install. This repository is a marketplace with one plugin, `toolgate`.

Before you start, install the `toolgate` command and make sure that it is on your `PATH`. The plugin runs it. Your admin gives you the URL of your gateway.

1. In Claude Code, run `/plugin marketplace add solidDoWant/toolgate`.
2. Run `/plugin install toolgate@toolgate`.
3. When Claude Code asks for the "Gateway URL", type the URL of your gateway, for example `https://toolgate.example.com`. Do not add `/mcp` at the end.
4. Restart Claude Code.

You can also install the plugin from a shell, outside Claude Code:

```
claude plugin marketplace add solidDoWant/toolgate
claude plugin install toolgate@toolgate
```

After the install from a shell, set the gateway URL with `/plugin configure toolgate@toolgate` in Claude Code. To change the URL later, use the same command.

The plugin adds three things to Claude Code:

- An MCP server with the name `toolgate`, at the gateway URL followed by `/mcp`. Claude Code runs `toolgate headers` to get the headers for each connection.
- A hook that runs `toolgate hook session-start` when a session starts.
- A skill that tells Claude how to use the gateway: list the servers first, turn on only the servers that the task needs, and turn them off when the task is done.

## Make sure that the plugin works

Do these steps after you install the plugin and sign in with `toolgate login`.

1. Start a new Claude Code session.
2. Run `/mcp`. Make sure that the `toolgate` server shows as connected and has four tools: `toolgate_list_servers`, `toolgate_enable_server`, `toolgate_disable_server` and `toolgate_server_status`.
3. Ask Claude to list the toolgate servers. Make sure that Claude calls `toolgate_list_servers`.
4. Ask Claude to use a tool of one of the servers. Make sure that Claude calls a tool with a name that starts with the server name, for example `test__echo`. Claude does not need to call `toolgate_enable_server` first. The call turns on the server.
5. Ask Claude to turn the server off. Make sure that Claude calls `toolgate_disable_server`.
6. Run `/status` and write down the session ID.
7. Ask your admin to find these calls in the audit log of the gateway. Make sure that the audit log shows the same session ID.

If the `toolgate` server shows as failed in step 2, run `toolgate status` in a shell to make sure that you are signed in. Then make sure that the gateway URL in `/plugin configure toolgate@toolgate` is correct.

## Grants

Your admin can mark a server or a tool as `opt-in`. To use it from your own machine, you need a grant. A grant lasts from 1 minute to 7 days. When you need a grant, the refusal gives a link to the form in the web UI.

1. Open the link. If you signed in more than 5 minutes ago, sign in again.
2. Choose how long the grant lasts.
3. If only one Claude Code session needs the grant, choose that session.
4. Click "Create grant".
5. Ask Claude to try again.

The refusal that sends you here says that the server or tool "needs a grant on a signed-in user's machine", and gives the link to the form. A grant that you create for one session works only in that session. A grant without a session works in all your sessions. Creating a grant needs a fresh sign-in. Revoking one does not.

The page `/ui/grants` lists your grants. To stop a grant early, click "Revoke". The tools of the server leave Claude's tool list a few seconds after a grant ends.

If the grant form says that you have no access, or the grant does not match you, your admin must check that the web UI and `toolgate login` give you the same subject (`sub`). See [The web UI](../admin/web-ui.md#make-the-oidc-client).

## Tool calls

When Claude calls a tool of a server, for example `test__echo`, the gateway sends the call to the server and returns the result. You do not need to turn the server on first. If the server is allowed for you, the call turns it on. If it is not allowed in your workspace, the gateway asks you to allow it, in the same way as when Claude turns the server on. The gateway refuses a call in these cases:

- The server is not ready, for example because it failed. The error gives the status and the reason. `toolgate_server_status` shows them too.
- A rule from your admin blocks the tool. The error gives the reason of the rule.
- The request is larger than 4 MiB. Send smaller tool arguments.
- The result is larger than 16 MiB. The gateway does not send it to Claude Code.

The gateway records each call and each refusal in its audit log.

## Approve a tool call

Your admin can mark a tool so that each call needs your approval. When Claude calls such a tool, the call fails with a message like this: "Tool `github__delete_repository` needs approval. Approve request `ID` at `URL`, then call the tool again with the same arguments."

1. Open the address from the message in a browser. If you are not signed in to the toolgate web UI, the browser goes to your identity provider first. Sign in there.
2. Read the server, the tool, the session and the workspace of the call.
3. If you signed in more than 4 minutes ago, click "Sign in again to continue" first.
4. Click Approve to let the call through, or click Deny to refuse it.
5. Ask Claude to call the tool again with the same arguments.

If your client does not support URL elicitation (a request from the gateway to open a web page), the call fails at once with this message, and you open the address yourself. If the gateway has no web UI, the message says "needs approval, but this gateway has no web UI. Ask an admin." If you do not decide in time, the request expires and the call fails. Ask Claude to call the tool again to make a new request.

An approval lets one call through. It stays usable for the approval timeout that your admin set, 10 minutes by default. The page `/ui/approvals` lists all your requests that wait for approval. Only you can see and decide your own requests.

## Sign in

Your admin gives you the URL of your gateway. Run:

```
toolgate login --gateway https://toolgate.example.com
```

toolgate shows a web address and a code. Open the address in a browser, sign in with your identity provider, and make sure that the browser shows the same code. When you approve the sign-in, `toolgate login` shows "Signed in as" and your email address.

toolgate saves the gateway URL in the file `toolgate/config.json` in your user configuration directory. If you sign in again later, you can leave out `--gateway`.

To see who is signed in, run `toolgate status`. It shows the gateway URL, your principal (your user ID at the identity provider) and your email address. If your identity provider did not verify the email address, "(unverified)" follows it. The gateway decides what you can use. `toolgate status` only shows information.

## See the servers that you can use

To see the servers that you can use, run `toolgate servers`. It shows the same list that Claude gets from `toolgate_list_servers`: the name, the scope, the status, whether the server is enabled, and a description. If a server is blocked for you, the status also shows the reason.

The list depends on the workspace. `toolgate servers` sends the same workspace that Claude Code sends from the current directory. If you set `TOOLGATE_SESSION`, the command uses that session, and the `ENABLED` column shows the servers that are enabled in it.

## See your open sessions

To see your open sessions, run `toolgate sessions`. A session is one Claude Code conversation with the gateway. The command prints a table with these columns: `SESSION`, `WORKSPACE`, `LAST SEEN` and `SERVERS`. The `SERVERS` column lists the servers that are enabled in the session. A dash means that no server is enabled. If a session has servers or tools that you allowed or denied for that session only, the table also has the columns `ALLOWED` and `DENIED`.

The command shows only your own sessions. It leaves out sessions that ended and sessions that had no request for longer than the idle time that your admin set. If you have more than 200 open sessions, the command shows the newest 200 and says so.

## Watch audit events

To print audit events as they happen, run `toolgate audit tail`. An audit event is one record of what the gateway did, for example a tool call that it allowed or refused. The command prints one line for each event: the time, the type, the user, the workspace, the session, the server, the tool, and the decision or result. A dash means that the event has no value for that field.

The command first prints your last 20 events from the past 24 hours. Then it prints new events until you press Ctrl-C. An event appears about 5 to 7 seconds after it happens.

You see only your own events. If you are an admin, you see the events of all users. Your admin decides which group of users are admins.

The command has these options:

- `--json` prints each event as one line of JSON. Each line has a `key` field that identifies the event.
- `--after KEY` starts after the event with that key. The command goes back 31 days at most.

The gateway closes the connection when your sign-in token expires, after one hour, or when the gateway restarts. The command then connects again and continues after the last event that it printed. It prints no event twice.

The command shows a view of the audit log. It is not the record. In rare cases, an event that takes more than 5 seconds to save does not appear in the command output. The event is still in the audit log.

If you have 4 tails open, the gateway refuses a fifth one with "Too many audit tails are open. Close one first."

## Use servers in Claude Code

You do not call the gateway yourself. Ask Claude for what you need, and Claude uses four tools of the `toolgate` server:

- `toolgate_list_servers` shows the servers that you can use.
- `toolgate_enable_server` turns on a server in this session. If the server is not running, toolgate starts it, and Claude shows the progress. Then the tools of the server become available.
- `toolgate_disable_server` turns off a server in this session.
- `toolgate_server_status` shows if a server is running, and why it failed.

Claude can call a tool of a server that the rules allow without `toolgate_enable_server` first. The call turns on the server in this session. If the server is not running, the call starts it and waits. Claude shows the progress, so the first call can take longer. If the server cannot start, the call fails with the reason.

The name of each tool of a server starts with the name of the server and two underscores, for example `github__list_issues`. One session can have 32 servers turned on at most.

If the gateway has a web UI, the catalog also has a server named `toolgate`. It runs inside the gateway and is always ready. Its tool `toolgate_list_scopes` shows your own workspace scope: your defaults, the servers and tools that each workspace allows or denies, and the servers that each open session allows or denies. It shows at most 200 workspaces and 200 sessions. Its tool `toolgate_deny` removes servers or tools from one of your workspaces or open sessions at once. Give it `workspace` or `session`, and `servers` with 1 to 32 names. A name is a server, or `<server>__<tool>` for one tool, for example `github__delete_repository`. Its tool `toolgate_allow` asks to add servers or tools to one of your workspaces, or to one of your open sessions that has a verified workspace. It takes the same input as `toolgate_deny`. Nothing changes when you call it. The tool makes an access request and gives you a link to its approval page. You open the page in your browser, sign in again, and approve or deny the change. If your client can open links, the tool asks it to open the page, and then waits for your decision. The result is a status: `approved`, `denied`, `expired` or `pending`, with the request ID and the link. If the status is `pending`, you can approve the change at the link later. An allow for one session ends when that session ends. You cannot allow one tool of a server that you denied as a whole. Allow the server first. You can use these tools only in a session where you allow the `toolgate` server, in the same way as any other server.

A server has one of three scopes. Your admin sets the scope:

- `per-session`: the server is only for this session. It stops when you turn it off or when the session ends.
- `per-user`: your sessions share one copy of the server. It stops some time after none of your sessions use it.
- `shared`: all users share one copy of the server.

By default, a session ends after 2 hours without requests. Then its servers stop. If you resume the session in Claude Code from the same workspace within 24 hours, you get your servers back, and you do not turn them on again. The servers start again, and their tools come back when each server is ready. If Claude calls a tool of a server that is still starting, the call waits until the server is ready, and then it runs. Your admin can change the 24 hours. If you resume the session from another workspace, or after that time, the session starts with no servers turned on.

When a session resumes, toolgate checks each server again. If the rules or your workspace list no longer allow a server, or you reached the limit of running servers, toolgate turns that server off.

## Set your secrets

Some servers need a secret from you, for example a GitHub token. If a secret is missing, Claude tells you the command to run.

To set a secret, run `toolgate secret set` with the name of the secret:

```
toolgate secret set github-token
```

If you run the command in a terminal, toolgate asks for the value and does not show what you type. You can also give the value on standard input. In that case, toolgate sends every byte, including a newline at the end. To send a value without a newline, use `printf`:

```
printf '%s' "$TOKEN" | toolgate secret set github-token
```

A name contains only letters, digits, `.`, `_` and `-`, and has 128 characters at most. A value is 4 MiB at most. If you set a secret again, the new value replaces the old value.

To see your secrets, run `toolgate secret list`. It shows the name of each secret and when you set it. toolgate never shows a value after you set it.

You can also manage your secrets in the web UI, on the Secrets page. Type a value or choose a file there.

To delete a secret, run `toolgate secret delete github-token`. A server that is already running keeps the value that it started with.

Some servers run outside toolgate, and toolgate sends your secret to them as your key. An example is mail-archiver. Create an API key in the web page of mail-archiver, and then set it:

```sh
toolgate secret set mail-archiver-api-key
```

If the server does not accept your key, Claude tells you that the server refused your credential. Make sure that the key is correct, and set it again with the same command. toolgate uses the new key at the next connection.

## Where toolgate keeps your sign-in

When you sign in, toolgate gets a refresh token from your identity provider. A refresh token is a secret that lets toolgate get new access tokens without a new sign-in. toolgate keeps it in the keychain of your operating system: the macOS Keychain, the Windows Credential Manager, or the Secret Service on Linux.

If your computer has no keychain, toolgate writes the token to the file `toolgate/refresh-token` in your user configuration directory. On Linux this directory is `~/.config`. Only your user account can read the file (mode 0600).

To remove the token from the keychain and from the file, run `toolgate logout`.

Claude Code can start several `toolgate` commands at the same time, for example at the start of a session. Only one of them gets a new access token at a time. The others wait for it, and then use the newest refresh token. Some identity providers give a new refresh token on each use and refuse the old one. Without the wait, such a provider would end your sign-in. To make the commands wait, toolgate uses the file `toolgate/token.lock` in your user configuration directory. toolgate uses this file also when the token is in the keychain.

### Choose the store

Run `toolgate login --token-store auto|keychain|file` to choose where toolgate keeps the token. You can also set the `TOOLGATE_TOKEN_STORE` environment variable. The default is `auto`: toolgate tries the keychain and uses the file if the keychain does not work.

toolgate writes the store it used to `config.json` as `tokenStore`. Every later run reads and writes only that store. It never switches to the other one. If the store is `keychain` and the keychain stops working, toolgate asks you to sign in again with `--token-store file`. A `config.json` from before this setting has no `tokenStore`, and toolgate keeps trying the keychain first and then the file.

### Sign in from a workspace

A workspace (for example a Coder workspace) has no browser. `toolgate login` prints a link and a code. Open the link on any device, check the code, and approve. The command prints how long the code is valid. If nobody approves in time, it shows "The sign-in code expired. Run `toolgate login` again." and exits with code 1.

The interactive shell of a workspace can have a keychain session that the processes of the agent do not have. Then a token saved in the keychain is invisible to those processes. To avoid this, follow these rules:

1. Run `toolgate login --token-store file` in the workspace.
2. Keep the home directory on a persistent volume. If the volume is lost when the workspace restarts, the sign-in is lost too.

The sign-in lasts until your identity provider ends it. For Authentik, this is the "Refresh token validity" setting of the OAuth2 provider. When the refresh token stops working, `toolgate headers` shows "Sign in again: run `toolgate login`.", prints no headers and exits with a non-zero code.

## Servers in each workspace

If your gateway has the web UI, toolgate limits the servers that each of your sessions can use. The set of servers that a session can use is its effective list.

A session has a verified workspace when its workspace proved where it runs, for example a Coder workspace with a workload token. A session without this proof is unverified. The workspace name that your machine sends does not prove anything.

- In a verified workspace, the effective list is your list for that workspace. If you have no list for it, your default for verified workspaces applies: all your servers, or none. Servers that you added to this session only are also on the effective list.
- In an unverified session, your default for unverified sessions applies: all your servers, none, or only the servers on your unverified allow list. The unverified allow list is a list of servers for all your unverified sessions. It holds at most 64 servers. If the default is "only these servers" and the list is empty, the session gets no servers. Workspace lists and servers added to one session do not apply.
- If you did not set your defaults, both defaults are none. Then no session can use a server.

The effective list never gives you a server that your admin does not let you use.

In a verified workspace, `toolgate_list_servers` shows a server that is not on the effective list as `blocked`, with "not allowed in this workspace; enable it to ask". In an unverified session, it shows the server as `blocked` in the same way, with the reason "not allowed in sessions without a verified workspace" and the commands that fix it. Claude does not get the tools of the server. If a server leaves the effective list, it stays turned on, but its tools go away. If the server comes back on the list, its tools come back, and you do not turn it on again.

### Change your lists and defaults

Use `toolgate workspace` to see and change them:

- `toolgate workspace list` shows your two defaults and a table with the columns `WORKSPACE`, `SERVERS` and `UPDATED`. If a workspace has tools that you allowed one by one, or servers or tools that you denied, the table also has the columns `TOOLS` and `DENIED`. `(no list)` means that the workspace has no list, so your default applies to it.
- `toolgate workspace allow SERVER...` adds servers to a workspace's list. `toolgate workspace deny SERVER...` removes them and denies them in the workspace. A denied server stays refused, also when your default is all or the workspace has no list. To deny one tool, write `SERVER__TOOL`, with the tool name that Claude sees, for example `github__delete_repository`. `toolgate workspace clear` deletes the list and the tools that you allowed one by one, so your default applies again. It keeps the servers and tools that you denied. These commands act on the verified workspace of the machine. To use another workspace, add `--workspace ID`. If the machine has no verified workspace, the commands stop with "This machine has no verified workspace. Pass --workspace."
- `toolgate workspace deny --session ID SERVER...` denies servers or `SERVER__TOOL` tools in one of your open sessions only, and removes servers that you added to that session. It also works for a session without a verified workspace. Do not use it with `--workspace`. The deny stays when the session ends and resumes. It goes away when toolgate forgets the session, after the resume time.
- `toolgate workspace allow SERVER__TOOL` allows one tool in a workspace, with the tool name that Claude sees. `toolgate workspace allow --session ID SERVER...` allows servers or `SERVER__TOOL` tools in one of your open sessions only. The session must have a verified workspace. Do not use `--session` with `--workspace`. An allow for a session ends when the session ends. An allow also removes a deny of the same server or tool. You cannot allow a tool of a server that you denied. Allow the server first.
- `toolgate workspace defaults --verified none|all --unverified none|all|list` sets the defaults. With `--unverified list`, your unverified sessions get only the servers on your unverified allow list. Without options, the command shows the defaults.
- `toolgate workspace allow --unverified SERVER...` adds servers to your unverified allow list. `toolgate workspace deny --unverified SERVER...` removes them. Do not use `--unverified` with `--workspace` or `--session`. `toolgate workspace list` shows the unverified allow list in the line "Unverified allow list".

A change that gives more access needs your approval in the browser, because a process that has your token must not widen your access alone. The command prints "Approve this change at `URL`. Waiting…" and checks every 2 seconds. It exits with code 0 only when you approve. Ctrl-C stops the command, but the request stays open until it expires. A change that gives less access applies at once. If the gateway refuses a change, the command shows the gateway's message.

A workspace list does not protect you from a process that has your token. The process can leave out the workload token, and toolgate then treats its session as unverified. A workspace's list limits a process holding your token only while the default for unverified sessions is nothing.

### The Workspaces page

Open `/ui/workspaces` on the gateway to see and change your lists and defaults. The page shows:

- Your two defaults: one for verified workspaces without a list, and one for unverified sessions.
- Under "Only these servers", a checkbox for each server that you can use. If you set the default for unverified sessions to "only these servers", your unverified sessions get the checked servers.
- Each of your workspace lists, with its servers.
- The verified workspaces of your open sessions that have no list yet. Each one has an "Add a list" button.

Some changes give more access: adding a server to a list, setting a default to all, checking a server under "Only these servers", changing the default for unverified sessions from nothing to "only these servers" while servers are checked, and clearing a list while your default for verified workspaces is all. These changes need a fresh sign-in, no more than 5 minutes old. If your sign-in is older, the page shows "Sign in again to continue". Other changes, for example removing a server from a list, apply at once.

If you change a list with `toolgate workspace allow` or `toolgate workspace defaults` and the change gives more access, the command prints a link to an approval page. The page shows your list or defaults before and after the change. For a change of the unverified allow list, the page shows the list and the default before and after. For an allow of a tool, or an allow in one session, the page also shows the target workspace or session, and the session that asked for the change. Click Approve to apply the change, after a fresh sign-in, or click Deny. Only you can open the page. When the request is no longer waiting, the page shows its status.

### Allow a server that is not on the list

In a verified workspace, Claude can ask to turn on a server that is not on the effective list. Then toolgate asks you. If Claude Code can open a page for you, it opens the approval page. If it cannot, the error gives the link: "Server `immich` isn't allowed in workspace `coder/alice/mail`. Allow it at ..., then enable it again."

The approval page shows the server, the session and the workspace. Select one of these buttons:

- "Allow for this session" adds the server to this session only. Other sessions in the workspace do not get it. When the session ends, toolgate removes the server again.
- "Always allow in this workspace" adds the server to your list for the workspace. If the workspace has no list, toolgate makes a list with only this server.
- "Deny" refuses the server.

Both "Allow" buttons need a fresh sign-in, no more than 5 minutes old. If Claude Code waits on the page, the server turns on after you allow it. If not, tell Claude to turn the server on again.

If nobody decides in time, the request expires. Your admin sets this time (10 minutes by default). If the session ends first, the request expires too. In an unverified session, toolgate never asks. It refuses the server and tells you how to change the default for unverified sessions.

toolgate refuses an approval in these cases, and the request stays open:

- The session ended. The page shows "This session has ended."
- The session moved to another workspace, or lost its verified workspace. The page shows "This session is now in another workspace."
- The session already has 32 servers that you added for it only, the list already has 64 servers, or you already have 200 lists. Remove one, then approve again.

To remove a server that you added to one session, run `toolgate workspace deny --session ID SERVER`.

### No servers?

If `toolgate_list_servers` shows your servers as `blocked`, or Claude finds no tools, check if the session is unverified. A laptop or a devcontainer is unverified. The default for unverified sessions is `none`, so every server is blocked there. Your access groups are not the problem.

To fix it, do one of these:

- Set the default for unverified sessions on the Workspaces page, or run `toolgate workspace defaults --unverified all`.
- Allow one server with `toolgate workspace allow --unverified SERVER`.

A server that is not in the list at all is not in your catalog. Ask your admin to give you access.

## The headers that Claude Code sends

The toolgate plugin tells Claude Code to run `toolgate headers` before it connects to the gateway. This command prints three request headers as JSON:

- `Authorization`: a new access token. toolgate gets it from your identity provider with the stored refresh token, so you do not have to sign in again when an access token expires.
- `X-Toolgate-Session`: the ID of your Claude Code session.
- `X-Toolgate-Workspace`: a name for the place where you work.

toolgate gets the session ID from a file that the plugin writes when a Claude Code session starts. If `toolgate headers` finds no such file within 5 seconds, it shows "No Claude Code session found" and prints no headers. Claude Code then shows that the connection to the gateway failed. To run `toolgate headers` outside Claude Code, set the session ID yourself in the `TOOLGATE_SESSION` environment variable.

The workspace name is the value of the `TOOLGATE_WORKSPACE` environment variable when you set it. If you do not set it, toolgate uses the name of your computer and the name of your project. The project name is the name of the repository at the git remote `origin`. If there is no such remote, it is the name of the current directory. toolgate changes each character that is not a letter, a digit, `.`, `_` or `-` to `-`, and keeps the first 128 characters. For example, on the computer `laptop` in a clone of `github.com/acme/widgets`, the workspace is `laptop-widgets`. The gateway shows this name to your admin's rules as `claimed/laptop-widgets`.

### Headers for telemetry

If your admin sends Claude Code telemetry to the gateway, Claude Code runs `toolgate otel-headers`. This command prints two request headers as JSON: `Authorization` and `X-Toolgate-Workspace`. They are the same as in `toolgate headers`: a new access token, and the workspace name. It does not print a session ID and it does not wait for one. If you are not signed in, it shows "not signed in", prints no headers and exits with a non-zero code. Your access token must be valid for at least 5 minutes, because Claude Code runs the command only every 4 minutes.

### Sign in with a workload identity (SPIFFE)

Use this in a place that runs a SPIFFE Workload API, for example a Coder workspace with a SPIRE agent, when your admin has set it up. Set the variable `TOOLGATE_SPIFFE_SOCKET` to the address of the Workload API, for example `unix:///run/spire/agent.sock`.

Then these commands do not use your stored sign-in: `headers`, `otel-headers`, `hook session-start`, `servers`, `secret`, `sessions`, `workspace` and `audit tail`. Each run gets a new JWT-SVID (the token that proves the identity of the workload) for the audience `toolgate` and sends it as the access token. If toolgate cannot get the token within 5 seconds, the command shows the error and exits with a non-zero code. It never uses your stored sign-in instead, because that would change who you are without a warning.

If you have not signed in with `toolgate login`, set `TOOLGATE_GATEWAY` to the gateway URL. `toolgate headers` and `toolgate otel-headers` do not need it, because they do not call the gateway. toolgate does not read `SPIFFE_ENDPOINT_SOCKET`, because other tools may set it.

### Sign in through another service (token command)

Use this when another service already holds your consent and can print your access token. An example is Coder's external auth. Save the command and the gateway URL with `toolgate configure`, for example `toolgate configure --token-command 'coder external-auth access-token toolgate' --gateway https://toolgate.example.com`. You do not run `toolgate login`. You can also set the variables `TOOLGATE_TOKEN_COMMAND` and `TOOLGATE_GATEWAY`. If a variable is set, toolgate uses it and not the saved setting. Claude Code removes `TOOLGATE_TOKEN_COMMAND` from the environment of the toolgate plugin helper, so the plugin needs the saved setting.

Every command that needs your token runs the command with `/bin/sh -c` (`cmd.exe /d /c` on Windows) and uses its output instead of your stored sign-in. The command gets no standard input and has 5 seconds to finish. It must print one line with the token and at most 16 KiB. The command's own standard error reaches you, cut at 4 KiB.

toolgate keeps the token in its runtime directory while it has more than 60 seconds to live. A token that has no `exp` claim is not kept. If the gateway answers 401 to `servers`, `secret`, `sessions`, `workspace`, `audit tail` or `hook session-start`, toolgate deletes the kept token, runs the command again and tries once more.

These errors name the cause and never show the output of the command:

- `TOOLGATE_TOKEN_COMMAND exited with status 1.`
- `TOOLGATE_TOKEN_COMMAND didn't finish within 5 s.`
- `TOOLGATE_TOKEN_COMMAND printed more than 16 KiB.`
- `TOOLGATE_TOKEN_COMMAND printed no token.`
- `TOOLGATE_TOKEN_COMMAND printed something that isn't a token.`

Do not set `TOOLGATE_SPIFFE_SOCKET` as well. Every command then exits with `Set only one of TOOLGATE_SPIFFE_SOCKET and TOOLGATE_TOKEN_COMMAND.` Your admin sets up the gateway first: see [Coder workspaces](../admin/coder.md).

### Prove where your session runs (attestation)

Use this in a workspace that your admin attests, for example a Coder workspace. You sign in as usual. In addition, toolgate sends a workload token that proves where the session runs. If the gateway finds that the workload belongs to you, the session is attested, and its workspace is verified, for example `coder/alice/dev`. Then you can use `opt-in` servers without a grant.

Set at most one of these variables:

- `TOOLGATE_ATTESTATION_TOKEN_FILE`: the path of a projected service-account token with the audience `toolgate`. toolgate reads the file on each run, because Kubernetes rotates it.
- `TOOLGATE_ATTESTATION_SPIFFE_SOCKET`: the address of a SPIFFE Workload API. toolgate gets a new JWT-SVID for the audience `toolgate` on each run, within 5 seconds.
- `TOOLGATE_ATTESTATION_TOKEN_REQUEST`: for a KubeVirt virtual machine. The value is the directory of the mounted `serviceAccount` disk, which holds `token`, `namespace` and `ca.crt`. toolgate asks the Kubernetes API server for a token with the audience `toolgate` that lasts 600 seconds. It uses the URL in `TOOLGATE_KUBE_API`, or `https://kubernetes.default.svc` when that is not set. The virtual machine must be able to reach the API server. toolgate asks again on each run.

Every command that calls the gateway sends the token in the `X-Toolgate-Attestation` header: `headers`, `otel-headers`, `hook session-start`, `servers`, `secret`, `sessions`, `workspace` and `audit tail`. `toolgate headers` adds the header to its output.

If toolgate cannot get the token, the command shows the reason, never the token, and exits with a non-zero code. It sends no request, because a request without the attestation would change the attestation of your session without a warning. If the Kubernetes API server refuses a token request, the command shows its status and message.

If you set more than one of the variables, every command exits with `Set only one of TOOLGATE_ATTESTATION_TOKEN_FILE, TOOLGATE_ATTESTATION_SPIFFE_SOCKET and TOOLGATE_ATTESTATION_TOKEN_REQUEST.` You cannot combine them with `TOOLGATE_SPIFFE_SOCKET`.

## Settings from your admin

Your admin can give you Claude Code settings, for example permissions or environment variables. When a Claude Code session starts, the plugin runs `toolgate hook session-start`. This command gets the settings for you from the gateway and writes them into `~/.claude/settings.json`.

toolgate changes only the settings that it wrote before. It keeps a list of these settings and their values in the file `toolgate/owned-settings.json` in your user configuration directory. The rules are:

- If you change a setting that toolgate wrote, toolgate does not change it again. At the start of the session, toolgate tells Claude Code the names of these settings.
- If your admin removes a setting, toolgate removes it from your file. If you changed that setting, toolgate keeps your value.
- toolgate does not touch the other settings in the file.

To get the value from your admin again, delete your value from `~/.claude/settings.json`. If toolgate never wrote the setting, it does not replace a value that is already in the file.

If you are not signed in, the session starts as usual, and Claude Code tells you to run `toolgate login`.

## When toolgate refuses a call

toolgate checks each request. If it refuses a request, Claude gets a short message that tells you why. Claude usually shows it to you. These are the messages, and what to do:

| Message | What it means | What to do |
|---|---|---|
| You don't have access to server "NAME". | Your admin did not give you access to this server. | Ask your admin for access. |
| Server "NAME" can be used only from an attested environment, not from a signed-in user's machine. | The server works only from pods in Kubernetes, not from your computer. The message can also name one tool. | Use the server from an allowed pod, or ask your admin. |
| Tool "NAME" is blocked by the server's template. | Your admin turned off this tool for all users. | Do the task in a different way. |
| Refused by rule NAMESPACE/NAME: REASON | A rule of your admin refused the call. The message gives the name of the rule and the reason that your admin wrote. | If you think that the rule is wrong, give your admin the name of the rule. |
| NAME needs a grant on a signed-in user's machine. Create one at URL, then try again. | Your admin marked the server or tool `opt-in`. | Create a grant. See [Grants](#grants). |
| Tool `NAME` needs approval. Approve request `ID` at URL, then call the tool again with the same arguments. | Each call of this tool needs your approval. | See [Approve a tool call](#approve-a-tool-call). |
| Secret `NAME` isn't set. Run `toolgate secret set NAME`. | The server needs a secret from you. | Run the command, and then turn on the server again. See [Set your secrets](#set-your-secrets). |
| The remote server refused your credential (401 or 403). | The server did not accept the secret that you set. | Make sure that the key is correct, and set it again with the command in the message. See [Set your secrets](#set-your-secrets). |
| You already have 20 servers running, the limit for each user. | You reached the number of servers that one user can run. | Turn off servers that you do not use, or wait until idle servers stop. |
| The gateway is already running its limit of 200 servers. | The gateway runs as many servers as it can. | Try again later, or ask your admin. |
| Server `NAME` isn't in the list for workspace `WORKSPACE`. Allow it in the Workspaces page or with `toolgate workspace allow NAME`. | The server is not on the effective list of this verified workspace. | Add the server to the list of the workspace. See [Servers in each workspace](#servers-in-each-workspace). |
| Server `NAME` isn't allowed in sessions without a verified workspace. ... | The session is unverified, and your default for unverified sessions is none. | Set the default for unverified sessions to all, or use the server from a verified workspace. See [Servers in each workspace](#servers-in-each-workspace). |
| This session already has 32 servers enabled. | One session can have 32 servers turned on at most. | Turn off a server in this session first. |

The numbers in the limit messages are the values that your admin set. A tool that a rule blocks does not show in the list of tools, so Claude does not try to use it.

Each refusal is recorded in the audit log of the gateway with the name of the rule. Your admin can find it there.

If the message says that a server failed to start, the message gives the reason. Run `toolgate servers`, or ask Claude for the status of the server. If the problem continues, give the message to your admin.

If Claude Code cannot connect to the gateway at all, run `toolgate status`. If you are not signed in, run `toolgate login`.
