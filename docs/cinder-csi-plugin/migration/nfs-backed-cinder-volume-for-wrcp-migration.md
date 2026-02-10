# Design Proposal: NFS-Backed Cinder Volume Direct Mount for WRCP/WRC Migration

> **Status**: Draft  
> **Version**: 0.1  
> **Date**: February 2026  
> **Authors**: WindRiver Migration Framework Team

---

## Table of Contents

- [Design Proposal: NFS-Backed Cinder Volume Direct Mount for WRCP/WRC Migration](#design-proposal-nfs-backed-cinder-volume-direct-mount-for-wrcpwrc-migration)
  - [Table of Contents](#table-of-contents)
  - [1. Problem Statement](#1-problem-statement)
    - [1.1 Current Architecture Limitations](#11-current-architecture-limitations)
    - [1.2 Network Isolation Problem](#12-network-isolation-problem)
  - [2. Proposed Solution](#2-proposed-solution)
    - [2.1 Key Insight: NFS-Backed Cinder Volumes](#21-key-insight-nfs-backed-cinder-volumes)
    - [2.2 Design Goals](#22-design-goals)
  - [3. Architecture Overview](#3-architecture-overview)
    - [3.1 Current Architecture (CSI-Based, Addon K8S Cluster)](#31-current-architecture-csi-based-addon-k8s-cluster)
    - [3.2 Proposed Architecture (NFS Direct Mount on WRCP/WRC Host)](#32-proposed-architecture-nfs-direct-mount-on-wrcpwrc-host)
    - [3.3 Architecture Comparison](#33-architecture-comparison)
  - [4. Detailed Design — CSI RPC Mapping](#4-detailed-design--csi-rpc-mapping)
    - [4.0 CSI Volume Lifecycle Reference](#40-csi-volume-lifecycle-reference)
    - [4.1 CSI Identity Service](#41-csi-identity-service)
    - [4.2 CSI Controller Service](#42-csi-controller-service)
      - [4.2.1 `CreateVolume` — Cinder Volume Provisioning + Shadow VM](#421-createvolume--cinder-volume-provisioning--shadow-vm)
      - [4.2.2 `ControllerPublishVolume` — NFS Connection Discovery](#422-controllerpublishvolume--nfs-connection-discovery)
      - [4.2.3 `ControllerUnpublishVolume` — NFS Connection Release](#423-controllerunpublishvolume--nfs-connection-release)
      - [4.2.4 `DeleteVolume` — Shadow VM Cleanup + Cinder Volume Deletion](#424-deletevolume--shadow-vm-cleanup--cinder-volume-deletion)
    - [4.3 CSI Node Service](#43-csi-node-service)
      - [4.3.1 `NodeStageVolume` — NFS Mount on WRCP/WRC Worker Host](#431-nodestagevolume--nfs-mount-on-wrcpwrc-worker-host)
      - [4.3.2 `NodePublishVolume` — Bind Mount Volume File into Pod](#432-nodepublishvolume--bind-mount-volume-file-into-pod)
      - [4.3.3 `NodeUnpublishVolume` — Remove Pod Bind Mount](#433-nodeunpublishvolume--remove-pod-bind-mount)
      - [4.3.4 `NodeUnstageVolume` — Unmount NFS Export](#434-nodeunstagevolume--unmount-nfs-export)
    - [4.4 End-to-End CSI RPC Call Sequence](#44-end-to-end-csi-rpc-call-sequence)
    - [4.5 Volume Finalization and VM Creation (Post-CSI)](#45-volume-finalization-and-vm-creation-post-csi)
  - [5. Component Flow](#5-component-flow)
    - [5.1 End-to-End Workflow](#51-end-to-end-workflow)
    - [5.2 Data Path Visualization](#52-data-path-visualization)
  - [6. Implementation Details](#6-implementation-details)
    - [6.1 NFS Volume Discovery Script](#61-nfs-volume-discovery-script)
    - [6.2 PV/PVC Definition for NFS Volume](#62-pvpvc-definition-for-nfs-volume)
    - [6.3 CDI Importer Pod Specification](#63-cdi-importer-pod-specification)
    - [6.4 Shadow VM Lifecycle Management](#64-shadow-vm-lifecycle-management)
  - [7. Network Architecture](#7-network-architecture)
    - [7.1 Network Topology](#71-network-topology)
    - [7.2 Network Requirements](#72-network-requirements)
  - [8. Prerequisites](#8-prerequisites)
  - [9. Risks and Mitigations](#9-risks-and-mitigations)
  - [10. Future Work](#10-future-work)
  - [11. References](#11-references)

---

## 1. Problem Statement

### 1.1 Current Architecture Limitations

The current V2O (VMware to OpenStack) warm migration design, as described in the [Native CBT Enhancement Proposal](../../../vm-migration-wrc/doc/native-cbt-for-VMware-to-OpenStack.md), relies on the **OpenStack Cinder CSI Driver** (`cinder.csi.openstack.org`) deployed on a **dedicated addon Kubernetes cluster** running on the target OpenStack environment.

This architecture requires:

1. **An addon K8S cluster on target OpenStack** — Control plane and worker VMs are managed by the target OpenStack so the CSI `ControllerPublishVolume` RPC can attach Cinder volumes as block devices (e.g., `/dev/vdb`) to the worker VMs via the Nova compute API.

2. **The CSI Node Service** on the addon K8S cluster handles the host-level device mapping, staging, and bind-mounting of the attached block device into the CDI importer pod.

The **CSI ControllerPublishVolume** flow depends on the K8S worker VM being a Nova instance on the same OpenStack so that Cinder can call `os-attach` / Nova `os-volume_attachments` to present the volume as a virtio block device to the VM.

### 1.2 Network Isolation Problem

In many production deployments, the **VMware vCenter** runs on a **management network** that is isolated from the OpenStack tenant VM networks. This creates a critical connectivity issue:

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                     NETWORK ISOLATION PROBLEM                               │
└─────────────────────────────────────────────────────────────────────────────┘

┌─────────────────────┐                        ┌─────────────────────────────┐
│  Management Network │                        │  OpenStack Tenant Network   │
│                     │         ✗ ISOLATED      │                             │
│  ┌───────────────┐  │      ◄─────────────►   │  ┌───────────────────────┐  │
│  │ VMware vCenter│  │                        │  │ Addon K8S Cluster     │  │
│  │ (VDDK Source) │  │                        │  │  ┌─────────────────┐  │  │
│  └───────────────┘  │                        │  │  │ CDI Importer Pod│  │  │
│                     │                        │  │  │ (Cannot reach   │  │  │
│                     │                        │  │  │  vCenter!)      │  │  │
│                     │                        │  │  └─────────────────┘  │  │
│                     │                        │  └───────────────────────┘  │
└─────────────────────┘                        └─────────────────────────────┘
```

The CDI importer pod running on the addon K8S cluster (which lives on the OpenStack tenant network) **cannot reach the vCenter VDDK endpoint** on the management network. This blocks the entire VDDK-based warm migration data path.

Meanwhile, the **WRCP/WRC infrastructure** (WindRiver Cloud Platform / WindRiver Conductor) typically has connectivity to **both** the management network (where vCenter resides) and the OpenStack storage network (where NFS backends are accessible). The WRC K8S cluster is the orchestration platform that already runs the migration framework.

**The fundamental constraint**: We need CDI importer pods to have access to both:
- The **vCenter/VDDK source** (management network)
- The **Cinder volume destination** (OpenStack storage)

Only the WRCP/WRC host environment satisfies both connectivity requirements.

---

## 2. Proposed Solution

### 2.1 Key Insight: NFS-Backed Cinder Volumes

When the OpenStack Cinder backend uses **NFS-based storage** (e.g., NetApp NFS), the underlying Cinder volume is simply a **file on an NFS export**. Unlike iSCSI or FC-backed volumes that require Nova attachment to present a block device, NFS volumes can be accessed by **any host that can mount the NFS export** — including WRCP/WRC worker nodes that are not managed by the target OpenStack.

The solution is to:

1. **Provision a Cinder volume** on the target OpenStack (via a Shadow VM to ensure proper NFS backend allocation)
2. **Query the NFS connection info** from the Cinder volume attachment properties
3. **Mount the NFS export directly** on the WRCP/WRC worker host
4. **Bind-mount the volume file** as a block device into the CDI importer pod running on the WRC K8S cluster
5. **CDI writes directly** to the Cinder volume file over NFS — the data lands on the final destination volume

This eliminates the need for the addon K8S cluster entirely for the data path, while the CDI importer pod on WRCP/WRC has full access to both vCenter (management network) and the Cinder volume (NFS storage network).

### 2.2 Design Goals

| Goal | Description |
|------|-------------|
| **Eliminate addon K8S cluster dependency** | Remove the requirement for a dedicated K8S cluster on target OpenStack for the migration data path |
| **Resolve network isolation** | CDI importer pods on WRCP/WRC have native access to the vCenter management network |
| **Direct-to-Cinder writes** | Maintain the zero-copy-at-cutover benefit — data written during migration lands directly in the Cinder volume |
| **Minimal cutover downtime** | Final delta sync + driver injection is all that remains in the cutover window |
| **Leverage existing NFS infrastructure** | Use the NFS backend that Cinder already provisions on (no additional storage required) |
| **Compatible with warm migration workflow** | Integrate with existing CDI multi-stage VDDK import and WRC blueprint orchestration |

---

## 3. Architecture Overview

### 3.1 Current Architecture (CSI-Based, Addon K8S Cluster)

```
┌──────────────────┐       ┌────────────────────────────────────────────────┐
│  VMware vCenter  │       │  Target OpenStack                              │
│  (Mgmt Network)  │       │                                                │
│  ┌────────────┐  │       │  ┌──────────────────────────────────────────┐  │
│  │ Source VM   │  │       │  │  Addon K8S Cluster (OpenStack VMs)      │  │
│  │ VMDK Disks  │──┼──✗───│  │                                        │  │
│  └────────────┘  │       │  │  CDI Importer Pod                      │  │
│                  │       │  │     │                                    │  │
└──────────────────┘       │  │     ▼ (writes to /dev/cdi-block-volume) │  │
                           │  │  ┌─────────────────┐                    │  │
                           │  │  │ Cinder Volume    │◄─ CSI Attach      │  │
                           │  │  │ (block device)   │   (Nova API)      │  │
                           │  │  └─────────────────┘                    │  │
                           │  └──────────────────────────────────────────┘  │
                           └────────────────────────────────────────────────┘

Problem: CDI Importer cannot reach vCenter on management network  ✗
```

### 3.2 Proposed Architecture (NFS Direct Mount on WRCP/WRC Host)

```
┌──────────────────┐       ┌────────────────────────────────────────────────┐
│  VMware vCenter  │       │  Target OpenStack                              │
│  (Mgmt Network)  │       │                                                │
│  ┌────────────┐  │       │  ┌────────────────────────────────────┐        │
│  │ Source VM   │  │       │  │  Cinder (NFS Backend e.g. NetApp) │        │
│  │ VMDK Disks  │  │       │  │                                    │        │
│  └────────────┘  │       │  │  NFS Export:                        │        │
│        │         │       │  │  192.168.57.105:/trident_pvc_xxx    │        │
│        │ VDDK    │       │  │    └── volume-ba833668-xxx (raw)    │        │
│        │         │       │  └────────────────────────────────────┘        │
└────────┼─────────┘       │               │                                │
         │                 └───────────────┼────────────────────────────────┘
         │                                 │ NFS Mount
         │                                 │
┌────────┼─────────────────────────────────┼────────────────────────────────┐
│        │           WRCP / WRC Platform   │                                │
│        │                                 │                                │
│  ┌─────┼─────────────────────────────────┼──────────────────────────┐     │
│  │     │         WRC K8S Cluster         │                          │     │
│  │     │                                 ▼                          │     │
│  │     │    ┌──────────────────────────────────────────────────┐    │     │
│  │     │    │  WRCP/WRC Worker Host                            │    │     │
│  │     │    │                                                  │    │     │
│  │     │    │  NFS Mount Point:                                │    │     │
│  │     │    │  /var/lib/cinder-nfs/vol-xxx/                    │    │     │
│  │     │    │    └── volume-ba833668-xxx  (raw file)           │    │     │
│  │     │    └──────────────────────────────────────────────────┘    │     │
│  │     │                       │ Bind Mount                         │     │
│  │     │                       ▼                                    │     │
│  │     │    ┌──────────────────────────────────────────────────┐    │     │
│  │     ▼    │  CDI Importer Pod                                │    │     │
│  │  ┌──────────┐                                               │    │     │
│  │  │ VDDK     │  Writes to → /dev/cdi-block-volume            │    │     │
│  │  │ Client   │  (bind-mounted from NFS volume file)          │    │     │
│  │  └──────────┘                                               │    │     │
│  │          │   └──────────────────────────────────────────────┘    │     │
│  └──────────┼──────────────────────────────────────────────────────┘     │
│             │                                                            │
│     ✓ Has access to vCenter (Mgmt Network)                               │
│     ✓ Has access to NFS storage (Storage Network)                        │
└──────────────────────────────────────────────────────────────────────────┘
```

### 3.3 Architecture Comparison

| Aspect | Current (Addon K8S + CSI) | Proposed (NFS Direct Mount on WRCP) |
|--------|---------------------------|-------------------------------------|
| **K8S cluster for data path** | Dedicated addon K8S on target OpenStack | WRC K8S cluster (existing) |
| **Volume attach mechanism** | CSI ControllerPublishVolume → Nova API | NFS mount on WRCP host |
| **Block device presentation** | virtio block device (`/dev/vdb`) | Volume file on NFS export, bind-mounted as block |
| **vCenter connectivity** | ✗ Blocked by network isolation | ✓ WRCP has management network access |
| **Storage network access** | ✓ Via Nova attachment | ✓ Via direct NFS mount from WRCP host |
| **Infrastructure overhead** | Requires addon K8S cluster VMs | No additional infrastructure |
| **Cinder volume types supported** | Any (iSCSI, FC, NFS, etc.) | **NFS-backed only** |
| **Data path** | CDI → block device → Cinder volume | CDI → NFS file → Cinder volume |
| **Cutover downtime** | Near-instantaneous (delta sync only) | Near-instantaneous (delta sync only) |

---

## 4. Detailed Design — CSI RPC Mapping

This section maps each migration phase to the corresponding **CSI Specification RPCs** (as defined in the [Container Storage Interface Spec](https://github.com/container-storage-interface/spec/blob/master/spec.md)). The new NFS-Cinder CSI driver (`cinder-nfs.csi.openstack.org`) implements the standard CSI gRPC services but replaces the Nova-attach block path with an NFS direct-mount path.

### 4.0 CSI Volume Lifecycle Reference

The CSI spec defines the following volume lifecycle for a dynamically provisioned volume with `STAGE_UNSTAGE_VOLUME` capability:

```
   CreateVolume +------------+ DeleteVolume
 +------------->|  CREATED   +--------------+
 |              +---+----^---+              |
 |       Controller |    | Controller       v
+++         Publish |    | Unpublish       +++
|X|          Volume |    | Volume          | |
+-+             +---v----+---+             +-+
                | NODE_READY |
                +---+----^---+
               Node |    | Node
              Stage |    | Unstage
             Volume |    | Volume
                +---v----+---+
                |  VOL_READY |
                +---+----^---+
               Node |    | Node
            Publish |    | Unpublish
             Volume |    | Volume
                +---v----+---+
                | PUBLISHED  |
                +------------+
```

The table below summarizes how each CSI RPC maps to OpenStack operations in the **existing Cinder CSI driver** vs. the **proposed NFS-Cinder CSI driver**:

| CSI RPC | Existing `cinder.csi.openstack.org` | Proposed `cinder-nfs.csi.openstack.org` |
|---------|--------------------------------------|------------------------------------------|
| **`CreateVolume`** | Cinder `POST /v3/volumes` | Cinder `POST /v3/volumes` + Shadow VM create (triggers NFS attachment record) |
| **`DeleteVolume`** | Cinder `DELETE /v3/volumes/{id}` | Shadow VM delete + Cinder `DELETE /v3/volumes/{id}` |
| **`ControllerPublishVolume`** | Nova `POST /v2/servers/{id}/os-volume_attachments` (block attach) | Query Cinder attachment → extract NFS export/path from `connection_info` properties |
| **`ControllerUnpublishVolume`** | Nova `DELETE /v2/servers/{id}/os-volume_attachments/{vid}` | No-op (NFS connection info is stateless; Shadow VM attachment persists) |
| **`NodeStageVolume`** | Discover `/dev/vdb` by serial → `FormatAndMount` to staging path | `mount -t nfs` NFS export → staging path on WRCP host |
| **`NodeUnstageVolume`** | `umount` staging path | `umount` NFS mount from staging path |
| **`NodePublishVolume`** | Bind mount staging path (or raw block device) → target path | Bind mount NFS volume file → target path as block device (`/dev/cdi-block-volume`) |
| **`NodeUnpublishVolume`** | `umount` target path | `umount` bind mount at target path |
| **`NodeGetInfo`** | Returns Nova instance ID + AZ topology | Returns WRCP host ID + topology label |
| **`ValidateVolumeCapabilities`** | Validates `SINGLE_NODE_WRITER` | Validates `SINGLE_NODE_WRITER`, `Block` access type, `nfs` driver_volume_type |

### 4.1 CSI Identity Service

The NFS-Cinder CSI driver registers itself with a distinct driver name and advertises the required capabilities.

**`GetPluginInfo`** returns:

| Field | Value |
|-------|-------|
| `name` | `cinder-nfs.csi.openstack.org` |
| `vendor_version` | `1.0.0` |

**`GetPluginCapabilities`** advertises:

| Capability | Type | Rationale |
|------------|------|-----------|
| `CONTROLLER_SERVICE` | PluginCapability.Service | Driver implements Controller RPCs for volume provisioning and NFS connection discovery |
| `VOLUME_ACCESSIBILITY_CONSTRAINTS` | PluginCapability.Service | Volumes are only accessible from nodes with NFS storage network access |

**`Probe`** verifies:
- OpenStack credentials are valid (Keystone token obtainable)
- NFS client utilities (`nfs-utils`) are installed on the node (for Node plugin)
- Target Cinder NFS backend is reachable

### 4.2 CSI Controller Service

The Controller plugin runs on the WRC K8S cluster (does **not** need to run on the target OpenStack). It communicates with the target OpenStack APIs (Keystone, Cinder, Nova) over HTTPS.

**Controller Service Capabilities** (`ControllerGetCapabilities`):

| Capability | Supported | Notes |
|------------|-----------|-------|
| `CREATE_DELETE_VOLUME` | Yes | Provisions Cinder NFS volumes via Shadow VM pattern |
| `PUBLISH_UNPUBLISH_VOLUME` | Yes | Discovers NFS connection info from Cinder attachment properties |
| `LIST_VOLUMES` | Yes | Lists Cinder volumes filtered by NFS type |
| `EXPAND_VOLUME` | Yes | Delegates to Cinder `os-extend` API |
| `CREATE_DELETE_SNAPSHOT` | No | Not required for migration use case |
| `CLONE_VOLUME` | No | Not required for migration use case |

#### 4.2.1 `CreateVolume` — Cinder Volume Provisioning + Shadow VM

**CSI Spec Reference:** *"A Controller Plugin MUST implement this RPC call if it has `CREATE_DELETE_VOLUME` controller capability. This RPC will be called by the CO to provision a new volume on behalf of a user."*

The `CreateVolume` RPC encapsulates both Cinder volume creation and the Shadow VM lifecycle needed to populate NFS attachment properties.

**Request → Response Mapping:**

| CSI Field | Source / Value |
|-----------|----------------|
| `req.Name` | `migration-${VM_NAME}-${DISK_LABEL}` (idempotency key) |
| `req.CapacityRange.RequiredBytes` | Source VM disk size |
| `req.Parameters["type"]` | `netapp-nfs` (StorageClass parameter) |
| `req.Parameters["availability"]` | Target AZ |
| `resp.Volume.VolumeId` | Cinder volume UUID |
| `resp.Volume.VolumeContext["nfs_export"]` | `192.168.57.105:/trident_pvc_xxx` |
| `resp.Volume.VolumeContext["nfs_volume_file"]` | `volume-ba833668-xxx` |
| `resp.Volume.VolumeContext["shadow_vm_id"]` | Shadow VM Nova instance UUID |

**Implementation Flow:**

```
┌────────────────────────────────────────────────────────────────────────────┐
│          CreateVolume RPC — Controller Plugin                              │
└────────────────────────────────────────────────────────────────────────────┘

  CSI Controller Plugin                     Target OpenStack
       │                                         │
       │  1. Idempotency check:                   │
       │     cloud.GetVolumesByName(req.Name)     │
       │     → Cinder GET /v3/volumes?name=...    │
       ├────────────────────────────────────────► │
       │                                          │
       │  2. Create Cinder volume:                │
       │     cloud.CreateVolume(                  │
       │       Name: req.Name,                    │
       │       Size: req.CapacityRange,           │
       │       VolumeType: req.Parameters["type"],│
       │       AZ: req.Parameters["availability"] │
       │     )                                    │
       │     → Cinder POST /v3/volumes            │
       ├────────────────────────────────────────► │
       │                                          │  Cinder provisions volume
       │  ◄── VOLUME_ID                           │  on NFS backend
       │                                          │
       │  3. Create Shadow VM (attachment trigger):│
       │     cloud.CreateServer(                  │
       │       Name: "shadow-"+req.Name,          │
       │       Flavor: m1.small,                  │
       │       Volume: VOLUME_ID,                 │
       │       Network: migration-network         │
       │     )                                    │
       │     → Nova POST /v2/servers              │
       ├────────────────────────────────────────► │
       │                                          │  Nova creates VM, attaches vol
       │                                          │  → Attachment record created
       │  4. Stop Shadow VM:                      │     with NFS connection_info
       │     cloud.StopServer(shadow_id)          │
       │     → Nova POST /v2/servers/{id}/action  │
       ├────────────────────────────────────────► │
       │                                          │
       │  5. Return CreateVolumeResponse:         │
       │     Volume.VolumeId = VOLUME_ID          │
       │     Volume.VolumeContext = {             │
       │       "shadow_vm_id": shadow_id,         │
       │       "nfs_export": (queried later       │
       │                      in ControllerPublish)│
       │     }                                    │
```

**Why a Shadow VM in CreateVolume?**

- Cinder's NFS driver populates the `volume attachment` properties (export path, volume filename, mount options) **only when a volume is attached to an instance via Nova**.
- The Shadow VM is a lightweight instance (`m1.small`) whose sole purpose is to trigger this attachment record creation.
- Once stopped, the Shadow VM consumes negligible resources but keeps the attachment record intact.
- This maps cleanly to the CSI `CreateVolume` semantic: *"provision a new volume"* — in this case, provisioning includes ensuring the NFS connection metadata is available.

#### 4.2.2 `ControllerPublishVolume` — NFS Connection Discovery

**CSI Spec Reference:** *"This RPC will be called by the CO when it wants to place a workload that uses the volume onto a node. The Plugin SHOULD perform the work that is necessary for making the volume available on the given node."*

In the existing Cinder CSI driver, this RPC calls `Nova os-volume_attachments` to attach a block device to the worker VM. In the NFS-Cinder driver, this RPC **discovers the NFS connection info** from the Cinder volume attachment record and returns it as `publish_context` — which the CO will forward to `NodeStageVolume`.

**Request → Response Mapping:**

| CSI Field | Source / Value |
|-----------|----------------|
| `req.VolumeId` | Cinder volume UUID |
| `req.NodeId` | WRCP worker hostname (from `NodeGetInfo`) |
| `req.VolumeContext["shadow_vm_id"]` | Shadow VM ID (from `CreateVolume`) |
| `resp.PublishContext["nfs_export"]` | `192.168.57.105:/trident_pvc_xxx` |
| `resp.PublishContext["nfs_volume_file"]` | `volume-ba833668-xxx` |
| `resp.PublishContext["nfs_mount_options"]` | `rw,hard,intr` |
| `resp.PublishContext["volume_format"]` | `raw` |
| `resp.PublishContext["driver_volume_type"]` | `nfs` |

**Implementation Flow:**

```
┌────────────────────────────────────────────────────────────────────────────┐
│      ControllerPublishVolume RPC — Controller Plugin                       │
└────────────────────────────────────────────────────────────────────────────┘

  CSI Controller Plugin                     Target OpenStack (Cinder API)
       │                                         │
       │  1. Validate volume exists:              │
       │     cloud.GetVolume(req.VolumeId)        │
       │     → Cinder GET /v3/volumes/{id}        │
       ├────────────────────────────────────────► │
       │                                          │
       │  2. Validate driver_volume_type:         │
       │     if volume.VolumeType != nfs-backed   │
       │       → return INVALID_ARGUMENT          │
       │                                          │
       │  3. Get attachment ID:                   │
       │     cloud.ListVolumeAttachments(          │
       │       VolumeId: req.VolumeId             │
       │     )                                    │
       │     → Cinder GET /v3/attachments?vol=... │
       ├────────────────────────────────────────► │
       │                                          │
       │  ◄── ATTACHMENT_ID                       │
       │                                          │
       │  4. Get attachment connection_info:      │
       │     cloud.GetVolumeAttachment(            │
       │       ATTACHMENT_ID                      │
       │     )                                    │
       │     → Cinder GET /v3/attachments/{id}    │
       ├────────────────────────────────────────► │
       │                                          │
       │  ◄── connection_info JSON:               │
       │      {                                   │
       │        "export": "192.168.57.105:/trident_pvc_xxx",
       │        "name": "volume-ba833668-xxx",    │
       │        "options": null,                  │
       │        "format": "raw",                  │
       │        "driver_volume_type": "nfs",      │
       │        "mount_point_base": "/opt/stack/data/cinder/mnt"
       │      }                                   │
       │                                          │
       │  5. Return ControllerPublishVolumeResponse:
       │     PublishContext = {                    │
       │       "nfs_export": connection_info.export,
       │       "nfs_volume_file": connection_info.name,
       │       "nfs_mount_options": "rw,hard,intr",
       │       "volume_format": "raw",            │
       │       "driver_volume_type": "nfs"        │
       │     }                                    │
```

**Key Design Decision:** The `publish_context` map returned here is passed by the CO (kubelet) to `NodeStageVolume` and `NodePublishVolume`. This is exactly how the CSI spec intends controller-to-node communication to work — the opaque `publish_context` carries the NFS connection details that the Node plugin needs to mount the volume.

#### 4.2.3 `ControllerUnpublishVolume` — NFS Connection Release

**CSI Spec Reference:** *"This RPC is a reverse operation of ControllerPublishVolume. It MUST be called after all NodeUnstageVolume and NodeUnpublishVolume on the volume are called and succeed."*

In the existing Cinder CSI driver, this calls `Nova DELETE /v2/servers/{id}/os-volume_attachments/{vid}`. In the NFS-Cinder driver, this is effectively a **no-op** because:

- The NFS connection info is stateless (it's just metadata from the attachment record).
- The Shadow VM attachment must persist until `DeleteVolume` is called.
- The actual NFS unmount happens in `NodeUnstageVolume` on the node side.

```
  CSI Controller Plugin
       │
       │  ControllerUnpublishVolume(req):
       │    - Validate volume exists
       │    - No-op: Shadow VM attachment stays intact
       │    - Return ControllerUnpublishVolumeResponse{}
       │
       │  The NFS mount cleanup is handled by NodeUnstageVolume.
       │  The Shadow VM cleanup is handled by DeleteVolume.
```

#### 4.2.4 `DeleteVolume` — Shadow VM Cleanup + Cinder Volume Deletion

**CSI Spec Reference:** *"A Controller Plugin MUST implement this RPC call if it has CREATE_DELETE_VOLUME controller capability. This RPC will be called by the CO to deprovision a volume."*

This RPC reverses `CreateVolume` — it cleans up the Shadow VM and deletes the Cinder volume.

```
┌────────────────────────────────────────────────────────────────────────────┐
│          DeleteVolume RPC — Controller Plugin                              │
└────────────────────────────────────────────────────────────────────────────┘

  CSI Controller Plugin                     Target OpenStack
       │                                         │
       │  1. Detach volume from Shadow VM:        │
       │     cloud.DetachVolume(shadow_vm_id,      │
       │                       req.VolumeId)      │
       │     → Nova DELETE .../os-volume_attachments
       ├────────────────────────────────────────► │
       │                                          │
       │  2. Delete Shadow VM:                    │
       │     cloud.DeleteServer(shadow_vm_id)     │
       │     → Nova DELETE /v2/servers/{id}       │
       ├────────────────────────────────────────► │
       │                                          │
       │  3. Delete Cinder volume:                │
       │     cloud.DeleteVolume(req.VolumeId)     │
       │     → Cinder DELETE /v3/volumes/{id}     │
       ├────────────────────────────────────────► │
       │                                          │
       │  4. Return DeleteVolumeResponse{}        │
```

**Note:** In the migration use case, `DeleteVolume` is typically **not** called after a successful migration — the volume is detached from the Shadow VM and then used to boot the target VM. `DeleteVolume` is called only for cleanup on migration failure/cancellation.

### 4.3 CSI Node Service

The Node plugin runs on each **WRCP/WRC worker host** where CDI importer pods may be scheduled. It handles the actual NFS mount/unmount and bind-mount operations on the host.

**Node Service Capabilities** (`NodeGetCapabilities`):

| Capability | Supported | Notes |
|------------|-----------|-------|
| `STAGE_UNSTAGE_VOLUME` | Yes | NFS mount/unmount at staging path (per-volume, per-node) |
| `GET_VOLUME_STATS` | Yes | Report NFS-based volume stats |
| `EXPAND_VOLUME` | No | NFS volume expansion is handled controller-side via Cinder |

#### 4.3.1 `NodeStageVolume` — NFS Mount on WRCP/WRC Worker Host

**CSI Spec Reference:** *"This RPC is called by the CO prior to the volume being consumed by any workloads on the node by NodePublishVolume. The Plugin SHALL assume that this RPC will be executed on the node where the volume will be used."*

In the existing Cinder CSI driver, `NodeStageVolume` discovers the local block device (`/dev/vdb`) by serial number and calls `FormatAndMount`. In the NFS-Cinder driver, it **mounts the NFS export** to the staging path.

**Request → Action Mapping:**

| CSI Field | Source / Value |
|-----------|----------------|
| `req.VolumeId` | Cinder volume UUID |
| `req.PublishContext["nfs_export"]` | `192.168.57.105:/trident_pvc_xxx` (from `ControllerPublishVolume`) |
| `req.PublishContext["nfs_volume_file"]` | `volume-ba833668-xxx` |
| `req.PublishContext["nfs_mount_options"]` | `rw,hard,intr` |
| `req.StagingTargetPath` | `/var/lib/kubelet/plugins/kubernetes.io/csi/pv/.../globalmount` |
| `req.VolumeCapability` | `Block` access type, `SINGLE_NODE_WRITER` |

**Implementation Flow:**

```
┌────────────────────────────────────────────────────────────────────────────┐
│          NodeStageVolume RPC — Node Plugin (WRCP Worker Host)              │
└────────────────────────────────────────────────────────────────────────────┘

  WRCP/WRC Worker Host
  ┌──────────────────────────────────────────────────────────────────────┐
  │                                                                      │
  │  1. Parse publish_context:                                           │
  │     nfs_export  = req.PublishContext["nfs_export"]                    │
  │     volume_file = req.PublishContext["nfs_volume_file"]               │
  │     mount_opts  = req.PublishContext["nfs_mount_options"]             │
  │                                                                      │
  │  2. Idempotency check:                                               │
  │     if staging_target_path already mounted → return OK               │
  │                                                                      │
  │  3. Create staging directory:                                        │
  │     mkdir -p ${req.StagingTargetPath}                                │
  │                                                                      │
  │  4. Mount NFS export to staging path:                                │
  │     mount -t nfs -o ${mount_opts} \                                  │
  │       ${nfs_export} \                                                │
  │       ${req.StagingTargetPath}                                       │
  │                                                                      │
  │  5. Verify volume file exists at staging path:                       │
  │     stat ${req.StagingTargetPath}/${volume_file}                     │
  │     → Confirm file size matches req.VolumeCapability size            │
  │                                                                      │
  │  6. Return NodeStageVolumeResponse{}                                 │
  │                                                                      │
  └──────────────────────────────────────────────────────────────────────┘

  Result:
  ┌────────────────────┐         NFS          ┌─────────────────────────┐
  │ WRCP Worker Host   │◄───────────────────►│ NFS Server              │
  │ staging_target_path│   192.168.57.x net  │ (NetApp / Cinder NFS)   │
  │  └── volume-xxx    │                      │  └── volume-xxx (raw)   │
  └────────────────────┘                      └─────────────────────────┘
```

**Comparison with existing Cinder CSI `NodeStageVolume`:**

| Step | Existing (`cinder.csi.openstack.org`) | NFS-Cinder (`cinder-nfs.csi.openstack.org`) |
|------|----------------------------------------|----------------------------------------------|
| Device discovery | `GetDevicePath()` → scan `/dev/disk/by-id/` for virtio serial | Parse `publish_context["nfs_export"]` |
| Mount type | `FormatAndMount(devicePath, stagingPath, fsType)` | `mount -t nfs -o opts nfs_export stagingPath` |
| Format required | Yes (ext4/xfs on raw block device) | No (NFS volume file is pre-allocated raw) |
| Dev path source | Nova block device attachment (`/dev/vdb`) | NFS network mount |

#### 4.3.2 `NodePublishVolume` — Bind Mount Volume File into Pod

**CSI Spec Reference:** *"This RPC is called by the CO when a workload that wants to use the specified volume is placed (scheduled) on a node. For volumes with an access type of block, the SP SHALL place the block device at target_path."*

In the existing Cinder CSI driver, `NodePublishVolume` bind-mounts the staged block device or mount point to the pod's target path. In the NFS-Cinder driver, it **bind-mounts the specific volume file** from the NFS staging path to the pod's `target_path` as a raw block device.

**Request → Action Mapping:**

| CSI Field | Source / Value |
|-----------|----------------|
| `req.VolumeId` | Cinder volume UUID |
| `req.StagingTargetPath` | NFS mount point (from `NodeStageVolume`) |
| `req.TargetPath` | `/var/lib/kubelet/pods/{uid}/volumeDevices/...` |
| `req.VolumeCapability` | `Block` access type |
| `req.PublishContext["nfs_volume_file"]` | `volume-ba833668-xxx` |

**Implementation Flow:**

```
┌────────────────────────────────────────────────────────────────────────────┐
│        NodePublishVolume RPC — Node Plugin (WRCP Worker Host)              │
└────────────────────────────────────────────────────────────────────────────┘

  WRCP/WRC Worker Host
  ┌──────────────────────────────────────────────────────────────────────┐
  │                                                                      │
  │  1. Determine volume file path:                                      │
  │     source = ${req.StagingTargetPath}/${publish_context.volume_file}  │
  │     target = ${req.TargetPath}   (e.g. /dev/cdi-block-volume in pod) │
  │                                                                      │
  │  2. Idempotency check:                                               │
  │     if target already bind-mounted → return OK                       │
  │                                                                      │
  │  3. For Block access type:                                           │
  │     a. Create target file if not exists:                             │
  │        touch ${req.TargetPath}                                       │
  │     b. Bind mount volume file to target:                             │
  │        mount --bind ${source} ${req.TargetPath}                      │
  │                                                                      │
  │  4. Return NodePublishVolumeResponse{}                               │
  │                                                                      │
  └──────────────────────────────────────────────────────────────────────┘

  Data Path (inside CDI Importer Pod):
  ┌──────────────────────────────────────────────────────────────────────┐
  │  CDI Importer Pod                                                    │
  │                                                                      │
  │  /dev/cdi-block-volume                                               │
  │    │  (bind mount from staging_target/volume-ba833668-xxx)           │
  │    │                                                                 │
  │    └─► VDDK Client writes VMDK data here                            │
  │        → write goes through bind mount                               │
  │        → lands on NFS-mounted volume file                            │
  │        → NFS write-through to Cinder NFS backend                    │
  │        → Cinder volume updated in-place                              │
  └──────────────────────────────────────────────────────────────────────┘
```

#### 4.3.3 `NodeUnpublishVolume` — Remove Pod Bind Mount

**CSI Spec Reference:** *"This RPC is a reverse operation of NodePublishVolume. This RPC MUST undo the work by the corresponding NodePublishVolume."*

```
  Node Plugin:
    1. umount ${req.TargetPath}     // remove bind mount
    2. Remove target file/dir
    3. Return NodeUnpublishVolumeResponse{}
```

#### 4.3.4 `NodeUnstageVolume` — Unmount NFS Export

**CSI Spec Reference:** *"This RPC is a reverse operation of NodeStageVolume. This RPC MUST undo the work by the corresponding NodeStageVolume."*

```
  Node Plugin:
    1. umount ${req.StagingTargetPath}   // unmount NFS export
    2. rmdir ${req.StagingTargetPath}
    3. Return NodeUnstageVolumeResponse{}
```

**Important:** Per the CSI spec, the CO guarantees that `NodeUnstageVolume` is called **only after** all `NodeUnpublishVolume` calls for the volume have returned success. This ensures the NFS mount is removed only after all pod bind mounts are cleaned up.

### 4.4 End-to-End CSI RPC Call Sequence

The following diagram shows the complete CSI RPC call sequence as orchestrated by the CO (kubelet + external-provisioner + external-attacher sidecars) during the migration lifecycle:

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│              CSI RPC Call Sequence — Migration Lifecycle                         │
└─────────────────────────────────────────────────────────────────────────────────┘

 CO (kubelet + sidecars)        Controller Plugin           Node Plugin
 ══════════════════════         ═════════════════           ═══════════
        │                              │                         │
  PVC Created                          │                         │
        │                              │                         │
  ┌─────┤  1. CreateVolume             │                         │
  │     ├─────────────────────────────►│                         │
  │     │                              │── Cinder create vol     │
  │     │                              │── Nova create shadow VM │
  │     │                              │── Nova stop shadow VM   │
  │     │◄─────────────────────────────┤                         │
  │     │  VolumeId + VolumeContext    │                         │
  │     │                              │                         │
  │  Pod Scheduled to WRCP Worker      │                         │
  │     │                              │                         │
  │     │  2. ControllerPublishVolume  │                         │
  │     ├─────────────────────────────►│                         │
  │     │                              │── Query attachment      │
  │     │                              │── Extract NFS conn info │
  │     │◄─────────────────────────────┤                         │
  │     │  PublishContext (NFS info)   │                         │
  │     │                              │                         │
  │     │  3. NodeStageVolume          │                         │
  │     ├──────────────────────────────┼────────────────────────►│
  │     │                              │                         │── mount -t nfs
  │     │                              │                         │   nfs_export →
  │     │                              │                         │   staging_path
  │     │◄─────────────────────────────┼─────────────────────────┤
  │     │                              │                         │
  │     │  4. NodePublishVolume        │                         │
  │     ├──────────────────────────────┼────────────────────────►│
  │     │                              │                         │── mount --bind
  │     │                              │                         │   vol_file →
  │     │                              │                         │   target_path
  │     │◄─────────────────────────────┼─────────────────────────┤
  │     │                              │                         │
  │ CDI Importer Pod runs:             │                         │
  │ ┌───────────────────────────┐      │                         │
  │ │ VDDK → /dev/cdi-block-vol │      │                         │
  │ │ (full copy + precopies)   │      │                         │
  │ └───────────────────────────┘      │                         │
  │     │                              │                         │
  │ Migration complete / cutover       │                         │
  │     │                              │                         │
  │     │  5. NodeUnpublishVolume      │                         │
  │     ├──────────────────────────────┼────────────────────────►│
  │     │                              │                         │── umount target
  │     │◄─────────────────────────────┼─────────────────────────┤
  │     │                              │                         │
  │     │  6. NodeUnstageVolume        │                         │
  │     ├──────────────────────────────┼────────────────────────►│
  │     │                              │                         │── umount NFS
  │     │◄─────────────────────────────┼─────────────────────────┤
  │     │                              │                         │
  │     │  7. ControllerUnpublishVol   │                         │
  │     ├─────────────────────────────►│                         │
  │     │                              │── no-op                 │
  │     │◄─────────────────────────────┤                         │
  │     │                              │                         │
  │  (Migration success path:          │                         │
  │   Volume NOT deleted — used for    │                         │
  │   target VM boot)                  │                         │
  │     │                              │                         │
  │  OR (Migration failure path):      │                         │
  │     │  8. DeleteVolume             │                         │
  │     ├─────────────────────────────►│                         │
  │     │                              │── Delete shadow VM      │
  │     │                              │── Delete Cinder volume  │
  │     │◄─────────────────────────────┤                         │
  └─────┤                              │                         │
```

### 4.5 Volume Finalization and VM Creation (Post-CSI)

After all CSI RPCs complete (volume is unpublished, unstaged, and controller-unpublished), the WRC blueprint orchestrator performs the final migration steps **outside of CSI**:

```
┌────────────────────────────────────────────────────────────────────────────┐
│       Post-CSI: Volume Finalization & VM Creation                          │
│       (Orchestrated by WRC Blueprint, not CSI RPCs)                        │
└────────────────────────────────────────────────────────────────────────────┘

  WRC Orchestrator                          Target OpenStack
       │                                         │
       │  1. virt-v2v-in-place                    │
       │     (inject virtio drivers into          │
       │      the volume via NFS re-mount         │
       │      or via Shadow VM)                   │
       │                                          │
       │  2. Detach volume from Shadow VM         │
       │     openstack server remove volume       │
       │     shadow-${VM_NAME} ${VOLUME_ID}       │
       ├────────────────────────────────────────► │
       │                                          │
       │  3. Delete Shadow VM                     │
       │     openstack server delete              │
       │     shadow-${VM_NAME}                    │
       ├────────────────────────────────────────► │
       │                                          │
       │  4. Set volume bootable                  │
       │     openstack volume set --bootable      │
       │     ${VOLUME_ID}                         │
       ├────────────────────────────────────────► │
       │                                          │
       │  5. Create target VM from volume         │
       │     openstack server create              │
       │     --volume ${VOLUME_ID}                │
       │     --flavor ${FLAVOR}                   │
       │     --network ${NETWORK}                 │
       │     target-${VM_NAME}                    │
       ├────────────────────────────────────────► │
       │                                          │  VM boots from Cinder volume
       │                                          │  ✓ Migration complete
```

---

## 5. Component Flow

### 5.1 End-to-End Workflow

```
┌────────────────────────────────────────────────────────────────────────────────┐
│                    END-TO-END NFS-BACKED MIGRATION FLOW                        │
└────────────────────────────────────────────────────────────────────────────────┘

 WRC Blueprint Orchestrator
 ═════════════════════════

 Stage 0: PRECHECK
    │  Validate CBT, inspect source VM
    │  Validate target OpenStack NFS backend
    ▼
 Stage 1: CLEANUP
    │  Remove previous migration artifacts
    │  Cleanup old NFS mounts on WRCP host
    ▼
 ┌─────────────────────────────────────────────────────────────┐
 │ NEW: NFS Volume Setup (replaces addon K8S + CSI)            │
 │                                                             │
 │  1. Create Cinder volume (--type netapp-nfs)                │
 │  2. Create Shadow VM (triggers NFS attachment properties)   │
 │  3. Stop Shadow VM                                          │
 │  4. Query volume attachment → extract NFS export info       │
 │  5. Mount NFS export on WRCP worker host                    │
 │  6. Create static PV/PVC pointing to volume file            │
 └─────────────────────────────────────────────────────────────┘
    │
    ▼
 Stage 2: FULL COPY
    │  Create initial VMware snapshot
    │  CDI importer on WRC K8S → VDDK full disk copy
    │  Writes to /dev/cdi-block-volume → NFS → Cinder volume
    ▼
 Stage 3: PRECOPY (Loop)
    │  Create incremental snapshot
    │  CDI importer → VDDK delta copy (changed blocks only)
    │  Writes delta to same NFS-backed Cinder volume
    │  Repeat until cutover triggered
    ▼
 Stage 4: CUTOVER
    │  Power off source VM
    │  Final snapshot + final delta transfer
    │  Unmount NFS on WRCP host
    ▼
 ┌─────────────────────────────────────────────────────────────┐
 │ NEW: Volume Finalization                                    │
 │                                                             │
 │  1. virt-v2v-in-place (inject virtio drivers)               │
 │  2. Detach volume from Shadow VM                            │
 │  3. Delete Shadow VM                                        │
 │  4. Mark volume bootable                                    │
 └─────────────────────────────────────────────────────────────┘
    │
    ▼
 Stage 5+7: CREATE VM (OpenStack)
    │  openstack server create --volume ${VOLUME_ID}
    │  VM boots from Cinder volume directly
    ▼
 ✓ MIGRATION COMPLETE
```

### 5.2 Data Path Visualization

```
VMware vCenter (Mgmt Network)          WRCP/WRC Worker Host              OpenStack Cinder (NFS Backend)
═══════════════════════════             ═══════════════════               ═════════════════════════════

┌───────────────┐                   ┌──────────────────────┐          ┌──────────────────────────┐
│  Source VM    │                   │  WRC K8S Cluster     │          │  NFS Server              │
│  ┌─────────┐ │    VDDK API       │                      │          │  (NetApp / NFS-Ganesha)  │
│  │ VMDK    │─┼───────────────►   │  ┌────────────────┐  │   NFS    │                          │
│  │ Disks   │ │                   │  │CDI Importer Pod│  │  Write   │  ┌──────────────────────┐│
│  └─────────┘ │                   │  │                │──┼────────► │  │volume-ba833668-xxx   ││
│              │                   │  │/dev/cdi-block- │  │          │  │(raw disk image)      ││
│              │                   │  │    volume      │  │          │  │                      ││
│              │                   │  └────────────────┘  │          │  │  = Cinder Volume     ││
│              │                   │                      │          │  └──────────────────────┘│
└──────────────┘                   └──────────────────────┘          └──────────────────────────┘
                                                                              │
                                                                              │ At cutover:
                                                                              ▼
                                                                    ┌──────────────────────────┐
                                                                    │  Target OpenStack VM     │
                                                                    │  boots from this volume  │
                                                                    │  (zero additional copy)  │
                                                                    └──────────────────────────┘
```

---

## 6. Implementation Details

### 6.1 NFS Volume Discovery Script

The following script demonstrates the NFS connection discovery from Cinder volume attachment properties:

```bash
#!/bin/bash
# =============================================================================
# NFS Volume Discovery for WRCP/WRC Migration
# =============================================================================

# --- Configuration ---
VM_NAME="${1:?Usage: $0 <VM_NAME> <DISK_SIZE_GB>}"
DISK_SIZE="${2:?Usage: $0 <VM_NAME> <DISK_SIZE_GB>}"
NFS_MOUNT_BASE="/var/lib/cinder-nfs"

# =============================================================================
# PHASE 1: Create Shadow VM with Target Volume
# =============================================================================

# 1.1 Create Cinder volume on target OpenStack (backed by NetApp NFS)
openstack volume create \
  --size ${DISK_SIZE} \
  --type netapp-nfs \
  --bootable \
  migration-${VM_NAME}-vda

VOLUME_ID=$(openstack volume show migration-${VM_NAME}-vda -f value -c id)

# 1.2 Create shadow VM to trigger volume attachment record
openstack server create \
  --flavor m1.small \
  --volume ${VOLUME_ID} \
  --network migration-network \
  --wait \
  shadow-${VM_NAME}

# 1.3 Stop the shadow VM (only needed for attachment record)
openstack server stop shadow-${VM_NAME}

# =============================================================================
# PHASE 2: Retrieve NFS Mount Information via Volume Attachment API
# =============================================================================

# 2.1 Get attachment ID
ATTACHMENT_ID=$(openstack volume attachment list \
  --volume-id ${VOLUME_ID} -f value -c ID | head -1)

# 2.2 Query attachment details
ATTACHMENT_JSON=$(openstack volume attachment show ${ATTACHMENT_ID} -f json)

# 2.3 Parse NFS export information from Properties
NFS_EXPORT=$(echo "${ATTACHMENT_JSON}" | jq -r '.Properties.export')
VOLUME_FILE=$(echo "${ATTACHMENT_JSON}" | jq -r '.Properties.name')
MOUNT_OPTIONS=$(echo "${ATTACHMENT_JSON}" | jq -r '.Properties.options // "rw,hard,intr"')
VOLUME_FORMAT=$(echo "${ATTACHMENT_JSON}" | jq -r '.Properties.format // "raw"')
DRIVER_TYPE=$(echo "${ATTACHMENT_JSON}" | jq -r '.Properties.driver_volume_type')

# 2.4 Verify this is an NFS volume
if [ "${DRIVER_TYPE}" != "nfs" ]; then
  echo "ERROR: Volume is not NFS-backed. driver_volume_type=${DRIVER_TYPE}"
  exit 1
fi

# 2.5 Derive NFS server and export path
NFS_SERVER=$(echo "${NFS_EXPORT}" | cut -d: -f1)
NFS_EXPORT_PATH=$(echo "${NFS_EXPORT}" | cut -d: -f2)
NFS_MOUNT_SOURCE="${NFS_SERVER}:${NFS_EXPORT_PATH}"

# =============================================================================
# PHASE 3: Mount NFS on WRCP/WRC Worker Host
# =============================================================================

MOUNT_POINT="${NFS_MOUNT_BASE}/migration-${VM_NAME}-vda"
mkdir -p "${MOUNT_POINT}"

mount -t nfs -o "${MOUNT_OPTIONS}" \
  "${NFS_MOUNT_SOURCE}" \
  "${MOUNT_POINT}"

VOLUME_PATH="${MOUNT_POINT}/${VOLUME_FILE}"

# Verify volume file exists and has expected size
ls -lh "${VOLUME_PATH}"

echo "========================================"
echo "NFS Volume Ready for CDI Import"
echo "========================================"
echo "Volume ID:     ${VOLUME_ID}"
echo "NFS Export:    ${NFS_EXPORT}"
echo "Volume File:   ${VOLUME_FILE}"
echo "Mount Point:   ${MOUNT_POINT}"
echo "Volume Path:   ${VOLUME_PATH}"
echo "Format:        ${VOLUME_FORMAT}"
echo "========================================"
```

### 6.2 PV/PVC Definition for NFS Volume

**Static PV and PVC** to expose the NFS-mounted Cinder volume file as a block device to the CDI importer pod:

```yaml
# =============================================================================
# PersistentVolume: points to the NFS-mounted Cinder volume file on the host
# =============================================================================
apiVersion: v1
kind: PersistentVolume
metadata:
  name: migration-${VM_NAME}-vda-pv
  labels:
    migration.wrc.windriver.com/volume: "migration-${VM_NAME}-vda"
    migration.wrc.windriver.com/type: "nfs-cinder"
spec:
  capacity:
    storage: ${DISK_SIZE}Gi
  volumeMode: Block
  accessModes:
    - ReadWriteOnce
  persistentVolumeReclaimPolicy: Retain
  # hostPath points to the volume FILE on the NFS mount
  # This file IS the Cinder volume — writes go through NFS to the backend
  local:
    path: /var/lib/cinder-nfs/migration-${VM_NAME}-vda/${VOLUME_FILE}
  nodeAffinity:
    required:
      nodeSelectorTerms:
        - matchExpressions:
            - key: kubernetes.io/hostname
              operator: In
              values:
                - ${WRCP_WORKER_HOSTNAME}
---
# =============================================================================
# PersistentVolumeClaim: CDI importer pod binds to this
# =============================================================================
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: migration-${VM_NAME}-vda-pvc
  namespace: default
  labels:
    app: containerized-data-importer
    migration.wrc.windriver.com/volume: "migration-${VM_NAME}-vda"
  annotations:
    cdi.kubevirt.io/storage.createdByController: "false"
    cdi.kubevirt.io/storage.import.source: vddk
spec:
  accessModes:
    - ReadWriteOnce
  volumeMode: Block
  resources:
    requests:
      storage: ${DISK_SIZE}Gi
  storageClassName: ""   # Empty string for static binding
  volumeName: migration-${VM_NAME}-vda-pv
```

### 6.3 CDI Importer Pod Specification

The CDI importer pod runs on the WRC K8S cluster, with access to both vCenter (management network) and the NFS-backed Cinder volume (via bind mount):

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: importer-migration-${VM_NAME}-vda
  namespace: default
  labels:
    app: containerized-data-importer
    app.kubernetes.io/component: storage
    app.kubernetes.io/managed-by: cdi-controller
    cdi.kubevirt.io: importer
    migration.wrc.windriver.com/type: nfs-cinder
spec:
  restartPolicy: OnFailure
  # Pin to the worker node where NFS is mounted
  nodeSelector:
    kubernetes.io/hostname: ${WRCP_WORKER_HOSTNAME}
  containers:
    - name: importer
      image: registry.local:9001/quay.io/kubevirt/cdi-importer:v1.58.0
      imagePullPolicy: IfNotPresent
      args:
        - "-v=1"
        - "import"
        - "--insecure=true"
      env:
        - name: IMPORTER_SOURCE
          value: vddk
        - name: IMPORTER_ENDPOINT
          value: https://${VCENTER_IP}    # Reachable from WRCP mgmt network
        - name: IMPORTER_CONTENTTYPE
          value: kubevirt
        - name: IMPORTER_IMAGE_SIZE
          value: "${IMAGE_SIZE_BYTES}"
        - name: IMPORTER_UUID
          value: ${VMWARE_VM_UUID}
        - name: IMPORTER_BACKING_FILE
          value: '${VMDK_BACKING_FILE}'
        - name: IMPORTER_THUMBPRINT
          value: "${VCENTER_THUMBPRINT}"
        - name: IMPORTER_ACCESS_KEY_ID
          valueFrom:
            secretKeyRef:
              name: vddk-credentials
              key: accessKeyId
        - name: IMPORTER_SECRET_KEY
          valueFrom:
            secretKeyRef:
              name: vddk-credentials
              key: secretKey
      # Block device access to the NFS-mounted Cinder volume
      volumeDevices:
        - name: cdi-data-vol
          devicePath: /dev/cdi-block-volume
      volumeMounts:
        - mountPath: /opt
          name: vddk-vol-mount
  initContainers:
    - name: vddk-side-car
      image: registry.local:9001/public/vddk-init:v1.0
      securityContext:
        allowPrivilegeEscalation: false
        runAsNonRoot: true
        runAsUser: 107
      volumeMounts:
        - mountPath: /opt
          name: vddk-vol-mount
  volumes:
    # PVC backed by static PV → NFS-mounted Cinder volume file
    - name: cdi-data-vol
      persistentVolumeClaim:
        claimName: migration-${VM_NAME}-vda-pvc
    - name: vddk-vol-mount
      emptyDir: {}
```

### 6.4 Shadow VM Lifecycle Management

The Shadow VM is a temporary resource used only for Cinder volume attachment record creation. Its lifecycle:

```
Created ──► Running ──► Stopped ──► (Migration runs) ──► Volume Detached ──► Deleted
   │            │            │                                   │                │
   └ Triggers   └ Attachment └ Minimal resource               └ Volume freed   └ Cleanup
     volume       record       consumption                      for target VM
     attachment   populated
```

**Cleanup Script:**

```bash
# After migration is complete and target VM is created:

# 1. Unmount NFS from WRCP host
umount /var/lib/cinder-nfs/migration-${VM_NAME}-vda
rmdir /var/lib/cinder-nfs/migration-${VM_NAME}-vda

# 2. Delete K8S PV/PVC resources
kubectl delete pvc migration-${VM_NAME}-vda-pvc
kubectl delete pv migration-${VM_NAME}-vda-pv

# 3. Detach volume from Shadow VM
openstack server remove volume shadow-${VM_NAME} ${VOLUME_ID}

# 4. Delete Shadow VM
openstack server delete shadow-${VM_NAME}

# 5. Volume is now available for target VM creation
openstack server create --volume ${VOLUME_ID} ...
```

---

## 7. Network Architecture

### 7.1 Network Topology

```
┌───────────────────────────────────────────────────────────────────────────────┐
│                           NETWORK TOPOLOGY                                    │
└───────────────────────────────────────────────────────────────────────────────┘

  ┌─────────────────────────────────────────────────────────────────────────┐
  │                    Management Network (e.g., 10.10.0.0/24)              │
  │                                                                         │
  │   ┌──────────────┐          ┌──────────────┐         ┌──────────────┐   │
  │   │ VMware vCenter│          │ WRCP Control │         │ OpenStack    │   │
  │   │ 10.10.0.50   │          │ Plane        │         │ Controller   │   │
  │   │              │          │ 10.10.0.10   │         │ 10.10.0.20   │   │
  │   └──────────────┘          └──────┬───────┘         └──────────────┘   │
  │          ▲                         │                                     │
  │          │ VDDK API                │                                     │
  │          │                         │                                     │
  └──────────┼─────────────────────────┼─────────────────────────────────────┘
             │                         │
  ┌──────────┼─────────────────────────┼─────────────────────────────────────┐
  │          │    WRCP/WRC Internal     │   (e.g., 192.168.100.0/24)         │
  │          │                         │                                     │
  │          │    ┌────────────────────┴────────────────────┐                │
  │          │    │  WRC K8S Cluster                         │                │
  │          │    │                                          │                │
  │          │    │   ┌──────────────────────────────┐       │                │
  │          └────┼───┤  CDI Importer Pod            │       │                │
  │               │   │  ✓ Can reach vCenter (mgmt)  │       │                │
  │               │   │  ✓ Can reach NFS (storage)   │       │                │
  │               │   └──────────────────────────────┘       │                │
  │               │                    │                      │                │
  │               └────────────────────┼──────────────────────┘                │
  │                                    │ NFS I/O                               │
  └────────────────────────────────────┼───────────────────────────────────────┘
                                       │
  ┌────────────────────────────────────┼───────────────────────────────────────┐
  │                Storage Network     │    (e.g., 192.168.57.0/24)            │
  │                                    │                                       │
  │   ┌──────────────────┐        ┌────┴──────────────────┐                    │
  │   │ OpenStack Cinder │        │  NFS Server (NetApp)  │                    │
  │   │ NFS Backend      │◄──────►│  192.168.57.105       │                    │
  │   │                  │        │  :/trident_pvc_xxx     │                    │
  │   └──────────────────┘        └───────────────────────┘                    │
  │                                                                            │
  └────────────────────────────────────────────────────────────────────────────┘
```

### 7.2 Network Requirements

| Network Path | Protocol | Port | Purpose |
|-------------|----------|------|---------|
| WRCP Worker → vCenter | HTTPS | 443 | VDDK data transfer (CDI importer) |
| WRCP Worker → NFS Server | NFS | 2049 | Volume data writes (NFS mount) |
| WRCP Worker → NFS Server | RPC | 111 | NFS portmapper/rpcbind |
| WRC Orchestrator → OpenStack API | HTTPS | 5000, 8776, 8774 | Keystone, Cinder, Nova API calls |
| WRCP Worker → OpenStack Keystone | HTTPS | 5000 | Authentication (if CSI controller co-exists) |

---

## 8. Prerequisites

| Requirement | Details |
|-------------|---------|
| **OpenStack Cinder NFS Backend** | Cinder must be configured with an NFS-based volume type (e.g., NetApp NFS, NFS-Ganesha). The `driver_volume_type` must be `nfs`. |
| **NFS Network Accessibility** | WRCP/WRC worker hosts must have network connectivity to the NFS server on the storage network. |
| **NFS Client Packages** | WRCP/WRC worker hosts must have `nfs-utils` (RHEL/CentOS) or `nfs-common` (Ubuntu) installed. |
| **Management Network Access** | WRCP/WRC worker hosts must have connectivity to VMware vCenter on the management network for VDDK data transfer. |
| **OpenStack Credentials** | Valid OpenStack credentials with permissions for Cinder volume operations, Nova instance management, and volume attachment queries. |
| **WRC K8S Cluster** | Existing WRC K8S cluster with CDI installed and operational. |
| **Privileged Host Access** | Ability to run `mount` commands on WRCP/WRC worker hosts (for NFS mounting). |
| **Static PV Support** | WRC K8S cluster must support `local` PersistentVolumes with `volumeMode: Block`. |

---

## 9. Risks and Mitigations

| Risk | Impact | Likelihood | Mitigation |
|------|--------|------------|------------|
| **NFS performance under heavy write load** | Slower migration throughput compared to direct block device | Medium | Use NFS mount options optimized for throughput (`rsize=1048576,wsize=1048576,hard,intr`). NetApp NFS typically provides excellent performance. |
| **NFS mount failures during migration** | Migration interruption, data loss risk | Low | Use `hard` mount option to retry indefinitely. Implement health checks on NFS mount before each CDI stage. |
| **Shadow VM resource consumption** | Minor compute resource waste | Low | Shadow VM uses `m1.small` flavor and is stopped immediately. Negligible resource impact. |
| **Volume attachment record inconsistency** | NFS properties may not be queryable | Low | Verify attachment properties immediately after Shadow VM creation. Fail fast if `driver_volume_type != nfs`. |
| **Non-NFS Cinder backends** | Design does not support iSCSI/FC backends | Medium | Clearly document NFS-only limitation. Fall back to addon K8S + CSI approach for non-NFS backends. |
| **SELinux denials on NFS mount** | Volume file inaccessible from pod | Medium | Mount under `/var/lib/cinder-nfs/` or apply appropriate SELinux context (`svirt_sandbox_file_t`). |
| **Concurrent migration conflicts** | Multiple migrations mounting to same NFS export | Low | Use unique mount point per migration (`/var/lib/cinder-nfs/migration-${VM_NAME}-${DISK}/`). |
| **Data integrity on NFS write-through** | Partial writes if NFS client crashes | Low | NFS `hard` mount guarantees write completion. CDI checkpoint mechanism allows resume from last successful delta. |

---

## 10. Future Work

1. **Automation via WRC Blueprint Integration** — Integrate the NFS mount, PV/PVC creation, and Shadow VM lifecycle into the WRC migration blueprint as dedicated node templates, eliminating manual steps.

2. **Multi-Disk Support** — Extend the workflow to handle VMs with multiple disks, creating one Cinder volume + NFS mount + PV/PVC per disk.

3. **NFS Mount DaemonSet** — Deploy a privileged DaemonSet on WRC workers that handles NFS mount/unmount operations via a gRPC or REST API, removing the need for direct host SSH access.

4. **Custom CSI Driver for NFS-Cinder** — Build a thin CSI driver (`cinder-nfs.csi.wrc.windriver.com`) that encapsulates the Shadow VM + NFS discovery + mount logic behind standard CSI RPCs, enabling seamless CDI/DataVolume integration without static PV/PVC management.

5. **Support for Non-NFS Backends** — Investigate extending the approach to other Cinder backends (e.g., using `tgt`/iSCSI initiator on WRCP hosts for iSCSI-backed volumes).

6. **virt-v2v Driver Injection via NFS** — Perform virtio driver injection directly on the NFS-mounted volume file from the WRCP host, avoiding the need to re-mount or use the Shadow VM for this step.

---

## 11. References

- [Native CBT Enhancement Proposal for VMware to OpenStack](../../../vm-migration-wrc/doc/native-cbt-for-VMware-to-OpenStack.md)
- [VMware Warm Migration Solution Design](../../../vm-migration-wrc/doc/vmware-warm-migration-solution-design.md)
- [Migration Framework Installation - Performance Supplement](../../../vm-migration-wrc/doc/migration-framework-installation-performance-supplement.md)
- [OpenStack Cinder CSI Driver Documentation](../using-cinder-csi-plugin.md)
- [Kubernetes CSI Specification](https://github.com/container-storage-interface/spec)
- [CDI Multi-stage VDDK Import](https://github.com/kubevirt/containerized-data-importer)
- [OpenStack Volume Attachment API](https://docs.openstack.org/api-ref/block-storage/v3/#volume-attachments)
- [Kubernetes Local Persistent Volumes](https://kubernetes.io/docs/concepts/storage/volumes/#local)
