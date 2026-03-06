# CDI Volume Populator Lifecycle (Warm Migration)

This document details the CDI (Containerized Data Importer) internal lifecycle
for warm V2O migrations, learned from e2e testing. Use this knowledge to debug
failures and validate whether an e2e test has passed at each stage.

## Architecture: PVC Prime Pattern

CDI uses a **Volume Populator** model with a "PVC Prime" (PVC') intermediate.
The key resources involved are:

| Resource | Purpose |
|----------|---------|
| **DataVolume** (DV) | User-facing CRD, declares source (VDDK), storage, checkpoints |
| **VolumeImportSource** | CDI-internal CR, references the DV's VDDK source + checkpoints |
| **Target PVC** | Named after the DV (e.g. `dv-vddk-ation-fedora-bios-disk-0`), has `DataSourceRef` → VolumeImportSource |
| **Prime PVC** (PVC') | Named `prime-<uid>`, created by CDI populator, actually provisioned and bound to a PV |
| **Importer Pod** | CDI-created pod that runs against the prime PVC, performs VDDK import |

**Source code references** (kubevirt/containerized-data-importer):
- `pkg/controller/populators/import-populator.go` — import populator reconciler
- `pkg/controller/populators/populator-base.go` — PVC prime creation, rebind logic
- `pkg/controller/import-controller.go` — legacy import controller (non-populator path)

## Warm Migration Checkpoint Flow

A warm migration DataVolume has `spec.checkpoints[]`. Each checkpoint represents
one data copy pass (fullcopy or incremental precopy). The flow is:

```
Checkpoint N (not final):
  1. VolumeImportSource created/updated with checkpoint.current
  2. CDI creates Prime PVC (PVC') → provisioned via StorageClass → PV Bound
  3. Importer pod runs against PVC' → copies data from VDDK source
  4. Pod succeeds → IsMultiStageImportInProgress == true
  5. CDI emits "ImportPaused" event → DataVolume phase = Paused
  6. Target PVC stays PENDING (PV still bound to PVC')
  7. PVC' is NOT cleaned up (retained for next checkpoint)

Final Checkpoint (cutover):
  1. VolumeImportSource updated with final checkpoint, FinalCheckpoint=true
  2. New importer pod runs incremental delta against same PVC'
  3. Pod succeeds → IsMultiStageImportInProgress == false (final)
  4. CDI calls Rebind: PV re-bound from PVC' → Target PVC
  5. Target PVC becomes BOUND, DataVolume phase = Succeeded
  6. PVC' enters "Lost" state → CDI reconcileCleanup deletes it
```

## Key CDI Code Paths

**Multistage pause** (import-populator.go `reconcileTargetPVC`):
```go
case string(corev1.PodSucceeded):
    if cc.IsMultiStageImportInProgress(pvcPrime) {
        cc.UpdatesMultistageImportSucceeded(pvcPrime, checkpointArgs)
        r.recorder.Eventf(pvc, EventTypeNormal, ImportPaused, MessageImportPaused, pvc.Name)
        break  // Does NOT proceed to Rebind
    }
    if cc.IsPVCComplete(pvcPrime) && cc.IsUnbound(pvc) {
        cc.Rebind(ctx, r.client, pvcPrime, pvcCopy)  // Only on final checkpoint
    }
```

**Cleanup skip during multistage** (populator-base.go `reconcile`):
```go
if cc.IsPVCComplete(pvcPrime) && !cc.IsMultiStageImportInProgress(pvc) {
    res, err = r.reconcileCleanup(pvcPrime)  // Only after final checkpoint
}
```

## Expected Resource States by Phase

Use these tables to validate whether the e2e test is progressing correctly.

### After fullcopy-source-vm (install workflow, first checkpoint — not final)

| Resource | Expected State | Validation Command |
|----------|----------------|-------------------|
| DataVolume | `phase: Paused`, `progress: 100.0%` | `kubectl get dv <name> -o yaml` |
| Target PVC | `Pending`, `volumeMode: Block` | `kubectl get pvc <name>` |
| Prime PVC | `Bound`, `volumeMode: Block`, capacity matches | `kubectl get pvc prime-<uid>` |
| Importer Pod | `Succeeded` (or cleaned up) | N/A |
| DV condition `Running` | `status: False`, `reason: Completed`, `message: Import Complete; VDDK: ...` | In DV status |
| DV condition `Bound` | `status: False`, `message: PVC <name> Pending` | In DV status |
| Cloudify node instance | `fullcopy-source-vm` state: `started` | `cfy node-instances list -d <ID>` |

**Key PVC annotations to verify on target PVC:**
```
cdi.kubevirt.io/storage.checkpoint.current: fullcpy-<timestamp>
cdi.kubevirt.io/storage.condition.running: false
cdi.kubevirt.io/storage.condition.running.message: Import Complete; VDDK: {"Version":"8.0.3","Host":""}
cdi.kubevirt.io/storage.condition.running.reason: Completed
cdi.kubevirt.io/storage.pod.phase: Succeeded
cdi.kubevirt.io/storage.populator.progress: 100.0%
volume.kubernetes.io/storage-provisioner: cinder-iscsi.csi.windriver.com
```

### After precopy-source-vm (incremental delta checkpoints — not final)

| Resource | Expected State |
|----------|----------------|
| DataVolume | `phase: Paused`, `progress: 100.0%` (resets per checkpoint) |
| Target PVC | Still `Pending` |
| Prime PVC | Still `Bound` (same PV, re-used for each checkpoint) |
| PVC annotation `checkpoint.current` | Updated to latest precopy checkpoint name |

### After cutover-w-lastcpy (final checkpoint + rebind)

| Resource | Expected State |
|----------|----------------|
| DataVolume | `phase: Succeeded`, `progress: 100.0%` |
| Target PVC | **`Bound`** (PV rebound from prime → target) |
| Prime PVC | `Lost` → eventually deleted by CDI |
| DV condition `Bound` | `status: True` |
| DV condition `Ready` | `status: True` |

### After create-guest-vm (VM creation from migrated volume)

| Resource | Expected State |
|----------|----------------|
| KubeVirt VM / VMI | `Running` (if applicable) |
| Target PVC | `Bound`, in use by VM pod |

### After uninstall (cleanup)

| Resource | Expected State |
|----------|----------------|
| DataVolume | Deleted |
| Target PVC | Deleted |
| Prime PVC | Already deleted (by CDI) or deleted during uninstall |
| Cinder Volume | Deleted (`DeleteVolume` CSI call) |
| VolumeImportSource | Deleted |

## CDI StorageProfile Prerequisite

CDI uses a **StorageProfile** CRD (auto-created per StorageClass) to determine
the default `volumeMode` and `accessModes` for PVCs it creates. The StorageProfile
is populated automatically for provisioners in CDI's known list
(`pkg/storagecapabilities/storagecapabilities.go`, map `CapabilitiesByProvisionerKey`).

**Problem:** The `cinder-iscsi.csi.windriver.com` provisioner is NOT in CDI's
hardcoded map (only `cinder.csi.openstack.org` is). Without a manual patch,
CDI creates PVCs with `volumeMode: Filesystem`, which the iSCSI driver rejects
(it only supports Block).

**Required fix before any migration with `csi-sc-cinder-iscsi`:**
```bash
kubectl --kubeconfig=$HOME/.kube/config-staging apply -f \
  manifests/cinder-iscsi-csi-plugin/cdi-storageprofile-patch.yaml
```

This sets `claimPropertySets: [{accessModes: [ReadWriteOnce], volumeMode: Block}]`
on the `csi-sc-cinder-iscsi` StorageProfile. Verify with:
```bash
kubectl --kubeconfig=$HOME/.kube/config-staging get storageprofile csi-sc-cinder-iscsi \
  -o jsonpath='{.status.claimPropertySets}' | jq .
```
Expected: `[{"accessModes":["ReadWriteOnce"],"volumeMode":"Block"}]`

The `dev-deploy` skill Step 6b also auto-detects and offers this patch.
