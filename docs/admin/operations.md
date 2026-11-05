# Operations

For: admins who keep toolgate running.

This page tells you what to back up, how to upgrade, how to rotate the key for user secrets, how to limit the number of servers, how to store audit payloads, and how to remove toolgate.

## Back up

toolgate keeps its state in three places:

- Git: your `MCPServerTemplate`, `ToolgateRule` and `SettingsProfile` objects. Flux applies them from Git, so Git is their backup.
- PostgreSQL: the sessions, the instances, the encrypted user secrets and the audit log. Back up the database with the tools that you use for PostgreSQL, for example `pg_dump` or the backups of your database service.
- The keyset for user secrets: a Kubernetes Secret in the namespace of the gateway. Back it up in a different place from the database.

Do not lose the keyset. Without it, nobody can decrypt the user secrets in a database backup, and each user must set their secrets again. A person who has both the database backup and the keyset can read every secret.

To restore, restore the database first. Then start the gateway with the same keyset. The gateway compares the instances in the database with the namespaces in the cluster, and starts or deletes instances to make them agree. [Instance stops and cleanup](instances.md#recovery-after-a-restart-or-a-leader-change) tells how.

## Upgrade

Upgrade the chart with `helm upgrade`, or change the chart version in your Flux `HelmRelease`. Each gateway pod updates the database tables when it starts, before it becomes ready. You do not run a separate command.

During a rolling upgrade, the old pods and the new pods use the same database. A new version only adds tables, columns and indexes, so the old pods continue to work. If you roll back to an older version, the gateway writes a warning to its log and starts. It never changes the tables back.

[The chart page](chart.md#crd-upgrades) tells how the chart updates the CRDs.

## Rotate the key for user secrets

Rotate the key when a person who had the keyset leaves, or on a fixed schedule. [User secrets](user-secrets.md#rotate-the-key) gives the steps. The command `toolgate admin rewrap` encrypts every stored secret again with the new key.

## Limit the number of servers

Two chart values limit the number of running instances:

- `config.limits.instancesPerUser`: the maximum for one user. The default is 20.
- `config.limits.instances`: the maximum for the gateway. The default is 200.

[Instance limits](instances.md#instance-limits) tells you which instances count and what a user sees at the limit. The same page gives metrics and alert examples for the number of instances.

## Read the audit log

The gateway writes each audit event as one line of JSON to its standard output, and stores it in PostgreSQL. Collect the pod logs with the log system that you use for the cluster. The lines on standard output never contain tokens, secrets, tool arguments or tool results.

The gateway deletes audit events that are older than 90 days. It deletes them in batches, once an hour.

### Store tool arguments and results

The gateway can also store audit payloads. A payload is the arguments of a tool call or an approval request, or the result of a tool call. Payloads are off by default. To turn them on, set these chart values:

- `config.audit.payloads`: `true`.
- `config.audit.s3.bucket`: the S3 bucket for large payloads. It is required when payloads are on.
- `config.audit.s3.endpoint`, `config.audit.s3.region` and `config.audit.s3.pathStyle`: how the gateway connects to S3. Many S3-compatible servers need `pathStyle: true`.
- `config.audit.s3.prefix`: the prefix of the object keys, for example `toolgate/`.
- `config.audit.s3.existingSecret`: a Secret with the keys `accessKeyID` and `secretAccessKey`. If it is empty, the gateway uses the other credential sources of the AWS SDK.

A payload under 64 KiB (65,536 bytes) goes in the audit row in PostgreSQL. A larger payload goes to S3, with the key `<prefix>payloads/<yyyy>/<mm>/<dd>/<uuid>.json`. The row holds the key. The gateway writes the object before the row.

When it starts, the gateway reads the object lock configuration of the bucket. If the bucket has object lock, the gateway locks each object in governance mode for 90 days. An admin with the S3 permission to bypass governance mode can still delete the objects. The gateway does not need that permission. If the gateway cannot read the configuration for 30 seconds, it stops with an error.

If the gateway cannot store the arguments of a call, it refuses the call. The user sees "The call couldn't be recorded. Try again later." If the gateway cannot store a result, it still returns the result. The audit row then has a `payload_error` field.

When the gateway deletes old audit events, it deletes their S3 objects first. If S3 refuses to delete an object, the gateway keeps its row and tries again in the next run.

Add a lifecycle rule to the bucket, for the payload prefix. The rule must expire current objects and noncurrent versions after 90 days plus a margin, for example 97 days. The rule removes objects that have no row, for example when a row insert failed. It also removes the old versions that a versioned bucket keeps after a delete.

The gateway serves these metrics on its metrics port:

| Metric | Type | Meaning |
| --- | --- | --- |
| `toolgate_audit_payload_errors_total` | counter | Payloads that the gateway could not store. |
| `toolgate_audit_retention_refused_total` | counter | Old audit rows that the gateway kept for a later run, because S3 refused to delete their objects. |

Alert when either counter goes up. S3 is then not available, or the credentials of the gateway do not allow the operation.

## Remove toolgate

When you uninstall the chart, the namespaces of running MCP servers stay. [Uninstall](chart.md#uninstall) on the chart page gives the command that deletes them, and tells you what happens to the CRDs and your objects.
