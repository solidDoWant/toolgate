# Settings profiles

For: admins who give Claude Code settings to their users.

A `SettingsProfile` holds a part of a Claude Code `settings.json` file and a selector. The selector says which users get the profile. When a Claude Code session starts, the toolgate plugin gets the settings for the user from `GET /api/v1/settings` and writes them into the user's `~/.claude/settings.json`.

```yaml
apiVersion: toolgate.soliddowant.dev/v1alpha1
kind: SettingsProfile
metadata:
  name: dev
spec:
  selector:
    groups: [dev]
  settings:
    env:
      HTTPS_PROXY: http://proxy.example.com:3128
    permissions:
      deny: ["Bash(rm -rf:*)"]
```

## Who gets a profile

A profile applies to a user when the user is in one of its `groups` or is one of its `principals`. If you set `attestation`, the request must also have that attestation (`user` or `attested`). A profile with no groups and no principals applies to no one.

## How profiles merge

The gateway merges all profiles that apply to the user, in the order of their names. The merge works key by key:

- Each top-level key is one key.
- Each key inside `permissions` and inside `env` is one key. If profile `a` sets `env.FOO` and profile `b` sets `env.BAR`, the user gets both.
- Other values, for example `hooks`, are replaced as a whole.

When two profiles set the same key, the profile with the later name wins. If a profile sets `permissions` or `env` to a value that is not an object, the gateway skips that profile and logs a warning.

## What happens on the client

The client changes only the keys that it wrote before. If a user changed a key by hand, the client keeps the user's value and tells the session about it. If you remove a key from the profile, the client removes it from the file, but only if the file still holds the value that the client wrote. The [user guide](../user/README.md#settings-from-your-admin) tells users about these rules.

Do not put a hook in a profile unless you trust it on every computer. Claude Code runs hooks as commands on the user's computer, so a profile with hooks runs code on every client that gets it.
