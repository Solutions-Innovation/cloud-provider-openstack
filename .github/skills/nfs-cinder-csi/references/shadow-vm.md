# Shadow VM Lifecycle

## Purpose

The Shadow VM is a lightweight Nova instance whose **sole purpose** is to trigger
Cinder's NFS attachment record creation. Cinder populates `connection_info`
(NFS export path, volume filename, mount options) **only when a volume is attached
to an instance via Nova**. Without the Shadow VM, there is no attachment record to
query for NFS mount details.

## State machine

```
                              ┌──────────────┐
                              │  No Shadow   │
                              │     VM       │
                              └──────┬───────┘
                                     │ CreateVolume called
                                     ▼
                              ┌──────────────┐
                              │   Creating   │
                              └──────┬───────┘
                                     │ WaitServerStatus("ACTIVE")
                                     ▼
                              ┌──────────────┐
                              │    Active    │
                              └──────┬───────┘
                                     │ Volume attached (boot-from-volume)
                                     ▼
                              ┌──────────────┐
                              │   Volume     │
                              │  Attached    │
                              └──────┬───────┘
                                     │ StopServer
                                     ▼
                              ┌──────────────┐
                              │   Stopped    │ ← STEADY STATE
                              │  Shadow VM   │   (volume in-use, VM stopped,
                              └──────┬───────┘    NFS export queryable)
                                     │
                    ┌────────────────┼────────────────┐
                    │                │                │
                    ▼                ▼                ▼
            ControllerPublish   ExpandVolume     DeleteVolume
            (query conn_info)   (Cinder extend)  (cleanup)
                    │                                │
                    ▼                                ▼
            Return NFS export              Detach → Delete Shadow VM
            in PublishContext               Volume → "available"
```

## Lifecycle invariants

| Phase | Shadow VM State | Volume State | Attachment Record |
|-------|----------------|--------------|-------------------|
| After `CreateVolume` | Stopped | `in-use` | Exists (NFS connection_info populated) |
| During CDI precopy stages | Stopped | `in-use` | Persists (ControllerUnpublish = no-op) |
| During O2O NBD receiver | Stopped | `in-use` | Persists (long-running pod) |
| Between CDI stages (no pod) | Stopped | `in-use` | Persists (acts as lock) |
| After `DeleteVolume` (success) | Deleted | `available` | Removed |
| After `DeleteVolume` (failure cleanup) | Deleted | Deleted | Removed |

## Shadow VM ID storage

Stored in Cinder volume metadata:
```
csi.shadow_vm_id = <nova-server-uuid>
```

Set during `CreateVolume`, read during `ControllerPublishVolume` and `DeleteVolume`.

## Shadow VM creation parameters

From `ShadowVMOpts` config (driver.conf ConfigMap):

| Parameter | Source | Example |
|-----------|--------|---------|
| Name | `{prefix}-{req.Name}` | `shadow-migration-myvm-vda` |
| Flavor | `ShadowVM.flavor-ref` | `m1.small` |
| Volume | Volume created in same RPC | `vol-uuid` (boot-from-volume) |
| Network | `ShadowVM.network-id` | `8e3f3c4a-...` |
| AZ | `ShadowVM.availability-zone` | `nova` |

The Shadow VM should use the **smallest available flavor** — it is stopped immediately
and performs zero I/O. It exists only for the attachment record.

## Critical design rule: ControllerUnpublishVolume = NO-OP

CDI multi-phase precopy creates and destroys pods between stages. Between stages:

```
Pod N completes:
  → NodeUnpublishVolume
  → NodeUnstageVolume
  → ControllerUnpublishVolume  ← FIRES HERE

Pod N+1 scheduled:
  → ControllerPublishVolume    ← NEEDS SAME ATTACHMENT
  → NodeStageVolume
  → NodePublishVolume
```

If `ControllerUnpublishVolume` detached the volume or deleted the Shadow VM,
`ControllerPublishVolume` in the next stage would fail — no attachment to query.

**The Shadow VM and its attachment must persist until `DeleteVolume` (PVC deletion).**

## DeleteVolume cleanup behavior

Controlled by `csi.cleanupVolume` Cinder volume metadata:

| Scenario | Metadata value | DeleteVolume behavior |
|----------|---------------|----------------------|
| Migration success (default) | Not set or `"false"` | Detach → delete Shadow VM → volume `available` (NOT deleted) |
| Migration failure | Blueprint sets `"true"` | Detach → delete Shadow VM → DELETE volume |

**Success path:** Volume left `available` for Blueprint to create target VM from.

**Failure cleanup:**
```bash
openstack volume set --property csi.cleanupVolume=true ${VOLUME_ID}
kubectl delete pvc migration-${VM_NAME}-vda-pvc
# → DeleteVolume: full cleanup, no orphans
```

## Idempotency in DeleteVolume

Handle all partial states:
- Volume already deleted → return success
- Shadow VM already deleted → skip Shadow VM cleanup
- Volume already detached → skip detach
- Volume in `available` state → skip detach, delete Shadow VM if exists

## Implementation file

Core Shadow VM logic lives in `pkg/csi/cinder-nfs/shadowvm.go`:
- `CreateShadowVM(ctx, volumeID, name)` → create, wait ACTIVE, stop, return serverID
- `DeleteShadowVM(ctx, serverID, volumeID)` → detach, wait available, delete server
- `GetShadowVMID(ctx, volumeID)` → read from Cinder volume metadata

Nova API calls live in `pkg/csi/cinder-nfs/openstack/openstack_servers.go`.
