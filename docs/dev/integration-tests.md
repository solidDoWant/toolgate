# Integration tests

For: people who change the toolgate code.

Integration tests are Go tests with the `//go:build integration` tag. Run them with `make test-integration`. They use real dependencies that the package `internal/testutil` starts for each test:

- `StartPostgres` gives the test its own database on a PostgreSQL 17 server in Docker. All tests of one test binary share that server, which starts on the first call. Each database is a copy of a template that the binary migrates once. The database is named `t_` and a random string, and it is dropped when the test ends. The server accepts TCP connections only over TLS with a client certificate. The common name of the certificate must be the database user. `ConnString` returns a URL that uses the client certificate and verifies the server certificate. `IssueClientCert` makes more client certificates.
- `StartPostgresEmpty` is the same, but the database has no tables. Use it to test the migrations.
- `StartPostgresDedicated` starts a server for one test, with an empty database. Use it when the test stops or restarts the server, ends connections of other databases, changes server settings (`ALTER SYSTEM`, `pg_hba`), or depends on something that spans the whole server. Roles belong to the whole server, so a test on the shared server names each role with `pg.RoleName(...)`, which adds the prefix of the test's database. A test in `internal/testutil` fails when a test runs `ALTER SYSTEM`, `CREATE ROLE` without `RoleName`, or `pg_terminate_backend` without `current_database()`, and does not use `StartPostgresDedicated`.
- `StartS3` starts the Versity S3 Gateway (`versity/versitygw`) in Docker and creates one empty bucket. `Client` returns an AWS SDK client for it with path-style addressing.
- `StartOIDCProvider` starts a fake OIDC provider in the test process. It needs no Docker. The e2e harness can pass its own listener and public issuer URL.
- `StartEnvtest` starts a real Kubernetes API server and etcd in the test process with envtest (see [envtest tests](#envtest-tests)). It needs no Docker.

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

## envtest tests

envtest is the test environment of controller-runtime (`sigs.k8s.io/controller-runtime/pkg/envtest`). It runs `kube-apiserver` and `etcd` as local processes. The dev shell gets both programs from nixpkgs and sets `KUBEBUILDER_ASSETS` to the directory that holds them. The tests download nothing.

`StartEnvtest` installs the toolgate CRDs from `config/crd/bases`, the same files the chart installs. It returns an `Envtest` value with the REST configuration, the scheme and a controller-runtime client. The client reads from the API server directly and has no cache. The helper stops the API server and etcd when the test ends.

To write an envtest test:

1. Put the test in a file with the `//go:build integration` tag.
2. Call `env := testutil.StartEnvtest(t)` at the start of the test.
3. Use `env.Client` to apply, read and update objects.

If `KUBEBUILDER_ASSETS` is not set, `StartEnvtest` skips the test. Run the tests in the dev shell to set it.

### Which kind of test to write

Use the fake client of controller-runtime for unit tests of logic that only reads and writes objects. It is fast, but it does not do server-side apply, status subresources, CEL validation or admission policies as a real API server does.

Use envtest when the result depends on the API server. Examples are server-side apply with field managers, updates through the status subresource, CRD defaults and CEL validation rules.

Use the [kind e2e tests](e2e-tests.md) when the test needs controllers or pods. envtest runs no controllers and no kubelet. A Deployment never gets pods, so tests for real readiness stay in the e2e tier.

## Without Docker

If no Docker daemon answers, `StartPostgres` and `StartS3` skip the test with the message "integration dependencies need Docker, and Docker is not available". If `DOCKER_HOST` is set, only that daemon counts.

## Cleanup

`StartPostgresDedicated` and the other helpers remove their container when the test ends, also when the test fails. The shared PostgreSQL server lives until the test binary exits; the reaper removes it then. If the test process is killed, the testcontainers reaper (Ryuk) removes the containers some seconds later.
