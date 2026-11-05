# toolgate for developers

For: people who change the toolgate code.

toolgate is in early development, and these pages are not written yet. They will describe:

- the architecture and the packages under `internal/`;
- how to build and test: `make help` lists the commands, and the test tiers are unit, integration (PostgreSQL and S3 in Docker) and e2e (kind);
- how acceptance tests are tagged and counted by `make acceptance`;
- how a release is made.

[Integration tests](integration-tests.md) describes the dependencies that integration tests start.

[End-to-end tests](e2e-tests.md) tells how the e2e tests run on kind, and describes the test MCP server.

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
