# Phase 2 acceptance checklist: security model and web UI

Same rules as phase 1. Each scenario has a `Tests:` line. Browser scenarios are automated with a headless browser against the kind environment, except those marked **manual**.

## Web UI

- **AC-2-01 Sign-in.** A user signs in to the web UI through the IdP. A user who isn't an admin doesn't see the Blocks page and gets 403 from its URL.
  Tests: `test/e2e: TestAC2_01NonAdminHasNoBlocksPage`
- **AC-2-02 Secrets.** A user sets, replaces and deletes a secret in the UI. After saving, the value is never shown again on any page or API response.
  Tests: `internal/ui: TestAC2_02SecretsInBrowser`
- **AC-2-03 CSRF.** A form submission without a valid CSRF token is refused and changes nothing.
  Tests: `internal/ui: TestAC2_03FormWithoutCSRFTokenIsRefused`
- **AC-2-04 Sessions page.** A user sees their open sessions, each session's enabled servers, and its recent calls. They don't see other users' sessions. An admin sees all.
  Tests: `internal/ui: TestAC2_04Sessions`
- **AC-2-05 Audit page.** Filtering by principal, workspace, session, server, tool and time returns exactly the matching events.
  Tests: `internal/ui: TestAC2_05AuditFilters`
- **AC-2-06 Restart.** A user restarts their outdated instance from the UI. The new instance uses the current template version, and open sessions keep the server enabled.
  Tests: `test/e2e: TestAC2_06RestartFromTheUI`

## Grants and step-up

- **AC-2-07 Grant.** A signed-in laptop user can't enable an `opt-in` server. After creating a grant for it in the UI, they can. After the grant expires or is revoked, enabling is refused, and an already enabled server's tools are withdrawn within 5 seconds.
  Tests: `internal/gateway: TestAC2_07Grant`
- **AC-2-08 Session-narrowed grant.** A grant narrowed to one session lets that session enable the server and no other session of the same user.
  Tests: `internal/gateway: TestAC2_08SessionNarrowedGrant`
- **AC-2-09 Step-up.** Creating a grant or approving a call when the user last authenticated more than 5 minutes ago sends them to the IdP to sign in again first.
  Tests: `internal/ui: TestAC2_09GrantNeedsAFreshSignIn`
- **AC-2-10 Per-tool environment.** On a server available to the user, a tool marked `environments: attested` is missing from the laptop user's tool list, and present for an attested pod.
  Tests: `test/e2e: TestAC2_10PerToolEnvironment`

## Approvals

- **AC-2-11 Approval in a session (manual).** In Claude Code, a call to a tool marked `approval: required` opens the approval page in the browser. Approving lets the call finish. Denying returns an error saying it was denied.
- **AC-2-12 No elicitation.** For a client that doesn't support elicitation, the same call fails at once with an error naming the approval needed. The approval page still lists it, so the user can approve and retry.
  Tests: `test/e2e: TestAC2_12ApprovalOnThePage`
- **AC-2-13 Timeout.** An approval nobody acts on fails the call after the approval timeout.
  Tests: `test/e2e: TestAC2_13ApprovalTimeout`

## Blocks

- **AC-2-14 Temporary block.** An admin's temporary block from the UI applies to open sessions within 5 seconds, and ends by itself at its expiry.
  Tests: `test/e2e: TestAC2_14TemporaryBlock`
- **AC-2-15 Standing rules shown.** The Blocks page lists the `ToolgateRule` objects from git, read-only.
  Tests: `test/e2e: TestAC2_15StandingRulesShown`

## Templates and secrets

- **AC-2-16 HelmRelease template.** A template made of an OCIRepository and a HelmRelease becomes ready when Flux reports the release ready, serves tools, and is fully removed when it stops.
  Tests: `test/e2e: TestAC2_16HelmReleaseTemplate`
- **AC-2-17 Failing HelmRelease.** A HelmRelease that fails to install fails `toolgate_enable_server` with the HelmRelease's reason.
  Tests: `test/e2e: TestAC2_17FailingHelmRelease`
- **AC-2-18 Resolver.** A template using `${resolver:test}` gives the test server the value from the test resolver. When that value expires, the instance is restarted with a fresh value.
  Tests: `test/e2e: TestAC2_18Resolver`

## Audit payloads

- **AC-2-19 Payloads.** With payloads on, a tool result over 64 KiB is stored in S3 and viewable from its audit event in the UI. Smaller ones are stored inline. With payloads off, neither is stored.
  Tests: `test/e2e: TestAC2_19AuditPayloads`

## Release

- **AC-2-20 Release.** Release `0.2.0` is published, with user docs for grants and approvals and admin docs for the UI, step-up and HelmRelease templates.
