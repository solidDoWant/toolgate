# Operations

For: admins who keep toolgate running.

This page tells you what to back up, how to upgrade, how to rotate the key for user secrets, how to limit the number of servers, and how to remove toolgate.

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

## Remove toolgate

When you uninstall the chart, the namespaces of running MCP servers stay. [Uninstall](chart.md#uninstall) on the chart page gives the command that deletes them, and tells you what happens to the CRDs and your objects.
