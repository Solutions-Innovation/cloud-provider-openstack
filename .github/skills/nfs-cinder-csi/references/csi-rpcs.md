# CSI RPC Implementation Mapping

## Identity Service

| RPC | Implementation |
|-----|----------------|
| `GetPluginInfo` | Return `cinder-nfs.csi.openstack.org` + version |
| `GetPluginCapabilities` | `CONTROLLER_SERVICE`, `VOLUME_EXPANSION` (offline only) |
| `Probe` | Verify Keystone auth + NFS client utils installed |

## Controller Service

### Capabilities (`ControllerGetCapabilities`)

| Capability | Supported | Notes |
|------------|-----------|-------|
| `CREATE_DELETE_VOLUME` | Yes | Cinder + Shadow VM |
| `PUBLISH_UNPUBLISH_VOLUME` | Yes | NFS connection discovery |
| `LIST_VOLUMES` | Yes | Filter by NFS type |
| `EXPAND_VOLUME` | Yes | Cinder `os-extend` |
| `CREATE_DELETE_SNAPSHOT` | No | Not needed for migration |

### CreateVolume

**Input:** `name`, `capacity`, `parameters` (volume_type, availability), `secrets`

**Flow:**
1. Idempotency check: `cloud.GetVolumesByName(req.Name)`
2. Create Cinder volume: `cloud.CreateVolume(opts)`
3. Wait for volume `available`
4. Store Shadow VM ID in Cinder metadata: `csi.shadow_vm_id`
5. Create Shadow VM: `cloud.CreateServer(opts)` with volume attached
6. Wait for Shadow VM `ACTIVE`
7. Stop Shadow VM: `cloud.StopServer(serverID)`
8. Return `VolumeId` + `VolumeContext{shadow_vm_id}`

**Response context:**
- `resp.Volume.VolumeId` = Cinder volume UUID
- `resp.Volume.VolumeContext["shadow_vm_id"]` = Shadow VM Nova instance UUID

### DeleteVolume

**Input:** `volumeID`, `secrets`

**Flow:**
1. Get volume: `cloud.GetVolume(volumeID)` — if not found, return success (idempotent)
2. Extract Shadow VM ID from volume metadata
3. If Shadow VM exists:
   - Detach volume: `cloud.DetachVolume(shadow_vm_id, volumeID)`
   - Wait for volume status → `available` (timeout: 120s)
   - Delete Shadow VM: `cloud.DeleteServer(shadow_vm_id)`
4. Check `csi.cleanupVolume` metadata:
   - `"true"` → `cloud.DeleteVolume(volumeID)` (full cleanup)
   - Default → leave volume `available`, remove CSI metadata

**Idempotency:** Handle all partial states — volume deleted, Shadow VM deleted,
volume already detached.

### ControllerPublishVolume

**Input:** `volumeID`, `nodeID`, `volumeCapability`, `volumeContext`, `secrets`

**Flow:**
1. Validate volume exists: `cloud.GetVolume(volumeID)`
2. Get attachment ID: `cloud.ListVolumeAttachments(volumeID)`
3. Get connection_info: `cloud.GetVolumeAttachment(attachmentID)`
4. Validate `driver_volume_type == "nfs"` — else return `INVALID_ARGUMENT`
5. Return `PublishContext` map

**PublishContext returned:**
```
"nfs_export"        → "192.168.57.105:/trident_pvc_xxx"
"nfs_volume_file"   → "volume-ba833668-xxx"
"nfs_mount_options" → "rw,hard,intr"
"volume_format"     → "raw"
"driver_volume_type"→ "nfs"
```

This context flows through the CO to `NodeStageVolume` and `NodePublishVolume`.

### ControllerUnpublishVolume

**ALWAYS A NO-OP.** Return `ControllerUnpublishVolumeResponse{}` immediately.

Reason: CDI multi-phase precopy cycles pods between stages. Between stages, the CO
fires `ControllerUnpublishVolume`. Shadow VM attachment must persist so the next
`ControllerPublishVolume` can query the same NFS connection info.

### ControllerExpandVolume

**Flow:**
1. Get volume from Cinder
2. `cloud.ExpandVolume(volumeID, status, newSize)`
3. Return `NodeExpansionRequired: false` (NFS clients see new size automatically)

## Node Service

### Capabilities (`NodeGetCapabilities`)

| Capability | Supported | Notes |
|------------|-----------|-------|
| `STAGE_UNSTAGE_VOLUME` | Yes | NFS mount/unmount at staging path |
| `GET_VOLUME_STATS` | Yes | `statfs` on NFS mount |
| `EXPAND_VOLUME` | No | NFS expansion is controller-side only |

### NodeStageVolume

**Input:** `volumeID`, `publishContext`, `stagingTargetPath`, `volumeCapability`

**Flow:**
1. Parse `publishContext`:
   - `nfs_export` = NFS server:path
   - `nfs_volume_file` = volume filename
   - `nfs_mount_options` = mount options
2. Idempotency: if `stagingTargetPath` already mounted → return OK
3. `mkdir -p ${stagingTargetPath}`
4. `mount -t nfs -o ${mount_opts} ${nfs_export} ${stagingTargetPath}`
5. Verify volume file exists: `stat ${stagingTargetPath}/${volume_file}`

### NodeUnstageVolume

**Flow:**
1. `umount ${stagingTargetPath}`
2. `rmdir ${stagingTargetPath}`

### NodePublishVolume

**Input:** `volumeID`, `stagingTargetPath`, `targetPath`, `volumeCapability`, `publishContext`

**Flow:**
1. `source = ${stagingTargetPath}/${publishContext["nfs_volume_file"]}`
2. Idempotency: if `targetPath` already bind-mounted → return OK
3. For Block access type:
   - `touch ${targetPath}` (create target file)
   - `mount --bind ${source} ${targetPath}`

### NodeUnpublishVolume

**Flow:**
1. `umount ${targetPath}`
2. Remove target file/dir

### NodeGetInfo

Returns WRCP host ID + topology label. `MaxVolumesPerNode` = unlimited for NFS.

### NodeGetVolumeStats

`statfs(volumePath)` on NFS mount → return bytes + inodes usage.

### NodeExpandVolume

No-op for NFS — expansion is controller-side only. NFS clients see new size
immediately after Cinder extend.

## End-to-end CSI call sequence (CDI multi-phase)

```
PVC Created → CreateVolume (Cinder + Shadow VM)
  ╔═ CDI Stage 1 (Full Copy) ═══════════════════════════╗
  ║ ControllerPublishVolume → query NFS info             ║
  ║ NodeStageVolume → mount NFS                          ║
  ║ NodePublishVolume → bind mount volume file           ║
  ║ CDI writes full disk copy                            ║
  ║ NodeUnpublishVolume + NodeUnstageVolume → umount      ║
  ║ ControllerUnpublishVolume → NO-OP                    ║
  ╚══════════════════════════════════════════════════════╝
  (gap: no pod, Shadow VM still attached, volume in-use)
  ╔═ CDI Stage N (Precopy) ═════════════════════════════╗
  ║ ControllerPublishVolume → query SAME attachment      ║
  ║ NodeStage + NodePublish → mount NFS + bind mount     ║
  ║ CDI writes delta copy                                ║
  ║ NodeUnpublish + NodeUnstage + ControllerUnpublish    ║
  ╚══════════════════════════════════════════════════════╝
  ... repeat for precopy N ...
PVC Deleted → DeleteVolume (detach + delete Shadow VM → volume "available")
Blueprint → set bootable → server create --volume
```
