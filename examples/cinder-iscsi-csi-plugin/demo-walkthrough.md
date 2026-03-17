# Cinder iSCSI CSI Driver Demo Walkthrough

This runbook is a live demo script for showing how the
`cinder-iscsi.csi.windriver.com` driver provisions a Cinder-backed iSCSI volume,
attaches it to a pod as a raw block device, and exposes the iSCSI session
details on the Kubernetes node.

This walkthrough assumes the driver is already deployed. It does **not** cover
image build, image push, or full e2e automation.

## What This Demo Proves

1. Kubernetes can dynamically provision a PV/PVC by using the new iSCSI driver.
2. A pod can consume that volume as a raw block device.
3. The node plugin performs iSCSI discovery and login against the backend.
4. The resulting iSCSI session and block device can be validated with
   Kubernetes-native commands.

## Important Constraint

This driver only supports `volumeMode: Block`.

- Use `volumeDevices` in pods.
- Do not use filesystem mounts for the main demo.
- Do not use this walkthrough to demonstrate formatted filesystems.

## Demo Assets Used

- `plugin/` for mapping the deployed driver assets.
- `storageclass.yaml` for dynamic provisioning.
- `cdi-storageprofile-patch.yaml` for patching the CDI StorageProfile to default to `volumeMode: Block`.
- `pvc-only.yaml` for provisioning-only validation.
- `block/block.yaml` for the pod + raw block device demo.

## Prerequisites

1. A running Kubernetes cluster with the Cinder iSCSI CSI driver already deployed.
2. An OpenStack cloud with a Cinder iSCSI backend and a valid volume type.
3. Network connectivity from Kubernetes nodes to OpenStack APIs and iSCSI targets.
4. `kubectl` access to the cluster.
5. Optional: OpenStack CLI or Horizon access for backend confirmation.

## Phase 1: Show Driver Evidence

Use these commands first so the audience sees that the driver assets already
exist before any workload is created.

### Show the CSI driver registration

```bash
kubectl get csidriver,storageclass -o wide
kubectl get csidriver cinder-iscsi.csi.windriver.com -o yaml | yq
kubectl get storageclass csi-sc-cinder-iscsi -o yaml | yq
```

## Show the CDI storageprofile patch

```bash
kubectl get storageprofiles
kubectl get storageprofiles csi-sc-cinder-iscsi -oyaml | yq
```

Call out:

- The driver name is `cinder-iscsi.csi.windriver.com`.
- The cluster already recognizes the driver before a PVC is created.

### Show controller and node plugin readiness

```bash
kubectl -n kube-system get deploy csi-cinder-iscsi-controllerplugin
kubectl -n kube-system get ds csi-cinder-iscsi-nodeplugin
kubectl -n kube-system get pods -o wide | grep csi-cinder-iscsi
```

Call out:

- The controller Deployment handles provisioning and controller publish.
- The node DaemonSet runs on each node and performs iSCSI operations.

### Show config and secret objects

```bash
kubectl -n kube-system get secret cinder-iscsi-cloud-config
kubectl -n kube-system get configmap cinder-iscsi-driver-config -o yaml
```

Call out:

- `cloud.conf` provides OpenStack credentials.
- `driver.conf` contains iSCSI-specific behavior such as CHAP and multipath.

## Phase 2: Explain the Attach Flow Briefly

Keep this short and verbal while the audience is looking at the cluster.

1. The PVC triggers dynamic provisioning through the StorageClass.
2. The controller creates or reuses a Cinder attachment.
3. The controller returns `target_portal`, `target_iqn`, and `target_lun`.
4. The node plugin performs discovery, optional CHAP setup, and login.
5. The block device appears on the node and is exposed into the pod.

If you want a code anchor while presenting:

- `ControllerPublishVolume` builds the publish context.
- `NodeStageVolume` performs iSCSI discovery/login and waits for the device.

## Phase 3: Create the StorageClass and Patch the CDI StorageProfile

Apply the example StorageClass, then patch the CDI StorageProfile so that
DataVolume PVCs default to `volumeMode: Block` instead of `Filesystem`.

```bash
kubectl apply -f examples/cinder-iscsi-csi-plugin/storageclass.yaml
kubectl get storageclass csi-sc-cinder-iscsi -o yaml
```

Call out:

- Provisioner: `cinder-iscsi.csi.windriver.com`
- `volumeBindingMode: Immediate`
- `allowVolumeExpansion: true`
- The configured Cinder volume type under `parameters.type`

### Patch the CDI StorageProfile

