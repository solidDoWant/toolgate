# Integration tests

For: people who change the toolgate code.

Integration tests are Go tests with the `//go:build integration` tag. Run them with `make test-integration`. They use real dependencies that the package `internal/testutil` starts for each test:

- `StartPostgres` starts PostgreSQL 17 in Docker. The server accepts TCP connections only over TLS with a client certificate. The common name of the certificate must be the database user. `ConnString` returns a URL that uses the client certificate and verifies the server certificate. `IssueClientCert` makes more client certificates.
- `StartS3` starts the Versity S3 Gateway (`versity/versitygw`) in Docker and creates one empty bucket. `Client` returns an AWS SDK client for it with path-style addressing.
- `StartOIDCProvider` starts a fake OIDC provider in the test process. It needs no Docker. The e2e harness can pass its own listener and public issuer URL.

The two images are pinned by digest in `internal/testutil`.

## The fake OIDC provider

The provider uses the OpenID provider of `github.com/zitadel/oidc`. It serves discovery, the keys (JWKS), the device authorization endpoint, the token endpoint and userinfo.

To make a token with chosen claims, call `MintAccessToken` or `MintIDToken` with a `TokenClaims` value. You can set the subject, email, `email_verified`, groups, audience, issuer and expiry. Empty fields get the provider issuer, the provider audience (`toolgate` by default) and an expiry one hour later. The provider signs minted tokens with the same key as the tokens it issues.

To run the device-code flow:

1. Add the user with `AddUser`. Its email and groups go into the tokens.
2. Start the flow with `golang.org/x/oauth2` against the public client `ClientID`.
3. Call `ApproveDevice` with the user code and the subject. This does what the user does in the browser. `DenyDevice` refuses the code.
4. Poll the token endpoint. The access token is a JWT. Its audience is the provider audience and the client ID.

Tokens from the device-code flow expire after 5 minutes. The provider issues no refresh tokens. To test other expiry times, mint the token.

## Without Docker

If no Docker daemon answers, `StartPostgres` and `StartS3` skip the test with the message "integration dependencies need Docker, and Docker is not available". If `DOCKER_HOST` is set, only that daemon counts.

## Cleanup

Each helper removes its container when the test ends, also when the test fails. If the test process is killed, the testcontainers reaper (Ryuk) removes the containers some seconds later.
