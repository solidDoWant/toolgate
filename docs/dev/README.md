# toolgate for developers

For: people who change the toolgate code.

toolgate is in early development, and these pages are not written yet. They will describe:

- the architecture and the packages under `internal/`;
- how to build and test: `make help` lists the commands, and the test tiers are unit, integration (PostgreSQL and S3 in Docker) and e2e (kind);
- how acceptance tests are tagged and counted by `make acceptance`;
- how a release is made.

[Integration tests](integration-tests.md) describes the dependencies that integration tests start.

Until then, read [`SPEC.md`](../../SPEC.md) and the design for the current phase in [`docs/design/`](../design/phase-1.md).
