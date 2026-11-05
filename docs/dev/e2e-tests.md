# End-to-end tests

For: people who change the toolgate code.

End-to-end (e2e) tests are Go tests with the `//go:build e2e` tag. They run against a Kubernetes cluster that has toolgate installed. The tests are in `test/e2e`.

## Run the tests

You need Docker and the dev shell. The dev shell gives `kind`, `kubectl`, `helm` and `flux`.

- `make test-e2e` runs every e2e test.
- `make acceptance` runs every acceptance test and prints the scoreboard for the phase checklist. An acceptance test is a test with a scenario tag such as `[AC-1-07]` in its name. The last line of the scoreboard gives the number of passing scenarios, for example `passing 3/30`.

Both targets use `scripts/e2e/run.sh`. The script does these steps:

1. It builds the gateway image from your checkout with `scripts/build-images.sh`.
2. It builds the image of the test MCP server from `test/mcpserver`.
3. It creates a kind cluster with a new name, `toolgate-<worktree>-<random>`, and loads the two images into it.
4. It installs Flux.
5. It installs PostgreSQL and an S3 server in the namespace `toolgate-e2e`. It uses the images that `internal/testutil` pins.
6. It installs the chart from `deploy/charts/toolgate` as the release `toolgate` in the namespace `toolgate-system`, and waits until the gateway is ready.
7. It runs the tests.
8. It deletes the cluster. It also does this when a step fails.

If you set `E2E_CLUSTER`, the script uses that cluster name. If you set `E2E_KEEP_CLUSTER=true`, the script keeps the cluster after the tests, so that you can look at it. You must then delete the cluster yourself with `kind delete cluster --name <name>`.

In CI, the `e2e` job runs the e2e tests and then the acceptance scoreboard on one cluster. The job writes the scoreboard to the job summary. A failing e2e test fails the job.

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

The tests get the image name from `TOOLGATE_E2E_MCPSERVER_IMAGE`. The image is only in the kind cluster, so use `imagePullPolicy: Never`.

## Variables for the tests

`scripts/e2e/run.sh` sets these variables for the tests:

- `KUBECONFIG`: the kubeconfig file of the cluster.
- `TOOLGATE_E2E_NAMESPACE`: the release namespace, `toolgate-system`.
- `TOOLGATE_E2E_MCPSERVER_IMAGE`: the image of the test MCP server.
- `TOOLGATE_E2E_OIDC_LISTEN` and `TOOLGATE_E2E_OIDC_ISSUER`: the listen address and the issuer URL of the fake identity provider.
- `TOOLGATE_E2E_OTLP_LISTEN`: the listen address of the test OTLP receiver. The test process runs it and collects the spans that the gateway sends. The chart's `config.otlpEndpoint` points to it.
- `TOOLGATE_E2E_S3_ENDPOINT`: the URL of the S3 server in the cluster.
- `TOOLGATE_E2E_WORKLOAD_NAMESPACE`: a namespace for test pods. The gateway accepts the service account tokens of pods in this namespace, so a test can connect as an attested workload.

If a variable is not set, the tests that need it skip.
