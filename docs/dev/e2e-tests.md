# End-to-end tests

For: people who change the toolgate code.

End-to-end (e2e) tests are Go tests with the `//go:build e2e` tag. They run against a Kubernetes cluster that has toolgate installed. The tests are in `test/e2e`.

## Run the tests

You need Docker and the dev shell. The dev shell gives `kind`, `kubectl`, `helm` and `flux`.

- `make test-e2e` runs every e2e test.
- `make acceptance` runs every acceptance test and prints the scoreboard for the phase checklist. An acceptance test is a test with a scenario tag such as `[AC-1-07]` in its name. The last line of the scoreboard gives the number of passing scenarios, for example `passing 3/30`.

Both targets use `scripts/e2e/run.sh`. The script does these steps:

1. It builds the gateway image from your checkout with `scripts/build-images.sh` and the `e2e` build tag (`GO_TAGS=e2e`). See [The test time scale](#the-test-time-scale).
2. It builds the image of the test MCP server from `test/mcpserver`.
3. It creates a kind cluster with a new name, `toolgate-<worktree>-<random>`, and loads the two images into it.
4. It installs Flux.
5. It installs PostgreSQL, an S3 server, an OCI registry and the telemetry test backend (`grafana/otel-lgtm`: a collector, Prometheus, Tempo and Loki in one pod) in the namespace `toolgate-e2e`. It port-forwards the backend to the host, and the test OTLP receiver passes every export on to it. Tests query it through `TOOLGATE_E2E_PROMETHEUS_URL`, `TOOLGATE_E2E_LOKI_URL` and `TOOLGATE_E2E_TEMPO_URL`. PostgreSQL and S3 use the images that `internal/testutil` pins. The script then pushes the test chart (see below) to the registry.
6. It installs the chart from `deploy/charts/toolgate` as the release `toolgate` in the namespace `toolgate-system`, and waits until the gateway is ready. The value `rbac.extraRules` lets the gateway create the Flux kinds HelmRelease and OCIRepository. The install also sets `config.workspaceAttestation` for the workload namespace (with the SPIFFE ID pattern for `allowed.test`) and gives the gateway a new secrets keyset, made by `test/keyset/cmd/keyset`.
7. It runs the tests.
8. It deletes the cluster. It also does this when a step fails.

If you set `E2E_CLUSTER`, the script uses that cluster name. If you set `E2E_KEEP_CLUSTER=true`, the script keeps the cluster after the tests, so that you can look at it. You must then delete the cluster yourself with `kind delete cluster --name <name>`.

## The test time scale

Some tests wait for product durations, for example the 1-minute minimum of an approval timeout. To make these tests faster, the e2e gateway image can make such durations shorter.

The package `internal/testtime` has the function `Scale(d)`. Gateway code calls it on a fixed wait or a minimum duration:

- In a normal build, such as the release image from `make build-images`, `Scale` returns `d` unchanged. It never reads an environment variable.
- In a build with the `e2e` build tag, `Scale` multiplies `d` by the value of `TOOLGATE_TEST_TIME_SCALE`. If the variable is not set, or its value is not a positive number, `Scale` returns `d` unchanged.

The gateway scales the durations that it stores or waits for. It does not scale the values in the configuration, the forms of the web UI, their limits or the messages. For example, a temporary block of "1 minute" from the Blocks page lasts 6 seconds with the scale `0.1`, and the page still shows 1 minute. These durations use the scale:

- The approval timeout (`approvalTimeout`), the interval at which a waiting call reads its approval again, and the 1 minute after which the leader expires a pending approval.
- The duration of a temporary block and of a grant.
- `sessionIdleTimeout` and `sessionResumeWindow`.

`scripts/e2e/run.sh` builds the gateway image with the `e2e` tag. It sets `TOOLGATE_TEST_TIME_SCALE` to `0.1` in the chart (through `config.extraEnv`) and for the tests. To use a different value, set `TOOLGATE_TEST_TIME_SCALE` before you run the script. The default values of the chart never set the variable.

The runtime in `internal/runtime` scales these waits:

- The 30-second interval between two full passes of the reconciler (`DefaultPassInterval`).
- The 60 seconds that an object's `Ready` condition must stay `False` before the object counts as failed (`ReadyFailureHold`).
- The 2-minute minimum validity of a resolved value (`MinResolvedLifetime`), and the 60 seconds before its expiry when the instance is replaced (`ResolverExpiryMargin`).
- The interval of a custom health check and of the MCP ping. The health check timeout bounds a real request, so the runtime does not scale it.

The CRD limits for a health check do not change: the `interval` is 10 seconds to 10 minutes, and the `timeout` is 1 second to 60 seconds. The API server enforces them. The tests use the smallest values that the CRD allows, and the e2e gateway then checks every second.

The e2e tests are built with the `e2e` tag too, so they read the same scale. To get the deadline for a scaled wait, call `scaled(d)` in `test/e2e`. Do not write the scaled number of seconds in the test. `TestTimeScaleMatchesGateway` makes sure that the gateway and the tests use the same value.

## Run one shard

A shard is one part of the tests. If you set `E2E_SHARD=i/n`, `make test-e2e` and `make acceptance` run only shard `i` of `n` shards. For example, `E2E_SHARD=2/3 make test-e2e` runs the second of three shards. If you do not set `E2E_SHARD`, the targets run all the tests on one cluster.

`scripts/e2e/shard.py` selects the tests of a shard. It gets the names of the top-level tests from `go test -list`. It reads the recorded test durations in `test/e2e/durations.json` (test name to seconds). It puts the tests with a recorded duration into shards with greedy bin packing: longest test first, each into the shard with the least total time so far. So the shards take about the same time. A test with no recorded duration goes into the shard that the SHA-256 hash of the test name gives, modulo `n`. Thus each test is in exactly one shard, and a new test goes into a shard automatically. You do not update a list.

To refresh the durations, download the `acceptance-results-*` artifacts of a recent `master` CI run, for example with `gh run download <run-id> -p 'acceptance-results-*'`. Then run `scripts/e2e/refresh-durations.py` with the `go-e2e-shard-*.json` files from the artifacts as arguments, and commit the new `test/e2e/durations.json`. The script reads the `go test -json` events and records the elapsed time of each top-level test.

With `E2E_SHARD`, `make acceptance` writes the results to `build/acceptance/results/go-acceptance-shard-i.json`, and its scoreboard shows only the tests of that shard.

## In CI

In CI, the `e2e` job has three shards. Each shard runs on its own kind cluster and runs its part of the e2e tests, acceptance tests included. A failing test fails the shard. The `unit`, `integration` and `e2e` jobs each write their `go test -json` output (`GO_TEST_JSON=file` does this for `make test` and `make test-integration`) and upload it. Then the `e2e-scoreboard` job puts all these results together with `scripts/ci/e2e-scoreboard.sh`. It does not run any test. It prints one scoreboard for each phase listed in `ACCEPTANCE_PHASES` (a workflow `env` value in `.github/workflows/ci.yml`) and writes them to the job summary. If a test ran in more than one job, it counts one time: it fails if it failed in any job, and passes if it passed in any job. A change only under `docs/acceptance/` runs the `scoreboard-tests` job (the tests of the scoreboard script) and the docs checks, not the other test jobs.

## The fake identity provider

The fake OIDC provider of `internal/testutil` runs in the test process, not in the cluster. `TestMain` in `test/e2e` starts it. The provider listens on the address of the host in the Docker network of kind. Thus the tests and the gateway pods use the same issuer URL. The chart is installed with this issuer URL and the audience `toolgate`. To get the provider in a test, call `oidcProvider(t)`.

## Browser tests

Browser tests drive a headless Chromium with `github.com/chromedp/chromedp`. The dev shell puts `chromium` on PATH on Linux. To use another Chromium binary, set `TOOLGATE_CHROMIUM` to its path. If the tests find no Chromium, they skip.

These helpers of `internal/testutil` are for browser tests:

- `StartBrowser(t)` starts a new browser with no cookies and returns a chromedp context for it. The browser stops when the test ends.
- `SignInToOIDC(user)` is a chromedp action. It fills the sign-in page of the fake identity provider and sends it. The page signs the browser in as `user`, with its email, `email_verified` and groups.

The fake identity provider has a second client for the UI, `toolgate-ui`. This client uses the authorization code flow and must use PKCE (a code challenge in each sign-in request). Its redirect URI is `http://127.0.0.1:*/ui/auth/callback`, so a test can reach the gateway through a port-forward on any local port. You can change the client ID and the redirect URIs with `OIDCOptions`.

The provider keeps a session in a browser cookie after a sign-in. On the next sign-in request, the provider uses the session and does not show the sign-in page. It shows the page again in these cases:

- The request has `prompt=login`.
- The request has a `max_age` that is shorter than the age of the session.
- The browser has no session.

The fake identity provider has a second authorization code client for connectors. Its client ID is `provider.ConnectorClientID`, `toolgate-connector` by default. Set `OIDCOptions.ConnectorClientID` and `ConnectorRedirectURIs` to change it. It is a public client that must use PKCE. The provider puts the client ID in the audience of its tokens, so the connector client ID is the audience of its access tokens. The go-sdk's `auth.AuthorizationCodeHandler` works with it: use `PreregisteredClient` with this client ID, and a scope filter that asks for `openid`, `email` and `profile`. It signs in through the same sign-in page as the UI client. `TestOIDCProviderConnectorClient` in `internal/testutil` shows the flow.

The ID token has `auth_time`, the time of the sign-in that made the session. If you set `OIDCOptions.OmitAuthTime`, the provider leaves `auth_time` out of the ID tokens of the UI client.

`TestBrowserSignsInThroughGatewayIdP` in `test/e2e` shows a full sign-in. It reads the issuer from the gateway at `/.well-known/toolgate-client`, opens the sign-in page, and signs in as a user with groups.

## The test MCP server

`test/mcpserver` is a small MCP server. Tests use it as the workload of a catalog template, to see what the gateway put into the pod. It serves streamable HTTP at `/mcp` on port 8080. Its tools are:

- `env` returns the environment variables of the server.
- `secrets` returns the files in a directory, by file name. Mount a Secret at that directory.
- Any other tool name returns its name and its arguments.

These settings control the server:

| Environment variable | Flag | Default | Meaning |
|---|---|---|---|
| `MCPSERVER_TOOLS` | `-tools` | `env,secrets` | The tool names, separated by commas. |
| `MCPSERVER_SECRETS_DIR` | `-secrets-dir` | `/secrets` | The directory that the `secrets` tool reads. |
| `MCPSERVER_LISTEN` | `-listen` | `:8080` | The listen address. |
| `MCPSERVER_MODE` | `-mode` | `default` | `default`, or `2026-only`. See below. |
| `MCPSERVER_TOOLS_FILE` | `-tools-file` | none | A file that declares the tools. See below. |

`GET /calls` returns the number of calls to each tool, as a JSON object from tool name to count. A tool that was never called is not in it. Every mode counts calls.

**Mode `2026-only`.** The server is stateless and speaks only protocol revision 2026-07-28. It refuses a request for any other revision with HTTP 400. Its tools are the names in `-tools`, and they echo their arguments, unless you set `-tools-file`.

**Tools from a file.** With `-tools-file`, the server serves the tools that the file declares. The file is a JSON array of `{"name", "description", "inputSchema"}` objects, or an object with that array in `tools`. `inputSchema` must have type `object`. If you leave it out, the tool takes any arguments. Each tool echoes its arguments. Use this to serve any declared tool list, such as the servers of the tool search fixture.

**Changing the tool list.** In `2026-only` mode, and with `-tools-file`, `PUT /tools` replaces the tool list. The body has the format of the file. Or use `PUT /tools?names=a,b` for echo tools with no schema. The server tells open sessions that the list changed. A request with a bad list gets HTTP 400 and changes nothing. In the default mode with no file, the list is fixed and `PUT /tools` gets 404.

The tests get the image name from `TOOLGATE_E2E_MCPSERVER_IMAGE`. The image is only in the kind cluster, so use `imagePullPolicy: Never`.

## The test chart

`test/e2e/charts/mcpserver` is a Helm chart that runs the test MCP server as the Service `mcp`. Tests use it in templates that contain an OCIRepository and a HelmRelease. Its values are:

- `image`: the image of the test MCP server. If you do not set it, the chart does not install. Tests use this to make a HelmRelease fail.
- `tools`: the tool names, separated by commas.

The chart is in the registry of the cluster at `TOOLGATE_E2E_CHART_REPOSITORY`, with the tag `0.1.0`. The registry uses plain HTTP, so set `insecure: true` in the OCIRepository.

## Variables for the tests

`scripts/e2e/run.sh` sets these variables for the tests:

- `KUBECONFIG`: the kubeconfig file of the cluster.
- `TOOLGATE_TEST_TIME_SCALE`: the factor of the [test time scale](#the-test-time-scale), `0.1` by default.
- `TOOLGATE_E2E_NAMESPACE`: the release namespace, `toolgate-system`.
- `TOOLGATE_E2E_MCPSERVER_IMAGE`: the image of the test MCP server.
- `TOOLGATE_E2E_OIDC_LISTEN` and `TOOLGATE_E2E_OIDC_ISSUER`: the listen address and the issuer URL of the fake identity provider.
- `TOOLGATE_E2E_OTLP_LISTEN`: the listen address of the test OTLP receiver. The test process runs it and collects the spans that the gateway sends. The chart's `config.otlpEndpoint` points to it.
- `TOOLGATE_E2E_S3_ENDPOINT`: the URL of the S3 server in the cluster.
- `TOOLGATE_E2E_UI_LISTEN`: a loopback address. The chart's `config.ui.publicURL` is `https://` and this address, and `config.ui.clientID` is `toolgate-ui`. A browser test calls `webUI(t, endpoint)`, which serves a TLS proxy to a gateway pod at this address. The fake identity provider allows the callback on `https://127.0.0.1:*`. The e2e install sets `config.ui.approvalTimeout` to `1m`, the smallest value, so that a test of an approval timeout runs for about 1 minute.
- `TOOLGATE_E2E_WORKLOAD_NAMESPACE`: a namespace for test pods. The gateway accepts the service account tokens of pods in this namespace, so a test can connect as an attested workload.
- `TOOLGATE_E2E_DATABASE_URL`: a connection string for the gateway's PostgreSQL database, through a port-forward. Whenever the fake identity provider issues a token for a principal, the harness inserts a `user_scopes` row for it with `verified_default` and `unverified_default` both `all`, through `store.EnsureUserScope`. Without this row the workspace scoping step would show the principal no servers, because the e2e install turns the web UI on. The insert does nothing when the row exists. A test that needs other defaults, such as the tests of workspace scoping, writes its own row for its own principal before that principal first gets a token. The harness then leaves the row as it is.
- `TOOLGATE_E2E_CHART_REPOSITORY`: the OCI repository of the test chart, as Flux in the cluster reaches it.
- `TOOLGATE_E2E_SPIFFE_LISTEN`: the listen address of the SPIFFE bundle endpoint of the trust domain `allowed.test`. The test process serves it over HTTPS with the certificate of the test resolver. The chart's `config.spiffe.trustDomains` points to it.

If a variable is not set, the tests that need it skip.