CDI auto-creates a StorageProfile per StorageClass, but our custom
provisioner is not in CDI's hardcoded capability map. Without this patch,
CDI defaults to `volumeMode: Filesystem`, which the driver rejects.

```bash
kubectl apply -f examples/cinder-iscsi-csi-plugin/cdi-storageprofile-patch.yaml
kubectl get storageprofile csi-sc-cinder-iscsi -o yaml | yq
```

Call out:

- `spec.claimPropertySets` now shows `volumeMode: Block`.
- `status.claimPropertySets` should reflect the same after CDI reconciles.
- This patch must be re-applied every time the StorageClass is deleted and
  recreated, because CDI resets the StorageProfile to `spec: {}`.

## Phase 4: Provisioning-Only Checkpoint

This is the first live proof point. It isolates provisioning from node attach.

### Create a PVC without a consumer pod

```bash
kubectl apply -f examples/cinder-iscsi-csi-plugin/pvc-only.yaml
kubectl get pvc csi-pvc-validate-iscsi
kubectl get pvc csi-pvc-validate-iscsi -o wide
kubectl describe pvc csi-pvc-validate-iscsi
```

### Show the generated PV

```bash
PV_NAME=$(kubectl get pvc csi-pvc-validate-iscsi -o jsonpath='{.spec.volumeName}')
kubectl get pv "$PV_NAME" -o yaml
```

Call out:

- The claim should reach `Bound`.
- A PV is created dynamically.
- The PV should show CSI driver `cinder-iscsi.csi.windriver.com`.
- This proves controller-side provisioning works even before a pod consumes it.

### Optional OpenStack cross-check

If OpenStack visibility is available, correlate the volume handle from the PV.

The `cinder-iscsi-cloud-config` secret stores one `cloud.conf` entry, not
individual `OS_*` fields. Load the OpenStack CLI environment from that file
before running `openstack` commands.

```bash
VOL_ID=$(kubectl get pv "$PV_NAME" -o jsonpath='{.spec.csi.volumeHandle}')
echo "$VOL_ID"

trim_value() {
  sed 's/^[[:space:]]*//; s/[[:space:]]*$//'
}

CLOUD_CONF=$(mktemp)
kubectl -n kube-system get secret cinder-iscsi-cloud-config -o jsonpath='{.data.cloud\.conf}' | base64 -d > "$CLOUD_CONF"

export OS_AUTH_URL=$(awk -F= '/^[[:space:]]*auth-url[[:space:]]*=/{print $2}' "$CLOUD_CONF" | trim_value)
export OS_USERNAME=$(awk -F= '/^[[:space:]]*username[[:space:]]*=/{print $2}' "$CLOUD_CONF" | trim_value)
export OS_PASSWORD=$(awk -F= '/^[[:space:]]*password[[:space:]]*=/{print $2}' "$CLOUD_CONF" | trim_value)
export OS_PROJECT_ID=$(awk -F= '/^[[:space:]]*tenant-id[[:space:]]*=/{print $2}' "$CLOUD_CONF" | trim_value)
export OS_USER_DOMAIN_NAME=$(awk -F= '/^[[:space:]]*domain-name[[:space:]]*=/{print $2}' "$CLOUD_CONF" | trim_value)
export OS_PROJECT_DOMAIN_NAME="$OS_USER_DOMAIN_NAME"
export OS_REGION_NAME=$(awk -F= '/^[[:space:]]*region[[:space:]]*=/{print $2}' "$CLOUD_CONF" | trim_value)
export OS_IDENTITY_API_VERSION=3

openstack token issue
openstack volume show "$VOL_ID"

rm -f "$CLOUD_CONF"
```

## Phase 5: Create a Pod That Uses the Volume as a Block Device

Use the raw block example for the main demo.

```bash
kubectl apply -f examples/cinder-iscsi-csi-plugin/block/block.yaml
kubectl get pvc csi-pvc-cinder-iscsi-block
kubectl get pod test-block-cinder-iscsi -o wide
kubectl describe pod test-block-cinder-iscsi
```

Call out:

- The workload uses `volumeMode: Block`.
- The pod receives the device through `volumeDevices`.
- The block device is exposed at `/dev/xvda` inside the container.

### Verify the device inside the pod

```bash
kubectl exec test-block-cinder-iscsi -- ls -l /dev/xvda
kubectl exec test-block-cinder-iscsi -- sh -c 'lsblk /dev/xvda || true'
kubectl exec test-block-cinder-iscsi -- sh -c 'stat /dev/xvda || true'
```

Expected outcome:

- `/dev/xvda` exists in the container.
- The pod can see the block device even though it is not mounted as a filesystem.

