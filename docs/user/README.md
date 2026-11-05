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

## Tool calls

When Claude calls a tool of a server, for example `test__echo`, the gateway sends the call to the server and returns the result. The gateway refuses a call in these cases:

- The server is not turned on in this session. Turn it on with `toolgate_enable_server` first.
- The server is not ready, for example because it failed. The error gives the status and the reason. `toolgate_server_status` shows them too.
- A rule from your admin blocks the tool. The error gives the reason of the rule.
- The request is larger than 4 MiB. Send smaller tool arguments.
- The result is larger than 16 MiB. The gateway does not send it to Claude Code.

The gateway records each call and each refusal in its audit log.

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

To delete a secret, run `toolgate secret delete github-token`. A server that is already running keeps the value that it started with.

## Where toolgate keeps your sign-in

When you sign in, toolgate gets a refresh token from your identity provider. A refresh token is a secret that lets toolgate get new access tokens without a new sign-in. toolgate keeps it in the keychain of your operating system: the macOS Keychain, the Windows Credential Manager, or the Secret Service on Linux.

If your computer has no keychain, toolgate writes the token to the file `toolgate/refresh-token` in your user configuration directory. On Linux this directory is `~/.config`. Only your user account can read the file (mode 0600).

To remove the token from the keychain and from the file, run `toolgate logout`.

## The headers that Claude Code sends

The toolgate plugin tells Claude Code to run `toolgate headers` before it connects to the gateway. This command prints three request headers as JSON:

- `Authorization`: a new access token. toolgate gets it from your identity provider with the stored refresh token, so you do not have to sign in again when an access token expires.
- `X-Toolgate-Session`: the ID of your Claude Code session.
- `X-Toolgate-Workspace`: a name for the place where you work.

toolgate gets the session ID from a file that the plugin writes when a Claude Code session starts. If `toolgate headers` finds no such file within 5 seconds, it shows "No Claude Code session found" and prints no headers. Claude Code then shows that the connection to the gateway failed. To run `toolgate headers` outside Claude Code, set the session ID yourself in the `TOOLGATE_SESSION` environment variable.

The workspace name is the value of the `TOOLGATE_WORKSPACE` environment variable when you set it. If you do not set it, toolgate uses the name of your computer and the name of your project. The project name is the name of the repository at the git remote `origin`. If there is no such remote, it is the name of the current directory. toolgate changes each character that is not a letter, a digit, `.`, `_` or `-` to `-`, and keeps the first 128 characters. For example, on the computer `laptop` in a clone of `github.com/acme/widgets`, the workspace is `laptop-widgets`. The gateway shows this name to your admin's rules as `claimed/laptop-widgets`.

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
| Secret `NAME` isn't set. Run `toolgate secret set NAME`. | The server needs a secret from you. | Run the command, and then turn on the server again. See [Set your secrets](#set-your-secrets). |
| You already have 20 servers running, the limit for each user. | You reached the number of servers that one user can run. | Turn off servers that you do not use, or wait until idle servers stop. |
| The gateway is already running its limit of 200 servers. | The gateway runs as many servers as it can. | Try again later, or ask your admin. |
| This session already has 32 servers enabled. | One session can have 32 servers turned on at most. | Turn off a server in this session first. |

The numbers in the limit messages are the values that your admin set. A tool that a rule blocks does not show in the list of tools, so Claude does not try to use it.

Each refusal is recorded in the audit log of the gateway with the name of the rule. Your admin can find it there.

If the message says that a server failed to start, the message gives the reason. Run `toolgate servers`, or ask Claude for the status of the server. If the problem continues, give the message to your admin.

If Claude Code cannot connect to the gateway at all, run `toolgate status`. If you are not signed in, run `toolgate login`.
