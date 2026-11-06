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
4. Ask Claude to turn on one of the servers and to use one of its tools. Make sure that Claude calls `toolgate_enable_server`, and then calls a tool with a name that starts with the server name, for example `test__echo`.
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

When Claude calls a tool of a server, for example `test__echo`, the gateway sends the call to the server and returns the result. The gateway refuses a call in these cases:

- The server is not turned on in this session. Turn it on with `toolgate_enable_server` first.
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

To see your open sessions, run `toolgate sessions`. A session is one Claude Code conversation with the gateway. The command prints a table with these columns: `SESSION`, `WORKSPACE`, `LAST SEEN` and `SERVERS`. The `SERVERS` column lists the servers that are enabled in the session. A dash means that no server is enabled.

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

The name of each tool of a server starts with the name of the server and two underscores, for example `github__list_issues`. One session can have 32 servers turned on at most.

A server has one of three scopes. Your admin sets the scope:

- `per-session`: the server is only for this session. It stops when you turn it off or when the session ends.
- `per-user`: your sessions share one copy of the server. It stops some time after none of your sessions use it.
- `shared`: all users share one copy of the server.

By default, a session ends after 2 hours without requests. Then its servers are turned off. Servers that you turned on stay on when Claude Code connects again.

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

## Where toolgate keeps your sign-in

When you sign in, toolgate gets a refresh token from your identity provider. A refresh token is a secret that lets toolgate get new access tokens without a new sign-in. toolgate keeps it in the keychain of your operating system: the macOS Keychain, the Windows Credential Manager, or the Secret Service on Linux.

If your computer has no keychain, toolgate writes the token to the file `toolgate/refresh-token` in your user configuration directory. On Linux this directory is `~/.config`. Only your user account can read the file (mode 0600).

To remove the token from the keychain and from the file, run `toolgate logout`.

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
- In an unverified session, your default for unverified sessions applies: all your servers, or none. Lists and servers added to one session do not apply.
- If you did not set your defaults, both defaults are none. Then no session can use a server.

The effective list never gives you a server that your admin does not let you use.

In a verified workspace, `toolgate_list_servers` shows a server that is not on the effective list as `blocked`, with "not allowed in this workspace; enable it to ask". In an unverified session, it does not show the server. Claude does not get the tools of the server. If a server leaves the effective list, it stays turned on, but its tools go away. If the server comes back on the list, its tools come back, and you do not turn it on again.

A workspace list does not protect you from a process that has your token. The process can leave out the workload token, and toolgate then treats its session as unverified. A workspace's list limits a process holding your token only while the default for unverified sessions is nothing.

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

Then these commands do not use your stored sign-in: `headers`, `otel-headers`, `hook session-start`, `servers`, `secret`, `sessions` and `audit tail`. Each run gets a new JWT-SVID (the token that proves the identity of the workload) for the audience `toolgate` and sends it as the access token. If toolgate cannot get the token within 5 seconds, the command shows the error and exits with a non-zero code. It never uses your stored sign-in instead, because that would change who you are without a warning.

If you have not signed in with `toolgate login`, set `TOOLGATE_GATEWAY` to the gateway URL. `toolgate headers` and `toolgate otel-headers` do not need it, because they do not call the gateway. toolgate does not read `SPIFFE_ENDPOINT_SOCKET`, because other tools may set it.

### Sign in through another service (token command)

Use this when another service already holds your consent and can print your access token. An example is Coder's external auth. Set the variable `TOOLGATE_TOKEN_COMMAND` to the command, for example `coder external-auth access-token toolgate`, and `TOOLGATE_GATEWAY` to the gateway URL. You do not run `toolgate login`.

Every command that needs your token runs the command with `/bin/sh -c` (`cmd.exe /d /c` on Windows) and uses its output instead of your stored sign-in. The command gets no standard input and has 5 seconds to finish. It must print one line with the token and at most 16 KiB. The command's own standard error reaches you, cut at 4 KiB.

toolgate keeps the token in its runtime directory while it has more than 60 seconds to live. A token that has no `exp` claim is not kept. If the gateway answers 401 to `servers`, `secret`, `sessions`, `audit tail` or `hook session-start`, toolgate deletes the kept token, runs the command again and tries once more.

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

Every command that calls the gateway sends the token in the `X-Toolgate-Attestation` header: `headers`, `otel-headers`, `hook session-start`, `servers`, `secret`, `sessions` and `audit tail`. `toolgate headers` adds the header to its output.

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
| You already have 20 servers running, the limit for each user. | You reached the number of servers that one user can run. | Turn off servers that you do not use, or wait until idle servers stop. |
| The gateway is already running its limit of 200 servers. | The gateway runs as many servers as it can. | Try again later, or ask your admin. |
| Server `NAME` isn't in the list for workspace `WORKSPACE`. Allow it in the Workspaces page or with `toolgate workspace allow NAME`. | The server is not on the effective list of this verified workspace. | Add the server to the list of the workspace. See [Servers in each workspace](#servers-in-each-workspace). |
| Server `NAME` isn't allowed in sessions without a verified workspace. ... | The session is unverified, and your default for unverified sessions is none. | Set the default for unverified sessions to all, or use the server from a verified workspace. See [Servers in each workspace](#servers-in-each-workspace). |
| This session already has 32 servers enabled. | One session can have 32 servers turned on at most. | Turn off a server in this session first. |

The numbers in the limit messages are the values that your admin set. A tool that a rule blocks does not show in the list of tools, so Claude does not try to use it.

Each refusal is recorded in the audit log of the gateway with the name of the rule. Your admin can find it there.

If the message says that a server failed to start, the message gives the reason. Run `toolgate servers`, or ask Claude for the status of the server. If the problem continues, give the message to your admin.

If Claude Code cannot connect to the gateway at all, run `toolgate status`. If you are not signed in, run `toolgate login`.
