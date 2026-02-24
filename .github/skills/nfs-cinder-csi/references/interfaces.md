# IOpenStackNFS Interface & Config Structs

## IOpenStackNFS interface

Defined in `pkg/csi/cinder-nfs/openstack/openstack.go`:

```go
type IOpenStackNFS interface {
    // ── Volume Operations (Cinder) ──
    CreateVolume(ctx context.Context, opts *volumes.CreateOpts,
        schedulerHints volumes.SchedulerHintOptsBuilder) (*volumes.Volume, error)
    DeleteVolume(ctx context.Context, volumeID string) error
    GetVolume(ctx context.Context, volumeID string) (*volumes.Volume, error)
    GetVolumesByName(ctx context.Context, name string) ([]volumes.Volume, error)
    ExpandVolume(ctx context.Context, volumeID string, status string, newSize int) error
    WaitVolumeTargetStatus(ctx context.Context, volumeID string, tStatus []string) error

    // ── Volume Attachment Operations (Cinder v3) ──
    CreateVolumeAttachment(ctx context.Context, volumeID, serverID string) (*VolumeAttachment, error)
    GetVolumeAttachment(ctx context.Context, volumeID, serverID string) (*VolumeAttachment, error)
    DeleteVolumeAttachment(ctx context.Context, attachmentID string) error
    GetConnectionInfo(ctx context.Context, volumeID, serverID string) (*NFSConnectionInfo, error)

    // ── Shadow VM Operations (Nova) ──
    CreateServer(ctx context.Context, opts *ServerCreateOpts) (*servers.Server, error)
    GetServer(ctx context.Context, serverID string) (*servers.Server, error)
    StopServer(ctx context.Context, serverID string) error
    DeleteServer(ctx context.Context, serverID string) error
    WaitServerStatus(ctx context.Context, serverID string, targetStatus string) error

    // ── Snapshot Operations (Cinder) ──
    CreateSnapshot(ctx context.Context, name, volID string, tags map[string]string) (*snapshots.Snapshot, error)
    DeleteSnapshot(ctx context.Context, snapID string) error
    GetSnapshotByID(ctx context.Context, snapshotID string) (*snapshots.Snapshot, error)
    ListSnapshots(ctx context.Context, filters map[string]string) ([]snapshots.Snapshot, string, error)
    WaitSnapshotReady(ctx context.Context, snapshotID string) (string, error)

    // ── Configuration ──
    GetNFSOpts() NFSOpts
    GetShadowVMOpts() ShadowVMOpts
}
```

## Data types

```go
// NFSConnectionInfo from Cinder volume attachment connection_info
type NFSConnectionInfo struct {
    DriverVolumeType string // "nfs"
    ExportPath       string // "10.0.0.5:/cinder-volumes/vol-xxx"
    NFSServer        string // "10.0.0.5"
    NFSSharePath     string // "/cinder-volumes/vol-xxx"
    Options          string // NFS mount options from backend
}

// VolumeAttachment wraps Cinder v3 volume attachment
type VolumeAttachment struct {
    ID             string
    VolumeID       string
    ServerID       string
    Status         string
    ConnectionInfo *NFSConnectionInfo
}

// ServerCreateOpts for Shadow VM creation
type ServerCreateOpts struct {
    Name      string
    FlavorID  string
    ImageID   string
    NetworkID string
    ProjectID string
    Metadata  map[string]string
}
```

## Configuration structs

Parsed from `driver.conf` (ConfigMap) using `gcfg`:

