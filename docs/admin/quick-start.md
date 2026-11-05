# Quick start

For: admins who want to install toolgate in a test cluster.

This page installs the toolgate Helm chart with the smallest set of values. The result is two gateway pods that answer `/healthz`. [The chart page](chart.md) tells you what the chart installs.

## Before you start

You need these items:

- A Kubernetes 1.35 cluster, with Flux v2 installed.
- A PostgreSQL database that the gateway can reach, and its connection string.
- An S3 bucket.
- An OIDC identity provider that users sign in with. You need its issuer URL and the audience of its access tokens for toolgate.
- Helm 3.8 or later, and `kubectl` with cluster-admin rights.

## Install the chart

1. Create the namespace for the gateway:

   ```sh
   kubectl create namespace toolgate-system
   ```

2. Put the PostgreSQL connection string in a Secret in that namespace. The key must be `url`:

   ```sh
   kubectl --namespace toolgate-system create secret generic toolgate-database \
     --from-literal=url='postgres://toolgate:PASSWORD@postgres.example:5432/toolgate'
   ```

3. Write the quick-start values to a file named `values.yaml`. Use the name of the Secret from step 2, and the issuer URL and audience of your identity provider:

   ```yaml
   config:
     database:
       existingSecret: toolgate-database
     oidc:
       issuerURL: https://idp.example.com/realms/main
       audience: toolgate
   ```

4. Install the chart with the release name `toolgate`:

   ```sh
   helm install toolgate oci://ghcr.io/soliddowant/charts/toolgate \
     --namespace toolgate-system --values values.yaml
   ```

5. Wait until the two gateway pods are ready:

   ```sh
   kubectl --namespace toolgate-system rollout status deployment/toolgate
   ```

The pods are ready when the gateway can connect to PostgreSQL. If they do not become ready, read the logs with `kubectl --namespace toolgate-system logs deployment/toolgate`.

## Next steps

- [The chart values reference](chart-values.md) lists every value of the chart.
- [The gateway configuration reference](configuration.md) lists the settings of the gateway. Set them with the `config.extraEnv` chart value.
