# toolgate for users

For: people who use Claude Code with a toolgate gateway.

toolgate is in early development, and these pages are not written yet. They will tell you how to:

- install the toolgate plugin for Claude Code and set the address of your gateway;
- sign in with `toolgate login`;
- list, turn on and turn off MCP servers in a session;
- set the secrets that your servers need with `toolgate secret set`;
- read a refusal, and find out which rule refused a call.

## Sign in

Your admin gives you the URL of your gateway. Run:

```
toolgate login --gateway https://toolgate.example.com
```

toolgate shows a web address and a code. Open the address in a browser, sign in with your identity provider, and make sure that the browser shows the same code. When you approve the sign-in, `toolgate login` shows "Signed in as" and your email address.

toolgate saves the gateway URL in the file `toolgate/config.json` in your user configuration directory. If you sign in again later, you can leave out `--gateway`.

To see who is signed in, run `toolgate status`. It shows the gateway URL, your principal (your user ID at the identity provider) and your email address. If your identity provider did not verify the email address, "(unverified)" follows it. The gateway decides what you can use. `toolgate status` only shows information.

## Where toolgate keeps your sign-in

When you sign in, toolgate gets a refresh token from your identity provider. A refresh token is a secret that lets toolgate get new access tokens without a new sign-in. toolgate keeps it in the keychain of your operating system: the macOS Keychain, the Windows Credential Manager, or the Secret Service on Linux.

If your computer has no keychain, toolgate writes the token to the file `toolgate/refresh-token` in your user configuration directory. On Linux this directory is `~/.config`. Only your user account can read the file (mode 0600).

To remove the token from the keychain and from the file, run `toolgate logout`.