```go
type NfsCinderConfig struct {
    Global   map[string]*client.AuthOpts  // from cloud.conf (Secret)
    ShadowVM ShadowVMOpts                 // from driver.conf [ShadowVM]
    NFS      NFSOpts                      // from driver.conf [NFS]
    Volume   VolumeOpts                   // from driver.conf [Volume]
}

type ShadowVMOpts struct {
    FlavorRef        string `gcfg:"flavor-ref"`        // Required. Nova flavor
    NetworkID        string `gcfg:"network-id"`        // Required. Neutron network UUID
    AvailabilityZone string `gcfg:"availability-zone"` // Default: "" (Nova picks)
    NamePrefix       string `gcfg:"name-prefix"`       // Default: "shadow"
    CreateTimeout    int    `gcfg:"create-timeout"`    // Default: 300s
    StopTimeout      int    `gcfg:"stop-timeout"`      // Default: 120s
    SecurityGroups   string `gcfg:"security-groups"`   // Optional comma-separated UUIDs
    ImageRef         string `gcfg:"image-ref"`         // Optional (boot from volume default)
}

type NFSOpts struct {
    MountOptions  string `gcfg:"mount-options"`    // Default: "rw,hard,intr"
    MountBasePath string `gcfg:"mount-base-path"`  // Default: "/var/lib/cinder-nfs"
    NFSVersion    string `gcfg:"nfs-version"`      // Default: "4.1"
}

type VolumeOpts struct {
    CreateTimeout     int    `gcfg:"create-timeout"`      // Default: 300s
    DetachTimeout     int    `gcfg:"detach-timeout"`      // Default: 120s
    DefaultVolumeType string `gcfg:"default-volume-type"` // Optional
    MetadataPrefix    string `gcfg:"metadata-prefix"`     // Default: "csi"
}
```

## Configuration validation (startup)

| Field | Required | Default | Validation |
|-------|----------|---------|------------|
| `ShadowVM.flavor-ref` | Yes | — | Must resolve to valid Nova flavor |
| `ShadowVM.network-id` | Yes | — | Must be valid Neutron network UUID |
| `ShadowVM.availability-zone` | No | `""` | If set, must match existing Nova AZ |
| `ShadowVM.name-prefix` | No | `shadow` | Non-empty, safe for Nova server names |
| `ShadowVM.create-timeout` | No | `300` | > 0 |
| `ShadowVM.stop-timeout` | No | `120` | > 0 |
| `NFS.mount-options` | No | `rw,hard,intr` | Valid NFS mount option string |
| `NFS.mount-base-path` | No | `/var/lib/cinder-nfs` | Absolute path, writable |
| `NFS.nfs-version` | No | `4.1` | `3`, `4`, `4.0`, `4.1`, or `4.2` |
| `Volume.create-timeout` | No | `300` | > 0 |
| `Volume.detach-timeout` | No | `120` | > 0 |
| `Volume.metadata-prefix` | No | `csi` | Non-empty |

## StorageClass parameter overrides

ConfigMap defaults can be overridden per-PVC via StorageClass parameters:

```yaml
parameters:
  type: netapp-nfs                          # → Cinder volume type
  availability: az-2                        # → Volume AZ
  shadow-vm-flavor-ref: m1.tiny             # → Override ShadowVM.flavor-ref
  shadow-vm-network-id: <uuid>             # → Override ShadowVM.network-id
  nfs-mount-options: "rw,hard,intr,nfsvers=3" # → Override NFS.mount-options
```

**Resolution order (highest priority first):**
1. StorageClass parameters (per-PVC override)
2. ConfigMap `driver.conf` (cluster-wide defaults)
3. Hardcoded defaults in driver code

## Key differences from existing IOpenStack

| Existing `IOpenStack` (block) | `IOpenStackNFS` (NFS) |
|-------------------------------|----------------------|
| `AttachVolume(instanceID, volumeID)` (Nova attach) | `CreateVolumeAttachment(volumeID, serverID)` (Cinder v3) |
| `DetachVolume(instanceID, volumeID)` (Nova detach) | `DeleteVolumeAttachment(attachmentID)` |
| `WaitDiskAttached` / `WaitDiskDetached` | `WaitServerStatus` / `WaitVolumeTargetStatus` |
| `GetAttachmentDiskPath` → `/dev/vdb` | `GetConnectionInfo` → NFS export path |
| `GetInstanceByID` (single method) | `CreateServer`, `StopServer`, `DeleteServer`, `GetServer` |
| No Shadow VM concept | Full Shadow VM lifecycle |

## OpenStack struct (internal)

```go
type OpenStack struct {
    compute      *gophercloud.ServiceClient  // Nova
    blockstorage *gophercloud.ServiceClient  // Cinder
    nfsOpts      NFSOpts
    shadowVMOpts ShadowVMOpts
    volumeOpts   VolumeOpts
}
```

Same two clients as existing driver (compute + blockstorage), but with additional
methods and a different interface contract.
