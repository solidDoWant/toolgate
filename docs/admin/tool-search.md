# Tool search

Tool search lets a session find tools by describing a task, instead of listing every tool of every server. Users turn it on with a header. You can also turn it on for all sessions with `config.toolSearch`. See [Tool search](../user/tool-search.md) for how users see it.

By default, search matches words. It stems the words of the task, adds a short list of general synonyms (for example "folder" and "directory"), and ranks tools in PostgreSQL. You do not need to set up anything for this.

## Optional embeddings

Words alone miss tasks that share no word with the tool. For example, "Catch me up on #general" does not share a word with a "channel history" tool. If you set an embeddings endpoint, search also ranks by meaning. The gateway then combines the word rank and the meaning rank.

| Chart value | Meaning |
|---|---|
| `config.toolSearchEmbeddings.url` | Base URL of an endpoint that speaks the OpenAI embeddings API (`POST <url>/v1/embeddings`). Examples are Hugging Face text-embeddings-inference and Ollama. Empty means words only. |
| `config.toolSearchEmbeddings.model` | Model name sent in each request, for example `BAAI/bge-base-en-v1.5`. Required when `url` is set. |
| `config.toolSearchEmbeddings.apiKeyFile` | Path of a file with an API key, mounted from a Secret. The gateway sends it as `Authorization: Bearer`. Optional. |
| `config.toolSearchEmbeddings.timeout` | Time limit of one request. The default is `2s`. |

How it works:

- The leader gateway embeds each tool when it rebuilds the search rows. If you change `model`, it embeds all tools again.
- Each search embeds the task text once.
- If the endpoint is down, slow or returns an error, the search uses words only and still answers. The metric `toolgate_tool_search_embedding_errors_total` counts the failures. A tool that could not be embedded is tried again at the next sync.
- The gateway stores the vectors in PostgreSQL and ranks them with the pgvector extension. The migration runs `CREATE EXTENSION vector` when the gateway starts. See below for the database image.

The gateway sends the search text and the tool names and descriptions to the endpoint. Run the endpoint in your cluster. Allow egress from the gateway pods only to the host and port of the endpoint. The chart does not add a network policy for the gateway pods, so add one that fits your cluster.

## Database image

The extension `vector` (pgvector) must be available in your PostgreSQL. The migration needs it, even when you set no endpoint. Use one of these:

- A standard CloudNativePG image, such as `ghcr.io/cloudnative-pg/postgresql:17-standard-trixie`. It includes pgvector.
- A minimal PostgreSQL image, plus the pgvector extension image mounted as a CloudNativePG image volume extension.

The database user that runs the migration must be allowed to create the extension. `pgvector` and `pg_trgm` are trusted extensions, so the database owner can do it.
