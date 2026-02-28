# iSCSI-Backed Cinder CSI Plugin — Detailed Implementation Design

| Field          | Value                                                    |
|----------------|----------------------------------------------------------|
| **Authors**    | iSCSI-Cinder CSI Design Team                             |
| **Status**     | Draft                                                    |
| **Created**    | 2026-02-27                                               |
| **Depends On** | [iSCSI-Backed Cinder Volume for WRCP Migration — Design Proposal](iscsi-backed-cinder-volume-for-wrcp-migration.md) |
| **Related**    | [NFS-Cinder CSI Implementation Design](nfs-cinder-csi-implementation-design.md) |
| **Repository** | `kubernetes/cloud-provider-openstack`                    |

---

## Table of Contents

- [iSCSI-Backed Cinder CSI Plugin — Detailed Implementation Design](#iscsi-backed-cinder-csi-plugin--detailed-implementation-design)
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
		- [3.2 Why the NFS-Cinder Driver Cannot Be Extended Either](#32-why-the-nfs-cinder-driver-cannot-be-extended-either)
		- [3.3 Reusable Components](#33-reusable-components)
	- [4. Comparison with Manila and NFS-Cinder CSI Drivers](#4-comparison-with-manila-and-nfs-cinder-csi-drivers)
	- [5. Proposed Folder Structure](#5-proposed-folder-structure)
		- [5.1 New Directories and Files](#51-new-directories-and-files)
		- [5.2 Files to Modify in Existing Tree](#52-files-to-modify-in-existing-tree)
	- [6. Interface Design](#6-interface-design)
		- [6.1 IOpenStackISCSI Interface](#61-iopenstackiscsi-interface)
		- [6.2 iSCSI-Specific Config Structs](#62-iscsi-specific-config-structs)
		- [6.3 Cinder v3 Attachment Lifecycle State Machine](#63-cinder-v3-attachment-lifecycle-state-machine)
		- [6.4 Architecture Decision: Gophercloud SDK for Cinder v3 Attachments](#64-architecture-decision-gophercloud-sdk-for-cinder-v3-attachments)
	- [7. Prerequisites and Runtime Validation](#7-prerequisites-and-runtime-validation)
		- [7.1 Environment Prerequisites](#71-environment-prerequisites)
		- [7.2 Startup Validation vs. Ongoing Validation](#72-startup-validation-vs-ongoing-validation)
		- [7.3 Microversion Detection Strategy](#73-microversion-detection-strategy)
	- [8. CSI RPC Implementation Map](#8-csi-rpc-implementation-map)
		- [8.1 Identity Service](#81-identity-service)
		- [8.2 Controller Service](#82-controller-service)
			- [CreateVolume](#createvolume)
			- [DeleteVolume](#deletevolume)
			- [ControllerPublishVolume](#controllerpublishvolume)
			- [ControllerUnpublishVolume](#controllerunpublishvolume)
			- [ControllerExpandVolume](#controllerexpandvolume)
		- [8.3 Node Service](#83-node-service)
			- [NodeStageVolume](#nodestagevolume)
			- [NodeUnstageVolume](#nodeunstagevolume)
			- [NodePublishVolume](#nodepublishvolume)
			- [NodeUnpublishVolume](#nodeunpublishvolume)
			- [NodeGetInfo](#nodegetinfo)
			- [NodeGetVolumeStats](#nodegetvolumestats)
			- [NodeExpandVolume](#nodeexpandvolume)
	- [9. K8S CSI Sidecar Architecture](#9-k8s-csi-sidecar-architecture)
		- [9.1 Controller Deployment Sidecars](#91-controller-deployment-sidecars)
		- [9.2 Node DaemonSet Sidecars](#92-node-daemonset-sidecars)
		- [9.3 K8S Calling Chain Reference](#93-k8s-calling-chain-reference)
	- [10. Build and Deployment](#10-build-and-deployment)
		- [10.1 Makefile Changes](#101-makefile-changes)
		- [10.2 Dockerfile Stage](#102-dockerfile-stage)
			- [10.2.1 Development Phase — Debian-Based Image (Current)](#1021-development-phase--debian-based-image-current)
			- [10.2.2 Production Phase — 3-Step Distroless Build (TODO)](#1022-production-phase--3-step-distroless-build-todo)
		- [10.3 Kubernetes Manifests](#103-kubernetes-manifests)
			- [`hostPID: true` — Security Implications and Alternatives](#hostpid-true--security-implications-and-alternatives)
		- [10.4 StorageClass, PVC, and CDI Pod Specifications](#104-storageclass-pvc-and-cdi-pod-specifications)
		- [10.5 Helm Chart](#105-helm-chart)
		- [10.6 Metrics](#106-metrics)
		- [10.7 Release Procedure Alignment](#107-release-procedure-alignment)
	- [11. Development Phases](#11-development-phases)
		- [Phase 1 — Scaffold and Interface](#phase-1--scaffold-and-interface)
		- [Phase 2 — Controller Service (Core)](#phase-2--controller-service-core)
		- [Phase 3 — Node Service (iSCSI Login/Logout)](#phase-3--node-service-iscsi-loginlogout)
		- [Phase 4 — Integration and E2E](#phase-4--integration-and-e2e)
		- [Phase 5 — CDI Multi-Phase Precopy](#phase-5--cdi-multi-phase-precopy)
	- [Appendix A: Key Differences — Existing Cinder CSI vs NFS-Cinder CSI vs iSCSI-Cinder CSI](#appendix-a-key-differences--existing-cinder-csi-vs-nfs-cinder-csi-vs-iscsi-cinder-csi)
	- [Appendix B: File-Level Reuse Decision Matrix](#appendix-b-file-level-reuse-decision-matrix)

---

## 1. Summary

This document provides a detailed implementation design for the iSCSI-backed Cinder CSI
driver (`cinder-iscsi.csi.windriver.com`), based on the analysis of the existing Cinder CSI
driver codebase and the companion NFS-Cinder CSI driver in `cloud-provider-openstack`.

The iSCSI driver replaces [NetApp NFS](nfs-backed-cinder-volume-for-wrcp-migration.md)
with [Pure Storage FlashArray iSCSI](iscsi-backed-cinder-volume-for-wrcp-migration.md) for
WRCP/WRC VM migration workloads. It uses **Cinder v3 self-service attachments** (microversion
3.27+) instead of Shadow VMs, eliminating all Nova compute dependency.

### Conclusion

**Neither the existing Cinder CSI driver NOR the NFS-Cinder CSI driver can be extended
for the iSCSI-backed driver. A new, separate package is required.**

The rationale is summarized below:

| Dimension              | Existing Cinder CSI                        | NFS-Cinder CSI                             | iSCSI-Cinder Requirement                                | Compatible with Existing? | Compatible with NFS? |
|------------------------|--------------------------------------------|--------------------------------------------|----------------------------------------------------------|:-------------------------:|:--------------------:|
| Driver name            | `cinder.csi.openstack.org` (hardcoded)     | `cinder-nfs.csi.windriver.com`             | `cinder-iscsi.csi.windriver.com`                         | **No**                    | **No**               |
| Volume lock            | Nova attachment                            | Shadow VM (Nova instance)                  | Cinder v3 reserved attachment (no Nova)                  | **No**                    | **No**               |
| Controller Publish     | Nova `AttachVolume` (block device)          | Query Shadow VM `connection_info`          | Update Cinder attachment connector → iSCSI target info   | **No**                    | **No**               |
| Controller Unpublish   | Nova `DetachVolume`                         | No-op (Shadow VM persists)                 | Delete attachment + recreate reserved (for next CDI stage)| **No**                    | **No**               |
| Create Volume          | Cinder POST only                           | Cinder POST + Shadow VM lifecycle          | Cinder POST + create reserved attachment (no Nova)       | **No**                    | **No**               |
| Delete Volume          | Cinder DELETE only                         | Shadow VM delete + Cinder DELETE           | Delete attachment + release volume (no Nova)             | **No**                    | **No**               |
| Node Stage             | `getDevicePath` + `FormatAndMount`         | `mount -t nfs`                             | `iscsiadm` discovery + CHAP auth + login → `/dev/sdX`   | **No**                    | **No**               |
| Node Unstage           | `Unmount` block device                     | `umount` NFS                               | `iscsiadm` logout + node DB cleanup                      | Partial                   | **No**               |
| Node ID format         | Nova instance ID                           | Hostname                                   | `hostname;iqn;ip` (iSCSI initiator identity)             | **No**                    | **No**               |
| Volume access mode     | `SINGLE_NODE_WRITER`                       | `MULTI_NODE_MULTI_WRITER`                  | `SINGLE_NODE_WRITER` (iSCSI exclusive)                   | Partial                   | **No**               |
| OpenStack API          | Nova + Cinder                              | Nova + Cinder (Shadow VM)                  | Cinder only (v3 attachment API, no Nova)                 | **No**                    | **No**               |
| IOpenStack interface   | Block-attach methods (25 methods)          | Shadow VM lifecycle + NFS conn_info        | Cinder v3 attachment CRUD + iSCSI connection parsing     | **No**                    | **No**               |
| Host dependencies      | None (block device via kernel)             | `nfs-common` / `nfs-utils`                 | `open-iscsi` (`iscsiadm`, `iscsid`)                      | **No**                    | **No**               |
| external-attacher      | Yes (Nova attach)                          | No (Shadow VM manages)                     | Yes (Cinder attachment connector update)                 | Partial                   | **No**               |

The project follows the precedent set by the **Manila CSI driver** (`pkg/csi/manila/`) and
the **NFS-Cinder CSI driver** (`pkg/csi/cinder-nfs/`), both of which are fully separate
packages within the same monorepo.

---

## 2. Existing Cinder CSI Code Analysis

> **Note:** This analysis is largely shared with the [NFS-Cinder implementation design](nfs-cinder-csi-implementation-design.md#2-existing-cinder-csi-code-analysis). The following subsections highlight aspects specifically relevant to the iSCSI driver.

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
│   ├── utils.go                   # Capability factories, gRPC logger
│   └── openstack/                 # OpenStack API wrapper
│       ├── openstack.go           # IOpenStack interface, Config
│       ├── openstack_volumes.go   # Volume CRUD, attach/detach
│       ├── openstack_instances.go # GetInstanceByID
│       ├── openstack_snapshots.go # Snapshot operations
│       ├── openstack_backups.go   # Backup operations
│       ├── openstack_mock.go      # Mock for unit tests
│       └── fixtures/              # Test fixtures
├── cinder-nfs/                    # ← NFS-Cinder CSI driver (companion)
│   ├── driver.go                  # NFS driver struct
│   ├── controllerserver.go        # Shadow VM lifecycle
│   ├── nodeserver.go              # NFS mount/umount
│   ├── shadowvm.go                # Shadow VM state machine
│   ├── connectioninfo.go          # NFS connection_info parser
│   └── openstack/                 # IOpenStackNFS interface
├── manila/                        # ← Manila CSI driver (precedent)
│   ├── driver.go
│   ├── controllerserver.go
│   ├── nodeserver.go
│   └── ...
```

### 2.2 driver.go — Driver Struct and Initialization

**File:** `pkg/csi/cinder/driver.go` (209 lines)

Key observations relevant to iSCSI driver:

1. **Driver name is a hardcoded constant:**
   ```go
   const driverName = "cinder.csi.openstack.org"
   ```
   The iSCSI driver requires `cinder-iscsi.csi.windriver.com`. The name is used in
   `CSIDriver` resource, socket path, and volume handle resolution.

2. **Capabilities are block-device specific:**
   ```go
   d.AddVolumeCapabilityAccessModes(
       []csi.VolumeCapability_AccessMode_Mode{
           csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
       })
   ```
   The iSCSI driver also uses `SINGLE_NODE_WRITER` (iSCSI targets are per-initiator,
   exclusive access). However, additional capabilities like `VOLUME_ACCESSIBILITY_CONSTRAINTS`
   are needed for topology-aware scheduling (zone + iSCSI network reachability).

3. **`SetupNodeService` accepts `BlockStorageOpts`:**
   ```go
   func (d *Driver) SetupNodeService(mount mount.IMount, metadata metadata.IMetadata,
       opts openstack.BlockStorageOpts, topologies map[string]string) {
   ```
   The iSCSI driver needs `ISCSIOpts` (login timeout, CHAP, multipath, device wait)
   rather than `BlockStorageOpts`.

4. **`SetupControllerService` accepts `map[string]openstack.IOpenStack`:**
   ```go
   func (d *Driver) SetupControllerService(clouds map[string]openstack.IOpenStack) {
   ```
   The iSCSI driver needs `IOpenStackISCSI` with Cinder v3 attachment methods
   (create, update connector, complete, delete) — a completely different interface
   with no Shadow VM or Nova methods.

### 2.3 controllerserver.go — Controller RPCs

**File:** `pkg/csi/cinder/controllerserver.go` (1118 lines)

Every Controller RPC is deeply coupled to the block-attach model via Nova:

| RPC                        | Existing Implementation                              | iSCSI-Cinder Requirement                                         |
|----------------------------|------------------------------------------------------|------------------------------------------------------------------|
| `CreateVolume`             | `cloud.CreateVolume(opts)` — Cinder POST only        | Cinder POST + create reserved Cinder v3 attachment (no Nova)     |
| `DeleteVolume`             | `cloud.DeleteVolume(volumeID)` — Cinder DELETE only  | Delete Cinder attachment + release volume (no Nova)              |
| `ControllerPublishVolume`  | `cloud.AttachVolume(instanceID, volumeID)` — Nova    | Update attachment connector (IQN/IP) → extract iSCSI target info |
| `ControllerUnpublishVolume`| `cloud.DetachVolume(instanceID, volumeID)` — Nova    | Delete attachment + recreate reserved (attachment rotation)      |

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
iSCSI driver instead needs:
```go
// Pseudocode for iSCSI ControllerPublishVolume
func (cs *iscsiControllerServer) ControllerPublishVolume(...) {
    // Parse node identity: "worker-3;iqn.xxx;10.0.0.103"
    host, iqn, ip := parseNodeID(req.NodeId)
    attachmentID := getAttachmentID(req.VolumeContext, req.VolumeId)
    // Update Cinder v3 attachment with iSCSI initiator connector
    iscsiOpts := cloud.GetISCSIOpts()
    connInfo, _ := cloud.UpdateAttachmentConnector(ctx, attachmentID, &AttachmentConnector{
        Initiator: iqn, IP: ip, Host: host,
        Multipath: iscsiOpts.EnableMultipath, // from driver.conf [ISCSI]
        Platform:  defaultIfEmpty(iscsiOpts.Platform, "x86_64"),
        OSType:    defaultIfEmpty(iscsiOpts.OSType, "linux2"),
    })
    // Return iSCSI target details in publish_context
    return &csi.ControllerPublishVolumeResponse{
        PublishContext: map[string]string{
            "target_portal": connInfo.TargetPortal,  // "10.0.0.1:3260"
            "target_iqn":    connInfo.TargetIQN,     // "iqn.xxx"
            "target_lun":    connInfo.TargetLUN,     // "0"
            "auth_method":   connInfo.AuthMethod,    // "CHAP"
            "auth_username": connInfo.AuthUsername,
            "auth_password": connInfo.AuthPassword,
        },
    }
}
```

These are fundamentally incompatible code paths — no amount of branching within the
existing `controllerServer` would be clean or maintainable.

### 2.4 nodeserver.go — Node RPCs

**File:** `pkg/csi/cinder/nodeserver.go` (443 lines)

The node server is entirely block-device oriented — incompatible with iSCSI initiator
operations:

| RPC                    | Existing Implementation                                   | iSCSI-Cinder Requirement                              |
|------------------------|-----------------------------------------------------------|--------------------------------------------------------|
| `NodeStageVolume`      | `getDevicePath(volumeID)` → `FormatAndMount(dev, target)` | `iscsiadm` discovery + CHAP auth + login → wait for `/dev/disk/by-path/...` |
| `NodeUnstageVolume`    | `Unmount(stagingTargetPath)`                              | `iscsiadm` logout + node DB cleanup + remove staging artifacts |
| `NodePublishVolume`    | `Mount(source, targetPath, fsType, ["bind"])`             | Bind mount block device (`/dev/sdX`) to target path    |
| `NodeUnpublishVolume`  | `UnmountPath(targetPath)`                                 | `umount` bind mount (same)                             |
| `NodeGetInfo`          | `metadata.GetInstanceID()` — Nova instance ID             | `hostname;iqn;ip` (iSCSI initiator identity)           |
| `NodeGetVolumeStats`   | `GetDeviceStats(volumePath)` — block device stats         | Block device stats via `statfs` or device size         |
| `NodeExpandVolume`     | `blockdevice.RescanBlockDeviceGeometry` + `Resize`        | `iscsiadm` rescan session (optional, future)           |

**Critical code path in `NodeStageVolume`:**
```go
func (ns *nodeServer) NodeStageVolume(...) {
    devicePath, err := getDevicePath(volumeID, m)    // scans /dev/ for SCSI disk serial
    err = ns.formatAndMountRetry(devicePath, stagingTarget, fsType, options)
}
```
The existing driver assumes a locally-attached block device discovered by SCSI serial scan.
The iSCSI driver must perform a full `iscsiadm` discovery → CHAP authentication → login
sequence, wait for the kernel to create `/dev/disk/by-path/ip-...-iscsi-...-lun-N`, and
resolve it to the real device path.

**`NodeGetInfo` returns Nova instance ID:**
```go
func (ns *nodeServer) NodeGetInfo(...) {
    nodeID, err := ns.Metadata.GetInstanceID()  // Nova instance ID
}
```
The iSCSI driver must return its own composite node ID format: `hostname;iqn;ip`. This
identity is parsed by `ControllerPublishVolume` to build the Cinder attachment connector.

### 2.5 identityserver.go — Identity RPCs

**File:** `pkg/csi/cinder/identityserver.go` (98 lines)

The identity server pattern is reusable but capabilities differ:

- Existing: `CONTROLLER_SERVICE`, `VOLUME_EXPANSION` (online + offline)
- iSCSI: `CONTROLLER_SERVICE`, `VOLUME_ACCESSIBILITY_CONSTRAINTS` (topology-aware, zone +
  iSCSI network reachability)

**Verdict:** New identity server with iSCSI-specific capabilities needed.

### 2.6 openstack/ — OpenStack API Layer

**File:** `pkg/csi/cinder/openstack/openstack.go` (244 lines)

The `IOpenStack` interface has 25 methods, all targeting Nova block-device attachment:

```go
type IOpenStack interface {
    CreateVolume(ctx, opts, schedulerHints) (*volumes.Volume, error)
    AttachVolume(ctx, instanceID, volumeID) (string, error)       // Nova attach
    DetachVolume(ctx, instanceID, volumeID) error                 // Nova detach
    WaitDiskAttached(ctx, instanceID, volumeID) error             // Poll Nova
    GetAttachmentDiskPath(ctx, instanceID, volumeID) (string, error) // /dev/vdb
    // ...
}
```

**Missing for iSCSI driver — Cinder v3 Attachment API:**

| Method Needed                                          | Purpose                                                       |
|--------------------------------------------------------|---------------------------------------------------------------|
| `CreateAttachment(ctx, volumeID)`                      | Create reserved attachment (no connector, no Nova instance)   |
| `UpdateAttachmentConnector(ctx, attachmentID, connector)` | Set iSCSI initiator connector → Cinder returns iSCSI target info |
| `CompleteAttachment(ctx, attachmentID)`                 | Mark attachment complete (microversion 3.44+, optional)       |
| `GetAttachment(ctx, attachmentID)`                     | Query attachment state                                        |
| `DeleteAttachment(ctx, attachmentID)`                  | Delete attachment (triggers `terminate_connection()` on backend) |
| `SetVolumeMetadata(ctx, volumeID, metadata)`           | Store `csi.attachment_id` in Cinder volume metadata           |
| `DeleteVolumeMetadata(ctx, volumeID, keys)`            | Remove CSI metadata from volume                               |

**Key difference from NFS driver's `IOpenStackNFS`:** The iSCSI interface has **no Nova
methods at all**. No `CreateServer`, `StopServer`, `DeleteServer`, `WaitServerStatus`. The
volume lock is achieved purely through Cinder's attachment status (`reserved`).

**The `OpenStack` struct only holds two clients:**
```go
type OpenStack struct {
    compute      *gophercloud.ServiceClient
    blockstorage *gophercloud.ServiceClient
}
```
The iSCSI driver only needs `blockstorage` — the `compute` client is unnecessary since
there is no Nova dependency.

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

**Verdict:** Fully reusable. Copy `server.go` + `utils.go` into the new package (same
approach as NFS-Cinder driver). These are unexported types in `package cinder`, so direct
import is not possible.

### 2.8 cmd/cinder-csi-plugin/main.go — Binary Entry Point

**File:** `cmd/cinder-csi-plugin/main.go` (153 lines)

The binary flow assumes Nova-based attachment. The iSCSI driver needs:
- Different driver name (`cinder-iscsi.csi.windriver.com`) and version
- iSCSI-specific config loading (`driver.conf` with `[ISCSI]` and `[Volume]` sections)
- **No Nova credentials validation** — only Cinder API access required
- Different service setup (`IOpenStackISCSI` controller, iSCSI node)
- Validation that `iscsiadm` and `iscsid` are available on node

---

## 3. Architectural Assessment: Extension vs. New Package

### 3.1 Why the Existing Code Cannot Be Extended

| Approach Considered          | Why It Fails                                                            |
|------------------------------|-------------------------------------------------------------------------|
| **Add iSCSI mode flag to existing driver** | Controller and node RPCs have incompatible code paths. `ControllerPublishVolume` calls Nova vs Cinder attachment API. `NodeStageVolume` scans SCSI serials vs runs `iscsiadm`. Driver name must differ. |
| **Subclass `controllerServer`** | Go does not support class inheritance. Struct embedding would inherit all block methods, requiring method override of every single RPC. |
| **Strategy pattern inside RPCs** | Would require injecting strategy objects into every RPC, changing the existing driver's internal structure — high risk of regression. |

### 3.2 Why the NFS-Cinder Driver Cannot Be Extended Either

The iSCSI driver shares the same migration use case as the NFS driver, but differs in
every implementation layer:

| Aspect                       | NFS-Cinder                                  | iSCSI-Cinder                                |
|------------------------------|---------------------------------------------|----------------------------------------------|
| **Volume lock**              | Shadow VM (Nova instance)                   | Cinder v3 reserved attachment (no Nova)      |
| **OpenStack dependency**     | Nova + Cinder                               | Cinder only                                  |
| **Compute quota**            | 1 VM per migration                          | Zero                                         |
| **ControllerPublishVolume**  | Query existing `connection_info`            | Update attachment connector → new iSCSI info |
| **ControllerUnpublishVolume**| No-op (Shadow VM persists)                  | Delete + recreate attachment (rotation)      |
| **Node Stage**               | `mount -t nfs`                              | `iscsiadm` login chain                       |
| **Node Unstage**             | `umount` NFS                                | `iscsiadm` logout + cleanup                  |
| **Access mode**              | `MULTI_NODE_MULTI_WRITER` (shared NFS)      | `SINGLE_NODE_WRITER` (exclusive iSCSI)       |
| **Node ID**                  | Hostname                                    | `hostname;iqn;ip`                            |
| **External-attacher sidecar**| Not needed                                  | Required (triggers `ControllerPublish/Unpublish`) |

These differences are structural — they touch every layer from the OpenStack interface
to the K8S sidecar deployment model. No amount of `if/else` branching within the NFS
driver would be clean.

### 3.3 Reusable Components

Despite the need for a separate package, significant code and patterns are reusable:

| Component                          | Location                          | Reuse Strategy              |
|------------------------------------|-----------------------------------|-----------------------------|
| gRPC server infrastructure         | `server.go`, `utils.go`          | Copy from cinder/           |
| CSI shared constants/helpers       | `pkg/csi/csi.go`                 | Import directly             |
| OpenStack auth client              | `pkg/client/`                    | Import directly             |
| Gophercloud Cinder v3 attachments  | `gophercloud/v2` (go.mod dep)    | Import `blockstorage/v3/attachments` directly |
| Config file parsing (`gcfg`)       | `openstack/openstack.go` pattern | Follow pattern, new structs |
| Metrics framework                  | `pkg/metrics/`                   | Import directly             |
| Metadata service                   | `pkg/util/metadata/`             | Import directly             |
| Mount utilities                    | `pkg/util/mount/`                | Import directly (bind mount)|
| Error utilities                    | `pkg/util/errors/`               | Import directly             |
| Volume CRUD (partial)              | `openstack_volumes.go`           | Extend with metadata ops    |
| Build system patterns              | `Makefile`, `Dockerfile`         | Add parallel entries        |
| Deployment manifest patterns       | `manifests/cinder-csi-plugin/`   | Copy and adapt              |
| Helm chart patterns                | `charts/cinder-csi-plugin/`      | Copy and adapt              |
| NFS driver cleanup flag pattern    | `cinder-nfs` DeleteVolume        | Adopt same `csi.cleanupVolume` metadata pattern |

---

## 4. Comparison with Manila and NFS-Cinder CSI Drivers

| Aspect                | Cinder CSI                     | Manila CSI                     | NFS-Cinder CSI                 | iSCSI-Cinder CSI (Proposed)     |
|-----------------------|--------------------------------|--------------------------------|--------------------------------|---------------------------------|
| Package               | `pkg/csi/cinder/`             | `pkg/csi/manila/`              | `pkg/csi/cinder-nfs/`         | `pkg/csi/cinder-iscsi/`        |
| Binary                | `cmd/cinder-csi-plugin/`      | `cmd/manila-csi-plugin/`       | `cmd/cinder-nfs-csi-plugin/`  | `cmd/cinder-iscsi-csi-plugin/` |
| Driver name           | `cinder.csi.openstack.org`    | `manila.csi.openstack.org`     | `cinder-nfs.csi.windriver.com`| `cinder-iscsi.csi.windriver.com`|
| OpenStack service     | Cinder + Nova                  | Manila                         | Cinder + Nova                  | Cinder only                     |
| Volume type           | Block (iSCSI/FC)               | Shared FS (NFS/CephFS)         | NFS-backed Cinder (Shadow VM) | iSCSI-backed Cinder (v3 attach) |
| Volume lock           | Nova attachment                | Manila share access rules      | Shadow VM                      | Cinder v3 reserved attachment   |
| Own gRPC server       | Yes (`server.go`)              | Yes (in `driver.go`)           | Yes (copy from cinder/)        | Yes (copy from cinder/)         |
| Own controller server | Yes                            | Yes                            | Yes                            | Yes                             |
| Own node server       | Yes                            | Yes (proxied)                  | Yes                            | Yes                             |
| Own OpenStack client  | `openstack/` subpackage        | `manilaclient/` subpackage     | `openstack/` subpackage        | `openstack/` subpackage         |
| external-attacher     | Yes                            | No                             | No                             | **Yes** (Cinder attachment)     |
| Makefile entry        | `BUILD_CMDS`, `IMAGE_NAMES`   | `BUILD_CMDS`, `IMAGE_NAMES`   | `BUILD_CMDS`, `IMAGE_NAMES`   | `BUILD_CMDS`, `IMAGE_NAMES`     |
| Dockerfile stage      | `cinder-csi-plugin`           | `manila-csi-plugin`            | `cinder-nfs-csi-plugin`       | `cinder-iscsi-csi-plugin`       |
| Manifests             | `manifests/cinder-csi-plugin/`| `manifests/manila-csi-plugin/` | `manifests/cinder-nfs-csi-plugin/`| `manifests/cinder-iscsi-csi-plugin/` |
| Helm chart            | `charts/cinder-csi-plugin/`   | `charts/manila-csi-plugin/`    | `charts/cinder-nfs-csi-plugin/`| `charts/cinder-iscsi-csi-plugin/` |

Key insight: This is the **fourth** independent CSI driver in the monorepo, following the
established pattern of Manila and NFS-Cinder. Each driver is a completely independent
implementation that happens to live in the same repository.

---

## 5. Proposed Folder Structure

### 5.1 New Directories and Files

```
pkg/csi/cinder-iscsi/
├── driver.go                     # iSCSI Driver struct, capabilities, NewDriver(), Run()
├── controllerserver.go           # iSCSI Controller RPCs (Cinder v3 attachment lifecycle)
├── nodeserver.go                 # iSCSI Node RPCs (iscsiadm login/logout, bind mount)
├── identityserver.go             # iSCSI Identity RPCs
├── server.go                     # NonBlockingGRPCServer (copied from cinder/)
├── utils.go                      # Capability factories, gRPC logger
├── connectioninfo.go             # Cinder attachment connection_info parser (iSCSI fields)
├── iscsi.go                      # iSCSI initiator helper (iscsiadm wrapper)
└── openstack/
    ├── openstack.go              # IOpenStackISCSI interface, iSCSI config structs
    ├── openstack_volumes.go      # Volume CRUD + metadata ops (Cinder API)
    ├── openstack_attachments.go  # Thin wrappers around gophercloud blockstorage/v3/attachments
    ├── openstack_mock.go         # Mock for unit tests
    └── fixtures/                 # Test fixtures

cmd/cinder-iscsi-csi-plugin/
└── main.go                       # Binary entry point, CLI flags, iSCSI config loading

manifests/cinder-iscsi-csi-plugin/
├── csi-secret-cinder-iscsi.yaml           # Secret (cloud.conf credentials)
├── csi-configmap-cinder-iscsi.yaml        # ConfigMap (driver.conf: iSCSI opts, volume opts)
├── cinder-iscsi-csi-controllerplugin.yaml # Deployment (controller + sidecars)
├── cinder-iscsi-csi-nodeplugin.yaml       # DaemonSet (node + registrar)
├── csi-cinder-iscsi-driver.yaml           # CSIDriver resource
└── csi-cinder-iscsi-storageclass.yaml     # StorageClass example

charts/cinder-iscsi-csi-plugin/
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
├── sanity/cinder-iscsi/          # CSI sanity tests
└── e2e/cinder-iscsi/             # E2E tests

tools/
├── csi-iscsi-deps.sh             # iSCSI client dependency extractor (production phase)
└── csi-iscsi-deps-check.sh       # Validates iSCSI binaries in distroless image
```

### 5.2 Files to Modify in Existing Tree

| File           | Change                                                          |
|----------------|-----------------------------------------------------------------|
| `Makefile`     | Add `cinder-iscsi-csi-plugin` to `IMAGE_NAMES` and `BUILD_CMDS`; add `test-cinder-iscsi-csi-sanity` target; add `gox` cross-build entry |
| `Dockerfile`   | Add `cinder-iscsi-csi-plugin` Debian-based stage (dev); migrate to 3-step distroless for production (see §10.2.2 TODO) |
| `go.mod`       | No change needed (same module)                                  |
| `OWNERS`       | Add reviewers/approvers for `pkg/csi/cinder-iscsi/`            |
| `tools/csi-iscsi-deps.sh` | New file (production phase) — extracts iSCSI client binaries + shared libs into `/dest` |
| `tools/csi-iscsi-deps-check.sh` | New file (production phase) — validates iSCSI utilities in distroless image |

---

## 6. Interface Design

### 6.1 IOpenStackISCSI Interface

The iSCSI driver defines its own interface. **Key difference from NFS driver: no Nova
methods. All operations use Cinder v3 attachment API.**

```go
// pkg/csi/cinder-iscsi/openstack/openstack.go

type IOpenStackISCSI interface {
    // ── Volume Operations (Cinder) ──────────────────────────────────────
    CreateVolume(ctx context.Context, opts *volumes.CreateOpts,
        schedulerHints volumes.SchedulerHintOptsBuilder) (*volumes.Volume, error)
    DeleteVolume(ctx context.Context, volumeID string) error
    GetVolume(ctx context.Context, volumeID string) (*volumes.Volume, error)
    GetVolumesByName(ctx context.Context, name string) ([]volumes.Volume, error)
    ExpandVolume(ctx context.Context, volumeID string, status string, newSize int) error
    WaitVolumeTargetStatus(ctx context.Context, volumeID string, tStatus []string) error
    SetVolumeMetadata(ctx context.Context, volumeID string, metadata map[string]string) error
    DeleteVolumeMetadata(ctx context.Context, volumeID string, keys []string) error

    // ── Cinder v3 Attachment Operations (REPLACES Shadow VM) ────────────
    CreateAttachment(ctx context.Context, volumeID string) (string, error)
    UpdateAttachmentConnector(ctx context.Context, attachmentID string,
        connector *AttachmentConnector) (*ISCSIConnectionInfo, error)
    CompleteAttachment(ctx context.Context, attachmentID string) error
    GetAttachment(ctx context.Context, attachmentID string) (*Attachment, error)
    DeleteAttachment(ctx context.Context, attachmentID string) error

    // ── Snapshot Operations (Cinder) ────────────────────────────────────
    CreateSnapshot(ctx context.Context, name, volID string, tags map[string]string) (*snapshots.Snapshot, error)
    DeleteSnapshot(ctx context.Context, snapID string) error
    GetSnapshotByID(ctx context.Context, snapshotID string) (*snapshots.Snapshot, error)

    // ── Configuration & Capabilities ────────────────────────────────────
    GetISCSIOpts() ISCSIOpts
    GetVolumeOpts() VolumeOpts
    GetCinderCapabilities() *CinderCapabilities
}

// ISCSIConnectionInfo from Cinder v3 attachment connection_info
type ISCSIConnectionInfo struct {
    DriverVolumeType string `json:"driver_volume_type"` // "iscsi"
    TargetPortal     string `json:"target_portal"`      // "69.167.149.97:3260"
    TargetIQN        string `json:"target_iqn"`         // "iqn.2010-10.org.openstack:volume-xxx"
    TargetLUN        int    `json:"target_lun"`          // 0
    AuthMethod       string `json:"auth_method"`         // "CHAP" or "" (Pure: null)
    AuthUsername      string `json:"auth_username"`       // CHAP username
    AuthPassword      string `json:"auth_password"`       // CHAP password
    VolumeID         string `json:"volume_id"`           // Cinder volume UUID
    Encrypted        bool   `json:"encrypted"`           // false
    TargetDiscovered bool   `json:"target_discovered"`   // false
    AccessMode       string `json:"access_mode"`         // "rw"
    AttachmentID     string `json:"attachment_id"`        // Cinder attachment UUID

    // Multipath fields (Pure Storage — arrays of targets)
    TargetPortals    []string `json:"target_portals,omitempty"`
    TargetIQNs       []string `json:"target_iqns,omitempty"`
    TargetLUNs       []int    `json:"target_luns,omitempty"`
    Discard          bool     `json:"discard,omitempty"`
    EnforceMultipath bool    `json:"enforce_multipath"`
}

// AttachmentConnector sent to Cinder PUT /v3/attachments/{id}
type AttachmentConnector struct {
    Initiator string `json:"initiator"` // IQN from /etc/iscsi/initiatorname.iscsi
    IP        string `json:"ip"`        // Storage network IP of WRCP host
    Host      string `json:"host"`      // WRCP hostname
    Multipath bool   `json:"multipath"` // false (single-path initially)
    Platform  string `json:"platform"`  // "x86_64"
    OSType    string `json:"os_type"`   // "linux2"
}

// Attachment wraps Cinder v3 attachment response
type Attachment struct {
    ID             string                `json:"id"`
    VolumeID       string                `json:"volume_id"`
    Status         string                `json:"status"`
    Instance       *string               `json:"instance"`
    ConnectionInfo *ISCSIConnectionInfo  `json:"connection_info"`
}
```

**Comparison with NFS driver's `IOpenStackNFS`:**

| NFS Driver (`IOpenStackNFS`)                    | iSCSI Driver (`IOpenStackISCSI`)                  | Rationale           |
|-------------------------------------------------|---------------------------------------------------|---------------------|
| `CreateServer()`, `StopServer()`, `DeleteServer()`, `GetServer()`, `WaitServerStatus()` | *(removed — no Shadow VM)* | No Nova dependency   |
| `CreateVolumeAttachment(volumeID, serverID)`    | `CreateAttachment(volumeID)` — no server          | Self-service attach  |
| `GetConnectionInfo() → *NFSConnectionInfo`      | `UpdateAttachmentConnector() → *ISCSIConnectionInfo` | Active operation, not query |
| `GetNFSOpts()`                                  | `GetISCSIOpts()`                                  | Different host deps  |
| `GetShadowVMOpts()`                             | *(removed — no Shadow VM)*                        | No Shadow VM         |
| —                                               | `SetVolumeMetadata()`, `DeleteVolumeMetadata()`   | Store attachment_id  |
| —                                               | `CompleteAttachment()`                             | Microversion 3.44+   |

### 6.2 iSCSI-Specific Config Structs

```go
// pkg/csi/cinder-iscsi/openstack/openstack.go

// ISCSICinderConfig for the iSCSI-Cinder CSI driver
type ISCSICinderConfig struct {
    Global map[string]*client.AuthOpts // from cloud.conf (Secret)
    ISCSI  ISCSIOpts                   // from driver.conf [ISCSI]
    Volume VolumeOpts                  // from driver.conf [Volume]
}

// ISCSIOpts controls iSCSI initiator behavior in NodeStageVolume/NodeUnstageVolume
// and the connector fields sent to Cinder during ControllerPublishVolume.
type ISCSIOpts struct {
    EnableMultipath   bool   `gcfg:"enable-multipath"`     // Default: false
    CHAPAuthEnabled   bool   `gcfg:"chap-auth-enabled"`    // Default: true
    LoginTimeout      int    `gcfg:"login-timeout"`        // Default: 30 (seconds)
    DeviceWaitTimeout int    `gcfg:"device-wait-timeout"`  // Default: 30 (seconds)
    ISCSIInterface    string `gcfg:"iscsi-interface"`      // Default: "default"
    StorageInterface  string `gcfg:"storage-interface"`    // Default: "" (primary IP)
    Platform          string `gcfg:"platform"`             // Default: "x86_64"
    OSType            string `gcfg:"os-type"`              // Default: "linux2"
}

// VolumeOpts controls Cinder volume lifecycle
type VolumeOpts struct {
    CreateTimeout     int    `gcfg:"create-timeout"`       // Default: 300 (seconds)
    DetachTimeout     int    `gcfg:"detach-timeout"`       // Default: 120 (seconds)
    DefaultVolumeType string `gcfg:"default-volume-type"`  // Optional
    MetadataPrefix    string `gcfg:"metadata-prefix"`      // Default: "csi"
    DeleteVolumeMode  string `gcfg:"delete-volume-mode"`   // Default: "retain" — see below
}
```

**`delete-volume-mode` configuration:**

This option sets the **driver-level default** for what happens when a PVC is deleted
(i.e., when the `external-provisioner` sidecar calls `DeleteVolume`). The migration
operator can override this default on a per-volume basis by setting the
`csi.cleanupVolume` metadata key on the Cinder volume.

| Value | Behavior | Use Case |
|-------|----------|----------|
| `retain` (default) | Delete attachment + remove CSI metadata, but **leave the Cinder volume available** for Blueprint to create the target VM | Normal migration success path |
| `delete` | Delete attachment + **delete the Cinder volume entirely** | Error/cleanup path, or non-migration workloads |

**Precedence rules:**
1. If `csi.cleanupVolume` metadata is set on the volume → use that value (`"true"` = delete)
2. Otherwise → use `delete-volume-mode` from `driver.conf`
3. If neither is set → default to `retain`

This means:
- **Migration operator** — does not need to set per-volume metadata in the success
  path. The driver default of `retain` ensures volumes survive PVC deletion.
- **Error handler** — sets `csi.cleanupVolume=true` on volumes that should be fully
  cleaned up (e.g., failed migrations).
- **Non-migration deployments** — set `delete-volume-mode = delete` in `driver.conf`
  so the driver behaves like a standard CSI driver.

**Example `driver.conf`:**
```ini
[ISCSI]
enable-multipath = false
chap-auth-enabled = true
login-timeout = 30
device-wait-timeout = 30
iscsi-interface = default
storage-interface =
platform = x86_64
os-type = linux2

[Volume]
create-timeout = 300
detach-timeout = 120
delete-volume-mode = retain
```

**Comparison with NFS driver config:**

| NFS Driver Config                  | iSCSI Driver Config              | Notes                              |
|------------------------------------|----------------------------------|------------------------------------|
| `ShadowVMOpts` (FlavorID, ImageID, SubnetID, NetworkID, etc.) | *(removed — no Shadow VM)* | Zero compute resource config |
| `NFSOpts` (MountOptions, NFSVersion, DefaultFsType) | `ISCSIOpts` (Multipath, CHAP, Timeouts, Interface, Platform, OSType) | Different host deps |
| `VolumeOpts` (DefaultVolumeType, DefaultVolumeAZ) | `VolumeOpts` (CreateTimeout, DetachTimeout, DefaultVolumeType, MetadataPrefix) | Extended for attachment lifecycle |

### 6.3 Cinder v3 Attachment Lifecycle State Machine

Unlike the NFS driver's Shadow VM state machine, the iSCSI driver uses a purely
API-driven attachment lifecycle with no compute resources:

```
                              ┌──────────────────┐
                              │  No Attachment   │
                              │  (vol: available) │
                              └──────┬───────────┘
                                     │ CreateVolume called
                                     │ → Cinder create volume
                                     │ → POST /v3/attachments (no connector)
                                     ▼
                              ┌──────────────────┐
                              │    Reserved      │
                              │  Attachment      │◄───── Steady State
                              │  (no connector)  │       (volume locked,
                              │  (vol: reserved)  │        no iSCSI target)
                              └──────┬───────────┘
                                     │
                    ┌────────────────┼────────────────────┐
                    │                │                    │
                    ▼                │                    ▼
          ControllerPublish   ExpandVolume         DeleteVolume
          (update connector)  (Cinder extend)      (PVC deleted)
                    │                                    │
                    ▼                                    │
          ┌──────────────────┐                           │
          │   Connected      │                           │
          │  Attachment      │                           │
          │  (connector set) │                           │
          │  (vol: reserved/ │                           │
          │   in-use)        │                           │
          └──────┬───────────┘                           │
                 │                                       │
                 │ iSCSI login + CDI data transfer       │
                 │                                       │
                 │ Pod exits                              │
                 ▼                                       │
          ┌──────────────────┐                           │
          │ ControllerUnpub  │                           │
          │ (delete + recreate)                          │
          │  old attachment  │                           │
          │  → available →   │                           │
          │  new reserved    │                           │
          │  attachment      │                           │
          └──────┬───────────┘                           │
                 │                                       │
                 └──────── Back to "Reserved" ◄──────────┘
                           (ready for next CDI stage      │
                            or PVC deletion)               │
                                                          ▼
                                               ┌──────────────────┐
                                               │  No Attachment   │
                                               │  (vol: available) │
                                               │  → Blueprint     │
                                               │    creates VM    │
                                               └──────────────────┘
```

The attachment ID is stored as volume metadata in Cinder:
```
csi.attachment_id = <attachment-uuid>
```

**Key differences from NFS Shadow VM state machine:**

| Aspect                 | NFS Shadow VM                       | iSCSI v3 Attachment                    |
|------------------------|-------------------------------------|----------------------------------------|
| Resource type          | Nova instance (stopped)             | Cinder attachment record (API object)  |
| Compute quota          | 1 VM consumed                       | Zero                                   |
| Provisioning time      | 30-120s (boot + stop)               | <5s (API call)                         |
| ControllerUnpublish    | No-op (VM persists)                 | Delete + recreate (attachment rotation)|
| Persistence            | Shadow VM ID in volume metadata     | Attachment ID in volume metadata       |
| Cleanup trigger        | `DeleteVolume` deletes Shadow VM    | `DeleteVolume` deletes attachment      |

### 6.4 Architecture Decision: Gophercloud SDK for Cinder v3 Attachments

The `IOpenStackISCSI` attachment methods (`CreateAttachment`, `UpdateAttachmentConnector`,
`CompleteAttachment`, `GetAttachment`, `DeleteAttachment`) will **not** be implemented as
custom HTTP client code. Instead, they will wrap the existing
**Gophercloud `blockstorage/v3/attachments`** package, which is already a dependency of
this project (`gophercloud/v2 v2.8.0` in `go.mod`).

**Decision:** Use Gophercloud's `blockstorage/v3/attachments` package for all Cinder v3
attachment API operations. No custom HTTP client.

**Rationale:**

| Aspect | Custom HTTP Client | Gophercloud SDK |
|--------|-------------------|------------------|
| **Auth token management** | Must implement Keystone token acquisition, caching, and refresh | Handled automatically by `gophercloud.ProviderClient` |
| **Endpoint discovery** | Must query Keystone service catalog, select correct API version | `openstack.NewBlockStorageV3()` handles this |
| **Microversion header** | Must manually set `OpenStack-API-Version: volume 3.27` | `client.Microversion = "3.27"` |
| **Request/response marshaling** | Hand-roll JSON encode/decode for each API | Built-in typed structs (`CreateOpts`, `UpdateOpts`, `Attachment`) |
| **Error handling** | Parse HTTP status codes and error bodies | Typed errors (`ErrDefault404`, `ErrDefault409`, etc.) |
| **Ongoing maintenance** | We own all Cinder API client code | Community-maintained (3k+ GitHub stars, active development) |
| **Consistency** | Diverges from existing codebase | Same SDK used by the existing `cinder.csi.openstack.org` driver |

**API coverage — Gophercloud provides all five operations we need:**

| Our Interface Method | Gophercloud Function | Cinder API |
|---|---|---|
| `CreateAttachment(volumeID)` | `attachments.Create(ctx, client, CreateOpts{VolumeUUID: volumeID})` | `POST /v3/{project}/attachments` |
| `UpdateAttachmentConnector(attachmentID, connector)` | `attachments.Update(ctx, client, id, UpdateOpts{Connector: map})` | `PUT /v3/{project}/attachments/{id}` |
| `CompleteAttachment(attachmentID)` | `attachments.Complete(ctx, client, id)` | `POST /v3/{project}/attachments/{id}/action` |
| `GetAttachment(attachmentID)` | `attachments.Get(ctx, client, id)` | `GET /v3/{project}/attachments/{id}` |
| `DeleteAttachment(attachmentID)` | `attachments.Delete(ctx, client, id)` | `DELETE /v3/{project}/attachments/{id}` |

**Self-service attachment (serverless):** Gophercloud's `CreateOpts.InstanceUUID` is a
string field — leaving it empty produces the self-service attachment behavior we need
(microversion 3.27+, no Nova instance required).

**ConnectionInfo parsing:** Gophercloud returns `ConnectionInfo` as `map[string]any`.
Our `openstack_attachments.go` will provide a thin typed wrapper:

```go
import "github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/attachments"

func (os *OpenStackISCSI) CreateAttachment(ctx context.Context, volumeID string) (string, error) {
    result, err := attachments.Create(ctx, os.blockstorage, attachments.CreateOpts{
        VolumeUUID: volumeID,
        // InstanceUUID intentionally omitted → self-service attachment
    }).Extract()
    if err != nil {
        return "", err
    }
    return result.ID, nil
}

func (os *OpenStackISCSI) UpdateAttachmentConnector(
    ctx context.Context, attachmentID string, connector *AttachmentConnector,
) (*ISCSIConnectionInfo, error) {
    result, err := attachments.Update(ctx, os.blockstorage, attachmentID,
        attachments.UpdateOpts{
            Connector: map[string]any{
                "initiator": connector.Initiator,
                "ip":        connector.IP,
                "host":      connector.Host,
                "multipath": connector.Multipath,
                "platform":  connector.Platform,
                "os_type":   connector.OSType,
            },
        }).Extract()
    if err != nil {
        return nil, err
    }
    return parseISCSIConnectionInfo(result.ConnectionInfo)
}

// parseISCSIConnectionInfo converts map[string]any → typed ISCSIConnectionInfo
func parseISCSIConnectionInfo(raw map[string]any) (*ISCSIConnectionInfo, error) {
    data, err := json.Marshal(raw)
    if err != nil {
        return nil, fmt.Errorf("failed to marshal connection_info: %w", err)
    }
    var info ISCSIConnectionInfo
    if err := json.Unmarshal(data, &info); err != nil {
        return nil, fmt.Errorf("failed to parse iSCSI connection_info: %w", err)
    }
    return &info, nil
}
```

**os-brick parity:** The `connection_info` fields returned by Cinder (`target_portal`,
`target_iqn`, `target_lun`, `auth_method`, `auth_username`, `auth_password`,
`target_portals`/`target_iqns`/`target_luns` for multipath) are identical to what
OpenStack's os-brick `ISCSIConnector` consumes on Nova compute hosts. Our
`ISCSIConnectionInfo` struct maps these fields 1:1, confirmed by reviewing the
[os-brick source](https://github.com/openstack/os-brick/blob/master/os_brick/initiator/connectors/iscsi.py)
(`connect_volume()` method). The `iscsiadm` command sequence in our `NodeStageVolume` —
discovery → CHAP configuration → login → `/dev/disk/by-path/` device path construction —
is the exact same sequence os-brick performs for every Nova iSCSI volume attach in
production, giving us high confidence in the approach.

---

## 7. Prerequisites and Runtime Validation

The iSCSI-Cinder CSI driver has specific environment requirements that must be satisfied
before the driver can operate. These are documented in the
[design doc §9](iscsi-backed-cinder-volume-for-wrcp-migration.md#9-prerequisites) and are
validated at different points in the driver lifecycle.

### 7.1 Environment Prerequisites

The following table lists all prerequisites and specifies **when** and **how** each is
validated by the driver:

| Prerequisite | Required On | Validated When | Validation Method | Failure Mode |
|---|---|---|---|---|
| **Cinder iSCSI backend** | Controller | First `CreateVolume` | `connection_info.driver_volume_type == "iscsi"` in `ControllerPublishVolume` response | `codes.InvalidArgument` — volume type does not produce iSCSI targets |
| **Cinder microversion 3.27+** | Controller | Startup (cached) | `GET /` → parse `max_version` from API version discovery; see [§7.3](#73-microversion-detection-strategy) | Driver refuses to start; logs fatal error with required vs available version |
| **Cinder microversion 3.44+** (optional) | Controller | Startup (cached) | Same discovery endpoint; enables `os-complete` | `os-complete` skipped; volume stays `reserved` instead of `in-use` (functionally equivalent) |
| **iSCSI network accessibility** (port 3260) | Node | `NodeStageVolume` | `iscsiadm -m discovery -t sendtargets -p ${portal}` | `codes.Internal` — discovery failed, include portal IP and error in message |
| **`open-iscsi` installed** (`iscsiadm`) | Node | Startup (`Probe`) | `exec.LookPath("iscsiadm")` | `Probe` returns `NOT_READY`; kubelet will not schedule CSI volumes to this node |
| **`iscsid` daemon running** | Node | Startup (`Probe`) | `iscsiadm -m session` (exit code 0 or 21 = no sessions, both OK; other = `iscsid` not running) | `Probe` returns `NOT_READY` |
| **Unique IQN per worker** | Node | Startup (`NodeGetInfo`) | Read `/etc/iscsi/initiatorname.iscsi`; if file missing or empty → error | `NodeGetInfo` fails → node-driver-registrar retries |
| **OpenStack credentials** | Controller | Startup (`Probe`) | Keystone token request | `Probe` returns `NOT_READY`; controller pod restarts via liveness probe |
| **Privileged host access** | Node | Startup | Container must run as privileged; verified implicitly by `iscsiadm` commands | `iscsiadm` commands fail with permission errors |
| **WRC K8S cluster with CDI** | Cluster | Deployment time | Not validated by driver — infrastructure prerequisite | CDI DataVolume creation fails (outside driver scope) |

### 7.2 Startup Validation vs. Ongoing Validation

The driver splits validation into two categories:

**Startup validation (checked once, results cached):**
- Keystone authentication — token is obtained and refreshed automatically by gophercloud
- Cinder API version discovery — microversion capabilities cached in a `CinderCapabilities`
  struct (see [§7.3](#73-microversion-detection-strategy))
- `iscsiadm` binary availability — checked via `exec.LookPath` on node startup
- `iscsid` daemon status — checked via `iscsiadm -m session` on node startup
- Initiator IQN — read from `/etc/iscsi/initiatorname.iscsi` once at `NodeGetInfo` time

**Ongoing validation (checked on every relevant RPC call):**
- `ControllerPublishVolume`: validates `driver_volume_type == "iscsi"` in connection info
- `NodeStageVolume`: validates iSCSI discovery succeeds (network connectivity to portal)
- `NodeStageVolume`: validates CHAP authentication succeeds (if auth_method == "CHAP")
- `NodeStageVolume`: validates block device appears within timeout
- All RPCs: Keystone token is auto-refreshed by gophercloud on 401 responses

### 7.3 Microversion Detection Strategy

The driver must handle two microversion thresholds:
- **3.27** (required): Self-service attachments without Nova instance
- **3.44** (optional): `os-complete` action to transition volume from `reserved` → `in-use`

**Detection mechanism — API version discovery at startup:**

```go
// pkg/csi/cinder-iscsi/openstack/openstack.go

// CinderCapabilities caches Cinder API version discovery results
type CinderCapabilities struct {
    MaxMicroversion string // e.g. "3.70"
    SupportsV327    bool   // Self-service attachments
    SupportsV344    bool   // os-complete action
}

// DiscoverCinderCapabilities queries Cinder API version discovery endpoint
// Called once at startup by the controller plugin.
func DiscoverCinderCapabilities(ctx context.Context, client *gophercloud.ServiceClient) (*CinderCapabilities, error) {
    // GET / on the block-storage endpoint returns API version metadata
    // Response includes: { "versions": [{ "id": "v3.0", "max_version": "3.70", ... }] }
    allPages, err := apiversions.List(client).AllPages(ctx)
    if err != nil {
        return nil, fmt.Errorf("failed to discover Cinder API versions: %w", err)
    }
    versions, err := apiversions.ExtractAPIVersions(allPages)
    if err != nil {
        return nil, fmt.Errorf("failed to parse Cinder API versions: %w", err)
    }

    caps := &CinderCapabilities{}
    for _, v := range versions {
        if v.ID == "v3.0" {
            caps.MaxMicroversion = v.MaxVersion
            caps.SupportsV327 = compareMicroversions(v.MaxVersion, "3.27") >= 0
            caps.SupportsV344 = compareMicroversions(v.MaxVersion, "3.44") >= 0
            break
        }
    }

    if !caps.SupportsV327 {
        return nil, fmt.Errorf(
            "Cinder microversion 3.27+ required for self-service attachments, "+
                "but server reports max_version=%s (need Queens 2018.1 or later)",
            caps.MaxMicroversion)
    }

    klog.Infof("Cinder capabilities: max_version=%s, v3.27=%v, v3.44=%v",
        caps.MaxMicroversion, caps.SupportsV327, caps.SupportsV344)
    return caps, nil
}

// compareMicroversions returns -1, 0, or 1 comparing "3.X" and "3.Y"
func compareMicroversions(a, b string) int {
    // Parse major.minor from each, compare numerically
    // e.g. "3.70" vs "3.44" → 1 (a > b)
}
```

**Usage in driver lifecycle:**

| Phase | Action | Failure Behavior |
|-------|--------|------------------|
| Controller startup (`main.go`) | Call `DiscoverCinderCapabilities()` | If < 3.27: **fatal error**, driver exits. Controller pod CrashLoopBackOff until OpenStack is upgraded. |
| `Probe` RPC (controller) | Return cached result | `NOT_READY` if discovery failed at startup |
| `ControllerPublishVolume` | If `caps.SupportsV344`: call `CompleteAttachment()` | If < 3.44: skip `os-complete`. Volume stays `reserved`. iSCSI login still works — validated in Phase 0 CLI testing. |
| `CompleteAttachment()` call | Best-effort even when 3.44 detected | If call returns HTTP 404 or 400: log warning, continue. Never fail the RPC due to `os-complete` failure. |

**Why not try-and-handle-404?** The alternative approach (always attempt `os-complete` and
handle HTTP 404 gracefully) has two drawbacks:
1. Every `ControllerPublishVolume` call would generate a 404 error log on pre-3.44 clouds,
   polluting logs
2. Error handling for "expected 404" vs "unexpected 404" is ambiguous

The startup-discovery approach is cleaner: check once, cache the result, branch cleanly.

---

## 8. CSI RPC Implementation Map

### 8.1 Identity Service

| RPC | Implementation |
|-----|----------------|
| `GetPluginInfo` | Return `{ name: "cinder-iscsi.csi.windriver.com", vendor_version: "1.0.0" }` |
| `GetPluginCapabilities` | `CONTROLLER_SERVICE`, `VOLUME_ACCESSIBILITY_CONSTRAINTS` |
| `Probe` | See detailed implementation below |

**`Probe` Implementation:**

The `Probe` RPC is called by the `livenessprobe` sidecar container at a configurable
interval (default: 10s). It must return quickly and indicate whether the driver is ready
to handle RPCs.

**Controller plugin `Probe`:**
```go
func (ids *identityServer) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
    // Startup validation results are cached — Probe returns them without re-checking
    if ids.cinderCaps == nil {
        // Discovery hasn't completed yet (still initializing)
        return &csi.ProbeResponse{Ready: &wrapperspb.BoolValue{Value: false}}, nil
    }
    if !ids.cinderCaps.SupportsV327 {
        // Should not happen (startup would have failed), but defensive
        return &csi.ProbeResponse{Ready: &wrapperspb.BoolValue{Value: false}}, nil
    }
    // Optionally: attempt a lightweight Cinder API call to verify connectivity
    // For now, return cached readiness — Keystone token auto-refresh handles auth
    return &csi.ProbeResponse{Ready: &wrapperspb.BoolValue{Value: true}}, nil
}
```

**Node plugin `Probe`:**
```go
func (ids *identityServer) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
    // Check iscsiadm binary exists (cached after first check)
    if ids.iscsiadmPath == "" {
        path, err := exec.LookPath("iscsiadm")
        if err != nil {
            klog.Errorf("iscsiadm not found: %v", err)
            return &csi.ProbeResponse{Ready: &wrapperspb.BoolValue{Value: false}}, nil
        }
        ids.iscsiadmPath = path
    }

    // Check iscsid is running (re-check each Probe call — daemon could restart)
    // iscsiadm -m session returns:
    //   exit 0  = sessions exist (iscsid running)
    //   exit 21 = no active sessions (iscsid running, just no sessions — OK)
    //   exit 22 = iscsid not running
    cmd := exec.CommandContext(ctx, "iscsiadm", "-m", "session")
    err := cmd.Run()
    if err != nil {
        if exitErr, ok := err.(*exec.ExitError); ok {
            if exitErr.ExitCode() == 21 {
                // No active sessions — iscsid is running, this is fine
                return &csi.ProbeResponse{Ready: &wrapperspb.BoolValue{Value: true}}, nil
            }
            // Exit code 22 or other = iscsid not running
            klog.Errorf("iscsid not running (iscsiadm exit code %d)", exitErr.ExitCode())
            return &csi.ProbeResponse{Ready: &wrapperspb.BoolValue{Value: false}}, nil
        }
        klog.Errorf("iscsiadm probe failed: %v", err)
        return &csi.ProbeResponse{Ready: &wrapperspb.BoolValue{Value: false}}, nil
    }
    return &csi.ProbeResponse{Ready: &wrapperspb.BoolValue{Value: true}}, nil
}
```

**Key design decisions:**

| Decision | Rationale |
|----------|-----------|
| `iscsiadm` binary: cached after first check | Binary path doesn't change at runtime |
| `iscsid` daemon: re-checked every `Probe` call | Daemon could crash/restart between probes |
| Keystone auth: NOT re-checked per Probe | gophercloud auto-refreshes tokens on 401; explicit re-auth per probe adds unnecessary latency |
| Cinder microversion: cached at startup | API version doesn't change without Cinder service restart (which outlives driver pods) |

**Block-Only Volume Mode Enforcement:**

This driver is a migration-specific driver (`cinder-iscsi.csi.windriver.com`) used
exclusively by the Wind River migration orchestrator (Blueprint) — it is **not** a
general-purpose Kubernetes StorageClass solution. It only supports `volumeMode: Block`
PVCs and explicitly rejects filesystem (`volumeMode: Filesystem`) requests at the CSI
RPC level.

*Why block-only:*
- CDI importer pods write raw VMDK/QCOW2 disk images byte-for-byte to the block device
  (`/dev/cdi-block-volume`). The resulting Cinder volume contains a guest OS partition
  table and filesystems — it must not be wrapped in a host filesystem (ext4/xfs).
- If filesystem mode were supported and accidentally used, the driver would `mkfs` the
  Cinder volume, then CDI would write the disk image as a *file inside* that filesystem.
  The resulting volume would be unusable as a VM boot disk — the migration would fail
  silently.
- O2O NBD receiver pods also write raw block data received via the NBD protocol.

*Enforcement points:*

Every CSI RPC that receives `VolumeCapability` validates that the access type is `Block`
and rejects `Mount`:

| RPC | Enforcement | Error |
|-----|-------------|-------|
| `CreateVolume` | Reject if any `VolumeCapability` has `GetMount() != nil` | `codes.InvalidArgument` — "filesystem (mount) volume mode is not supported; this driver only supports volumeMode: Block for migration workloads" |
| `ValidateVolumeCapabilities` | Return unconfirmed (no `Confirmed` field) if mount requested | Response message: "filesystem mount mode not supported; only raw block volumes" |
| `NodeStageVolume` | Reject if `VolumeCapability.GetMount() != nil` | `codes.InvalidArgument` — "filesystem mount not supported; use volumeMode: Block in PVC" |
| `ControllerPublishVolume` | Reject if mount capability | `codes.InvalidArgument` |

*Reference implementation:*

```go
// validateBlockOnlyCapability is called by CreateVolume, ControllerPublishVolume,
// ValidateVolumeCapabilities, and NodeStageVolume.
func validateBlockOnlyCapability(caps []*csi.VolumeCapability) error {
    for _, cap := range caps {
        if cap.GetMount() != nil {
            return status.Error(codes.InvalidArgument,
                "cinder-iscsi.csi.windriver.com: filesystem (mount) volume mode is not supported; "+
                    "this driver only supports volumeMode: Block for migration workloads")
        }
        if cap.GetBlock() == nil {
            return status.Error(codes.InvalidArgument,
                "volume capability must specify block access type")
        }
    }
    return nil
}
```

*What happens when `volumeMode` is omitted from PVC:*

Kubernetes defaults `volumeMode` to `Filesystem` when omitted. The `external-provisioner`
sidecar will call `CreateVolume` with `VolumeCapability{mount: {fsType: "ext4"}}`, and the
driver will reject it immediately with a clear error message. This is the correct behavior
— Blueprint always sets `volumeMode: Block` explicitly.

*Driver name signals non-generic purpose:*

The driver name `cinder-iscsi.csi.windriver.com` (not `*.openstack.org`) intentionally
signals that this is a Wind River migration-specific driver:

| Signal | Value | Meaning |
|--------|-------|--------|
| Driver name domain | `windriver.com` | Not an upstream community driver |
| StorageClass creation | By Blueprint (migration orchestrator) | Not by cluster admins |
| PVC creation | Programmatic (Blueprint) | Always `volumeMode: Block`, never user-created |
| Filesystem formatting | Not supported | No `mkfs`, no `fsGroupPolicy` |
| Topology awareness | Not needed | Migration workloads target specific WRCP workers |
| Ephemeral volumes | Not supported | `volumeLifecycleModes: [Persistent]` only |

### 8.2 Controller Service

**Controller Service Capabilities:**

| Capability | Supported | Notes |
|------------|-----------|-------|
| `CREATE_DELETE_VOLUME` | Yes | Cinder volume + attachment lifecycle |
| `PUBLISH_UNPUBLISH_VOLUME` | Yes | Cinder attachment connector update (iSCSI target discovery) |
| `LIST_VOLUMES` | Yes | List Cinder volumes |
| `EXPAND_VOLUME` | Yes | Cinder `os-extend` API |
| `CREATE_DELETE_SNAPSHOT` | No | Not required for migration use case |
| `CLONE_VOLUME` | No | Not required for migration use case |

#### CreateVolume

**K8S Calling Chain:** PVC created → `external-provisioner` sidecar detects unbound PVC
→ calls `CreateVolume` → PV created and bound to PVC.

```
Input: name, capacity, parameters (volume_type, availability), secrets (cloud)
│
├── 1. Validate request parameters
├── 2. Check for existing volume by name (idempotency)
│      cloud.GetVolumesByName(req.Name)
├── 3. Create Cinder volume:
│      cloud.CreateVolume(opts{Name, Size, VolumeType, AZ})
│      → POST /v3/volumes
├── 4. Wait for volume to be "available":
│      cloud.WaitVolumeTargetStatus(volumeID, ["available"])
├── 5. Create reserved attachment (no connector, no server):
│      cloud.CreateAttachment(volumeID)
│      → POST /v3/attachments { volume_uuid: volumeID }
│      Volume status → "reserved" (acts as lock)
├── 6. Store attachment_id in Cinder volume metadata:
│      cloud.SetVolumeMetadata(volumeID, {"csi.attachment_id": attachmentID})
│
Output: Volume{
    ID: volumeID,
    CapacityBytes: sizeBytes,
    VolumeContext: {"attachment_id": attachmentID}
}
```

#### DeleteVolume

**K8S Calling Chain:** PVC deleted → `external-provisioner` sidecar calls `DeleteVolume`
→ PV deleted. This is the **final cleanup point** in the migration lifecycle.

```
Input: volumeID, secrets
│
├── 1. Get volume from Cinder: cloud.GetVolume(volumeID)
│      → If not found: return success (idempotent)
├── 2. Extract metadata:
│      attachmentID = metadata["csi.attachment_id"]
│      cleanupVolume = metadata["csi.cleanupVolume"]
├── 3. If attachment exists:
│      cloud.DeleteAttachment(attachmentID)
│      → DELETE /v3/attachments/{id}
│      → Backend: terminate_connection() → iSCSI target removed
│      Volume status → "available"
├── 4. Check cleanup mode:
│      cleanupVolume = metadata["csi.cleanupVolume"]
│      IF cleanupVolume is not set:
│        → Use driver.conf delete-volume-mode (default: "retain")
│      IF cleanupVolume == "true" OR delete-volume-mode == "delete":
│        → cloud.DeleteVolume(volumeID)  (full cleanup)
│      ELSE (default — migration success):
│        → cloud.DeleteVolumeMetadata(volumeID, ["csi.attachment_id", "csi.cleanupVolume"])
│        → Volume stays "available" for Blueprint to create target VM
│
Output: DeleteVolumeResponse{}
```

**Volume Handoff to Blueprint (success path):**
```
Blueprint (after PVC deletion):
  1. openstack volume set --bootable ${VOLUME_ID}
  2. openstack server create --volume ${VOLUME_ID} ...
  ✓ Nova creates its own iSCSI attachment with compute host's IQN
  ✓ Target VM boots from the Cinder volume
```

#### ControllerPublishVolume

**K8S Calling Chain:** Pod scheduled to node → AD Controller creates `VolumeAttachment` CR
→ `external-attacher` sidecar calls `ControllerPublishVolume` with the node's identity.

```
Input: volumeID, nodeID ("worker-3;iqn.xxx;10.0.0.103"), volumeCapability, secrets
│
├── 1. Parse node identity:
│      host, iqn, ip = split(req.NodeId, ";")
├── 2. Get attachment_id from volume context or volume metadata:
│      attachmentID = req.VolumeContext["attachment_id"]
│      OR: cloud.GetVolume(req.VolumeId) → metadata["csi.attachment_id"]
├── 3. Update attachment with initiator connector:
│      connInfo = cloud.UpdateAttachmentConnector(attachmentID, &AttachmentConnector{
│          Initiator: iqn, IP: ip, Host: host,
│          Multipath: false, Platform: "x86_64", OSType: "linux2",
│      })
│      → PUT /v3/attachments/{id} { connector: {...} }
│      → Cinder calls backend initialize_connection()
│      → Backend creates iSCSI target for this IQN
├── 4. Optionally complete attachment (if microversion >= 3.44):
│      cloud.CompleteAttachment(attachmentID)
│      → Volume status → "in-use"
├── 5. Validate driver_volume_type == "iscsi"
│
Output: PublishContext{
    "target_portal":      connInfo.TargetPortal,    // "69.167.149.97:3260"
    "target_iqn":         connInfo.TargetIQN,       // "iqn.2010-10.org.openstack:volume-xxx"
    "target_lun":         connInfo.TargetLUN,        // "0"
    "auth_method":        connInfo.AuthMethod,       // "CHAP"
    "auth_username":      connInfo.AuthUsername,      // "Hkh2UcACt9zoUxYjnz4U"
    "auth_password":      connInfo.AuthPassword,     // "trtMa3STYUiMJT7K"
    "driver_volume_type": connInfo.DriverVolumeType, // "iscsi"
}
```

#### ControllerUnpublishVolume

**K8S Calling Chain:** Pod deleted → kubelet calls `NodeUnpublishVolume` +
`NodeUnstageVolume` → AD Controller deletes `VolumeAttachment` CR → `external-attacher`
sidecar calls `ControllerUnpublishVolume`.

**Critical difference from NFS driver:** This is **NOT** a no-op. iSCSI targets are
per-initiator — the attachment must be rotated (delete + recreate) so the next CDI stage
can update the connector with a potentially different worker's IQN.

```
Input: volumeID, nodeID
│
├── 1. Get current attachment_id from volume metadata:
│      cloud.GetVolume(req.VolumeId) → metadata["csi.attachment_id"]
├── 2. Delete current attachment:
│      cloud.DeleteAttachment(attachmentID)
│      → DELETE /v3/attachments/{id}
│      → Backend: terminate_connection() → iSCSI target removed
│      Volume status → "available"
├── 3. Create new reserved attachment:
│      newAttachmentID = cloud.CreateAttachment(req.VolumeId)
│      → POST /v3/attachments { volume_uuid: req.VolumeId }
│      Volume status → "reserved" (locked again)
├── 4. Update volume metadata:
│      cloud.SetVolumeMetadata(req.VolumeId, {"csi.attachment_id": newAttachmentID})
│
Output: ControllerUnpublishVolumeResponse{}

Volume status transition: reserved/in-use → available → reserved
```

**Idempotency:** If the old attachment is already deleted (e.g., driver crashed
mid-operation), skip step 2 and proceed to step 3. If a reserved attachment already
exists, reuse it.

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

### 8.3 Node Service

**Node Service Capabilities:**

| Capability | Supported | Notes |
|------------|-----------|-------|
| `STAGE_UNSTAGE_VOLUME` | Yes | iSCSI login/logout at staging phase |
| `GET_VOLUME_STATS` | Yes | Block device stats |
| `EXPAND_VOLUME` | No | Volume expansion is controller-side via Cinder |

#### NodeStageVolume

**K8S Calling Chain:** After `ControllerPublishVolume` completes, kubelet calls
`NodeStageVolume` with the `publish_context` containing iSCSI target details.

```
Input: volumeID, publishContext (iSCSI target info), stagingTargetPath, volumeCapability
│
├── 1. Parse publish_context:
│      portal   = req.PublishContext["target_portal"]
│      iqn      = req.PublishContext["target_iqn"]
│      lun      = req.PublishContext["target_lun"]
│      authMethod = req.PublishContext["auth_method"]
│      username = req.PublishContext["auth_username"]
│      password = req.PublishContext["auth_password"]
├── 2. Idempotency check:
│      if iSCSI session already active for IQN+portal AND device exists → return OK
├── 3. iSCSI discovery:
│      iscsiadm -m discovery -t sendtargets -p ${portal}
├── 4. Set CHAP auth (if auth_method == "CHAP"):
│      iscsiadm -m node -T ${iqn} -p ${portal} --op update -n node.session.auth.authmethod -v CHAP
│      iscsiadm -m node -T ${iqn} -p ${portal} --op update -n node.session.auth.username -v ${username}
│      iscsiadm -m node -T ${iqn} -p ${portal} --op update -n node.session.auth.password -v ${password}
├── 5. iSCSI login:
│      iscsiadm -m node -T ${iqn} -p ${portal} --login
├── 6. Wait for block device (with timeout):
│      device_path = /dev/disk/by-path/ip-${portal}-iscsi-${iqn}-lun-${lun}
│      Poll until device_path exists (timeout: config.ISCSI.DeviceWaitTimeout)
│      Resolve: readlink -f ${device_path} → /dev/sdc
├── 7. Store device path at staging target path:
│      mkdir -p ${req.StagingTargetPath}
│      echo ${device_path} > ${req.StagingTargetPath}/devicepath
│
Output: NodeStageVolumeResponse{}
```

**Comparison with NFS `NodeStageVolume`:**

| Step | NFS Driver | iSCSI Driver |
|------|-----------|-------------|
| Discovery | Parse `publish_context["nfs_export"]` | `iscsiadm -m discovery -t sendtargets` |
| Authentication | None (NFS ACL-based) | CHAP auth via `iscsiadm --op update` |
| Mount / Connect | `mount -t nfs -o opts export staging_path` | `iscsiadm --login` → kernel creates block device |
| Verification | `stat staging_path/volume_file` | Wait for `/dev/disk/by-path/...` to appear |
| Artifact at staging path | NFS mount point with volume file | File containing device path string |

#### NodeUnstageVolume

```
Input: volumeID, stagingTargetPath
│
├── 1. Read device info from staging path:
│      devicepath = readFile(${req.StagingTargetPath}/devicepath)
├── 2. Parse target IQN and portal from device path:
│      /dev/disk/by-path/ip-PORTAL-iscsi-IQN-lun-LUN
├── 3. iSCSI logout:
│      iscsiadm -m node -T ${iqn} -p ${portal} --logout
├── 4. Clean up node DB entry:
│      iscsiadm -m node -T ${iqn} -p ${portal} --op delete
├── 5. Remove staging directory artifacts:
│      rm ${req.StagingTargetPath}/devicepath
│      rmdir ${req.StagingTargetPath}
│
Output: NodeUnstageVolumeResponse{}
```

#### NodePublishVolume

```
Input: volumeID, stagingTargetPath, targetPath, volumeCapability
│
├── 1. Read device path from staging:
│      device = readFile(${req.StagingTargetPath}/devicepath)
├── 2. Idempotency check:
│      if target already bind-mounted → return OK
├── 3. For Block access type:
│      a. Create target file: touch ${req.TargetPath}
│      b. Bind mount: mount --bind ${device} ${req.TargetPath}
│
Output: NodePublishVolumeResponse{}
```

#### NodeUnpublishVolume

```
Input: volumeID, targetPath
│
├── 1. Unmount: umount ${req.TargetPath}
├── 2. Remove target file
│
Output: NodeUnpublishVolumeResponse{}
```

#### NodeGetInfo

**iSCSI-specific: Node ID is a composite value containing the initiator identity.**

```
Input: (none)
│
├── 1. Read initiator IQN:
│      iqn = parse(/etc/iscsi/initiatorname.iscsi)
├── 2. Get hostname:
│      host = os.Hostname()
├── 3. Get storage network IP (configurable interface):
│      ip = getInterfaceIP(config.ISCSI.StorageInterface)
│
Output: NodeGetInfoResponse{
    NodeId: fmt.Sprintf("%s;%s;%s", host, iqn, ip),
    AccessibleTopology: {
        "topology.cinder-iscsi.csi.windriver.com/zone": config.Zone
    }
}
```

**Comparison with NFS and existing Cinder drivers:**

| Driver      | NodeId Format              | Source                                  |
|-------------|----------------------------|-----------------------------------------|
| Cinder CSI  | Nova instance UUID         | `metadata.GetInstanceID()` (Nova API)   |
| NFS-Cinder  | Hostname                   | `os.Hostname()`                         |
| iSCSI-Cinder| `hostname;iqn;ip`          | `/etc/iscsi/initiatorname.iscsi` + hostname + interface IP |

#### NodeGetVolumeStats

```
Input: volumeID, volumePath
│
├── 1. Get block device size via ioctl or sysfs
│      OR statfs(volumePath) for filesystem stats
│
Output: NodeGetVolumeStatsResponse{Usage: [bytes]}
```

#### NodeExpandVolume

```
Input: volumeID, volumePath
│
├── 1. No-op for iSCSI (expansion is controller-side only)
│      iSCSI clients see new LUN size after Cinder extend
│      (may need iscsiadm rescan session — future work)
│
Output: NodeExpandVolumeResponse{}
```

---

## 9. K8S CSI Sidecar Architecture

This section documents the K8S CSI sidecar components required for the iSCSI-Cinder CSI
driver, the calling chain for each CSI RPC, and the component interactions. See
[kubernetes-csi-architecture-reference.md](kubernetes-csi-architecture-reference.md) for
the full architecture reference.

### 9.1 Controller Deployment Sidecars

The iSCSI driver **requires `external-attacher`** — unlike the NFS driver which does not
need it. This is because `ControllerPublishVolume` performs an active operation (updating
the Cinder attachment connector) rather than just querying existing connection info.

| Sidecar Container | Purpose | Triggers CSI RPC |
|-------------------|---------|------------------|
| `external-provisioner` | Watches PVC objects; creates PV via CSI | `CreateVolume`, `DeleteVolume` |
| `external-attacher` | Watches `VolumeAttachment` CRs from AD Controller | `ControllerPublishVolume`, `ControllerUnpublishVolume` |
| `livenessprobe` | Health check for the CSI driver | `Probe` |

**Comparison with NFS driver controller sidecars:**

| NFS-Cinder Controller | iSCSI-Cinder Controller | Reason |
|------------------------|--------------------------|--------|
| `external-provisioner` | `external-provisioner` | Same — PVC lifecycle |
| *(not needed)* | `external-attacher` | iSCSI needs active per-pod attachment management |
| `livenessprobe` | `livenessprobe` | Same — health check |

### 9.2 Node DaemonSet Sidecars

| Sidecar Container | Purpose | Triggers CSI RPC |
|-------------------|---------|------------------|
| `node-driver-registrar` | Registers CSI driver with kubelet | (registration only) |
| `livenessprobe` | Health check | `Probe` |

### 9.3 K8S Calling Chain Reference

**PVC Lifecycle (provisioner-driven):**
```
PVC created → external-provisioner → CreateVolume → PV created, bound to PVC
PVC deleted → external-provisioner → DeleteVolume → PV deleted
```

**Pod Lifecycle (AD Controller + attacher-driven + kubelet-driven):**
```
Pod scheduled to node
  → AD Controller creates VolumeAttachment CR
  → external-attacher detects CR → ControllerPublishVolume
  → kubelet → NodeStageVolume → NodePublishVolume
  → Pod running

Pod deleted
  → kubelet → NodeUnpublishVolume → NodeUnstageVolume
  → AD Controller deletes VolumeAttachment CR
  → external-attacher detects deletion → ControllerUnpublishVolume
```

**Key observation for CDI multi-phase precopy:** The K8S `VolumeAttachment` CR lifetime
equals the pod lifetime. When a CDI stage pod completes, the full unpublish chain fires,
followed by the full publish chain when the next stage pod is scheduled. This naturally
triggers the attachment rotation (delete + recreate) in `ControllerUnpublishVolume`.

---

## 10. Build and Deployment

### 10.1 Makefile Changes

Add `cinder-iscsi-csi-plugin` to `IMAGE_NAMES` and `BUILD_CMDS`:

```makefile
IMAGE_NAMES ?= openstack-cloud-controller-manager \
               cinder-csi-plugin \
               cinder-nfs-csi-plugin \
               cinder-iscsi-csi-plugin \         # ← NEW
               k8s-keystone-auth \
               octavia-ingress-controller \
               manila-csi-plugin \
               barbican-kms-plugin \
               magnum-auto-healer

BUILD_CMDS  ?= openstack-cloud-controller-manager \
               cinder-csi-plugin \
               cinder-nfs-csi-plugin \
               cinder-iscsi-csi-plugin \          # ← NEW
               k8s-keystone-auth \
               octavia-ingress-controller \
               manila-csi-plugin \
               barbican-kms-plugin \
               magnum-auto-healer \
               client-keystone-auth
```

Add a **sanity test target**:

```makefile
test-cinder-iscsi-csi-sanity: work
	go test $(GIT_HOST)/$(BASE_DIR)/tests/sanity/cinder-iscsi
```

Add a **cross-build entry** in the `build-cross` target:

```makefile
CGO_ENABLED=0 gox -parallel=$(GOX_PARALLEL) \
  -output="_dist/{{.OS}}-{{.Arch}}/{{.Dir}}" -osarch='$(TARGETS)' \
  $(GOFLAGS) $(if $(TAGS),-tags '$(TAGS)',) -ldflags '$(GOX_LDFLAGS)' \
  $(GIT_HOST)/$(BASE_DIR)/cmd/cinder-iscsi-csi-plugin/
```

Build command:
```bash
make cinder-iscsi-csi-plugin
# or
make build-local-image-cinder-iscsi-csi-plugin
```

**Linting and unit tests** — same as NFS driver:
```bash
make check   # golangci-lint on all packages including cinder-iscsi
make unit    # go test -tags=unit on all packages
```

### 10.2 Dockerfile Stage

#### 10.2.1 Development Phase — Debian-Based Image (Current)

During development, the image uses a **simple Debian base** with iSCSI tools installed
directly via `apt`. This prioritizes fast iteration and easy debugging over image size:

```dockerfile
##
## cinder-iscsi-csi-plugin (development)
##
FROM ${DEBIAN_IMAGE} AS cinder-iscsi-csi-plugin

RUN clean-install open-iscsi mount util-linux

COPY --from=builder /build/cinder-iscsi-csi-plugin /bin/cinder-iscsi-csi-plugin
COPY --from=certs /etc/ssl/certs /etc/ssl/certs

LABEL name="cinder-iscsi-csi-plugin" \
      license="Apache Version 2.0" \
      maintainers="Kubernetes Authors" \
      description="Cinder iSCSI CSI Plugin" \
      distribution-scope="public" \
      summary="Cinder iSCSI CSI Plugin for iSCSI-backed Cinder volumes" \
      help="none"

CMD ["/bin/cinder-iscsi-csi-plugin"]
```

This gives the container `iscsiadm`, `iscsid`, `mount`, `umount`, `findmnt`, and a
full shell — useful for exec-ing into the container to troubleshoot iSCSI sessions
during development.

#### 10.2.2 Production Phase — 3-Step Distroless Build (TODO)

> **TODO:** Before GA release, migrate to the 3-step distroless build pattern used by
> `cinder-csi-plugin` to minimize image size and attack surface. This involves:
>
> 1. Create `tools/csi-iscsi-deps.sh` — extract only the iSCSI client binaries
>    (`iscsiadm`, `iscsid`, `mount`, `umount`, `findmnt`) and their shared library
>    dependencies into a `/dest` folder, following the `tools/csi-deps.sh` pattern.
> 2. Create `tools/csi-iscsi-deps-check.sh` — validate the extracted binaries work in a
>    distroless context.
> 3. Switch the Dockerfile to the 3-step pattern.
>
> **Note:** The iSCSI dependency set includes `iscsid` (daemon), which must be running
> in the node container. This may require the container to run `iscsid` as a background
> process or rely on the host's `iscsid` daemon. For initial implementation, the node
> DaemonSet uses the host's `iscsid` via `hostPID: true` and privileged access.

> **Design rationale:** Bundling iSCSI tools in the container image ensures consistent
> behavior across WRCP/WRC worker hosts regardless of host OS version. The node DaemonSet
> requires `privileged: true` and `mountPropagation: Bidirectional` for `iscsiadm` commands
> and block device access. Unlike the NFS driver (which needs `mount.nfs`), the iSCSI
> driver primarily needs `iscsiadm` — the `iscsid` daemon is expected to run on the host.

### 10.3 Kubernetes Manifests

The manifest structure mirrors `manifests/cinder-csi-plugin/` with iSCSI-specific changes:

**Controller Deployment** (`cinder-iscsi-csi-controllerplugin.yaml`):
- Image: `registry.k8s.io/provider-os/cinder-iscsi-csi-plugin:latest`
- Sidecars: `external-provisioner`, `external-attacher`, `livenessprobe`
- **YES `external-attacher` sidecar** — required for `ControllerPublishVolume` to update
  Cinder attachment connector
- ConfigMap volume: `driver-config` → `/etc/config/driver.conf`
- Secret volume: `cloud-config` → `/etc/config/cloud.conf`

**Node DaemonSet** (`cinder-iscsi-csi-nodeplugin.yaml`):
- Image: `registry.k8s.io/provider-os/cinder-iscsi-csi-plugin:latest`
- Sidecars: `node-driver-registrar`, `livenessprobe`
- Socket: `/var/lib/kubelet/plugins/cinder-iscsi.csi.windriver.com/csi.sock`
- Host mounts:
  - `/var/lib/kubelet/pods` (for bind mount propagation)
  - `/dev` (for iSCSI block device access)
  - `/sys` (for device discovery)
  - `/etc/iscsi` (for initiator name and iSCSI node DB)
  - `/var/lib/iscsi` (for persistent iSCSI node DB, send_targets, and session state)
  - `/run/lock/iscsi` (for iscsiadm locking)
- `privileged: true` with `mountPropagation: Bidirectional` — required for `iscsiadm`
  commands and block device bind mounts
- `hostPID: true` — required for `iscsiadm` to communicate with the host's `iscsid`
  daemon via its PID namespace (see [security implications](#hostpid-true-security-implications-and-alternatives) below)

**CSIDriver resource** (`csi-cinder-iscsi-driver.yaml`):
```yaml
apiVersion: storage.k8s.io/v1
kind: CSIDriver
metadata:
  name: cinder-iscsi.csi.windriver.com
  labels:
    app.kubernetes.io/part-of: wrc-migration
    app.kubernetes.io/component: csi-driver
spec:
  attachRequired: true          # ControllerPublishVolume needed for iSCSI target discovery
  podInfoOnMount: false
  volumeLifecycleModes:
    - Persistent                # Only persistent volumes — no ephemeral inline volumes
  # No fsGroupPolicy — this driver does not support filesystem mounts.
  # All PVCs must use volumeMode: Block. Filesystem (mount) requests are
  # rejected at the CSI RPC level (see §8.1 Block-Only Volume Mode Enforcement).
```

**Comparison with NFS driver manifest differences:**

| Manifest Aspect | NFS-Cinder | iSCSI-Cinder |
|-----------------|------------|--------------|
| Controller sidecars | `external-provisioner`, `livenessprobe` | `external-provisioner`, `external-attacher`, `livenessprobe` |
| `attachRequired` | `true` | `true` |
| Node host mounts | `/var/lib/kubelet/pods` only | `/dev`, `/sys`, `/etc/iscsi`, `/var/lib/iscsi`, `/run/lock/iscsi`, `/var/lib/kubelet/pods` |
| `hostPID` | No | Yes (for host `iscsid`) |
| `privileged` | Yes (NFS mount) | Yes (iscsiadm + /dev access) |

#### `hostPID: true` — Security Implications and Alternatives

The node DaemonSet sets `hostPID: true` so that `iscsiadm` inside the container can
communicate with the host's `iscsid` daemon. This has security implications:

**Why `hostPID: true` is needed:**
- `iscsiadm` communicates with `iscsid` via a Unix domain socket (`/run/iscsid.comm`)
  and verifies `iscsid`'s PID via `/run/iscsid.pid`
- Without host PID namespace sharing, the containerized `iscsiadm` cannot locate the
  host's `iscsid` process or communicate with it through the expected socket path
- iSCSI sessions are kernel-level constructs managed by `iscsid` — they must be created
  in the host's PID namespace to persist across container restarts

**Security implications:**
- Container processes can see **all host PIDs** via `/proc`, including other workload
  processes, system daemons, and kubelet itself
- Container could potentially signal (kill/stop) host processes if also running as
  privileged (which this container does)
- Process environment variables of host processes may be visible, potentially exposing
  secrets passed via environment variables to other pods

**Mitigations in place:**
- The node plugin container is a **purpose-built CSI driver** — no user-facing shell
  access in production (distroless image in production phase)
- The DaemonSet is deployed by cluster administrators, not tenants
- RBAC restricts who can exec into the pod
- The container only needs `iscsiadm` and `mount` — no reason to enumerate or signal
  other processes

**Alternatives considered:**

| Alternative | Trade-off | Verdict |
|---|---|---|
| **Run `iscsid` inside the container** | Eliminates `hostPID: true`. Container manages its own iSCSI daemon. However: iSCSI sessions would not survive container restart; requires careful lifecycle management of `iscsid` + `iscsiadm` within the same container; conflicts with host-level `iscsid` if both run. | Viable for Phase 2. Requires container entrypoint to start `iscsid` before the CSI driver. |
| **Mount only `/run/iscsid.comm` socket** | More targeted than `hostPID: true`. Mount the Unix socket directly into the container. `iscsiadm` can communicate with host `iscsid` without seeing all PIDs. | **Preferred alternative** — test in Phase 4 integration. If `iscsiadm` works without PID visibility (just socket access), remove `hostPID: true`. |
| **Use `nsenter` from privileged container** | Enter host PID namespace only for `iscsiadm` commands via `nsenter --target 1 --pid -- iscsiadm ...`. No persistent `hostPID: true`. | Adds complexity. Each `iscsiadm` call wraps in `nsenter`. Error handling becomes harder. |
| **Host `iscsid` via `hostPath` sockets only** | Mount `/run/iscsid.comm` and `/var/lib/iscsi` as hostPath volumes. Do NOT set `hostPID: true`. | Test first — if `iscsiadm` only needs the socket and DB files (not PID verification), this is the cleanest solution. |

**Recommendation:** Start with `hostPID: true` in Phase 3 (known-working). In Phase 4
integration testing, validate the `/run/iscsid.comm` socket-only approach. If `iscsiadm`
operates correctly without host PID namespace access, drop `hostPID: true` before GA.

### 10.4 StorageClass, PVC, and CDI Pod Specifications

The following YAML specifications are used by Blueprint to create Kubernetes resources
that trigger the CSI driver. These are defined in the
[design doc §7.5](iscsi-backed-cinder-volume-for-wrcp-migration.md#75-storageclass-and-pvc-definition)
and
[§7.6](iscsi-backed-cinder-volume-for-wrcp-migration.md#76-cdi-importer-pod-specification).

**StorageClass:**

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: cinder-iscsi-migration
provisioner: cinder-iscsi.csi.windriver.com
parameters:
  type: pure-iscsi       # Cinder volume type (must be iSCSI-backed)
  # OR: type: lvmdriver-1  (for devstack/LVM)
  availability: nova     # Target availability zone
reclaimPolicy: Delete    # PVC deletion triggers DeleteVolume → attachment cleanup
volumeBindingMode: Immediate
```

The `provisioner` field must exactly match the driver name returned by `GetPluginInfo`.
The `type` parameter is passed to `CreateVolume` as `req.Parameters["type"]` and forwarded
to `Cinder POST /v3/volumes` as the `volume_type` field.

**PVC (created by Blueprint):**

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: migration-${VM_NAME}-vda-pvc
  namespace: default
  labels:
    app: containerized-data-importer
    migration.wrc.windriver.com/volume: "migration-${VM_NAME}-vda"
spec:
  accessModes:
    - ReadWriteOnce          # SINGLE_NODE_WRITER — iSCSI exclusive access
  volumeMode: Block          # Raw block device, not filesystem
  resources:
    requests:
      storage: ${DISK_SIZE}Gi
  storageClassName: cinder-iscsi-migration
```

Key observations:
- `volumeMode: Block` — CDI importer writes directly to the block device (`/dev/cdi-block-volume`)
- `accessModes: ReadWriteOnce` — maps to CSI `SINGLE_NODE_WRITER` (iSCSI target is per-initiator)
- `storageClassName` — binds PVC to the iSCSI-Cinder CSI driver's StorageClass

**CDI Importer Pod:**

The CDI importer pod spec is identical regardless of whether the backend is NFS or iSCSI — the CSI
driver abstracts the storage protocol. The pod sees `/dev/cdi-block-volume` as a raw block device.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: importer-migration-${VM_NAME}-vda
spec:
  restartPolicy: OnFailure
  containers:
    - name: importer
      image: registry.local:9001/quay.io/kubevirt/cdi-importer:v1.58.0
      args: ["-v=1", "import", "--insecure=true"]
      env:
        - name: IMPORTER_SOURCE
          value: vddk                              # V2O: VDDK; O2O: NBD
        - name: IMPORTER_ENDPOINT
          value: https://${VCENTER_IP}             # V2O: vCenter; O2O: NBD export
        - name: IMPORTER_THUMBPRINT
          value: ${VCENTER_THUMBPRINT}
        - name: IMPORTER_CURRENTCHECKPOINT
          value: ""                                # Full copy or checkpoint ID
        - name: IMPORTER_PREVIOUSCHECKPOINT
          value: ""                                # Previous checkpoint for delta
      volumeDevices:
        - name: cdi-data-vol
          devicePath: /dev/cdi-block-volume        # Block device seen by CDI
  volumes:
    - name: cdi-data-vol
      persistentVolumeClaim:
        claimName: migration-${VM_NAME}-vda-pvc    # References PVC above
```

The CSI RPC flow triggered by this pod:
1. PVC creation → `external-provisioner` → `CreateVolume` (Cinder volume + reserved attachment)
2. Pod scheduled → AD Controller → `external-attacher` → `ControllerPublishVolume` (attachment connector update → iSCSI target info)
3. kubelet → `NodeStageVolume` (iscsiadm login) → `NodePublishVolume` (bind mount)
4. Pod runs CDI data transfer
5. Pod exits → `NodeUnpublishVolume` → `NodeUnstageVolume` (iscsiadm logout) → `ControllerUnpublishVolume` (attachment rotation)

### 10.5 Helm Chart

A new Helm chart `charts/cinder-iscsi-csi-plugin/` follows the existing chart pattern:

```yaml
# charts/cinder-iscsi-csi-plugin/values.yaml (excerpt)
csi:
  plugin:
    image:
      repository: registry.k8s.io/provider-os/cinder-iscsi-csi-plugin
      tag: latest
    iscsi:
      enableMultipath: false
      chapAuthEnabled: true
      loginTimeout: 30
      deviceWaitTimeout: 30
      iscsiInterface: default
      storageInterface: ""
    volume:
      createTimeout: 300
      detachTimeout: 120
      defaultVolumeType: ""
      metadataPrefix: csi
  attacher:
    image:
      repository: registry.k8s.io/sig-storage/csi-attacher
      tag: v4.7.0
  provisioner:
    image:
      repository: registry.k8s.io/sig-storage/csi-provisioner
      tag: v5.1.0
  nodeDriverRegistrar:
    image:
      repository: registry.k8s.io/sig-storage/csi-node-driver-registrar
      tag: v2.12.0
  livenessprobe:
    image:
      repository: registry.k8s.io/sig-storage/livenessprobe
      tag: v2.14.0
storageClass:
  enabled: true
  name: cinder-iscsi-migration
  provisioner: cinder-iscsi.csi.windriver.com
  parameters:
    type: pure-iscsi
    availability: nova
  reclaimPolicy: Delete
  volumeBindingMode: Immediate
```

### 10.6 Metrics

The iSCSI-Cinder CSI driver integrates with the existing `pkg/metrics/` framework:

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `cinder_iscsi_csi_operation_duration_seconds` | Histogram | `operation` | Latency of CSI RPC operations |
| `cinder_iscsi_csi_operations_total` | Counter | `operation` | Total number of CSI RPC calls |
| `cinder_iscsi_csi_operation_errors_total` | Counter | `operation` | Total number of failed CSI RPC calls |
| `cinder_iscsi_csi_openstack_api_request_duration_seconds` | Histogram | `request` | Latency of OpenStack API calls |
| `cinder_iscsi_csi_openstack_api_requests_total` | Counter | `request` | Total OpenStack API calls |
| `cinder_iscsi_csi_openstack_api_request_errors_total` | Counter | `request` | Total failed OpenStack API calls |

Possible `operation` values: `create_volume`, `delete_volume`, `controller_publish_volume`,
`controller_unpublish_volume`, `node_stage_volume`, `node_unstage_volume`,
`node_publish_volume`, `node_unpublish_volume`, `create_attachment`,
`update_attachment_connector`, `delete_attachment`.

Possible `request` values: `volume_create`, `volume_get`, `volume_delete`,
`volume_extend`, `volume_metadata_set`, `volume_metadata_delete`,
`attachment_create`, `attachment_update`, `attachment_complete`,
`attachment_get`, `attachment_delete`.

### 10.7 Release Procedure Alignment

The iSCSI-Cinder CSI driver must integrate with the existing release process documented
in [docs/release-procedure.md](../../release-procedure.md):

1. **Version bumping:** `hack/bump-release.sh` performs string replacement across
   `docs/manifests/tests/examples` directories. New manifests and charts under
   `manifests/cinder-iscsi-csi-plugin/` and `charts/cinder-iscsi-csi-plugin/` must use
   consistent version strings.

2. **Helm chart bumping:** `hack/bump-charts.sh` updates chart versions. The new chart
   `charts/cinder-iscsi-csi-plugin/` must follow the version convention
   (`appVersion: 1.XX.Y`, `version: 2.XX.Y`).

3. **Image promotion:** After tagging a release, staging images are built at
   `gcr.io/k8s-staging-provider-os/cinder-iscsi-csi-plugin`. The image digest must be
   added to
   [`images.yaml`](https://github.com/kubernetes/k8s.io/blob/main/registry.k8s.io/images/k8s-staging-provider-os/images.yaml)
   using `hack/release-image-digests.sh`.

4. **CI jobs:** A new CI job must be added for the iSCSI-Cinder CSI driver to
   [`test-infra`](https://github.com/kubernetes/test-infra/tree/master/config/jobs/kubernetes/cloud-provider-openstack).

5. **Sidecar container versions:** Sidecar images (`external-provisioner`,
   `external-attacher`, `node-driver-registrar`, `livenessprobe`) in both manifests and
   Helm charts should be bumped in sync with Kubernetes releases.

---

## 11. Development Phases

### Phase 1 — Scaffold and Interface

**Goal:** Establish the new package structure, interfaces, and a buildable binary.

| Task | Description                                        | Files                                      |
|------|----------------------------------------------------|--------------------------------------------|
| 1.1  | Create `pkg/csi/cinder-iscsi/` package skeleton    | `driver.go`, `identityserver.go`           |
| 1.2  | Copy gRPC server infrastructure                    | `server.go`, `utils.go`                    |
| 1.3  | Define `IOpenStackISCSI` interface                 | `openstack/openstack.go`                   |
| 1.4  | Define iSCSI config structs                        | `openstack/openstack.go`                   |
| 1.5  | Create binary entry point                          | `cmd/cinder-iscsi-csi-plugin/main.go`      |
| 1.6  | Add Makefile/Dockerfile entries                    | `Makefile`, `Dockerfile`                   |
| 1.7  | Create mock for `IOpenStackISCSI`                  | `openstack/openstack_mock.go`              |
| 1.8  | Verify build: `make cinder-iscsi-csi-plugin`       |                                            |

**Deliverable:** Binary that starts, registers identity server, listens on unix socket.

### Phase 2 — Controller Service (Core)

**Goal:** Implement CreateVolume, DeleteVolume, ControllerPublishVolume, ControllerUnpublishVolume
with Cinder v3 attachment lifecycle.

| Task | Description                                        | Files                                          |
|------|----------------------------------------------------|--------------------------------------------|
| 2.1  | Implement Cinder v3 attachment operations          | `openstack/openstack_attachments.go`           |
| 2.2  | Implement volume metadata operations               | `openstack/openstack_volumes.go`               |
| 2.3  | Implement iSCSI connection_info parser             | `connectioninfo.go`                            |
| 2.4  | Implement `CreateVolume` (volume + reserved attachment) | `controllerserver.go`                     |
| 2.5  | Implement `DeleteVolume` (attachment cleanup + release) | `controllerserver.go`                     |
| 2.6  | Implement `ControllerPublishVolume` (connector update)  | `controllerserver.go`                     |
| 2.7  | Implement `ControllerUnpublishVolume` (delete + recreate) | `controllerserver.go`                   |
| 2.8  | Unit tests with mock                                     | `controllerserver_test.go`                |

**Deliverable:** Controller plugin creates iSCSI-backed Cinder volumes with v3 attachments.

### Phase 3 — Node Service (iSCSI Login/Logout)

**Goal:** Implement NodeStageVolume (iSCSI login) and NodePublishVolume (bind mount).

| Task | Description                                        | Files                                          |
|------|----------------------------------------------------|--------------------------------------------|
| 3.1  | Implement iSCSI initiator helper (iscsiadm wrapper) | `iscsi.go`                                    |
| 3.2  | Implement `NodeGetInfo` (hostname;iqn;ip)           | `nodeserver.go`                                |
| 3.3  | Implement `NodeStageVolume` (discovery + CHAP + login) | `nodeserver.go`                            |
| 3.4  | Implement `NodeUnstageVolume` (logout + cleanup)    | `nodeserver.go`                                |
| 3.5  | Implement `NodePublishVolume` (bind mount block)    | `nodeserver.go`                                |
| 3.6  | Implement `NodeUnpublishVolume` (umount)            | `nodeserver.go`                                |
| 3.7  | Implement `NodeGetVolumeStats` (block device size)  | `nodeserver.go`                                |
| 3.8  | Unit tests                                          | `nodeserver_test.go`                           |

**Deliverable:** Full CSI driver — volumes can be provisioned and connected via iSCSI on pods.

### Phase 4 — Integration and E2E

**Goal:** Deploy to real OpenStack (DevStack with LVM iSCSI), run CSI sanity and E2E tests.

| Task | Description                                        | Files                                         |
|------|----------------------------------------------------|--------------------------------------------|
| 4.1  | Create Kubernetes manifests                        | `manifests/cinder-iscsi-csi-plugin/`          |
| 4.2  | Create Helm chart                                  | `charts/cinder-iscsi-csi-plugin/`             |
| 4.3  | CSI sanity tests                                   | `tests/sanity/cinder-iscsi/`                  |
| 4.4  | E2E CI script + Ansible playbook                   | `tests/ci-csi-cinder-iscsi-e2e.sh`, `tests/playbooks/test-csi-cinder-iscsi-e2e.yaml` |
| 4.5  | E2E test: provision → iSCSI login → read/write → delete | (within playbook)                       |
| 4.6  | E2E test: attachment rotation (CDI stage simulation)| (within playbook)                            |
| 4.7  | E2E test: CHAP authentication                      | (within playbook)                             |
| 4.8  | Documentation                                      | `docs/cinder-csi-plugin/migration/`           |

**E2E Testing Convention:** Same as NFS driver — Ansible playbooks orchestrated by bash
scripts. The CI provisions a VM with DevStack (LVM iSCSI backend, `lvmdriver-1` volume
type), installs k3s, deploys the CSI driver, and runs the tests. The iSCSI E2E tests
additionally validate `iscsiadm` login/logout, CHAP authentication, and block device
discovery via `/dev/disk/by-path/`.

**Deliverable:** Production-ready iSCSI-Cinder CSI driver with tests and docs.

### Phase 5 — CDI Multi-Phase Precopy

**Goal:** Integrate with CDI importer for V2O/O2O migration workflows.

| Task | Description                                        | Notes                                         |
|------|----------------------------------------------------|--------------------------------------------|
| 5.1  | CDI DataVolume with iSCSI-Cinder StorageClass      | CDI uses CSI to provision PVC                 |
| 5.2  | Multi-phase precopy: initial full copy + incremental | CDI importer handles data copy phases        |
| 5.3  | Attachment rotation between CDI stages              | Validates delete + recreate pattern across workers |
| 5.4  | Final sync and cutover                              | Stop source VM, final delta, start dest       |
| 5.5  | Driver injection (virt-v2v-in-place helper pod)     | Same PVC, CSI mounts iSCSI volume into helper |
| 5.6  | Volume release + VM creation                       | PVC delete → DeleteVolume → available → server create |
| 5.7  | E2E: Full V2O migration workflow                   | VMware → CDI VDDK → iSCSI → OpenStack VM     |
| 5.8  | E2E: Full O2O migration workflow                   | OpenStack → NBD → iSCSI → OpenStack VM       |

**Deliverable:** Complete migration pipeline with iSCSI-backed storage.

---

## Appendix A: Key Differences — Existing Cinder CSI vs NFS-Cinder CSI vs iSCSI-Cinder CSI

```
Existing Cinder CSI                NFS-Cinder CSI                    iSCSI-Cinder CSI
─────────────────                  ──────────────                    ────────────────
cinder.csi.openstack.org           cinder-nfs.csi.windriver.com      cinder-iscsi.csi.windriver.com
Block device (iSCSI/FC via Nova)   NFS mount (via Shadow VM)         iSCSI login (via Cinder v3 attach)
Nova AttachVolume → /dev/vdb       Shadow VM → connection_info       Attachment connector → iSCSI target
FormatAndMount(ext4/xfs)           mount -t nfs                      iscsiadm --login → /dev/sdX
SINGLE_NODE_WRITER                 MULTI_NODE_MULTI_WRITER           SINGLE_NODE_WRITER
external-attacher sidecar          No external-attacher              external-attacher sidecar
NodeExpandVolume (block resize)    NodeExpandVolume no-op             NodeExpandVolume no-op
K8s node = Nova instance           K8s node = NFS client             K8s node = iSCSI initiator
ControllerUnpublish = Nova detach  ControllerUnpublish = no-op       ControllerUnpublish = delete+recreate
Volume lock = Nova attachment      Volume lock = Shadow VM           Volume lock = reserved attachment
Nova + Cinder dependency           Nova + Cinder dependency          Cinder only (no Nova)
Zero extra compute quota           1 Shadow VM per migration          Zero extra compute quota
```

## Appendix B: File-Level Reuse Decision Matrix

| Existing File                          | Reuse? | Strategy                                              |
|----------------------------------------|:------:|-------------------------------------------------------|
| `pkg/csi/csi.go`                       | ✅     | Import directly — shared constants and helpers         |
| `pkg/csi/cinder/server.go`            | 📋     | Copy to new package (unexported types)                 |
| `pkg/csi/cinder/utils.go`             | 📋     | Copy capability factories; rewrite server constructors |
| `pkg/csi/cinder/driver.go`            | 📝     | Rewrite — different name, capabilities, services       |
| `pkg/csi/cinder/controllerserver.go`  | 🆕     | New implementation — Cinder v3 attachment lifecycle     |
| `pkg/csi/cinder/nodeserver.go`        | 🆕     | New implementation — iSCSI login/logout chain          |
| `pkg/csi/cinder/identityserver.go`    | 📝     | Rewrite — different plugin capabilities                |
| `pkg/csi/cinder/openstack/openstack.go` | 📝  | New interface, new config structs, reuse config parsing |
| `pkg/csi/cinder/openstack/openstack_volumes.go` | 📋 | Partial copy — CreateVolume, GetVolume, add metadata ops |
| `pkg/csi/cinder/openstack/openstack_instances.go` | ❌ | Not needed — no Nova dependency                 |
| `pkg/csi/cinder-nfs/shadowvm.go`      | ❌     | Not needed — no Shadow VM                              |
| `pkg/csi/cinder-nfs/connectioninfo.go`| 📝     | Rewrite — iSCSI connection_info fields differ from NFS |
| `pkg/client/`                          | ✅     | Import directly — OpenStack auth                       |
| `pkg/metrics/`                         | ✅     | Import directly                                        |
| `pkg/util/metadata/`                   | ✅     | Import directly                                        |
| `pkg/util/mount/`                      | ✅     | Import directly — bind mount via `Mounter()`           |
| `pkg/util/errors/`                     | ✅     | Import directly                                        |
| `tools/csi-deps.sh`                   | 📝     | Adapt as `tools/csi-iscsi-deps.sh` — iSCSI utils only (production phase) |
| `tools/csi-deps-check.sh`             | 📝     | Adapt as `tools/csi-iscsi-deps-check.sh` (production phase) |

Legend: ✅ Import | 📋 Copy | 📝 Rewrite/Adapt | 🆕 New Implementation | ❌ Not Used
