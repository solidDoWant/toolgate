# toolgate for developers

For: people who change the toolgate code.

toolgate is in early development. The pages on the architecture are not written yet. They will describe:

- the architecture and the packages under `internal/`;
- how to build and test: `make help` lists the commands, and the test tiers are unit, integration (PostgreSQL and S3 in Docker) and e2e (kind);
- how acceptance tests are tagged and counted by `make acceptance`;

[Integration tests](integration-tests.md) describes the dependencies that integration tests start.

[End-to-end tests](e2e-tests.md) tells how the e2e tests run on kind, and describes the test MCP server.

[The OTLP fixture](otlp-fixture.md) tells how the recorded Claude Code telemetry was made, and what its content attributes are.

[Claude Code integration](claude-code-integration.md) tells how the client gets the Claude Code session ID.

Until then, read [`SPEC.md`](../../SPEC.md) and the design for the current phase in [`docs/design/`](../design/phase-1.md).

## The gateway image

`make build-images VERSION=x.y.z` builds the gateway image `ghcr.io/soliddowant/toolgate:x.y.z` for linux/amd64 and linux/arm64. The script `scripts/build-images.sh` does these steps:

1. It builds a static `toolgate` binary for each architecture with Go (`CGO_ENABLED=0`, `-trimpath`).
2. It runs `govulncheck -mode=binary` on each binary. If govulncheck reports a vulnerability that the code can reach, the target stops and builds no image.
3. It puts each binary into an image with Nix `dockerTools` (the `image-amd64` and `image-arm64` packages in `flake.nix`).
4. It loads the image for the architecture of your machine into Docker.

The image holds only the binary at `/bin/toolgate` and the CA certificates at `/etc/ssl/certs/ca-certificates.crt`. It runs as UID and GID 65532, not as root. All times in the image are fixed to 1970. The same source and the same `flake.lock` give the same image.

We use Nix `dockerTools` and not `ko`. Nix pins the CA certificates through `flake.lock`, and the image needs no base image from a registry. Nix reads the binary that Go built, so the flake has no `vendorHash` to update when Go modules change. For this reason, the build uses `nix build --impure`.

If you set `PUSH_ALL=true`, the target also pushes both images and a multi-architecture index to `$IMAGE:$VERSION` with `crane`. Only releases do this. `scripts/build-images_test.sh` tests the target. `make test` runs it, and it skips when Nix or Docker is not available.

## Making a release

`make release VERSION=x.y.z` makes a dry run of a release. Use `x.y.z-rc.K` for a release candidate. The script `scripts/release.sh` does these steps:

1. It reads the `## x.y.z` section of `CHANGELOG.md`. The text of this section becomes the release notes. If the section is missing or empty, the script stops before it builds anything.
2. It builds the gateway image with `scripts/build-images.sh` and loads it into Docker. It does not push the image.
3. It packages the Helm chart with the chart version and the app version set to `x.y.z`.
4. It builds the CLI binaries for Linux, macOS and Windows on amd64 and arm64, and writes their SHA-256 checksums to `checksums.txt`.
5. It prints the tag, the image, the chart, the release notes and the files that a publish would make.

All the files go to `build/release/x.y.z/`.

If you set `PUSH_ALL=true`, the script publishes the release. Before it builds, it stops if the working tree has changes, if `HEAD` is not on `origin/master`, or if the tag exists already. After the build, it creates the tag `x.y.z`, pushes the tag and creates the GitHub release with `gh`. The release notes are the `CHANGELOG.md` section, never the notes that GitHub generates. The release has the CLI binaries attached. A release candidate is marked as a pre-release.

The tag starts the `release` workflow (`.github/workflows/release.yml`). The workflow uses its own `GITHUB_TOKEN` to push the image `ghcr.io/soliddowant/toolgate:x.y.z` and the chart `oci://ghcr.io/soliddowant/charts/toolgate`. For a final version, it also tags the image `latest`. Only the release-manager agent publishes releases.

`scripts/release_test.sh` tests the script in a copy of the repository with a local remote and a stub for `gh`. `make test` runs it.

## The vendored htmx

The web UI uses htmx 2.0.7. The file `internal/ui/static/htmx-2.0.7.min.js` is in the repository and is embedded in the binary. There is no JavaScript build step.

`make vendor-htmx` downloads the package from the npm registry. It compares the package with the registry's `integrity` value, writes the file, and writes its SHA-256 to `internal/ui/static/SHA256SUMS`. `make check-generate` fails when the file and `SHA256SUMS` do not match. The version is in the file name, so a browser never uses an old copy from its cache with new pages. To change the version, change it in `scripts/vendor-htmx.sh`, in the file name in the layout template and in the tests.

[openapi.yaml](openapi.yaml) describes the gateway's `/api/v1/` routes. `make generate` writes it from the operations that each API package exports, and `make check-generate` fails when it is stale.
