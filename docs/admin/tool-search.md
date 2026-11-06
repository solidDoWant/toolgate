# Tool search

Tool search lets a session find tools by describing a task, instead of listing every tool of every server. Users turn it on with a header. You can also turn it on for all sessions with `config.toolSearch`. See [Tool search](../user/tool-search.md) for how users see it.

By default, search matches words. It stems the words of the task, adds a short list of general synonyms (for example "folder" and "directory"), and ranks tools in PostgreSQL. You do not need to set up anything for this.

## Optional embeddings

Words alone miss tasks that share no word with the tool. For example, "Catch me up on #general" does not share a word with a "channel history" tool. If you set an embeddings endpoint, search also ranks by meaning. The gateway then combines the word rank and the meaning rank.

| Chart value | Meaning |
|---|---|
| `config.toolSearchEmbeddings.url` | Base URL of an endpoint that speaks the OpenAI embeddings API (`POST <url>/v1/embeddings`). Examples are Hugging Face text-embeddings-inference and Ollama. Empty means words only. |
| `config.toolSearchEmbeddings.model` | Model name sent in each request, for example `BAAI/bge-small-en-v1.5`. Required when `url` is set. |
| `config.toolSearchEmbeddings.apiKeyFile` | Path of a file with an API key, mounted from a Secret. The gateway sends it as `Authorization: Bearer`. Optional. |
| `config.toolSearchEmbeddings.timeout` | Time limit of one request. The default is `2s`. |

How it works:

- The leader gateway embeds each tool when it rebuilds the search rows. If you change `model`, it embeds all tools again.
- Each search embeds the task text once.
- If the endpoint is down, slow or returns an error, the search uses words only and still answers. The result then has a `note` that tells the model the search used keywords only, and how to reword the request. The metric `toolgate_tool_search_embedding_errors_total` counts the failures. Its `use` label is `query` or `document`, and its `reason` label is `error` or `circuit_open`.
- After a call fails, the gateway skips that endpoint for 30 seconds and then tries it again. One server outage does not add the timeout to every search. A skipped call counts with the reason `circuit_open`.
- A search that finds nothing returns an empty list and a `note` that asks for a specific action and object. This is a normal result, not an error.
- A tool that could not be embedded is tried again at the next sync.
- The gateway stores the vectors in PostgreSQL and ranks them with the pgvector extension. The migration runs `CREATE EXTENSION vector` when the gateway starts. See below for the database image.

## Run the model server with the chart

You can let the chart run the embeddings server. It is off by default. It is a plain Deployment and Service of Hugging Face text-embeddings-inference.

| Chart value | Meaning |
|---|---|
| `embeddings.server.enabled` | Run the embeddings server. The chart then sets `config.toolSearchEmbeddings.url` and `model`. A `url` you set yourself wins. |
| `embeddings.server.model` | The Hugging Face model. The default is `BAAI/bge-small-en-v1.5`. |
| `embeddings.server.modelRevision` | A full commit SHA of the model repository. The Job downloads exactly this revision. |
| `embeddings.server.image` | The server image, pinned by digest. The default CPU image runs on x86-64 only. On ARM64, set another image. |
| `embeddings.server.cache.existingClaim` | A PersistentVolumeClaim for the model files. See below. |

How the model gets to the server:

1. Before each install and upgrade, a Helm hook Job downloads the model at `modelRevision` into the cache volume. It runs the `hf download` command of `huggingface_hub`, from a pinned `uv` image. Helm waits for the Job to finish.
2. The server starts from the local directory (`--model-id /models/embeddings`) with `HF_HUB_OFFLINE=1`. It needs no network access. Only the Job needs internet egress: `huggingface.co`, `*.huggingface.co` and `*.hf.co` on port 443, and `pypi.org` and `files.pythonhosted.org` to install `huggingface_hub`.

Create the cache claim yourself and set `embeddings.server.cache.existingClaim`. Use a claim with access mode `ReadWriteMany`. Then the files survive an uninstall, and the Job in an upgrade can mount the claim while the server runs. If you set no claim, the chart creates one that Helm keeps after an uninstall.

The first install takes longer, because the Job downloads the model. Raise the Helm `--timeout`, or the timeout of the Flux HelmRelease.

Memory: the embeddings server (`bge-small-en-v1.5`) used about 320 MiB, measured after the model was loaded and had served requests. The chart sets the memory request equal to the limit, and no CPU limit. The server has one replica. While it is down, searches use keywords only (see above).

The chart adds no NetworkPolicy. Add your own: allow ingress to the servers only from the gateway pods, on port 80.

To check a server, run this in a pod that the policy allows:

```
curl -s http://<release>-toolgate-embeddings/v1/embeddings -H 'Content-Type: application/json' -d '{"model":"BAAI/bge-small-en-v1.5","input":["list channels"]}'
```

## Database image

The extension `vector` (pgvector) must be available in your PostgreSQL. The migration needs it, even when you set no endpoint. Use one of these:

- A standard CloudNativePG image, such as `ghcr.io/cloudnative-pg/postgresql:17-standard-trixie`. It includes pgvector.
- A minimal PostgreSQL image, plus the pgvector extension image mounted as a CloudNativePG image volume extension.

The database user that runs the migration must be allowed to create the extensions `vector` and `pg_trgm`. `pg_trgm` is a trusted extension, so the database owner can create it. `vector` is not trusted in the standard images, so a superuser must create it, or the migration user must be a superuser. If a superuser has run `CREATE EXTENSION vector` in the database, the migration skips the step. The split-role SQL in the chart page does this.