## Phase 6: Validate iSCSI Session Information

This is the most important operator-facing validation step.

### Find the node hosting the pod

```bash
NODE_NAME=$(kubectl get pod test-block-cinder-iscsi -o jsonpath='{.spec.nodeName}')
echo "$NODE_NAME"
```

### Find the node plugin pod on that same node

```bash
NODEPLUGIN_POD=$(kubectl -n kube-system get pod -l app=csi-cinder-iscsi-nodeplugin -o jsonpath="{.items[?(@.spec.nodeName=='$NODE_NAME')].metadata.name}")
echo "$NODEPLUGIN_POD"
```

### Show node plugin logs around attach

```bash
kubectl -n kube-system logs "$NODEPLUGIN_POD" -c cinder-iscsi-csi-plugin --tail=200
```

Look for evidence of:

- `NodeStageVolume`
- iSCSI discovery
- login success
- device path resolution

### Show active iSCSI sessions from the node plugin container

```bash
kubectl -n kube-system exec "$NODEPLUGIN_POD" -c cinder-iscsi-csi-plugin -- iscsiadm -m session
```

Expected outcome:

- At least one active iSCSI session is present.
- The output should reference the backend portal used by the volume.

### Show discovered node records for the target

```bash
kubectl -n kube-system exec "$NODEPLUGIN_POD" -c cinder-iscsi-csi-plugin -- iscsiadm -m node
```

Expected outcome:

- The output includes the target portal and target IQN used by the Cinder volume.

### Show the device-by-path entry created by iSCSI login

```bash
kubectl -n kube-system exec "$NODEPLUGIN_POD" -c cinder-iscsi-csi-plugin -- sh -c 'ls -l /dev/disk/by-path | grep iscsi || true'
```

Expected outcome:

- A path like `ip-<portal>-iscsi-<iqn>-lun-<n>` is visible.
- That path resolves to the real block device used by the pod.

### Optional: inspect the host block device list from the node plugin pod

```bash
kubectl -n kube-system exec "$NODEPLUGIN_POD" -c cinder-iscsi-csi-plugin -- sh -c 'lsblk || true'
```

### Optional: correlate the pod device and node device

```bash
PV_BLOCK=$(kubectl get pvc csi-pvc-cinder-iscsi-block -o jsonpath='{.spec.volumeName}')
VOL_BLOCK=$(kubectl get pv "$PV_BLOCK" -o jsonpath='{.spec.csi.volumeHandle}')
echo "$VOL_BLOCK"

# 1. Show which host device the volume maps to via its by-path symlink
kubectl -n kube-system exec "$NODEPLUGIN_POD" -c cinder-iscsi-csi-plugin -- sh -c "ls -l /dev/disk/by-path | grep '$VOL_BLOCK'"

# 2. Resolve the symlink to the real device name (e.g. /dev/sdd)
ISCSI_PATH=$(kubectl -n kube-system exec "$NODEPLUGIN_POD" -c cinder-iscsi-csi-plugin -- sh -c "ls /dev/disk/by-path/ | grep '$VOL_BLOCK'")
echo "by-path entry: $ISCSI_PATH"

HOST_DEV=$(kubectl -n kube-system exec "$NODEPLUGIN_POD" -c cinder-iscsi-csi-plugin -- sh -c "readlink -f /dev/disk/by-path/$ISCSI_PATH")
echo "node device:   $HOST_DEV"

# 3. Compare major:minor numbers — they must match
kubectl -n kube-system exec "$NODEPLUGIN_POD" -c cinder-iscsi-csi-plugin -- sh -c "stat -c 'node device: %t:%T %n' '$HOST_DEV'"
kubectl exec test-block-cinder-iscsi -- sh -c "stat -c 'pod  device: %t:%T %n' /dev/xvda"
```

What to present:

- The PV volume handle is the Cinder volume UUID.
- The same UUID appears in the iSCSI by-path name on the node.
- The by-path symlink resolves to one concrete host device such as `/dev/sdd`.
- The major:minor device numbers for `/dev/xvda` in the pod and `/dev/sdd` on the node should match, proving they are the same block device exposed through different paths.

## Phase 7: Optional OpenStack Backend Confirmation

If OpenStack visibility is available, show that Kubernetes state matches the
backend attachment.

Load the OpenStack CLI environment from the `cloud.conf` stored in the secret.

