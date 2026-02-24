# NFS-Backed Cinder CSI Plugin — Detailed Implementation Design

| Field          | Value                                                    |
|----------------|----------------------------------------------------------|
| **Authors**    | NFS-Cinder CSI Design Team                               |
| **Status**     | Draft                                                    |
| **Created**    | 2025-07-08                                               |
| **Depends On** | [NFS-Backed Cinder Volume for WRCP Migration — Design Proposal](nfs-backed-cinder-volume-for-wrcp-migration.md) |
| **Repository** | `kubernetes/cloud-provider-openstack`                    |

---

## Table of Contents

- [NFS-Backed Cinder CSI Plugin — Detailed Implementation Design](#nfs-backed-cinder-csi-plugin--detailed-implementation-design)
  - [Table of Contents](#table-of-contents)
  - [1. Summary](#1-summary)
    - [Conclusion](#conclusion)
  - [2. Existing Cinder CSI Code Analysis](#2-existing-cinder-csi-code-analysis)
    - [2.1 Package Layout](#21-package-layout)
    - [2.2 driver.go — Driver Struct and Initialization](#22-drivergo--driver-struct-and-initialization)
    - [2.3 controllerserver.go — Controller RPCs](#23-controllerservergo--controller-rpcs)
    - [2.4 nodeserver.go — Node RPCs](#24-nodeservergo--node-rpcs)
    - [2.5 identityserver.go — Identity RPCs](#25-identityservergo--identity-rpcs)
    - [2.6 openstack/ — OpenStack API Layer](#26-openstack--openstack-api-layer)
    - [2.7 server.go and utils.go — gRPC Infrastructure](#27-servergo-and-utilsgo--grpc-infrastructure)
    - [2.8 cmd/cinder-csi-plugin/main.go — Binary Entry Point](#28-cmdcinder-csi-pluginmaingo--binary-entry-point)
  - [3. Architectural Assessment: Extension vs. New Package](#3-architectural-assessment-extension-vs-new-package)
    - [3.1 Why the Existing Code Cannot Be Extended](#31-why-the-existing-code-cannot-be-extended)
    - [3.2 Reusable Components](#32-reusable-components)
  - [4. Comparison with Manila CSI Driver](#4-comparison-with-manila-csi-driver)
  - [5. Proposed Folder Structure](#5-proposed-folder-structure)
    - [5.1 New Directories and Files](#51-new-directories-and-files)
    - [5.2 Files to Modify in Existing Tree](#52-files-to-modify-in-existing-tree)
  - [6. Interface Design](#6-interface-design)
    - [6.1 IOpenStackNFS Interface](#61-iopenstacknfs-interface)
    - [6.2 NFS-Specific Config Structs](#62-nfs-specific-config-structs)
    - [6.3 Shadow VM Lifecycle State Machine](#63-shadow-vm-lifecycle-state-machine)
  - [7. CSI RPC Implementation Map](#7-csi-rpc-implementation-map)
    - [7.1 Identity Service](#71-identity-service)
    - [7.2 Controller Service](#72-controller-service)
      - [CreateVolume](#createvolume)
      - [DeleteVolume](#deletevolume)
      - [ControllerPublishVolume](#controllerpublishvolume)
      - [ControllerUnpublishVolume](#controllerunpublishvolume)
      - [ControllerExpandVolume](#controllerexpandvolume)
    - [7.3 Node Service](#73-node-service)
      - [NodeStageVolume](#nodestagevolume)
      - [NodeUnstageVolume](#nodeunstagevolume)
      - [NodePublishVolume](#nodepublishvolume)
      - [NodeUnpublishVolume](#nodeunpublishvolume)
      - [NodeGetInfo](#nodegetinfo)
      - [NodeGetVolumeStats](#nodegetvolumestats)
      - [NodeExpandVolume](#nodeexpandvolume)
  - [8. Build and Deployment](#8-build-and-deployment)
    - [8.1 Makefile Changes](#81-makefile-changes)
    - [8.2 Dockerfile Stage](#82-dockerfile-stage)
      - [8.2.1 Development Phase — Debian-Based Image (Current)](#821-development-phase--debian-based-image-current)
      - [8.2.2 Production Phase — 3-Step Distroless Build (TODO)](#822-production-phase--3-step-distroless-build-todo)
    - [8.3 Kubernetes Manifests](#83-kubernetes-manifests)
    - [8.4 Helm Chart](#84-helm-chart)
    - [8.5 Metrics](#85-metrics)
    - [8.6 Release Procedure Alignment](#86-release-procedure-alignment)
  - [9. Development Phases](#9-development-phases)
    - [Phase 1 — Scaffold and Interface](#phase-1--scaffold-and-interface)
    - [Phase 2 — Controller Service (Core)](#phase-2--controller-service-core)
    - [Phase 3 — Node Service (NFS Mount)](#phase-3--node-service-nfs-mount)
    - [Phase 4 — Integration and E2E](#phase-4--integration-and-e2e)
    - [Phase 5 — CDI Multi-Phase Precopy](#phase-5--cdi-multi-phase-precopy)
  - [Appendix A: Key Differences from Existing Cinder CSI at a Glance](#appendix-a-key-differences-from-existing-cinder-csi-at-a-glance)
  - [Appendix B: File-Level Reuse Decision Matrix](#appendix-b-file-level-reuse-decision-matrix)

---

## 1. Summary

This document provides a detailed implementation design for the NFS-backed Cinder CSI
driver (`cinder-nfs.csi.windriver.com`), based on the analysis of the existing Cinder CSI
driver codebase in `cloud-provider-openstack`.

### Conclusion

**The existing Cinder CSI driver does NOT support a plugin-like architecture for the
NFS-backed driver. A new, separate package is required.**

The rationale is summarized below:

| Dimension              | Existing Cinder CSI                        | NFS-Cinder Requirement                            | Compatible? |
|------------------------|--------------------------------------------|----------------------------------------------------|:-----------:|
| Driver name            | `cinder.csi.openstack.org` (hardcoded)     | `cinder-nfs.csi.windriver.com`                     | **No**      |
| Controller Publish     | Nova `AttachVolume` (block device)          | Query `connection_info` for NFS export path         | **No**      |
| Controller Unpublish   | Nova `DetachVolume`                         | No-op (NFS requires no server-side detach)          | **No**      |
| Create Volume          | Cinder POST only                           | Cinder POST + Shadow VM create + attach + stop      | **No**      |
| Delete Volume          | Cinder DELETE only                          | Shadow VM delete + Cinder DELETE                    | **No**      |
| Node Stage             | `getDevicePath` + `FormatAndMount`          | `mount -t nfs <export_path> <staging_target>`       | **No**      |
| Node Unstage           | `Unmount` block device                      | `umount` NFS mount                                  | Partial     |
| Volume access mode     | `SINGLE_NODE_WRITER`                        | `MULTI_NODE_MULTI_WRITER`                           | **No**      |
| IOpenStack interface   | Block-attach methods (25 methods)           | Needs Shadow VM lifecycle + connection_info methods  | **No**      |
| Node capabilities      | Block resize, device stats                  | No block resize; NFS stats via `statfs`             | **No**      |

The project follows the precedent set by the **Manila CSI driver** (`pkg/csi/manila/`),
which is a fully separate package with its own driver, servers, binary, and manifests,
demonstrating that the monorepo supports multiple independent CSI drivers.

---

## 2. Existing Cinder CSI Code Analysis

### 2.1 Package Layout

```
pkg/csi/
├── csi.go                         # Shared constants, PVC lister, topology helpers
├── cinder/                        # ← Existing Cinder block CSI driver
│   ├── driver.go                  # Driver struct, capabilities, NewDriver(), Run()
│   ├── controllerserver.go        # Controller RPCs (1118 lines)
│   ├── nodeserver.go              # Node RPCs (443 lines)
│   ├── identityserver.go          # Identity RPCs
│   ├── server.go                  # NonBlockingGRPCServer, gRPC unix socket
│   ├── utils.go                   # Capability factories, gRPC logger, RunServicesInitialized
│   └── openstack/                 # OpenStack API wrapper
│       ├── openstack.go           # IOpenStack interface, Config, CreateOpenStackProvider
│       ├── openstack_volumes.go   # Volume CRUD, attach/detach, wait loops
│       ├── openstack_instances.go # GetInstanceByID (single method)
│       ├── openstack_snapshots.go # Snapshot operations
│       ├── openstack_backups.go   # Backup operations
│       ├── openstack_mock.go      # Mock for unit tests
│       └── fixtures/              # Test fixtures
├── manila/                        # ← Manila CSI driver (separate package, precedent)
│   ├── driver.go
│   ├── controllerserver.go
│   ├── nodeserver.go
│   ├── identityserver.go
│   ├── csiclient/
│   ├── manilaclient/
│   └── ...
```

### 2.2 driver.go — Driver Struct and Initialization

**File:** `pkg/csi/cinder/driver.go` (209 lines)

Key observations:

1. **Driver name is a hardcoded constant:**
   ```go
   const driverName = "cinder.csi.openstack.org"
   ```
   The NFS driver requires a different name (`cinder-nfs.csi.windriver.com`) to be
   registered as a separate CSI driver in Kubernetes. The name is used in `CSIDriver`
   resource, socket path, and volume handle resolution.

2. **Capabilities are block-device specific:**
   ```go
   d.AddVolumeCapabilityAccessModes(
       []csi.VolumeCapability_AccessMode_Mode{
           csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
       })
   ```
   NFS driver needs `MULTI_NODE_MULTI_WRITER`, `MULTI_NODE_READER_ONLY`, and
   `SINGLE_NODE_WRITER`.

3. **Controller capabilities include block-only operations:**
   ```go
   csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,   // Nova attach
   csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
   csi.ControllerServiceCapability_RPC_LIST_VOLUMES_PUBLISHED_NODES, // Nova attachment
   csi.ControllerServiceCapability_RPC_GET_VOLUME,
   ```
   NFS driver may not need `PUBLISH_UNPUBLISH_VOLUME` (Shadow VM manages attachment
   implicitly) or `LIST_VOLUMES_PUBLISHED_NODES` (NFS volumes aren't "attached" to nodes).

4. **`SetupControllerService` accepts `map[string]openstack.IOpenStack`:**
   ```go
   func (d *Driver) SetupControllerService(clouds map[string]openstack.IOpenStack) {
       d.cs = NewControllerServer(d, clouds)
   }
   ```
   NFS driver needs a different interface (e.g., `IOpenStackNFS`) with Shadow VM methods.

5. **`SetupNodeService` accepts `BlockStorageOpts`:**
   ```go
   func (d *Driver) SetupNodeService(mount mount.IMount, metadata metadata.IMetadata,
       opts openstack.BlockStorageOpts, topologies map[string]string) {
   ```
   NFS driver doesn't use `BlockStorageOpts`; it needs NFS-specific options (e.g., mount
   flags, NFS version).

### 2.3 controllerserver.go — Controller RPCs

**File:** `pkg/csi/cinder/controllerserver.go` (1118 lines)

Every Controller RPC is deeply coupled to the block-attach model:

| RPC                        | Existing Implementation                              | NFS-Cinder Requirement                                          |
|----------------------------|------------------------------------------------------|-----------------------------------------------------------------|
| `CreateVolume`             | `cloud.CreateVolume(opts, schedulerHints)` — Cinder POST only | Cinder POST + Shadow VM create + attach volume + stop Shadow VM |
| `DeleteVolume`             | `cloud.DeleteVolume(volumeID)` — Cinder DELETE only | Shadow VM delete + detach volume + Cinder DELETE                |
| `ControllerPublishVolume`  | `cloud.AttachVolume(instanceID, volumeID)` — Nova attach | Query `connection_info` from Cinder attachment → return NFS export path |
| `ControllerUnpublishVolume`| `cloud.DetachVolume(instanceID, volumeID)` — Nova detach | No-op (NFS export path remains valid; Shadow VM owns attachment) |
| `ValidateVolumeCapabilities` | Checks `SINGLE_NODE_WRITER` | Check `MULTI_NODE_MULTI_WRITER`                                 |
| `ListVolumes`              | Lists all Cinder volumes | Lists only NFS-backed Cinder volumes (filter by volume_type or metadata) |
| `CreateSnapshot`           | Cinder snapshot | Same (reusable logic)                                          |
| `ExpandVolume`             | Cinder extend | Cinder extend (potentially reusable)                           |

**Critical code path in `ControllerPublishVolume`:**
```go
func (cs *controllerServer) ControllerPublishVolume(...) {
    instanceID := req.GetNodeId()                              // K8s node = Nova instance
    cloud.AttachVolume(ctx, instanceID, volumeID)              // Nova attach API
    cloud.WaitDiskAttached(ctx, instanceID, volumeID)          // Poll until attached
    devicePath, _ := cloud.GetAttachmentDiskPath(ctx, ...)     // /dev/vdb
    return &csi.ControllerPublishVolumeResponse{
        PublishContext: map[string]string{"DevicePath": devicePath},
    }
}
```
NFS driver instead needs:
```go
// Pseudocode for NFS ControllerPublishVolume
func (cs *nfsControllerServer) ControllerPublishVolume(...) {
    // Volume is already attached to Shadow VM during CreateVolume
    connInfo, _ := cloud.GetVolumeConnectionInfo(ctx, volumeID, shadowVMID)
    nfsExportPath := connInfo.Data.ExportPath  // e.g., "10.0.0.5:/cinder-volumes/vol-xxx"
    return &csi.ControllerPublishVolumeResponse{
        PublishContext: map[string]string{"nfsExportPath": nfsExportPath},
    }
}
```

These are fundamentally incompatible code paths — no amount of `if/else` branching
within the existing `controllerServer` would be clean or maintainable.

### 2.4 nodeserver.go — Node RPCs

**File:** `pkg/csi/cinder/nodeserver.go` (443 lines)

The node server is entirely block-device oriented:

| RPC                    | Existing Implementation                              | NFS-Cinder Requirement                    |
|------------------------|------------------------------------------------------|-------------------------------------------|
| `NodeStageVolume`      | `getDevicePath(volumeID, m)` → `FormatAndMount(devicePath, stagingTarget, fsType, options)` | `mount -t nfs <nfsExportPath> <stagingTarget>` |
| `NodeUnstageVolume`    | `Unmount(stagingTargetPath)` | `umount <stagingTargetPath>` (same)       |
| `NodePublishVolume`    | `Mount(source, targetPath, fsType, ["bind"])` — bind mount from staging | Bind mount from staging (same pattern)   |
| `NodeUnpublishVolume`  | `UnmountPath(targetPath)` | `umount <targetPath>` (same)              |
| `NodeGetInfo`          | `metadata.GetInstanceID()` — Nova instance ID | Custom node ID (hostName-based, not Nova) |
| `NodeGetVolumeStats`   | `GetDeviceStats(volumePath)` — block device stats | `statfs` on NFS mount                    |
| `NodeExpandVolume`     | `blockdevice.RescanBlockDeviceGeometry` + `Resize` | N/A — NFS resize is controller-side only  |

**Critical code path in `NodeStageVolume`:**
```go
func (ns *nodeServer) NodeStageVolume(...) {
    devicePath, err := getDevicePath(volumeID, m)    // scans /dev/ for SCSI disk serial
    // ...
    err = ns.formatAndMountRetry(devicePath, stagingTarget, fsType, options)
}
```
This assumes a block device exists on the node. NFS driver has no block device — it
receives `nfsExportPath` in `PublishContext` and calls `mount -t nfs`.

**`NodeGetInfo` returns Nova instance ID:**
```go
func (ns *nodeServer) NodeGetInfo(...) {
    nodeID, err := ns.Metadata.GetInstanceID()  // Nova instance ID of the K8s node
    // ...
}
```
NFS driver may still use Nova instance ID as node ID (K8s nodes are Nova instances in
OpenStack), but the node does NOT participate in Nova volume attachment. The node ID
is only used for CSI bookkeeping.

### 2.5 identityserver.go — Identity RPCs

**File:** `pkg/csi/cinder/identityserver.go` (98 lines)

- `GetPluginInfo` returns `Driver.name` and `Driver.fqVersion` — reusable pattern
- `GetPluginCapabilities` lists `CONTROLLER_SERVICE`, `VOLUME_EXPANSION` (online + offline),
  and optionally `VOLUME_ACCESSIBILITY_CONSTRAINTS`
- NFS driver needs `CONTROLLER_SERVICE` and `VOLUME_EXPANSION` (offline only — NFS resize
  doesn't require node-side action)

**Verdict:** The identity server is simple and generic. The pattern is reusable but the
capabilities differ, so a new identity server with NFS-specific capabilities is needed.

### 2.6 openstack/ — OpenStack API Layer

**File:** `pkg/csi/cinder/openstack/openstack.go` (244 lines)

The `IOpenStack` interface has 25 methods, all targeting block-device lifecycle:

```go
type IOpenStack interface {
    CreateVolume(ctx, opts, schedulerHints) (*volumes.Volume, error)
    DeleteVolume(ctx, volumeID) error
    AttachVolume(ctx, instanceID, volumeID) (string, error)       // Nova attach
    DetachVolume(ctx, instanceID, volumeID) error                 // Nova detach
    WaitDiskAttached(ctx, instanceID, volumeID) error             // Poll Nova
    WaitDiskDetached(ctx, instanceID, volumeID) error             // Poll Nova
    GetAttachmentDiskPath(ctx, instanceID, volumeID) (string, error) // /dev/vdb
    GetVolume(ctx, volumeID) (*volumes.Volume, error)
    GetVolumesByName(ctx, name) ([]volumes.Volume, error)
    // ... snapshots, backups, expand ...
    GetInstanceByID(ctx, instanceID) (*servers.Server, error)     // Single compute method
    GetMaxVolLimit() int64
    GetBlockStorageOpts() BlockStorageOpts
}
```

**Missing for NFS driver:**

| Method Needed                           | Purpose                                                   |
|-----------------------------------------|-----------------------------------------------------------|
| `CreateServer(ctx, opts)`               | Create Shadow VM                                          |
| `StopServer(ctx, serverID)`             | Stop Shadow VM after volume attach                       |
| `DeleteServer(ctx, serverID)`           | Delete Shadow VM during volume deletion                  |
| `WaitServerStatus(ctx, serverID, status)` | Wait for Shadow VM state transitions                   |
| `GetVolumeAttachment(ctx, volID, serverID)` | Get `connection_info` for NFS export path            |
| `CreateVolumeAttachment(ctx, volID, serverID)` | Attach volume to Shadow VM                         |
| `DeleteVolumeAttachment(ctx, volID, serverID)` | Detach volume from Shadow VM                       |

**The `OpenStack` struct only holds two clients:**
```go
type OpenStack struct {
    compute      *gophercloud.ServiceClient
    blockstorage *gophercloud.ServiceClient
    bsOpts       BlockStorageOpts
    // ...
}
```
These clients are sufficient for the NFS driver (Shadow VM uses compute, volumes use
blockstorage), but the struct needs additional methods and a different interface contract.

**`openstack_instances.go`** has only one method:
```go
func (os *OpenStack) GetInstanceByID(ctx, instanceID) (*servers.Server, error)
```
NFS driver needs full server lifecycle (Create, Stop, Delete, WaitStatus).

### 2.7 server.go and utils.go — gRPC Infrastructure

**File:** `pkg/csi/cinder/server.go` (~120 lines)

The `NonBlockingGRPCServer` is generic CSI gRPC infrastructure:
```go
type NonBlockingGRPCServer interface {
    Start(endpoint string, ids csi.IdentityServer, cs csi.ControllerServer, ns csi.NodeServer)
    Wait()
    Stop()
    ForceStop()
}
```

**Verdict:** Fully reusable. The gRPC server, unix socket listener, and logging interceptor
are protocol-agnostic. However, they are defined in `package cinder` (unexported), so the
NFS driver package cannot import them directly. Options:
1. Copy `server.go` + `utils.go` into the new package (simple, some duplication)
2. Extract to a shared package `pkg/csi/shared/` (cleaner, larger refactor)
3. Re-implement minimal gRPC server in the NFS package

**Recommendation:** Option 1 for initial implementation, with follow-up refactor to Option 2.

**File:** `pkg/csi/cinder/utils.go` (122 lines)

Contains capability factory functions (`NewControllerServiceCapability`,
`NewNodeServiceCapability`, `NewVolumeCapabilityAccessMode`), `NewControllerServer`,
`NewNodeServer`, `RunServicesInitialized`, `ParseEndpoint`, `logGRPC`.

These are small utility functions. The NFS package will have its own versions.

### 2.8 cmd/cinder-csi-plugin/main.go — Binary Entry Point

**File:** `cmd/cinder-csi-plugin/main.go` (153 lines)

Key flow:
```go
func handle() {
    d := cinder.NewDriver(&cinder.DriverOpts{...})
    openstack.InitOpenStackProvider(cloudConfig, httpEndpoint)

    // Build cloud map
    clouds := make(map[string]openstack.IOpenStack)
    for _, cloudName := range cloudNames {
        cloud, _ := openstack.GetOpenStackProvider(cloudName)
        clouds[cloudName] = cloud
    }

    // Setup services based on flags
    if provideControllerService {
        d.SetupControllerService(clouds)
    }
    if provideNodeService {
        d.SetupNodeService(mount, metadata, cfg.BlockStorage, topologies)
    }

    d.Run()
}
```

**NFS driver needs its own binary with:**
- Different driver name and version
- NFS-specific config loading (driver.conf ConfigMap + cloud.conf Secret)
- Shadow VM config validation
- Different service setup (NFS controller, NFS node)

---

## 3. Architectural Assessment: Extension vs. New Package

### 3.1 Why the Existing Code Cannot Be Extended

| Approach Considered          | Why It Fails                                                            |
|------------------------------|-------------------------------------------------------------------------|
| **Add NFS mode flag to existing driver** | Controller and node RPCs have incompatible code paths. Would require every RPC to have `if nfsMode { ... } else { ... }` branching, destroying readability and testability. Driver name must differ. |
| **Subclass `controllerServer`** | Go does not support class inheritance. Struct embedding would inherit all block methods, requiring method override of every single RPC. |
| **Interface adapter pattern** | `IOpenStack` interface would need to be split or duplicated. Mock complexity doubles. Every RPC function signature and return value differs. |
| **Strategy pattern inside RPCs** | Would require injecting strategy objects into every RPC, changing the existing driver's internal structure — high risk of regression. |

**The fundamental issue:** The existing driver's abstractions (interface, structs, RPCs)
are designed around Nova block-device attachment as the _only_ volume access path. The NFS
driver uses a completely different access path (NFS export via Shadow VM). This isn't a
configuration difference — it's a structural difference in every layer.

### 3.2 Reusable Components

Despite the need for a separate package, significant code and patterns are reusable:

| Component                          | Location                          | Reuse Strategy              |
|------------------------------------|-----------------------------------|-----------------------------|
| gRPC server infrastructure         | `server.go`, `utils.go`          | Copy or extract to shared   |
| CSI shared constants/helpers       | `pkg/csi/csi.go`                 | Import directly             |
| OpenStack auth client              | `pkg/client/`                    | Import directly             |
| Config file parsing (`gcfg`)       | `openstack/openstack.go` pattern | Follow pattern, new structs |
| Metrics framework                  | `pkg/metrics/`                   | Import directly             |
| Metadata service                   | `pkg/util/metadata/`             | Import directly             |
| Mount utilities                    | `pkg/util/mount/`                | Import directly (NFS mount) |
| Error utilities                    | `pkg/util/errors/`               | Import directly             |
| Volume CRUD (partial)              | `openstack_volumes.go`           | Can extend OpenStack struct |
| Snapshot/Backup ops (partial)      | `openstack_snapshots.go`         | Can reuse if needed         |
| Build system patterns              | `Makefile`, `Dockerfile`         | Add parallel entries        |
| Deployment manifest patterns       | `manifests/cinder-csi-plugin/`   | Copy and adapt              |
| Helm chart patterns                | `charts/cinder-csi-plugin/`      | Copy and adapt              |

---

## 4. Comparison with Manila CSI Driver

The Manila CSI driver (`pkg/csi/manila/`) establishes the precedent for a second CSI
driver in this monorepo:

| Aspect                | Cinder CSI                           | Manila CSI                             | NFS-Cinder CSI (Proposed)              |
|-----------------------|--------------------------------------|----------------------------------------|----------------------------------------|
| Package               | `pkg/csi/cinder/`                   | `pkg/csi/manila/`                      | `pkg/csi/cinder-nfs/`                  |
| Binary                | `cmd/cinder-csi-plugin/`            | `cmd/manila-csi-plugin/`               | `cmd/cinder-nfs-csi-plugin/`           |
| Driver name           | `cinder.csi.openstack.org`          | `manila.csi.openstack.org`             | `cinder-nfs.csi.windriver.com`         |
| OpenStack service     | Cinder + Nova                        | Manila                                 | Cinder + Nova                          |
| Volume type           | Block (iSCSI/FC)                     | Shared filesystem (NFS/CephFS)         | NFS-backed Cinder (via Shadow VM)      |
| Own gRPC server       | Yes (`server.go`)                    | Yes (in `driver.go`)                   | Yes (copy from cinder `server.go`)     |
| Own controller server | Yes                                  | Yes                                    | Yes                                    |
| Own node server       | Yes                                  | Yes (proxied)                          | Yes                                    |
| Own identity server   | Yes                                  | Yes                                    | Yes                                    |
| Own OpenStack client  | `openstack/` subpackage              | `manilaclient/` subpackage             | `openstack/` subpackage (extended)     |
| Makefile entry        | `BUILD_CMDS`, `IMAGE_NAMES`         | `BUILD_CMDS`, `IMAGE_NAMES`            | `BUILD_CMDS`, `IMAGE_NAMES`            |
| Dockerfile stage      | `cinder-csi-plugin`                 | `manila-csi-plugin`                    | `cinder-nfs-csi-plugin`                |
| Manifests             | `manifests/cinder-csi-plugin/`      | `manifests/manila-csi-plugin/`         | `manifests/cinder-nfs-csi-plugin/`     |
| Helm chart            | `charts/cinder-csi-plugin/`         | `charts/manila-csi-plugin/`            | `charts/cinder-nfs-csi-plugin/`        |

The key insight from Manila: it does NOT extend or embed the Cinder driver. It is a
completely independent implementation that happens to live in the same repository. The
NFS-Cinder driver follows this exact pattern.

---

## 5. Proposed Folder Structure

### 5.1 New Directories and Files

```
pkg/csi/cinder-nfs/
├── driver.go                     # NFS Driver struct, capabilities, NewDriver(), Run()
├── controllerserver.go           # NFS Controller RPCs (Shadow VM lifecycle)
├── nodeserver.go                 # NFS Node RPCs (NFS mount/unmount)
├── identityserver.go             # NFS Identity RPCs
├── server.go                     # NonBlockingGRPCServer (copied from cinder/)
├── utils.go                      # Capability factories, gRPC logger
├── shadowvm.go                   # Shadow VM lifecycle manager (create, stop, delete, wait)
├── connectioninfo.go             # Cinder attachment connection_info query + parsing
└── openstack/
    ├── openstack.go              # IOpenStackNFS interface, NFS Config structs, provider init
    ├── openstack_volumes.go      # Volume CRUD (Cinder API, extended for NFS)
    ├── openstack_servers.go      # Shadow VM server lifecycle (Nova API)
    ├── openstack_attachments.go  # Volume attachment with connection_info (Cinder v3 API)
    ├── openstack_mock.go         # Mock for unit tests
    └── fixtures/                 # Test fixtures

cmd/cinder-nfs-csi-plugin/
└── main.go                       # Binary entry point, CLI flags, NFS config loading

manifests/cinder-nfs-csi-plugin/
├── csi-secret-cinder-nfs.yaml           # Secret (cloud.conf credentials)
├── csi-configmap-cinder-nfs.yaml        # ConfigMap (driver.conf: ShadowVM, NFS opts)
├── cinder-nfs-csi-controllerplugin.yaml # Deployment (controller + sidecars)
├── cinder-nfs-csi-nodeplugin.yaml       # DaemonSet (node + registrar)
├── csi-cinder-nfs-driver.yaml           # CSIDriver resource
└── csi-cinder-nfs-storageclass.yaml     # StorageClass example

charts/cinder-nfs-csi-plugin/
├── Chart.yaml
├── values.yaml
├── README.md
└── templates/
    ├── controllerplugin-deployment.yaml
    ├── nodeplugin-daemonset.yaml
    ├── csidriver.yaml
    ├── secret.yaml
    ├── configmap.yaml
    └── storageclass.yaml

tests/
├── sanity/cinder-nfs/            # CSI sanity tests
└── e2e/cinder-nfs/               # E2E tests

tools/                                  # (production phase — see Section 8.2.2 TODO)
├── csi-nfs-deps.sh               # NFS client dependency extractor (Debian → /dest)
└── csi-nfs-deps-check.sh         # Validates NFS binaries in distroless image
```

### 5.2 Files to Modify in Existing Tree

| File           | Change                                                          |
|----------------|-----------------------------------------------------------------|
| `Makefile`     | Add `cinder-nfs-csi-plugin` to `IMAGE_NAMES` and `BUILD_CMDS`; add `test-cinder-nfs-csi-sanity` target; add `gox` cross-build entry |
| `Dockerfile`   | Add `cinder-nfs-csi-plugin` Debian-based stage (dev); migrate to 3-step distroless for production (see §8.2.2 TODO) |
| `go.mod`       | No change needed (same module)                                  |
| `OWNERS`       | Add reviewers/approvers for `pkg/csi/cinder-nfs/` (the review process requires 2× `/lgtm` from reviewers + `/approve` from OWNERS approver) |
| `tools/csi-nfs-deps.sh` | New file (production phase) — extracts NFS client binaries + shared libs into `/dest` (follows `tools/csi-deps.sh` pattern) |
| `tools/csi-nfs-deps-check.sh` | New file (production phase) — validates NFS utilities in distroless image (follows `tools/csi-deps-check.sh` pattern) |

---

## 6. Interface Design

### 6.1 IOpenStackNFS Interface

The NFS driver defines its own interface extending the volume/server operations needed:

```go
// pkg/csi/cinder-nfs/openstack/openstack.go

type IOpenStackNFS interface {
    // ── Volume Operations (Cinder) ──────────────────────────────────────
    CreateVolume(ctx context.Context, opts *volumes.CreateOpts,
        schedulerHints volumes.SchedulerHintOptsBuilder) (*volumes.Volume, error)
    DeleteVolume(ctx context.Context, volumeID string) error
    GetVolume(ctx context.Context, volumeID string) (*volumes.Volume, error)
    GetVolumesByName(ctx context.Context, name string) ([]volumes.Volume, error)
    ExpandVolume(ctx context.Context, volumeID string, status string, newSize int) error
    WaitVolumeTargetStatus(ctx context.Context, volumeID string, tStatus []string) error

    // ── Volume Attachment Operations (Cinder v3) ────────────────────────
    CreateVolumeAttachment(ctx context.Context, volumeID, serverID string) (*VolumeAttachment, error)
    GetVolumeAttachment(ctx context.Context, volumeID, serverID string) (*VolumeAttachment, error)
    DeleteVolumeAttachment(ctx context.Context, attachmentID string) error
    GetConnectionInfo(ctx context.Context, volumeID, serverID string) (*NFSConnectionInfo, error)

    // ── Shadow VM Operations (Nova) ─────────────────────────────────────
    CreateServer(ctx context.Context, opts *ServerCreateOpts) (*servers.Server, error)
    GetServer(ctx context.Context, serverID string) (*servers.Server, error)
    StopServer(ctx context.Context, serverID string) error
    DeleteServer(ctx context.Context, serverID string) error
    WaitServerStatus(ctx context.Context, serverID string, targetStatus string) error

    // ── Snapshot Operations (Cinder) ────────────────────────────────────
    CreateSnapshot(ctx context.Context, name, volID string, tags map[string]string) (*snapshots.Snapshot, error)
    DeleteSnapshot(ctx context.Context, snapID string) error
    GetSnapshotByID(ctx context.Context, snapshotID string) (*snapshots.Snapshot, error)
    ListSnapshots(ctx context.Context, filters map[string]string) ([]snapshots.Snapshot, string, error)
    WaitSnapshotReady(ctx context.Context, snapshotID string) (string, error)

    // ── Configuration ───────────────────────────────────────────────────
    GetNFSOpts() NFSOpts
    GetShadowVMOpts() ShadowVMOpts
}

// NFSConnectionInfo represents the connection_info from a Cinder volume attachment
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

### 6.2 NFS-Specific Config Structs

```go
// pkg/csi/cinder-nfs/openstack/openstack.go

type Config struct {
    Global   map[string]*client.AuthOpts
    Metadata metadata.Opts
    ShadowVM ShadowVMOpts
    NFS      NFSOpts
    Volume   VolumeOpts
}

type ShadowVMOpts struct {
    FlavorID        string `gcfg:"flavor-id"`
    ImageID         string `gcfg:"image-id"`
    SubnetID        string `gcfg:"subnet-id"`
    NetworkID       string `gcfg:"network-id"`
    AvailabilityZone string `gcfg:"availability-zone"`
    ProjectID       string `gcfg:"project-id"`
    SecurityGroupID string `gcfg:"security-group-id"`
    KeyName         string `gcfg:"key-name"`
    NamePrefix      string `gcfg:"name-prefix"`
    StopAfterAttach bool   `gcfg:"stop-after-attach"`
    DeleteOnVolDel  bool   `gcfg:"delete-on-volume-delete"`
}

type NFSOpts struct {
    MountOptions string `gcfg:"mount-options"`
    NFSVersion   string `gcfg:"nfs-version"`
    DefaultFsType string `gcfg:"default-fs-type"`
}

type VolumeOpts struct {
    DefaultVolumeType string `gcfg:"default-volume-type"`
    DefaultVolumeAZ   string `gcfg:"default-volume-az"`
    IgnoreVolumeAZ    bool   `gcfg:"ignore-volume-az"`
}
```

### 6.3 Shadow VM Lifecycle State Machine

```
                              ┌──────────────┐
                              │  No Shadow   │
                              │     VM       │
                              └──────┬───────┘
                                     │ CreateVolume called
                                     ▼
                              ┌──────────────┐
                              │   Creating   │
                              │  Shadow VM   │
                              └──────┬───────┘
                                     │ WaitServerStatus("ACTIVE")
                                     ▼
                              ┌──────────────┐
                              │    Active    │
                              │  Shadow VM   │
                              └──────┬───────┘
                                     │ CreateVolumeAttachment(volumeID, serverID)
                                     ▼
                              ┌──────────────┐
                              │   Volume     │
                              │  Attached    │
                              └──────┬───────┘
                                     │ StopServer (stop-after-attach=true)
                                     ▼
                              ┌──────────────┐
                              │   Stopped    │◄─── Steady State
                              │  Shadow VM   │     (volume attached, VM stopped,
                              └──────┬───────┘      NFS export available)
                                     │
                    ┌────────────────┼────────────────┐
                    │                │                │
                    ▼                ▼                ▼
            ControllerPublish   ExpandVolume     DeleteVolume
            (query conn_info)   (Cinder extend)  (cleanup)
                    │                                │
                    │                                ▼
                    │                         ┌──────────────┐
                    │                         │   Deleting   │
                    │                         │  Shadow VM   │
                    │                         └──────┬───────┘
                    │                                │
                    │                                ▼
                    │                         ┌──────────────┐
                    │                         │   Deleted    │
                    │                         └──────────────┘
                    │
                    ▼
            Return nfsExportPath
            in PublishContext
```

The Shadow VM ID is stored as volume metadata in Cinder:
```
cinder.csi.openstack.org/shadow-vm-id = <server-uuid>
```

---

## 7. CSI RPC Implementation Map

### 7.1 Identity Service

| RPC                      | Implementation                                        |
|--------------------------|-------------------------------------------------------|
| `GetPluginInfo`          | Return `cinder-nfs.csi.windriver.com` + version       |
| `GetPluginCapabilities`  | `CONTROLLER_SERVICE`, `VOLUME_EXPANSION` (offline)     |
| `Probe`                  | Return success (basic health check)                    |

### 7.2 Controller Service

#### CreateVolume

```
Input: name, capacity, parameters (volume_type, availability), secrets (cloud)
│
├── 1. Validate request parameters
├── 2. Check for existing volume by name (idempotency)
├── 3. Create Cinder volume: cloud.CreateVolume(opts)
├── 4. Wait for volume to be "available"
├── 5. Create Shadow VM: cloud.CreateServer(shadowVMOpts)
├── 6. Wait for Shadow VM to be "ACTIVE"
├── 7. Attach volume to Shadow VM: cloud.CreateVolumeAttachment(volID, serverID)
├── 8. Wait for volume to be "in-use"
├── 9. (Optional) Stop Shadow VM: cloud.StopServer(serverID)
├── 10. Store Shadow VM ID in volume metadata
│
Output: Volume{ID, CapacityBytes, VolumeContext{shadowVMID, nfsExportHint}}
```

#### DeleteVolume

```
Input: volumeID, secrets
│
├── 1. Get volume from Cinder: cloud.GetVolume(volumeID)
├── 2. Extract Shadow VM ID from volume metadata
├── 3. If Shadow VM exists:
│   ├── 3a. Detach volume: cloud.DeleteVolumeAttachment(volID, serverID)
│   ├── 3b. Wait for volume to be "available"
│   └── 3c. Delete Shadow VM: cloud.DeleteServer(serverID)
├── 4. Delete Cinder volume: cloud.DeleteVolume(volumeID)
│
Output: DeleteVolumeResponse{}
```

#### ControllerPublishVolume

```
Input: volumeID, nodeID, volumeCapability, secrets
│
├── 1. Get volume from Cinder
├── 2. Extract Shadow VM ID from volume metadata
├── 3. Query connection_info: cloud.GetConnectionInfo(volID, shadowVMID)
├── 4. Parse NFS export path from connection_info
│
Output: PublishContext{
    "nfsExportPath": "10.0.0.5:/cinder-volumes/vol-xxx",
    "nfsServer": "10.0.0.5",
    "nfsSharePath": "/cinder-volumes/vol-xxx"
}
```

#### ControllerUnpublishVolume

```
Input: volumeID, nodeID
│
├── 1. No-op (NFS export path remains valid)
│      Shadow VM owns the attachment; K8s nodes mount via NFS
│
Output: ControllerUnpublishVolumeResponse{}
```

#### ControllerExpandVolume

```
Input: volumeID, capacityRange
│
├── 1. Get volume from Cinder
├── 2. Expand: cloud.ExpandVolume(volumeID, status, newSize)
├── 3. Wait for expansion to complete
│
Output: ControllerExpandVolumeResponse{CapacityBytes, NodeExpansionRequired: false}
```

### 7.3 Node Service

#### NodeStageVolume

```
Input: volumeID, publishContext (nfsExportPath), stagingTargetPath, volumeCapability
│
├── 1. Extract nfsExportPath from publishContext
├── 2. Check if already mounted at stagingTarget
├── 3. Create staging target directory if needed
├── 4. Mount NFS:
│      mount -t nfs -o <nfsMountOptions> <nfsExportPath> <stagingTargetPath>
│
Output: NodeStageVolumeResponse{}
```

#### NodeUnstageVolume

```
Input: volumeID, stagingTargetPath
│
├── 1. Unmount: umount <stagingTargetPath>
│
Output: NodeUnstageVolumeResponse{}
```

#### NodePublishVolume

```
Input: volumeID, stagingTargetPath, targetPath, volumeCapability
│
├── 1. Create target directory if needed
├── 2. Bind mount: mount --bind <stagingTargetPath> <targetPath>
│
Output: NodePublishVolumeResponse{}
```

#### NodeUnpublishVolume

```
Input: volumeID, targetPath
│
├── 1. Unmount: umount <targetPath>
│
Output: NodeUnpublishVolumeResponse{}
```

#### NodeGetInfo

```
Input: (none)
│
├── 1. Get node ID (hostname or Nova instance ID)
│
Output: NodeGetInfoResponse{NodeId, MaxVolumesPerNode (unlimited for NFS)}
```

#### NodeGetVolumeStats

```
Input: volumeID, volumePath
│
├── 1. statfs(volumePath) for NFS mount stats
│
Output: NodeGetVolumeStatsResponse{Usage: [bytes, inodes]}
```

#### NodeExpandVolume

```
Input: volumeID, volumePath
│
├── 1. No-op for NFS (expansion is controller-side only)
│      NFS clients see new size immediately after Cinder extend
│
Output: NodeExpandVolumeResponse{}
```

---

## 8. Build and Deployment

### 8.1 Makefile Changes

Add `cinder-nfs-csi-plugin` to `IMAGE_NAMES` and `BUILD_CMDS`:

```makefile
IMAGE_NAMES ?= openstack-cloud-controller-manager \
               cinder-csi-plugin \
               cinder-nfs-csi-plugin \           # ← NEW
               k8s-keystone-auth \
               octavia-ingress-controller \
               manila-csi-plugin \
               barbican-kms-plugin \
               magnum-auto-healer

BUILD_CMDS  ?= openstack-cloud-controller-manager \
               cinder-csi-plugin \
               cinder-nfs-csi-plugin \            # ← NEW
               k8s-keystone-auth \
               octavia-ingress-controller \
               manila-csi-plugin \
               barbican-kms-plugin \
               magnum-auto-healer \
               client-keystone-auth
```

Note: `client-keystone-auth` is in `BUILD_CMDS` but not `IMAGE_NAMES` (it is a
CLI tool, not a container image). `cinder-nfs-csi-plugin` is both a build target
and a container image, so it appears in both lists.

Add a **sanity test target** following the existing pattern (`test-cinder-csi-sanity`,
`test-manila-csi-sanity`):

```makefile
test-cinder-nfs-csi-sanity: work
	go test $(GIT_HOST)/$(BASE_DIR)/tests/sanity/cinder-nfs
```

Add a **cross-build entry** in the `build-cross` target (each binary has an explicit
`gox` line):

```makefile
CGO_ENABLED=0 gox -parallel=$(GOX_PARALLEL) \
  -output="_dist/{{.OS}}-{{.Arch}}/{{.Dir}}" -osarch='$(TARGETS)' \
  $(GOFLAGS) $(if $(TAGS),-tags '$(TAGS)',) -ldflags '$(GOX_LDFLAGS)' \
  $(GIT_HOST)/$(BASE_DIR)/cmd/cinder-nfs-csi-plugin/
```

Build command (automatic via existing pattern):
```bash
make cinder-nfs-csi-plugin
# or
make build-local-image-cinder-nfs-csi-plugin
```

**Linting:** The project uses `golangci-lint` via `make check`:
```bash
make check   # runs golangci-lint on all packages including cinder-nfs
```
All new code must pass linting before PR review. The dev guide requires CI checks
to pass before review.

**Unit tests:** The project convention uses the `-tags=unit` build tag:
```bash
make unit    # runs go test -tags=unit on all packages
```
Unit test files should use `//go:build unit` build constraints where appropriate,
following the existing pattern in `pkg/csi/cinder/`.

### 8.2 Dockerfile Stage

The existing `cinder-csi-plugin` uses a **3-step build** in the Dockerfile: (1) install
and copy filesystem utilities from a Debian image via `tools/csi-deps.sh`, (2) validate
them in a check stage, (3) build the final distroless image. This minimizes image size
and attack surface for production.

For the NFS-Cinder driver, we take a **two-phase approach**:

#### 8.2.1 Development Phase — Debian-Based Image (Current)

During development, the image uses a **simple Debian base** with NFS tools installed
directly via `apt`. This prioritizes fast iteration and easy debugging over image size:

```dockerfile
##
## cinder-nfs-csi-plugin (development)
##
FROM ${DEBIAN_IMAGE} AS cinder-nfs-csi-plugin

RUN clean-install nfs-common mount util-linux

COPY --from=builder /build/cinder-nfs-csi-plugin /bin/cinder-nfs-csi-plugin
COPY --from=certs /etc/ssl/certs /etc/ssl/certs

LABEL name="cinder-nfs-csi-plugin" \
      license="Apache Version 2.0" \
      maintainers="Kubernetes Authors" \
      description="Cinder NFS CSI Plugin" \
      distribution-scope="public" \
      summary="Cinder NFS CSI Plugin for NFS-backed Cinder volumes" \
      help="none"

CMD ["/bin/cinder-nfs-csi-plugin"]
```

This gives the container `mount.nfs`, `mount.nfs4`, `umount.nfs`, `showmount`,
`nfsstat`, `rpcbind`, `findmnt`, and a full shell — useful for exec-ing into the
container to troubleshoot NFS mount issues during development.

#### 8.2.2 Production Phase — 3-Step Distroless Build (TODO)

> **TODO:** Before GA release, migrate to the 3-step distroless build pattern used by
> `cinder-csi-plugin` to minimize image size and attack surface. This involves:
>
> 1. Create `tools/csi-nfs-deps.sh` — extract only the NFS client binaries (`mount.nfs`,
>    `mount.nfs4`, `umount.nfs`, `findmnt`, `mount`, `umount`) and their shared library
>    dependencies into a `/dest` folder, following the `tools/csi-deps.sh` pattern.
> 2. Create `tools/csi-nfs-deps-check.sh` — validate the extracted binaries work in a
>    distroless context.
> 3. Switch the Dockerfile to the 3-step pattern:
>    - Step 1: `FROM ${DEBIAN_IMAGE}` — install `nfs-common` + run `csi-nfs-deps.sh`
>    - Step 2: `FROM ${DISTROLESS_IMAGE}` — copy `/dest` + run `csi-nfs-deps-check.sh`
>    - Step 3: `FROM ${DISTROLESS_IMAGE}` — final image with checked deps + binary
>
> The production image should NOT include a shell, package manager, or any tools beyond
> the minimum NFS mount utilities. Reference implementation:
>
> ```dockerfile
> FROM ${DEBIAN_IMAGE} AS cinder-nfs-csi-plugin-utils
> RUN clean-install bash rsync mount nfs-common util-linux
> COPY tools/csi-nfs-deps.sh /tools/csi-nfs-deps.sh
> RUN /tools/csi-nfs-deps.sh
>
> FROM ${DISTROLESS_IMAGE} AS cinder-nfs-csi-plugin-utils-check
> COPY --from=cinder-nfs-csi-plugin-utils /dest /
> COPY --from=cinder-nfs-csi-plugin-utils /bin/sh /bin/sh
> COPY tools/csi-nfs-deps-check.sh /tools/csi-nfs-deps-check.sh
> SHELL ["/bin/sh"]
> RUN /tools/csi-nfs-deps-check.sh
>
> FROM ${DISTROLESS_IMAGE} AS cinder-nfs-csi-plugin
> COPY --from=cinder-nfs-csi-plugin-utils-check /tools/csi-nfs-deps-check.sh /bin/csi-nfs-deps-check.sh
> COPY --from=cinder-nfs-csi-plugin-utils /dest /
> COPY --from=builder /build/cinder-nfs-csi-plugin /bin/cinder-nfs-csi-plugin
> COPY --from=certs /etc/ssl/certs /etc/ssl/certs
> ```

> **Design rationale:** Bundling NFS tools in the container image (rather than relying on
> host-installed `nfs-common`/`nfs-utils`) ensures consistent behavior across WRCP/WRC
> worker hosts regardless of host OS version or package availability. This follows the
> same approach as `cinder-csi-plugin`, which bundles `mount`, `blkid`, `mkfs.*`, etc.
> The NFS dependency set is much smaller (no filesystem format tools needed), so the
> resulting image remains lightweight.
>
> The node DaemonSet still requires `privileged: true` and `mountPropagation: Bidirectional`
> to perform NFS mounts that are visible to kubelet and pods on the host.

### 8.3 Kubernetes Manifests

The manifest structure mirrors `manifests/cinder-csi-plugin/` with NFS-specific changes:

**Controller Deployment** (`cinder-nfs-csi-controllerplugin.yaml`):
- Image: `registry.k8s.io/provider-os/cinder-nfs-csi-plugin:latest`
- Sidecars: `external-provisioner`, `livenessprobe`
- NO `external-attacher` sidecar needed (ControllerPublish only queries connection_info,
  does not perform Nova attachment)
- ConfigMap volume: `driver-config` → `/etc/config/driver.conf`
- Secret volume: `cloud-config` → `/etc/config/cloud.conf`

**Node DaemonSet** (`cinder-nfs-csi-nodeplugin.yaml`):
- Image: `registry.k8s.io/provider-os/cinder-nfs-csi-plugin:latest`
- Sidecars: `node-driver-registrar`, `livenessprobe`
- Socket: `/var/lib/kubelet/plugins/cinder-nfs.csi.windriver.com/csi.sock`
- Host mount: `/var/lib/kubelet/pods` (for NFS mount propagation)
- `privileged: true` with `mountPropagation: Bidirectional` — required for NFS mounts
  to be visible to kubelet and pods (NFS tools are bundled in the container image,
  matching the `cinder-csi-plugin` convention)

**CSIDriver resource** (`csi-cinder-nfs-driver.yaml`):
```yaml
apiVersion: storage.k8s.io/v1
kind: CSIDriver
metadata:
  name: cinder-nfs.csi.windriver.com
spec:
  attachRequired: true          # ControllerPublishVolume needed for connection_info
  podInfoOnMount: false
  volumeLifecycleModes:
    - Persistent
```

### 8.4 Helm Chart

A new Helm chart `charts/cinder-nfs-csi-plugin/` follows the existing chart pattern with
NFS-specific `values.yaml`:

```yaml
# charts/cinder-nfs-csi-plugin/values.yaml (excerpt)
csi:
  plugin:
    image:
      repository: registry.k8s.io/provider-os/cinder-nfs-csi-plugin
      tag: latest
    shadowVM:
      flavorID: ""        # Required
      imageID: ""         # Required
      subnetID: ""        # Required
      networkID: ""       # Required
      stopAfterAttach: true
      deleteOnVolumeDelete: true
    nfs:
      mountOptions: "nfsvers=4.1,rsize=1048576,wsize=1048576"
      defaultFsType: "nfs4"
```

### 8.5 Metrics

The NFS-Cinder CSI driver should integrate with the existing `pkg/metrics/` framework
to expose Prometheus-compatible metrics. The project's metrics convention (see
[docs/metrics.md](../../metrics.md)) uses three metric types per operation:

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `cinder_nfs_csi_operation_duration_seconds` | Histogram | `operation` | Latency of CSI RPC operations |
| `cinder_nfs_csi_operations_total` | Counter | `operation` | Total number of CSI RPC calls |
| `cinder_nfs_csi_operation_errors_total` | Counter | `operation` | Total number of failed CSI RPC calls |
| `cinder_nfs_csi_openstack_api_request_duration_seconds` | Histogram | `request` | Latency of OpenStack API calls (Cinder, Nova) |
| `cinder_nfs_csi_openstack_api_requests_total` | Counter | `request` | Total OpenStack API calls |
| `cinder_nfs_csi_openstack_api_request_errors_total` | Counter | `request` | Total failed OpenStack API calls |

Possible `operation` values: `create_volume`, `delete_volume`, `controller_publish_volume`,
`controller_unpublish_volume`, `node_stage_volume`, `node_unstage_volume`,
`node_publish_volume`, `node_unpublish_volume`, `shadow_vm_create`, `shadow_vm_stop`,
`shadow_vm_delete`.

Possible `request` values: `volume_create`, `volume_get`, `volume_delete`,
`volume_extend`, `volume_attachment_list`, `volume_attachment_get`,
`server_create`, `server_get`, `server_stop`, `server_delete`.

The controller binary should expose metrics on a configurable HTTPS port (matching the
OCCM pattern of `--secure-port` with `--authorization-always-allow-paths="/metrics"`).

### 8.6 Release Procedure Alignment

The NFS-Cinder CSI driver must integrate with the existing release process documented
in [docs/release-procedure.md](../../release-procedure.md):

1. **Version bumping:** `hack/bump-release.sh` performs string replacement across
   `docs/manifests/tests/examples` directories. New manifests and charts under
   `manifests/cinder-nfs-csi-plugin/` and `charts/cinder-nfs-csi-plugin/` must use
   consistent version strings (e.g., `v1.XX.Y`) that match the patterns the script
   expects.

2. **Helm chart bumping:** `hack/bump-charts.sh` updates chart versions. The new chart
   `charts/cinder-nfs-csi-plugin/` must follow the version convention
   (`appVersion: 1.XX.Y`, `version: 2.XX.Y`).

3. **Image promotion:** After tagging a release, staging images are built at
   `gcr.io/k8s-staging-provider-os/cinder-nfs-csi-plugin`. The image digest must be
   added to
   [`images.yaml`](https://github.com/kubernetes/k8s.io/blob/main/registry.k8s.io/images/k8s-staging-provider-os/images.yaml)
   using `hack/release-image-digests.sh` and verified with `hack/verify-image-digests.sh`.

4. **CI jobs:** A new CI job for the NFS-Cinder CSI driver must be added to
   [`test-infra`](https://github.com/kubernetes/test-infra/tree/master/config/jobs/kubernetes/cloud-provider-openstack)
   for the release branch.

5. **Sidecar container versions:** Sidecar images (`external-provisioner`,
   `node-driver-registrar`, `livenessprobe`) in both manifests and Helm charts should
   be bumped in sync with Kubernetes releases, paying attention to major version bumps
   that may require manifest changes.

---

## 9. Development Phases

### Phase 1 — Scaffold and Interface

**Goal:** Establish the new package structure, interfaces, and a buildable binary.

| Task | Description                                      | Files                                      |
|------|--------------------------------------------------|--------------------------------------------|
| 1.1  | Create `pkg/csi/cinder-nfs/` package skeleton   | `driver.go`, `identityserver.go`           |
| 1.2  | Copy gRPC server infrastructure                  | `server.go`, `utils.go`                    |
| 1.3  | Define `IOpenStackNFS` interface                 | `openstack/openstack.go`                   |
| 1.4  | Define NFS config structs                        | `openstack/openstack.go`                   |
| 1.5  | Create binary entry point                        | `cmd/cinder-nfs-csi-plugin/main.go`        |
| 1.6  | Add Makefile/Dockerfile entries                  | `Makefile`, `Dockerfile`                   |
| 1.7  | Create mock for `IOpenStackNFS`                  | `openstack/openstack_mock.go`              |
| 1.8  | Verify build: `make cinder-nfs-csi-plugin`       |                                            |

**Deliverable:** Binary that starts, registers identity server, listens on unix socket.

### Phase 2 — Controller Service (Core)

**Goal:** Implement CreateVolume, DeleteVolume, ControllerPublishVolume with Shadow VM.

| Task | Description                                      | Files                                          |
|------|--------------------------------------------------|------------------------------------------------|
| 2.1  | Implement Shadow VM lifecycle manager            | `shadowvm.go`                                  |
| 2.2  | Implement Nova server operations                 | `openstack/openstack_servers.go`               |
| 2.3  | Implement Cinder attachment + connection_info    | `openstack/openstack_attachments.go`           |
| 2.4  | Implement `CreateVolume` with Shadow VM flow     | `controllerserver.go`                          |
| 2.5  | Implement `DeleteVolume` with Shadow VM cleanup  | `controllerserver.go`                          |
| 2.6  | Implement `ControllerPublishVolume` (conn_info)  | `controllerserver.go`                          |
| 2.7  | Implement `ControllerUnpublishVolume` (no-op)    | `controllerserver.go`                          |
| 2.8  | Unit tests with mock                             | `controllerserver_test.go`                     |

**Deliverable:** Controller plugin creates NFS-backed Cinder volumes with Shadow VM.

### Phase 3 — Node Service (NFS Mount)

**Goal:** Implement NodeStageVolume (NFS mount) and NodePublishVolume (bind mount).

| Task | Description                                      | Files                                          |
|------|--------------------------------------------------|------------------------------------------------|
| 3.1  | Implement `NodeStageVolume` (NFS mount)          | `nodeserver.go`                                |
| 3.2  | Implement `NodeUnstageVolume` (NFS umount)       | `nodeserver.go`                                |
| 3.3  | Implement `NodePublishVolume` (bind mount)       | `nodeserver.go`                                |
| 3.4  | Implement `NodeUnpublishVolume` (umount)         | `nodeserver.go`                                |
| 3.5  | Implement `NodeGetInfo` (node ID)                | `nodeserver.go`                                |
| 3.6  | Implement `NodeGetVolumeStats` (statfs)          | `nodeserver.go`                                |
| 3.7  | Unit tests                                        | `nodeserver_test.go`                           |

**Deliverable:** Full CSI driver — volumes can be provisioned and mounted as NFS on pods.

### Phase 4 — Integration and E2E

**Goal:** Deploy to real OpenStack, run CSI sanity and E2E tests.

| Task | Description                                      | Files                                         |
|------|--------------------------------------------------|-----------------------------------------------|
| 4.1  | Create Kubernetes manifests                      | `manifests/cinder-nfs-csi-plugin/`            |
| 4.2  | Create Helm chart                                | `charts/cinder-nfs-csi-plugin/`               |
| 4.3  | CSI sanity tests (`make test-cinder-nfs-csi-sanity`) | `tests/sanity/cinder-nfs/`                |
| 4.4  | E2E CI script + Ansible playbook                 | `tests/ci-csi-cinder-nfs-e2e.sh`, `tests/playbooks/test-csi-cinder-nfs-e2e.yaml` |
| 4.5  | E2E test: provision → mount → read/write → delete| (within playbook)                             |
| 4.6  | E2E test: multi-node mount (RWX)                 |                                               |
| 4.7  | E2E test: Shadow VM failure recovery             |                                               |
| 4.8  | Documentation                                    | `docs/cinder-csi-plugin/migration/`           |

**E2E Testing Convention:** The project's E2E tests use **Ansible playbooks** orchestrated
by bash scripts (e.g., `tests/ci-csi-cinder-e2e.sh` → `tests/playbooks/test-csi-cinder-e2e.yaml`).
The CI provisions a VM (Ubuntu 24.04, 4 vCPUs, 16 GB RAM recommended), installs DevStack
+ k3s, deploys the CSI driver, and runs the tests. The NFS-Cinder E2E tests should follow
this same pattern with `tests/ci-csi-cinder-nfs-e2e.sh` and a corresponding playbook.

**Deliverable:** Production-ready NFS-Cinder CSI driver with tests and docs.

### Phase 5 — CDI Multi-Phase Precopy

**Goal:** Integrate with CDI importer for V2O/O2O migration workflows.

| Task | Description                                      | Notes                                         |
|------|--------------------------------------------------|-----------------------------------------------|
| 5.1  | CDI DataVolume with NFS-Cinder StorageClass      | CDI uses CSI to provision PVC                 |
| 5.2  | Multi-phase precopy: initial + incremental        | CDI importer handles data copy phases         |
| 5.3  | Final sync and cutover                           | Stop source VM, final incremental, start dest |
| 5.4  | E2E: Full V2O migration workflow                 |                                               |
| 5.5  | E2E: Full O2O migration workflow                 |                                               |

**Deliverable:** Complete migration pipeline from VMware/OpenStack to KubeVirt.

---

## Appendix A: Key Differences from Existing Cinder CSI at a Glance

```
Existing Cinder CSI                     NFS-Cinder CSI
─────────────────                       ──────────────
cinder.csi.openstack.org                cinder-nfs.csi.windriver.com
Block device (iSCSI/FC)                 NFS mount
Nova AttachVolume → /dev/vdb            Shadow VM → connection_info → NFS export
FormatAndMount(ext4/xfs)                mount -t nfs
SINGLE_NODE_WRITER                      MULTI_NODE_MULTI_WRITER
Node needs SYS_ADMIN/privileged         Node needs mount propagation only
external-attacher sidecar               No external-attacher needed
NodeExpandVolume (block resize)         NodeExpandVolume no-op (NFS auto-resize)
K8s node = volume consumer              K8s node = NFS client, Shadow VM = attach host
```

## Appendix B: File-Level Reuse Decision Matrix

| Existing File                          | Reuse? | Strategy                                              |
|----------------------------------------|:------:|-------------------------------------------------------|
| `pkg/csi/csi.go`                       | ✅     | Import directly — shared constants and helpers         |
| `pkg/csi/cinder/server.go`            | 📋     | Copy to new package (unexported types)                 |
| `pkg/csi/cinder/utils.go`             | 📋     | Copy capability factories; rewrite server constructors |
| `pkg/csi/cinder/driver.go`            | 📝     | Rewrite — different name, capabilities, services       |
| `pkg/csi/cinder/controllerserver.go`  | 🆕     | New implementation — completely different logic         |
| `pkg/csi/cinder/nodeserver.go`        | 🆕     | New implementation — NFS mount instead of block device  |
| `pkg/csi/cinder/identityserver.go`    | 📝     | Rewrite — different plugin capabilities                |
| `pkg/csi/cinder/openstack/openstack.go` | 📝  | New interface, new config structs, reuse config parsing |
| `pkg/csi/cinder/openstack/openstack_volumes.go` | 📋 | Partial copy — CreateVolume, GetVolume reusable  |
| `pkg/csi/cinder/openstack/openstack_instances.go` | 📝 | Extend — add Create, Stop, Delete server       |
| `pkg/client/`                          | ✅     | Import directly — OpenStack auth                       |
| `pkg/metrics/`                         | ✅     | Import directly                                        |
| `pkg/util/metadata/`                   | ✅     | Import directly                                        |
| `pkg/util/mount/`                      | ✅     | Import directly — NFS mount via `Mounter()`            |
| `pkg/util/errors/`                     | ✅     | Import directly                                        |
| `tools/csi-deps.sh`                   | 📝     | Adapt as `tools/csi-nfs-deps.sh` — NFS utils only (production phase, see §8.2.2) |
| `tools/csi-deps-check.sh`             | 📝     | Adapt as `tools/csi-nfs-deps-check.sh` — validate NFS binaries (production phase, see §8.2.2) |

Legend: ✅ Import | 📋 Copy | 📝 Rewrite/Adapt | 🆕 New Implementation
