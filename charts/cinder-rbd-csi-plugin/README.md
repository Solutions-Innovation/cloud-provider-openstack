# openstack-cinder-rbd-csi

Helm chart for the Cinder RBD CSI plugin (`cinder-rbd.csi.windriver.com`): a
block-only CSI driver that provisions Ceph RBD-backed Cinder volumes and maps
them on worker nodes with exclusive kernel RBD.

> **Status: pre-production.** The driver has not completed production
> qualification. See the [implementation design](../../docs/cinder-csi-plugin/migration/rbd-cinder-csi-implementation-design.md).

## What this driver is for

Migrating VM disks into WRCP. A migration workflow writes a source disk into a
raw-block PVC, and the Cinder volume is **retained** after the PVC is deleted so
the migration Blueprint can attach it to the target VM.

It is not a general-purpose Cinder CSI driver. It supports raw block volumes with
`ReadWriteOnce` only — no filesystems, snapshots, clones, expansion, or
topology.

## Prerequisites

| Requirement | Notes |
|---|---|
| Kubernetes ≥ 1.29 | validated on 1.29.2 |
| Cinder API microversion ≥ 3.27 | mandatory; the controller exits at startup without it |
| A Ceph RBD-backed Cinder volume type | e.g. `ceph-rook-store` |
| `rbd` kernel module loaded on nodes | already loaded where Ceph-CSI is in use |
| The `client.cinder` Ceph key, duplicated into a Secret | see below |
| CDI StorageProfile, if using DataVolumes | rendered by this chart |

## Install

Two things must exist before installing.

**1. Cinder credentials** for the controller:

```bash
kubectl -n kube-system create secret generic cinder-rbd-csi-cloud-config \
  --from-file=cloud.conf=/etc/kubernetes/cloud.conf
```

**2. The Ceph credential** for the node plugin. The operator duplicates the
existing platform key; there is no automated synchronisation:

```bash
kubectl -n openstack get secret cinder-volume-rbd-keyring \
  -o jsonpath='{.data.key}' | base64 -d > /tmp/k && chmod 600 /tmp/k

kubectl -n kube-system create secret generic cinder-rbd-ceph-client \
  --from-literal=userID=cinder --from-file=userKey=/tmp/k

shred -u /tmp/k
```

Then install, supplying the cluster FSID:

```bash
helm install cinder-rbd charts/cinder-rbd-csi-plugin -n kube-system \
  --set driverConfig.rbd.expectedFsid="$(kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph fsid)"
```

## Values that have no safe default

Rendering **fails** rather than proceeding if these are wrong. Each guard exists
because the failure mode is a silent loss of safety, not a visible error.

| Value | Why it is required |
|---|---|
| `driverConfig.rbd.expectedFsid` | Pins the driver to one Ceph cluster. Empty means the node stops verifying that a mapped device belongs to the expected cluster. |
| `cephCredential.secretName` | The node plugin reads its key from this projected Secret. |
| `driverConfig.rbd.exclusive` | Must be `true`. A writable non-exclusive mapping permits a second writer and defeats the Ceph exclusive lock. |
| `driverConfig.rbd.mounter` | Must be `krbd`. Nothing else is implemented. |
| `driverConfig.volume.deleteVolumeMode` | Must be `retain` or `delete`. A typo such as `delet` would otherwise read as retain. |

## Volume retention

`driverConfig.volume.deleteVolumeMode` defaults to **`retain`**: deleting a PVC
removes the driver's metadata and the attachment record but **keeps the Cinder
volume**, because the migration Blueprint needs it to build the target VM.

Set it to `delete` only if you want PVC deletion to destroy the volume. A single
volume can override the default with the metadata key
`csi.rbd.cleanupVolume=true`.

## Security

The node plugin is **privileged** and mounts `/dev` and `/sys` read-write on
every node it runs on. This is inherent to kernel RBD mapping and matches what
platform Ceph-CSI already does on the same nodes, but it means:

- `/sys` **must not** be read-only. `rbd device map` works by writing to
  `/sys/bus/rbd/add`; a read-only mount makes every map fail.
- Access to the driver's ServiceAccount and to the Ceph credential Secret should
  be restricted. The node plugin needs **no** `secrets` RBAC — it reads files
  from a projected volume, not the API.
- Generated keyrings live in a memory-backed `emptyDir` (`runtimeDir`) so key
  material never reaches disk.
- `hostPID` is **not** required, unlike the iSCSI sibling driver.

## Using it

The PVC must be raw block, and pods must use `volumeDevices`:

```yaml
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Block          # required; Filesystem is rejected
  storageClassName: cinder-rbd-migration
```

See [`examples/cinder-rbd-csi-plugin/`](../../examples/cinder-rbd-csi-plugin/).

### CDI

CDI defaults unknown provisioners to `volumeMode: Filesystem`, which this driver
rejects, so DataVolume PVCs hang `Pending` with no useful event. The
`StorageProfile` rendered by this chart fixes that. Its name must equal the
StorageClass name, and it must be **reapplied after every StorageClass
recreation**.

## Verifying a change to this chart

```bash
helm lint charts/cinder-rbd-csi-plugin \
  -f charts/cinder-rbd-csi-plugin/testdata/minimal-valid-values.yaml
bash hack/verify-cinder-rbd-chart.sh
```

The verify script asserts the safety guards fire, `/sys` is writable, no iSCSI
paths leaked in, and no key material appears in rendered output.

## Troubleshooting

See the [operator runbook](../../docs/cinder-csi-plugin/migration/rbd-cinder-csi-operator-runbook.md)
for isolated mappings, duplicate attachment records, exclusive-lock conflicts,
and credential rotation.
