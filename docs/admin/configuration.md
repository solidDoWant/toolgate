# Gateway configuration reference

For: admins who deploy the toolgate gateway.

This page is generated from the code by `make generate-docs`. Do not edit it by hand.

The gateway reads its configuration when `toolgate serve` starts, in this order:

1. The default value of each setting.
2. The YAML file named by the `--config` flag, or else by the `TOOLGATE_CONFIG` environment variable. The file is optional. An unknown key in the file is an error.
3. The environment variables. A variable that is set and not empty replaces the value from the file.

If a setting is missing or not valid, the gateway does not start. It prints one line for each problem and exits with code 1.

| YAML key | Environment variable | Default | Description |
|---|---|---|---|
| `listenAddress` | `TOOLGATE_LISTEN_ADDRESS` | `:8080` | The address where the gateway listens for MCP and API requests and for the `/healthz` and `/readyz` probes. |
| `metricsListenAddress` | `TOOLGATE_METRICS_LISTEN_ADDRESS` | `:9090` | The address of the separate listener that serves Prometheus metrics at `/metrics`. It must differ from `listenAddress`. |
| `databaseURL` | `TOOLGATE_DATABASE_URL` | none | The PostgreSQL connection string, as a URL or as key/value pairs. Required. `/readyz` answers 503 while the gateway cannot connect to this database. |
| `shutdownTimeout` | `TOOLGATE_SHUTDOWN_TIMEOUT` | `30s` | After SIGTERM, the gateway stops accepting requests and waits this long for open requests to finish. Then it stops. Write it as a Go duration, such as `30s`. |
| `logLevel` | `TOOLGATE_LOG_LEVEL` | `info` | The lowest level of the JSON log lines that the gateway writes to stdout: `debug`, `info`, `warn` or `error`. |
| `otlpEndpoint` | `TOOLGATE_OTLP_ENDPOINT` | none | The OTLP/HTTP endpoint that receives the traces of the gateway, such as `http://otel-collector:4318`. The gateway sends traces to `/v1/traces` under this URL. If it is empty, the gateway does not export traces. |