```bash
PV_BLOCK=$(kubectl get pvc csi-pvc-cinder-iscsi-block -o jsonpath='{.spec.volumeName}')
VOL_BLOCK=$(kubectl get pv "$PV_BLOCK" -o jsonpath='{.spec.csi.volumeHandle}')
echo "$VOL_BLOCK"

trim_value() {
  sed 's/^[[:space:]]*//; s/[[:space:]]*$//'
}

CLOUD_CONF=$(mktemp)
kubectl -n kube-system get secret cinder-iscsi-cloud-config -o jsonpath='{.data.cloud\.conf}' | base64 -d > "$CLOUD_CONF"

export OS_AUTH_URL=$(awk -F= '/^[[:space:]]*auth-url[[:space:]]*=/{print $2}' "$CLOUD_CONF" | trim_value)
export OS_USERNAME=$(awk -F= '/^[[:space:]]*username[[:space:]]*=/{print $2}' "$CLOUD_CONF" | trim_value)
export OS_PASSWORD=$(awk -F= '/^[[:space:]]*password[[:space:]]*=/{print $2}' "$CLOUD_CONF" | trim_value)
export OS_PROJECT_ID=$(awk -F= '/^[[:space:]]*tenant-id[[:space:]]*=/{print $2}' "$CLOUD_CONF" | trim_value)
export OS_USER_DOMAIN_NAME=$(awk -F= '/^[[:space:]]*domain-name[[:space:]]*=/{print $2}' "$CLOUD_CONF" | trim_value)
export OS_PROJECT_DOMAIN_NAME="$OS_USER_DOMAIN_NAME"
export OS_REGION_NAME=$(awk -F= '/^[[:space:]]*region[[:space:]]*=/{print $2}' "$CLOUD_CONF" | trim_value)
export OS_IDENTITY_API_VERSION=3

openstack token issue
openstack volume show "$VOL_BLOCK"
openstack volume attachment list --volume "$VOL_BLOCK"

rm -f "$CLOUD_CONF"
```

Call out:

- The same volume backing the Kubernetes PV is attached in OpenStack.
- The attachment exists while the pod is running.

## Phase 8: Cleanup

Delete the workload objects and confirm cleanup.

```bash
kubectl delete -f examples/cinder-iscsi-csi-plugin/block/block.yaml
kubectl delete -f examples/cinder-iscsi-csi-plugin/pvc-only.yaml

## Optional: delete the StorageClass (note: this also resets the CDI StorageProfile)
kubectl delete -f examples/cinder-iscsi-csi-plugin/storageclass.yaml
# If you recreate the StorageClass later, re-apply the CDI patch:
#   kubectl apply -f examples/cinder-iscsi-csi-plugin/cdi-storageprofile-patch.yaml
```

Then verify:

```bash
kubectl get pvc
kubectl get pv
kubectl -n kube-system logs "$NODEPLUGIN_POD" -c cinder-iscsi-csi-plugin --tail=200
```

Optional OpenStack check:

```bash
openstack volume show "$VOL_BLOCK"
```

Call out:

- With `reclaimPolicy: Delete`, the backend volume should be cleaned up.
- The iSCSI session should disappear after detach and cleanup complete.

## Troubleshooting Branches for a Live Demo

### PVC stays Pending

Use:

```bash
kubectl describe pvc csi-pvc-validate-iscsi
kubectl -n kube-system logs deploy/csi-cinder-iscsi-controllerplugin -c cinder-iscsi-csi-plugin --tail=200
```

Typical causes:

- Wrong Cinder volume type in the StorageClass.
- Invalid `cloud.conf` credentials.
- Controller cannot reach OpenStack APIs.

### Pod stays in ContainerCreating or the device does not appear

Use:

```bash
kubectl describe pod test-block-cinder-iscsi
kubectl -n kube-system logs "$NODEPLUGIN_POD" -c cinder-iscsi-csi-plugin --tail=200
kubectl -n kube-system exec "$NODEPLUGIN_POD" -c cinder-iscsi-csi-plugin -- iscsiadm -m session
kubectl -n kube-system exec "$NODEPLUGIN_POD" -c cinder-iscsi-csi-plugin -- iscsiadm -m node
```

Typical causes:

- Node cannot reach the iSCSI portal.
- CHAP settings do not match the backend.
- Initiator IQN or node identity is wrong.
- The backend returned connection info but login failed on the node.

## Suggested Presenter Narrative

1. Show that the driver already exists in the cluster and highlight the controller and node roles.
2. Create a PVC by using the new driver and show the resulting PV.
3. Create a pod that uses the PV as a raw block device and show `/dev/xvda`.
4. Prove that this is a real iSCSI attach by showing the session, target IQN, target portal, and device-by-path on the host side.
5. If useful, correlate the same volume in OpenStack.
6. Clean everything up and show the detach path.