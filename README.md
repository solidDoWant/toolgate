# toolgate

toolgate is a gateway between Claude Code and MCP servers that you host yourself.

Claude Code makes one MCP connection to toolgate. In a session, the model lists the MCP servers that it is allowed to use, and turns them on and off as the task needs them. toolgate starts each server in Kubernetes, for one session or for one user, with that user's own secrets. It checks every tool call against the rules of the owner, and it records every decision.

Admins deploy toolgate with Helm and Flux, and keep the catalog of servers and the rules in git. Users sign in once with the `toolgate` command and a Claude Code plugin.

## Status

toolgate is in early development. Nothing works yet, and there is no release. Autonomous agents build it, one phase at a time. The owner's checklist for each phase is in [`docs/acceptance/`](docs/acceptance/phase-1.md).

## More information

- [`SPEC.md`](SPEC.md) describes the full system.
- [`docs/README.md`](docs/README.md) lists the documentation for users, admins and developers.

## License

toolgate is licensed under the GNU Affero General Public License v3.0. See [`LICENSE`](LICENSE).
