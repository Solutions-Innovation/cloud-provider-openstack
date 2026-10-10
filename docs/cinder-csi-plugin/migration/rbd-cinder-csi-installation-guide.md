# Installing the Cinder RBD CSI Plugin

This guide walks you through installing the Cinder RBD CSI plugin on a
Kubernetes cluster with Helm, using a chart package you build from this
repository, and confirming that it works by provisioning a raw block volume.

The plugin's driver name is `cinder-rbd.csi.windriver.com`.

## About the plugin

The plugin provisions volumes through OpenStack Cinder and attaches them to
Kubernetes worker nodes by mapping the underlying Ceph RBD image with the
Linux kernel RBD client (`krbd`). It is built for disk migration workflows,
where a tool writes a source disk into a raw block volume and a later step
attaches that same volume to a virtual machine.

Before you install, be aware of the following:

- **Pre-production.** The plugin has not completed production qualification.
  Evaluate it in a non-production environment first.
- **Raw block only.** Volumes must use `volumeMode: Block` and
  `ReadWriteOnce`. Filesystem volumes, snapshots, clones, volume expansion,
  and topology-aware provisioning are not supported.
- **Volumes are retained.** Deleting a PersistentVolumeClaim removes the
  Kubernetes objects but keeps the Cinder volume and its data. This is
  intentional, and it is the only supported behavior. See
  [Uninstall and clean up](#uninstall-and-clean-up).
- **Privileged node plugin.** The node plugin runs privileged and mounts the
  host's `/dev` and `/sys` read-write on every node where it runs. Kernel RBD
  mapping requires this. Restrict access to the plugin's namespace and
  ServiceAccounts accordingly.

## Prerequisites

| Requirement | Details |
|---|---|
| Kubernetes | 1.29 or later |
| Helm | 3.x on the workstation where you run the commands |
| OpenStack Cinder | API microversion 3.27 or later. The controller exits at startup if the API does not support it. |
| Cinder volume type | A volume type backed by a Ceph RBD pool |
| Ceph | Access to the Ceph cluster's FSID and to the key of the Ceph client that Cinder uses |
| Worker nodes | The `rbd` kernel module loaded (check with `lsmod \| grep '^rbd'`) |
| Network | Worker nodes can reach the Ceph monitors, and the controller can reach the Keystone and Cinder endpoints |
| CDI (optional) | KubeVirt CDI, if you import disk images with DataVolumes |

You also need a way to run `kubectl` against the target cluster with
permission to create namespaced Secrets and cluster-scoped RBAC and storage
resources.

## Step 1: Package the chart

From the root of a checkout of this repository:

```bash
make package-cinder-rbd-chart
```

The command lints the chart, runs the chart's render safety checks, and writes
the package to the `_dist/` directory:

```text
_dist/openstack-cinder-rbd-csi-<chart-version>.tgz
```

Only `helm` and `bash` are required. The package is not uploaded anywhere. To
write it to a different directory, run
`hack/package-cinder-rbd-chart.sh <output-dir>` instead.

> **Note:** The chart refuses to render until you supply your Ceph FSID, so a
> bare `helm template` or `helm lint` of the chart directory fails by design.
> `make package-cinder-rbd-chart` accounts for this.

If you prefer a published package, the chart is also available as an OCI
artifact at `oci://ghcr.io/solutions-innovation/charts/openstack-cinder-rbd-csi`
for tagged releases. The rest of this guide uses the local package.

## Step 2: Create the Secrets

The chart references two Secrets by name and does not create them. Create both
in the namespace you will install into. This guide uses `kube-system`.

### OpenStack configuration

The controller reads its Keystone and Cinder settings from a `cloud.conf` file.
Create a Secret from your existing file:

```bash
kubectl -n kube-system create secret generic cinder-rbd-csi-cloud-config \
  --from-file=cloud.conf=<path-to-cloud.conf>
```

A typical `cloud.conf` for password authentication looks like this. Use the
authentication method your OpenStack deployment supports:

```ini
[Global]
auth-url=https://<keystone-host>:5000/v3
username=<user>
password=<password>
domain-name=<user-domain>
tenant-id=<project-id>
tenant-domain-id=<project-domain-id>
region=<region>
```

Two fields cause most first-install failures:

- `region` must be the region name in your Keystone service catalog. List it
  with `openstack region list`. A wrong value fails with "No suitable endpoint
  could be found in the service catalog".
- `tenant-id` must be the **current** ID of the project the user is scoped to.
  A stale project ID, or a wrong password, fails with HTTP 401.

The controller reads this file only when it starts. After you change the
Secret, restart the controller:

```bash
kubectl -n kube-system rollout restart deployment/openstack-cinder-rbd-csi-controllerplugin
```

If your OpenStack endpoints use a certificate signed by a private CA, see
[Private CA certificates](#private-ca-certificates).

### Ceph client credential

The node plugin authenticates to Ceph as the same client that Cinder uses to
export volumes. Copy that client's name and key into a Secret with the keys
`userID` and `userKey`:

```bash
# Run on a host with Ceph admin access. Keep the key out of shell history and
# shared terminals.
umask 077
ceph auth get-key client.<cinder-client> > /tmp/ceph-key

kubectl -n kube-system create secret generic cinder-rbd-ceph-client \
  --from-literal=userID=<cinder-client> \
  --from-file=userKey=/tmp/ceph-key

shred -u /tmp/ceph-key
```

Replace `<cinder-client>` with the Ceph user name **without** the `client.`
prefix (for example `cinder`). It must match the user Cinder reports when it
returns connection information for a volume.

This Secret is a copy of the key, and nothing keeps it in sync. If the Ceph
key changes, update the Secret. The [operator runbook](rbd-cinder-csi-operator-runbook.md)
describes rotation; no pod restart is required.

Do not put either credential in a Helm values file or commit it to version
control.

## Step 3: Configure the values

Start from the example values file:

```bash
cp examples/cinder-rbd-csi-plugin/values.yaml my-values.yaml
```

Edit `my-values.yaml`:

| Value | What to set |
|---|---|
| `driverConfig.rbd.expectedFsid` | **Required.** Your Ceph cluster's FSID. Get it with `ceph fsid`. It identifies the cluster and is not a credential. |
| `driverConfig.rbd.expectedClusterName` | The Ceph cluster name. The default is `ceph`. |
| `storageClass.parameters.type` | The Cinder volume type backed by your Ceph RBD pool. |
| `storageClass.parameters.availability` | The Cinder availability zone to create volumes in. |
| `driverConfig.volume.defaultVolumeType` | Fallback volume type if a StorageClass does not set one. |
| `storageProfile.enabled` | Set to `false` if CDI is not installed. |
| `csi.plugin.nodePlugin.healthPort` | Optional. Port for the node plugin's health probe. The node plugin runs on the host network, so change this if another host-network driver on the same nodes already uses the default, `9808`. |
| `secret.name`, `cephCredential.secretName` | Change only if you used different Secret names in step 2. |

Leave `mounter`, `exclusive`, and `deleteVolumeMode` as they are. The chart
rejects any other value, because each one protects against data loss or
concurrent writers.

Confirm the file renders before you install:

```bash
helm template cinder-rbd _dist/openstack-cinder-rbd-csi-<chart-version>.tgz \
  --namespace kube-system -f my-values.yaml > /dev/null && echo OK
```

If the command reports that `expectedFsid` is required, the value is still
empty.

### Container image

By default the chart pulls
`ghcr.io/solutions-innovation/cinder-rbd-csi-plugin` with the tag set by the
chart's `appVersion`, and always checks for a newer image. If your cluster
cannot reach that registry, or the tag is not published, copy the image to
your own registry and override it:

```yaml
csi:
  plugin:
    image:
      repository: registry.example.com/cinder-rbd-csi-plugin
      tag: <tag>
      pullPolicy: IfNotPresent
imagePullSecrets:
  - name: <registry-secret>
```

To build and push your own image from a checkout of this repository:

```bash
make build-local-image-cinder-rbd-csi-plugin \
  REGISTRY=registry.example.com VERSION=v1.35.0-dev.1
docker push registry.example.com/cinder-rbd-csi-plugin:v1.35.0-dev.1
```

`VERSION` must start with `vMAJOR.MINOR.PATCH`, because the driver parses the
embedded version when it starts. Use the same tag in `csi.plugin.image.tag`.

The sidecar images (`csi-attacher`, `csi-provisioner`, `livenessprobe`, and
`csi-node-driver-registrar`) come from `registry.k8s.io` and can be overridden
under the corresponding `csi.<name>.image` keys.

### Private CA certificates

If your Keystone or Cinder endpoints use a private CA, mount the CA bundle into
the controller and point `cloud.conf` at it. Add to `my-values.yaml`:

```yaml
cacert:
  enabled: true
  source: hostPath        # or "secret"
  hostPath:
    path: /etc/ssl/certs
    type: Directory
```

and add `ca-file=/etc/ssl/certs/ca-certificates.crt` to the `[Global]` section
of `cloud.conf`. With `source: hostPath`, the file must already exist on every
node where the controller can run. The chart's
[README](../../../charts/cinder-rbd-csi-plugin/README.md) describes the Secret
source.

## Step 4: Install

```bash
helm install cinder-rbd _dist/openstack-cinder-rbd-csi-<chart-version>.tgz \
  --namespace kube-system \
  -f my-values.yaml
```

To make Helm wait for the pods and report a failure, add `--wait --timeout 120s`.
If a wait times out, Helm records the release as `failed` even if you later fix
the cause. Fix the cause (see [Step 5](#step-5-verify-the-installation)), then
run `helm upgrade` with the same package and values file to bring the release
back to `deployed`.

To install from the published OCI chart instead:

```bash
helm install cinder-rbd \
  oci://ghcr.io/solutions-innovation/charts/openstack-cinder-rbd-csi \
  --version <chart-version> \
  --namespace kube-system \
  -f my-values.yaml
```

## Step 5: Verify the installation

Check that the controller and the node plugins are running:

```bash
kubectl -n kube-system get deployment openstack-cinder-rbd-csi-controllerplugin
kubectl -n kube-system get daemonset openstack-cinder-rbd-csi-nodeplugin
kubectl -n kube-system get pods -l app=openstack-cinder-rbd-csi
```

The controller pod should show all containers ready, and there should be one
node plugin pod on each worker node.

Check that the driver and StorageClass are registered:

```bash
kubectl get csidriver cinder-rbd.csi.windriver.com
kubectl get storageclass cinder-rbd-migration
kubectl get storageprofile cinder-rbd-migration   # only if CDI is installed
```

A healthy controller log shows the leader-election leases acquired and the
detected Cinder API microversion. A healthy install also registers the driver
on every worker node:

```bash
kubectl get csinode -o custom-columns=NODE:.metadata.name,DRIVERS:.spec.drivers[*].name
```

If a pod does not become ready, read the logs of the plugin container:

```bash
kubectl -n kube-system logs deploy/openstack-cinder-rbd-csi-controllerplugin \
  -c cinder-rbd-csi-plugin
kubectl -n kube-system logs <nodeplugin-pod> -c cinder-rbd-csi-plugin
```

The most common causes are:

| Symptom | Likely cause |
|---|---|
| `ImagePullBackOff` | The image or tag is not reachable from the cluster. See [Container image](#container-image). |
| Controller exits at startup | Cinder does not support API microversion 3.27 or later, or `cloud.conf` is wrong or empty. Read the controller's `cinder-rbd-csi-plugin` log for the exact error. |
| Controller log: `got 401 instead` from Keystone | The password or the `tenant-id` in `cloud.conf` is wrong or stale. Fix the Secret, then restart the controller. |
| Controller log: `No suitable endpoint could be found in the service catalog` | `region` in `cloud.conf` does not match the Keystone catalog. Check `openstack region list`, fix the Secret, then restart the controller. |
| Node plugin or its liveness container fails with `address already in use` | Another host-network driver uses port 9808 on that node. Set `csi.plugin.nodePlugin.healthPort` to a free port and run `helm upgrade`. |
| Pods stay in `ContainerCreating` | A referenced Secret is missing in the release namespace. |
| Node plugin reports a missing module | The `rbd` kernel module is not loaded. Load it, or set `csi.plugin.nodePlugin.mountLibModules: true`. |

## Step 6: Provision a first volume

The repository includes example manifests in
[`examples/cinder-rbd-csi-plugin/`](../../../examples/cinder-rbd-csi-plugin/).
Create a raw block claim and a pod that writes to it:

```bash
kubectl apply -f examples/cinder-rbd-csi-plugin/raw-block-pvc.yaml
kubectl apply -f examples/cinder-rbd-csi-plugin/raw-block-pod.yaml

kubectl get pvc migration-target-disk
kubectl logs migration-writer
```

The claim becomes `Bound` once Cinder creates the volume, and the pod's log
shows the block device being written and read back. A claim must set
`volumeMode: Block`; without it the claim stays `Pending`.

### Importing images with CDI

If you use KubeVirt CDI, the chart's StorageProfile tells CDI to create
`Block` / `ReadWriteOnce` claims for this StorageClass. Without it, CDI
defaults to `Filesystem`, which this driver rejects.
[`cdi-datavolume.yaml`](../../../examples/cinder-rbd-csi-plugin/cdi-datavolume.yaml)
shows a DataVolume that imports a disk image into a volume.

## Upgrade

Rebuild the package from the new revision, then upgrade with your values file:

```bash
make package-cinder-rbd-chart
helm upgrade cinder-rbd _dist/openstack-cinder-rbd-csi-<chart-version>.tgz \
  --namespace kube-system \
  -f my-values.yaml
```

Compare your values file with the new revision's example values file before
you upgrade, in case settings were added or renamed. To return to the previous
revision, run `helm rollback cinder-rbd`.

## Uninstall and clean up

```bash
helm uninstall cinder-rbd --namespace kube-system
```

This removes the plugin, the StorageClass, and the StorageProfile. It does not
remove the two Secrets you created, or any PersistentVolumeClaims and Cinder
volumes. Delete the Secrets yourself if you no longer need them:

```bash
kubectl -n kube-system delete secret cinder-rbd-csi-cloud-config cinder-rbd-ceph-client
```

### Retained volumes

Deleting a PersistentVolumeClaim never deletes the Cinder volume. To reclaim
the storage, first confirm that no node still maps the volume and that Ceph
shows no watchers, then delete it with the OpenStack CLI. The checks are
important: Cinder can report a volume as `available` while a node still holds
a kernel mapping, and deleting the image in that state can corrupt data. The
[operator runbook](rbd-cinder-csi-operator-runbook.md) describes the procedure
in the section "Reclaiming a retained Cinder volume".

## Troubleshooting and further reading

- [Operator runbook](rbd-cinder-csi-operator-runbook.md): diagnosing mapping
  and attachment problems, rotating the Ceph credential, and reclaiming
  volumes.
- [Chart README](../../../charts/cinder-rbd-csi-plugin/README.md): the chart's
  safety guards, volume retention, and security notes.
- [`values.yaml`](../../../charts/cinder-rbd-csi-plugin/values.yaml): every
  setting the chart supports, with comments.
