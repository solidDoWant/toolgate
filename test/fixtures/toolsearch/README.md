# Tool search fixture

This folder is frozen. It is the fixed input for the tool search quality test. The search worker must not change it. If a change seems needed, ask on the issue.

- `servers.json`: tools from real public MCP servers. Each server records its source repository and licence.
- `tasks.json`: task descriptions written as a user would say them. Each names one expected tool by server and upstream name.

`fixture_test.go` checks the shape of both files.
