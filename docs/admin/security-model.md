# The security model

For: admins who decide what toolgate must protect, and who write its rules.

This page tells you which facts about a request toolgate can trust, what it protects against, and what it cannot protect against.

## What toolgate checks on every call

Every request to the gateway carries a token. The gateway refuses a request with 401 when the token is missing, expired, for another audience or from another issuer. Each refusal goes to the audit log as an `auth.refuse` event.

The gateway decides about each tool call before it sends the call to a server. It records each decision in the audit log as a `tool.decision` event, and the result as a `tool.result` event. It refuses a call in these cases:

1. The user is not in the `access` lists of the template.
2. The environment of the request does not match `environments`.
3. The template blocks the tool.
4. A `ToolgateRule` matches the call.

[Templates and rules](templates-and-rules.md) tells you how to write each of these.

## Verified and claimed facts

A verified fact comes from a source that toolgate trusts. A claimed fact is a value that the client sends, and that nobody checks.

| Fact | Where it comes from | Verified |
|---|---|---|
| Principal | The token from the identity provider, or the service-account token. | Yes |
| Email and groups | The claims in the token. toolgate uses an email address only when the identity provider verified it. | Yes |
| Workspace of a pod | The pod and its owner, from the service-account token: `k8s/NAMESPACE/OWNER-KIND/OWNER-NAME`. | Yes |
| Workspace of a user | The `X-Toolgate-Workspace` header, shown as `claimed/NAME`. | No |
| Session | The `X-Toolgate-Session` header. | No |
| Attestation | `attested` for a pod that signed in with a service-account token from an allowed namespace. `user` for a person. | Yes, computed |

The gateway checks that a claimed value contains only letters, digits, `.`, `_` and `-`, and has 128 characters at most. It refuses other values with 400. A claimed value can never get into a template as YAML. It is always one text value.

## What claimed workspaces and sessions cannot protect against

A user writes the workspace and the session ID on their own computer. The user can send any value that fits the pattern. So:

- A rule that matches only a `claimed/…` workspace, or only a session, does not stop a user who wants to avoid it. The user sends another value, and the rule does not match.
- A rule that matches a claimed value is useful to prevent mistakes. For example, it can stop Claude from deleting a repository from the workspace of the production repository. It does not stop a person who wants to do it.
- To limit a person, match a verified fact: `principals`, `groups`, `attestation`, `servers` or `tools`.
- The workspace and the session of a user appear in the audit log as the user sent them. Do not use them as proof of where a call came from.
- Two sessions of one user can send the same session ID. They then share one per-session instance. Different users never share a session, because a session belongs to the pair of the user and the session ID.

A claimed value can narrow what a user can do, but never widen it. Access lists use only verified facts. A rule only refuses.

## What a pod can prove

A pod that signs in with a service-account token is `attested`. Kubernetes vouches for its namespace, its service account and its owner. A rule that matches a `k8s/…` workspace or `attestation: attested` holds, if these conditions are true:

- Only trusted people can create pods in the namespaces in `serviceAccountNamespaces`.
- Nobody can get a token for the audience `toolgate` for a service account that they do not control.

See [Pod sign-in](pod-sign-in.md).

## Where instances run

Each `per-session` or `per-user` instance runs in its own namespace. The namespace enforces the Pod Security level `restricted`, and denies all network traffic except DNS and connections from the gateway. So an instance that holds the secrets of one user cannot be reached by the instances of other users.

The gateway can write only the kinds of objects that a template can contain, and only in namespaces with toolgate's label and the Pod Security level `restricted`. An admission policy enforces this, so a gateway that an attacker controls cannot do more. For example, it cannot start a privileged pod. [The chart page](chart.md) tells more about these permissions.

## Secrets

Users set their secrets with `toolgate secret set`. The gateway encrypts each value with a key that is not in PostgreSQL. A value is decrypted only when an instance of that user starts, and it is written only into a Secret in the namespace of that instance. The gateway never returns a value and never writes one to its log. [User secrets](user-secrets.md) tells how to keep and rotate the key.

A person who can read Secrets in the instance namespaces can read the values of running instances. Give this permission only to people who must have it.

## What toolgate does not protect against

- A server that does something bad with the access that it has. toolgate controls which tools the model can call, not what the server does inside a call.
- A user who has access to a server and calls its tools directly, without Claude Code. The same checks apply, because the user has the same token.
- A person with admin rights to the cluster, the database or the keyset.
- An identity provider that gives wrong groups or wrong email addresses.
