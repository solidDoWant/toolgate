# Persistent volumes

For: admins who want some MCP servers to keep files between runs. You must know Kubernetes storage (PersistentVolumes, StorageClasses) and the gateway chart.

Without this feature, toolgate deletes an instance namespace, and every volume in it, when the instance stops. With it, a template can ask for volumes that outlive the instance. A PersistentVolume (PV) is the cluster object that holds the data. When an instance stops, the PV stays. The next instance with the same template and scope binds to the same PV.

## Servers that benefit

Use a persistent volume for a server that keeps useful state on disk:

- A server with a SQLite or other file cache that is slow to build again.
- A server that runs a browser and keeps a browser profile, with its logins and settings.
- A server that builds a search index.

A server that keeps no state gains nothing. Do not give it a volume.

## Make a Retain StorageClass

toolgate refuses to start a new volume if its StorageClass has a `reclaimPolicy` other than `Retain`. With `Delete`, Kubernetes deletes the data when the claim goes, which happens at each stop. The start fails with a message that names the class and its policy. It also fails if the class does not exist.

Make a class that uses your volume provisioner and sets `reclaimPolicy: Retain`. For example:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ssd-retain
provisioner: ebs.csi.aws.com   # the provisioner that your cluster already uses
reclaimPolicy: Retain
volumeBindingMode: WaitForFirstConsumer
```

Two rules apply to the provisioner:

- It must name each PV `pvc-` followed by the UID of the claim. The default of the CSI external provisioner, the in-tree provisioners and the local-path provisioner do this. With another name, every new volume fails to start, and toolgate changes nothing on the PV.
- Keep no `Available` PV in a class that persistent volumes use. Kubernetes can bind a new claim to such a PV, and the pod can mount it before toolgate fails the start.

Each template names its class in `storageClassName`. [Templates and rules](templates-and-rules.md#persistent-volumes) lists the fields and the scope rules.

## Turn the feature on

Set the chart value `config.persistentVolumes.enabled` to `true`. [The chart page](chart.md#persistent-volumes) lists this value, `unusedRetention` and `limitPerUser`, the ClusterRole rules that the chart adds, and its admission rules. [The security model](security-model.md#persistent-volumes) says why the gateway cannot touch the PVs of other workloads.

Turn the feature on only after every gateway replica runs a version that knows it. An older leader does not know the field. It starts such a template with no volume, and it deletes the namespace without waiting for the pods to stop. Kubernetes keeps the data until the pods are gone, but the server runs without its volume.

If you turn the feature off, these things happen:

- Templates that set `persistentVolumes` become invalid.
- Running instances keep running.
- A start of the last valid version of such a template fails at once, with the reason that the setting is off.
- The gateway does no other volume work. It does not delete anything.
- Volumes and rows stay. If you turn the feature on again, the volumes are used again.

## Who shares a volume

A volume belongs to one template, one volume name and one scope. A `per-template` volume is one volume for the template. A `per-user` volume is one volume for each user. Two users never share a `per-user` volume, and two templates never share a volume, even with the same volume name. The scope of a volume must match the scope of the template. [Templates and rules](templates-and-rules.md#persistent-volumes) has the table.

Only one instance at a time uses a volume. If a new instance wants a volume that the old instance still holds, the new instance waits. A restart takes about two minutes longer for this reason. The wait counts toward the start timeout.

## Size limit

The setting `persistentVolumes.limitPerUser` (default `10Gi`) limits the total size of the `per-user` volumes of one user. The limit applies only when a start makes a new volume. If the new volume does not fit, the instance fails with this message:

> `<server>` needs a `<size>` volume `<name>`, but your volumes already use `<used>` of your `<limit>` limit. Ask an admin to delete a volume you no longer need (toolgate admin volumes).

A volume that exists always binds, even if the user is over the limit. A lower limit deletes nothing. A `per-template` volume counts toward no user. `0` means no limit.

## When toolgate deletes a volume

The gateway deletes a volume in these cases:

- Its template was deleted. The deletion is at once, with no grace time.
- No instance used it for `persistentVolumes.unusedRetention` (default `720h`). `0` turns this off.
- An admin deleted it with `toolgate admin volumes delete`.

The gateway never deletes a volume that an instance uses. A user who comes back before the gateway touched the PV keeps the data.

If you delete and create a template again, the new template has a new UID. It gets new, empty volumes, and the gateway deletes the old ones. A GitOps tool that prunes and creates a template again deletes its data.

To delete a volume, the gateway sets the reclaim policy of the PV to `Delete`. The provisioner then deletes the storage. If the PV is still `Bound` to a claim that is not toolgate's, or the provisioner never deletes it, the gateway forgets the volume after 1 hour. It logs a warning with the PV name. The PV keeps its labels, and the gateway never touches it again. To find these PVs, run:

```sh
kubectl get pv -l app.kubernetes.io/managed-by=toolgate
```

Add `-L toolgate.soliddowant.dev/template,toolgate.soliddowant.dev/volume` to see which template and volume each PV had. The same query finds PVs that the gateway labelled but has no record of, for example after a database restore.

## Admin commands

[Operations](operations.md#list-and-delete-persistent-volumes) describes `toolgate admin volumes list` and `toolgate admin volumes delete`, and the API behind them. A volume that an instance uses is deleted after that instance stops.

## Change a PV or PVC by hand

toolgate trusts only what it recorded and labelled. Follow these rules:

- If you delete the recorded PV, the next start finds it missing. The audit log gets a `volume.lost` event, and the instance gets a new, empty volume. Kubernetes keeps a PV that a running pod uses until the pod stops.
- If a recorded PV loses its toolgate labels, or has the label of another volume, the start fails and toolgate changes nothing on that PV. Clean-up forgets the row without deleting the PV.
- If you change the reclaim policy of a recorded PV to `Delete`, the start fails. toolgate does not bind it.
- If a recorded PV is still `Bound` to an old claim, the start waits and then fails with the claim in the reason.
- If you delete the claim of a stopped instance, nothing is lost. toolgate makes the claim again, bound to the same PV.
- If you delete the instance namespace by hand, the gateway starts the instance again and binds the PV again. If the PV was never recorded, toolgate loses it.
- A change of `size` or `storageClassName` in a template applies to new volumes only. A recorded volume keeps its size and class. toolgate does not expand a volume or move it to another class. To use a new size or class, delete the volume.

## Data stays on disk

An MCP server can write to its volume anything that it receives, including its own tokens. Before, that disk went away at each stop. Now it stays until the gateway deletes the volume. toolgate itself never writes a secret to a volume. [The security model](security-model.md#persistent-volumes) has the details.

## Metrics

[Metrics](metrics.md) lists the volume series. Alert on `delete_failed` and `drain_timeout` events.
