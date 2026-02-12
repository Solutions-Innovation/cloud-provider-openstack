# Design Proposal: NFS-Backed Cinder Volume Direct Mount for WRCP/WRC Migration

> **Status**: Draft  
> **Version**: 0.2  
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
    - [2.2 Supported Migration Use Cases](#22-supported-migration-use-cases)
    - [2.3 Design Goals](#23-design-goals)
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
      - [4.2.3 `ControllerUnpublishVolume` — No-Op (Shadow VM Persists)](#423-controllerunpublishvolume--no-op-shadow-vm-persists)
      - [4.2.4 `DeleteVolume` — Shadow VM Cleanup + Volume Release](#424-deletevolume--shadow-vm-cleanup--volume-release)
    - [4.3 CSI Node Service](#43-csi-node-service)
      - [4.3.1 `NodeStageVolume` — NFS Mount on WRCP/WRC Worker Host](#431-nodestagevolume--nfs-mount-on-wrcpwrc-worker-host)
      - [4.3.2 `NodePublishVolume` — Bind Mount Volume File into Pod](#432-nodepublishvolume--bind-mount-volume-file-into-pod)
      - [4.3.3 `NodeUnpublishVolume` — Remove Pod Bind Mount](#433-nodeunpublishvolume--remove-pod-bind-mount)
      - [4.3.4 `NodeUnstageVolume` — Unmount NFS Export](#434-nodeunstagevolume--unmount-nfs-export)
    - [4.4 Cinder Volume Status Lifecycle](#44-cinder-volume-status-lifecycle)
    - [4.5 End-to-End CSI RPC Call Sequence (CDI Multi-Phase Precopy)](#45-end-to-end-csi-rpc-call-sequence-cdi-multi-phase-precopy)
    - [4.6 Volume Finalization and VM Creation](#46-volume-finalization-and-vm-creation)
      - [4.6.1 Phase 1: Driver Injection (Pre-CSI Cleanup — PVC Still Exists)](#461-phase-1-driver-injection-pre-csi-cleanup--pvc-still-exists)
      - [4.6.2 Phase 2: Volume Release + VM Creation (Post-CSI)](#462-phase-2-volume-release--vm-creation-post-csi)
  - [5. Component Flow](#5-component-flow)
    - [5.1 End-to-End Workflow](#51-end-to-end-workflow)
      - [5.1.1 VMware → OpenStack (V2O) — CDI Multi-Phase Warm Migration](#511-vmware--openstack-v2o--cdi-multi-phase-warm-migration)
      - [5.1.2 OpenStack → OpenStack (O2O) — NBD Receiver + virsh blockcopy](#512-openstack--openstack-o2o--nbd-receiver--virsh-blockcopy)
    - [5.2 Data Path Visualization](#52-data-path-visualization)
  - [6. Implementation Details](#6-implementation-details)
    - [6.1 NFS Volume Discovery Script (Reference)](#61-nfs-volume-discovery-script-reference)
    - [6.2 StorageClass and PVC Definition](#62-storageclass-and-pvc-definition)
    - [6.3 CDI Importer Pod Specification](#63-cdi-importer-pod-specification)
    - [6.4 Shadow VM Lifecycle Management](#64-shadow-vm-lifecycle-management)
    - [6.5 Driver Configuration — ConfigMap and Secret](#65-driver-configuration--configmap-and-secret)
      - [6.5.1 Configuration File Format (`driver.conf`)](#651-configuration-file-format-driverconf)
      - [6.5.2 Go Config Struct](#652-go-config-struct)
      - [6.5.3 ConfigMap Manifest](#653-configmap-manifest)
      - [6.5.4 Secret Manifest (OpenStack Credentials)](#654-secret-manifest-openstack-credentials)
      - [6.5.5 Controller Deployment — Volume Mounts](#655-controller-deployment--volume-mounts)
      - [6.5.6 Node DaemonSet — Volume Mounts](#656-node-daemonset--volume-mounts)
      - [6.5.7 Configuration Validation and Defaults](#657-configuration-validation-and-defaults)
      - [6.5.8 StorageClass Parameter Overrides](#658-storageclass-parameter-overrides)
      - [6.5.9 Configuration Diagram](#659-configuration-diagram)
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

### 2.2 Supported Migration Use Cases

This design consolidates **two migration use cases** under a single CSI driver that owns the Shadow VM lifecycle:

| Use Case | Data Transfer Tool | Data Writer | NFS Mount Location | Description |
|----------|-------------------|-------------|-------------------|-------------|
| **VMware → OpenStack (V2O)** | CDI importer pod (VDDK multi-phase warm migration) | CDI pod on WRC K8S | WRCP/WRC worker host (via CSI Node RPCs) | CDI runs full copy + N precopy delta stages. Each stage may create/destroy importer pods. CSI must survive pod cycling without losing NFS metadata. |
| **OpenStack → OpenStack (O2O)** | NBD receiver pod + `virsh blockcopy` | NBD receiver pod on WRC K8S (writes to PVC via NBD) | WRCP/WRC worker host (via CSI Node RPCs) | Blueprint launches a long-running NBD receiver pod that mounts the PVC/PV (backed by this CSI driver) and exposes it via the NBD protocol. Source libvirt `virsh blockcopy` targets the NBD endpoint to mirror the source VM's vda to the Cinder volume. |

**Consolidation Principle:** The CSI driver's Controller plugin manages the Shadow VM lifecycle (create, stop, persist attachment, cleanup on `DeleteVolume`) identically for both use cases. Both use cases consume the Cinder volume via a PVC on the WRC K8S cluster — the CSI Node plugin mounts NFS on the WRCP worker and bind-mounts the volume file into the consumer pod. The difference is only in the **consumer pod**:

- **V2O**: CDI importer pod — multi-phase (pods cycle between full copy and precopy stages)
- **O2O**: NBD receiver pod — long-running single pod that exposes the volume via NBD protocol as the target for source libvirt `virsh blockcopy`

### 2.3 Design Goals

| Goal | Description |
|------|-------------|
| **Eliminate addon K8S cluster dependency** | Remove the requirement for a dedicated K8S cluster on target OpenStack for the migration data path |
| **Resolve network isolation** | CDI importer pods on WRCP/WRC have native access to the vCenter management network |
| **Consolidate Shadow VM ownership** | CSI driver owns the full Shadow VM lifecycle for both V2O and O2O use cases — no duplication in Blueprint |
| **Support CDI multi-phase precopy** | Shadow VM and NFS attachment must persist across CDI pod restarts between full copy and precopy stages |
| **Direct-to-Cinder writes** | Maintain the zero-copy-at-cutover benefit — data written during migration lands directly in the Cinder volume |
| **Minimal cutover downtime** | Final delta sync + driver injection is all that remains in the cutover window |
| **Leverage existing NFS infrastructure** | Use the NFS backend that Cinder already provisions on (no additional storage required) |
| **Clean volume handoff** | After PVC deletion, the Cinder volume is `available` with no attachments — ready for Blueprint to create target VM |
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
| **`DeleteVolume`** | Cinder `DELETE /v3/volumes/{id}` | Detach volume from Shadow VM + delete Shadow VM. Volume becomes `available` (not deleted). Cinder `DELETE` only on failure/cancellation. |
| **`ControllerPublishVolume`** | Nova `POST /v2/servers/{id}/os-volume_attachments` (block attach) | Query Cinder attachment → extract NFS export/path from `connection_info` properties. Idempotent — same attachment queried each CDI precopy stage. |
| **`ControllerUnpublishVolume`** | Nova `DELETE /v2/servers/{id}/os-volume_attachments/{vid}` | **No-op.** Shadow VM attachment persists across CDI precopy pod cycles. NFS unmount handled by `NodeUnstageVolume`. |
| **`NodeStageVolume`** | Discover `/dev/vdb` by serial → `FormatAndMount` to staging path | `mount -t nfs` NFS export → staging path on WRCP host |
| **`NodeUnstageVolume`** | `umount` staging path | `umount` NFS mount from staging path |
| **`NodePublishVolume`** | Bind mount staging path (or raw block device) → target path | Bind mount NFS volume file → target path as block device (`/dev/cdi-block-volume`) |
| **`NodeUnpublishVolume`** | `umount` target path | `umount` bind mount at target path |
| **`NodeGetInfo`** | Returns Nova instance ID + AZ topology | Returns WRCP host ID + topology label |
| **`ValidateVolumeCapabilities`** | Validates `SINGLE_NODE_WRITER` | Validates `SINGLE_NODE_WRITER`, `Block` access type; queries attachment `connection_info.driver_volume_type == "nfs"` |

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
       │  2b. Persist Shadow VM ID as             │
       │      Cinder volume metadata:             │
       │      cloud.SetVolumeMetadata(VOLUME_ID, {│
       │        "csi.shadow_vm_id": shadow_id,     │
       │      })                                  │
       │      → Cinder PUT /v3/volumes/{id}/metadata
       ├────────────────────────────────────────► │
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
       │  2. Get attachment ID:                   │
       │     cloud.ListVolumeAttachments(          │
       │       VolumeId: req.VolumeId             │
       │     )                                    │
       │     → Cinder GET /v3/attachments?vol=... │
       ├────────────────────────────────────────► │
       │                                          │
       │  ◄── ATTACHMENT_ID                       │
       │                                          │
       │  3. Get attachment connection_info:      │
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
       │  4. Validate driver_volume_type:         │
       │     if connection_info.driver_volume_type │
       │        != "nfs"                           │
       │       → return INVALID_ARGUMENT          │
       │     (Cinder NFS driver sets              │
       │      driver_volume_type = "nfs" —        │
       │      see NfsDriver.driver_volume_type    │
       │      in cinder/volume/drivers/nfs.py)    │
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

#### 4.2.3 `ControllerUnpublishVolume` — No-Op (Shadow VM Persists)

**CSI Spec Reference:** *"This RPC is a reverse operation of ControllerPublishVolume. It MUST be called after all NodeUnstageVolume and NodeUnpublishVolume on the volume are called and succeed."*

In the existing Cinder CSI driver, this calls `Nova DELETE /v2/servers/{id}/os-volume_attachments/{vid}`. In the NFS-Cinder driver, this RPC is a **no-op** — the Shadow VM and its volume attachment record must persist.

**Why No-Op?**

CDI warm migration uses **multi-phase precopy** (full copy → precopy 1 → precopy 2 → ... → cutover). Between each stage, the CDI importer pod exits and a new pod is created for the next stage. When a pod is deleted and no other pod references the PV on that node, the CO fires the full unmount + unpublish chain:

```
  CDI Stage N pod completes:
    → NodeUnpublishVolume    (umount bind mount)
    → NodeUnstageVolume      (umount NFS)
    → ControllerUnpublishVolume  ← FIRES HERE (between CDI stages)

  CDI Stage N+1 pod scheduled:
    → ControllerPublishVolume    ← FIRES AGAIN (needs NFS info)
    → NodeStageVolume            (mount NFS again)
    → NodePublishVolume          (bind mount again)
```

If `ControllerUnpublishVolume` were to detach the volume from the Shadow VM or delete the Shadow VM, then `ControllerPublishVolume` in the next stage would fail — there would be no attachment record to query for NFS connection info. Therefore, `ControllerUnpublishVolume` must be a no-op.

For the **O2O use case** (NBD receiver pod), the pod is long-running so `ControllerUnpublishVolume` fires only once (when the pod is deleted after blockcopy completes). The no-op behavior is equally correct here — cleanup is always deferred to `DeleteVolume`.

**Implementation:**

```
  CSI Controller Plugin
       │
       │  ControllerUnpublishVolume(req):
       │    1. Log: "No-op — Shadow VM attachment persists for
       │            potential CDI precopy re-publish"
       │    2. Return ControllerUnpublishVolumeResponse{}
       │
       │  NFS mount cleanup is handled by NodeUnstageVolume.
       │  Shadow VM cleanup is deferred to DeleteVolume (PVC deletion).
```

**Cinder Volume Status:** The Cinder volume remains `in-use` throughout the entire migration lifecycle (see [Section 4.4](#44-cinder-volume-status-lifecycle)). The Shadow VM holds the attachment, which acts as a lock preventing accidental deletion or double-attachment.

**StorageClass Definition:**

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: cinder-nfs-migration
provisioner: cinder-nfs.csi.openstack.org
parameters:
  type: netapp-nfs
  availability: nova
reclaimPolicy: Delete    # PVC deletion triggers DeleteVolume
volumeBindingMode: Immediate
```

#### 4.2.4 `DeleteVolume` — Shadow VM Cleanup + Volume Release

**CSI Spec Reference:** *"A Controller Plugin MUST implement this RPC call if it has CREATE_DELETE_VOLUME controller capability. This RPC will be called by the CO to deprovision a volume."*

This RPC is the **single cleanup point** for the entire migration lifecycle. It is called when the PVC is deleted (either after successful migration or on failure/cancellation). It performs Shadow VM cleanup and — depending on whether the migration succeeded or failed — either releases the volume as `available` or deletes it entirely.

**Behavior is controlled by Cinder volume metadata** (`csi.cleanupVolume`):

| Scenario | `csi.cleanupVolume` metadata | `DeleteVolume` behavior |
|----------|------------------------------|------------------------|
| **Migration success** (default) | Not set or `"false"` | Detach volume from Shadow VM → delete Shadow VM → volume becomes `available`. **Volume is NOT deleted.** Blueprint creates target VM from this volume. |
| **Migration failure / cancellation** | Blueprint sets `"true"` before deleting PVC | Detach from Shadow VM → delete Shadow VM → **delete Cinder volume**. Full cleanup. |

**Implementation Flow — Success Path (default):**

```
┌────────────────────────────────────────────────────────────────────────────┐
│    DeleteVolume RPC — Success Path (volume released as "available")         │
└────────────────────────────────────────────────────────────────────────────┘

  CSI Controller Plugin                     Target OpenStack
       │                                         │
       │  1. Read volume metadata:                │
       │     cloud.GetVolume(req.VolumeId)         │
       │     → Get properties["csi.shadow_vm_id"] │
       │     → Get properties["csi.cleanupVolume"]│
       ├────────────────────────────────────────► │
       │                                          │
       │  2. If volume not found:                 │
       │     → Return success (idempotent)        │
       │                                          │
       │  3. If Shadow VM exists:                 │
       │     a. Detach volume from Shadow VM:     │
       │        cloud.DetachVolume(shadow_vm_id,   │
       │                          req.VolumeId)   │
       │        → Nova DELETE .../os-volume_attachments
       ├────────────────────────────────────────► │
       │                                          │
       │     b. Wait for volume status:           │
       │        Poll until status == "available"  │
       │        (timeout: 120s)                   │
       │                                          │
       │     c. Delete Shadow VM:                 │
       │        cloud.DeleteServer(shadow_vm_id)   │
       │        → Nova DELETE /v2/servers/{id}     │
       ├────────────────────────────────────────► │
       │                                          │
       │  4. Check cleanup mode:                  │
       │     cleanupVolume = properties["csi.cleanupVolume"]
       │                                          │
       │     IF cleanupVolume == "true":          │
       │       → Delete Cinder volume             │
       │       cloud.DeleteVolume(req.VolumeId)   │
       │       → Cinder DELETE /v3/volumes/{id}   │
       │                                          │
       │     ELSE (default):                      │
       │       → Leave volume as "available"      │
       │       → Remove CSI metadata from volume  │
       │       cloud.DeleteVolumeMetadata(         │
       │         req.VolumeId,                    │
       │         ["csi.shadow_vm_id",             │
       │          "csi.cleanupVolume"])            │
       │                                          │
       │  5. Return DeleteVolumeResponse{}        │
```

**Volume Handoff to Blueprint:**

After `DeleteVolume` completes (success path), the Cinder volume is in `available` state with no attachments. The Blueprint can then:

```
  Blueprint (after PVC deletion):
    1. virt-v2v-in-place ${VOLUME_ID}    ← inject virtio drivers
    2. openstack volume set --bootable ${VOLUME_ID}
    3. openstack server create --volume ${VOLUME_ID} ...
    ✓ Target VM boots from the migration volume
```

**Failure/Cancellation Cleanup:**

When migration fails, the Blueprint sets the cleanup flag before deleting the PVC:

```bash
# Blueprint detects migration failure:
openstack volume set --property csi.cleanupVolume=true ${VOLUME_ID}

# Then deletes PVC — triggers DeleteVolume which fully cleans up
kubectl delete pvc migration-${VM_NAME}-vda-pvc
# → DeleteVolume: detach + delete Shadow VM + DELETE Cinder volume
```

**Idempotency:** `DeleteVolume` handles all partial-state scenarios gracefully:
- Volume already deleted → return success
- Shadow VM already deleted → skip Shadow VM cleanup, proceed with volume
- Volume already detached → skip detach, proceed with Shadow VM deletion
- Volume in `available` state (no attachment) → skip detach, delete Shadow VM if exists

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

### 4.4 Cinder Volume Status Lifecycle

The Cinder volume remains `in-use` for the **entire migration lifecycle** because the Shadow VM holds the attachment. This is by design — the `in-use` status acts as a lock.

```
  Cinder Volume Status Timeline:

  CSI RPC / Action                    Volume Status    Reason
  ─────────────────────────────────── ────────────── ──────────────────────────
  Cinder POST /v3/volumes             creating →      Normal Cinder lifecycle
                                      available

  Nova create Shadow VM               available →     Nova attaches the volume
    (boot from volume)                 in-use          to the Shadow VM instance

  Nova stop Shadow VM                  in-use          Stopping ≠ detaching.
                                                       Volume stays attached.

  ControllerPublishVolume              in-use          Just queries attachment
    (query attachment → NFS info)                      record. No status change.

  NodeStageVolume                      in-use          NFS mount on WRCP host is
    (mount -t nfs on WRCP host)                        invisible to Cinder.

  CDI full copy / precopy              in-use          Writes go NFS → storage.
    (pod writes to NFS vol file)                       Cinder doesn't know.

  NodeUnstageVolume                    in-use          NFS unmount. Cinder
    (umount NFS)                                       doesn't know.

  ControllerUnpublishVolume            in-use          No-op. Shadow VM still
    (no-op)                                            attached.

    ── CDI precopy cycles ──           in-use          Same throughout all stages.

  DeleteVolume (PVC deleted)           in-use →        Detach from Shadow VM.
    (detach from Shadow VM)            detaching →     Cinder releases volume.
    (delete Shadow VM)                 available

  Blueprint: server create             available →     Now attached to the
    --volume ${VOLUME_ID}              in-use          real target VM.
```

**Why `in-use` is correct and safe:**

| Protection | How it works |
|---|---|
| **Prevents accidental deletion** | `openstack volume delete` on `in-use` volume → rejected by Cinder |
| **Prevents double-attachment** | Another Nova instance can't attach an `in-use` volume (unless multi-attach) |
| **Prevents concurrent migration** | No other workflow can claim this volume while migration runs |
| **NFS mount is invisible to Cinder** | WRCP host mounts NFS directly as a client — Cinder only sees the Shadow VM attachment |

The two data planes are decoupled:

```
  Cinder Control Plane:                NFS Data Plane:
  ───────────────────                  ──────────────
  Shadow VM ──attach──► Volume         WRCP Host ──NFS mount──► Same files
  (stopped, idle)       (in-use)       (active, writing)        on NFS server

  Cinder sees: volume attached         Reality: WRCP host is
  to Shadow VM. Status: in-use.        the actual writer.
                                       Shadow VM is stopped,
                                       not doing any I/O.
```

This is safe because:
1. **Shadow VM is stopped** — it performs zero I/O on the volume
2. **NFS is a shared filesystem** — multiple clients can mount the same export
3. **Single writer** — only the CDI pod writes to the specific volume file
4. **No filesystem corruption** — the volume file is used as a raw block image, not mounted as a filesystem by the Shadow VM

### 4.5 End-to-End CSI RPC Call Sequence (CDI Multi-Phase Precopy)

The following diagram shows the complete CSI RPC call sequence during a CDI multi-phase warm migration. The key insight is that `ControllerUnpublishVolume` fires **between each CDI stage** (when the importer pod exits) and must be a no-op to preserve the Shadow VM attachment:

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│   CSI RPC Call Sequence — CDI Multi-Phase Precopy Lifecycle (V2O)               │
└─────────────────────────────────────────────────────────────────────────────────┘

 CO (kubelet + sidecars)        Controller Plugin           Node Plugin
 ══════════════════════         ═════════════════           ═══════════
        │                              │                         │
  PVC Created                          │                         │
        │                              │                         │
  ┌─────┤  1. CreateVolume             │                         │
  │     ├─────────────────────────────►│                         │
  │     │                              │── Cinder create vol     │
  │     │                              │── Set metadata:         │
  │     │                              │   csi.shadow_vm_id      │
  │     │                              │── Nova create shadow VM │
  │     │                              │── Nova stop shadow VM   │
  │     │◄─────────────────────────────┤                         │
  │     │  VolumeId + VolumeContext    │                         │
  │     │                              │                         │
  │ ╔═══╪══ CDI Stage 1: Full Copy ════╪═════════════════════════╪═══╗
  │ ║   │                              │                         │   ║
  │ ║ Pod 1 Scheduled to WRCP Worker   │                         │   ║
  │ ║   │                              │                         │   ║
  │ ║   │  2. ControllerPublishVolume  │                         │   ║
  │ ║   ├─────────────────────────────►│                         │   ║
  │ ║   │                              │── Query attachment      │   ║
  │ ║   │                              │── Extract NFS conn info │   ║
  │ ║   │◄─────────────────────────────┤                         │   ║
  │ ║   │  PublishContext (NFS info)   │                         │   ║
  │ ║   │                              │                         │   ║
  │ ║   │  3. NodeStageVolume          │                         │   ║
  │ ║   ├──────────────────────────────┼────────────────────────►│   ║
  │ ║   │                              │                         │── mount -t nfs
  │ ║   │                              │                         │   ║
  │ ║   │  4. NodePublishVolume        │                         │   ║
  │ ║   ├──────────────────────────────┼────────────────────────►│   ║
  │ ║   │                              │                         │── mount --bind
  │ ║   │                              │                         │   ║
  │ ║   │ CDI Full Copy runs:          │                         │   ║
  │ ║   │ ┌───────────────────────┐    │                         │   ║
  │ ║   │ │ VDDK → /dev/cdi-block │    │                         │   ║
  │ ║   │ └───────────────────────┘    │                         │   ║
  │ ║   │                              │                         │   ║
  │ ║ Pod 1 completes                  │                         │   ║
  │ ║   │  5. NodeUnpublishVolume      │                         │   ║
  │ ║   ├──────────────────────────────┼────────────────────────►│   ║
  │ ║   │                              │                         │── umount bind
  │ ║   │  6. NodeUnstageVolume        │                         │   ║
  │ ║   ├──────────────────────────────┼────────────────────────►│   ║
  │ ║   │                              │                         │── umount NFS
  │ ║   │  7. ControllerUnpublishVol   │                         │   ║
  │ ║   ├─────────────────────────────►│                         │   ║
  │ ║   │                              │── NO-OP ✓               │   ║
  │ ║   │                              │── Shadow VM persists    │   ║
  │ ║   │◄─────────────────────────────┤                         │   ║
  │ ╚═══╪══════════════════════════════╪═════════════════════════╪═══╝
  │     │                              │                         │
  │     │  ── gap: no pod, no VolumeAttachment on node ──        │
  │     │  ── Shadow VM still attached: volume still in-use ──   │
  │     │                              │                         │
  │ ╔═══╪══ CDI Stage 2: Precopy 1 ═══╪═════════════════════════╪═══╗
  │ ║   │                              │                         │   ║
  │ ║ Pod 2 Scheduled                  │                         │   ║
  │ ║   │  2. ControllerPublishVolume  │                         │   ║
  │ ║   ├─────────────────────────────►│                         │   ║
  │ ║   │                              │── Query SAME attachment │   ║
  │ ║   │                              │── SAME NFS info ✓       │   ║
  │ ║   │◄─────────────────────────────┤                         │   ║
  │ ║   │                              │                         │   ║
  │ ║   │  3-4. NodeStage + Publish    │                         │   ║
  │ ║   ├──────────────────────────────┼────────────────────────►│   ║
  │ ║   │                              │                         │── mount NFS
  │ ║   │                              │                         │── bind mount
  │ ║   │ CDI Delta Copy runs:         │                         │   ║
  │ ║   │ ┌───────────────────────┐    │                         │   ║
  │ ║   │ │ VDDK delta → block vol│    │                         │   ║
  │ ║   │ └───────────────────────┘    │                         │   ║
  │ ║   │                              │                         │   ║
  │ ║ Pod 2 completes                  │                         │   ║
  │ ║   │  5-7. Unpublish+Unstage+     │                         │   ║
  │ ║   │       CtrlUnpublish (NO-OP)  │                         │   ║
  │ ╚═══╪══════════════════════════════╪═════════════════════════╪═══╝
  │     │                              │                         │
  │     │  ... repeat for precopy N ...│                         │
  │     │                              │                         │
  │ ╔═══╪══ CDI Final Stage: Cutover ══╪═════════════════════════╪═══╗
  │ ║   │  (same pattern as above —    │                         │   ║
  │ ║   │   CtrlUnpublish still NO-OP) │                         │   ║
  │ ╚═══╪══════════════════════════════╪═════════════════════════╪═══╝
  │     │                              │                         │
  │ Migration complete!                │                         │
  │     │                              │                         │
  │ PVC Deleted (reclaimPolicy: Delete)│                         │
  │     │                              │                         │
  │     │  8. DeleteVolume             │                         │
  │     ├─────────────────────────────►│                         │
  │     │                              │── Detach from Shadow VM │
  │     │                              │── Wait: status→available│
  │     │                              │── Delete Shadow VM      │
  │     │                              │── Volume now "available"│
  │     │                              │   (NOT deleted)         │
  │     │◄─────────────────────────────┤                         │
  │     │                              │                         │
  │ Blueprint takes over:              │                         │
  │   virt-v2v → set bootable →        │                         │
  │   server create --volume           │                         │
  └─────┤                              │                         │
```

**OpenStack → OpenStack (O2O) variant** — the sequence is simpler because there is a single long-running NBD receiver pod (no CDI pod cycles):

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│   CSI RPC Call Sequence — NBD receiver pod (O2O)                                │
└─────────────────────────────────────────────────────────────────────────────────┘

 Blueprint                   Controller Plugin           Node Plugin (WRCP host)
 ═════════                   ═════════════════           ═══════════════════════
     │                              │                            │
     │─ Create PVC ────────────────►│                            │
     │                              │── CreateVolume:            │
     │                              │   Cinder vol + Shadow VM   │
     │                              │                            │
     │─ Launch NBD receiver pod ────┤                            │
     │  (PVC mounted as volume)     │                            │
     │                              │── ControllerPublishVolume: │
     │                              │   Query attachment → NFS   │
     │                              │──────────────────────────►│
     │                              │                            │── NodeStage: mount NFS
     │                              │                            │── NodePublish: bind mount
     │                              │                            │   vol file into pod
     │                              │                            │
     │  NBD receiver pod running ───┤   (exposes volume via NBD) │
     │                              │                            │
     │─ virsh blockcopy ───────────►│   source libvirt writes    │
     │  source-vda → nbd://receiver │   to NBD → vol file → NFS  │
     │                              │                            │
     │─ blockcopy complete ─────────┤                            │
     │─ virsh blockjob --pivot ─────┤                            │
     │                              │                            │
     │─ Delete NBD receiver pod ────┤                            │
     │                              │                            │── NodeUnpublish + Unstage
     │                              │── ControllerUnpublish:     │
     │                              │   NO-OP                    │
     │                              │                            │
     │─ Launch helper pod ──────────┤  (PVC still exists)        │
     │  (virt-v2v-in-place)         │                            │
     │                              │── ControllerPublish        │
     │                              │──────────────────────────►│── NodeStage + NodePublish
     │  helper pod: inject virtio   │                            │   (mount NFS, bind mount)
     │  drivers into volume         │                            │
     │─ Helper pod exits ───────────┤                            │── NodeUnpublish + Unstage
     │                              │── ControllerUnpublish:     │
     │                              │   NO-OP                    │
     │                              │                            │
     │─ Delete PVC ────────────────►│                            │
     │                              │── DeleteVolume:            │
     │                              │   Detach + delete Shadow VM│
     │                              │   Volume → "available"     │
     │                              │                            │
     │─ set bootable + server create┤                            │
     ✓ Done                         │                            │
```

### 4.6 Volume Finalization and VM Creation

Volume finalization is a **two-phase** process: driver injection happens **before** PVC deletion (while NFS is still accessible via CSI), and VM creation happens **after** volume release.

#### 4.6.1 Phase 1: Driver Injection (Pre-CSI Cleanup — PVC Still Exists)

The Blueprint launches a **helper pod** on the WRC K8S cluster that mounts the same PVC. This pod runs `virt-v2v-in-place` to inject virtio drivers into the volume. Because the PVC still exists, the full CSI mount chain is available (ControllerPublish → NodeStage → NodePublish).

```
┌────────────────────────────────────────────────────────────────────────────┐
│       Driver Injection via Helper Pod (PVC still exists)                    │
│       (Orchestrated by WRC Blueprint, uses CSI mount chain)                │
└────────────────────────────────────────────────────────────────────────────┘

  WRC Blueprint                             WRC K8S Cluster
       │                                         │
       │  PVC still bound to PV                   │
       │  Shadow VM still holds attachment         │
       │                                          │
       │  1. Launch helper pod:                   │
       │     - References same PVC                │
       │     - volumeMode: Block                  │
       │     - image: virt-v2v tooling             │
       │     CSI fires:                           │
       │       ControllerPublish → NFS info        │
       │       NodeStage → mount NFS              │
       │       NodePublish → bind mount vol file   │
       │                                          │
       │  2. Helper pod runs virt-v2v-in-place:    │
       │     - Injects virtio drivers             │
       │     - Fixes boot config for KVM          │
       │     - Writes to /dev/cdi-block-volume     │
       │       → NFS volume file (same path)       │
       │                                          │
       │  3. Helper pod exits:                    │
       │     CSI fires:                           │
       │       NodeUnpublish + NodeUnstage         │
       │       ControllerUnpublish → NO-OP         │
       │                                          │
       │  Volume now has virtio drivers injected   │
       │  PVC/PV still exist, Shadow VM intact     │
```

#### 4.6.2 Phase 2: Volume Release + VM Creation (Post-CSI)

After driver injection completes, the Blueprint deletes the PVC to trigger `DeleteVolume`, which releases the volume from the Shadow VM.

```
┌────────────────────────────────────────────────────────────────────────────┐
│       Volume Release + VM Creation                                         │
│       (Orchestrated by WRC Blueprint)                                      │
└────────────────────────────────────────────────────────────────────────────┘

  WRC Orchestrator                          Target OpenStack
       │                                         │
       │  1. Delete PVC:                          │
       │     kubectl delete pvc migration-xxx     │
       │     CSI DeleteVolume:                    │
       │       Detach volume from Shadow VM       │
       │       Delete Shadow VM                   │
       │     Volume status: "available"           │
       │                                          │
       │  2. Set volume bootable                  │
       │     openstack volume set --bootable      │
       │     ${VOLUME_ID}                         │
       ├────────────────────────────────────────► │
       │                                          │
       │  3. Create target VM from volume         │
       │     openstack server create              │
       │     --volume ${VOLUME_ID}                │
       │     --flavor ${FLAVOR}                   │
       │     --network ${NETWORK}                 │
       │     target-${VM_NAME}                    │
       ├────────────────────────────────────────► │
       │                                          │  VM boots from Cinder volume
       │                                          │  ✓ Migration complete
```

**Failure/Cancellation Cleanup:**

If migration fails, the Blueprint sets the cleanup flag and deletes the PVC:

```bash
# Mark volume for full cleanup
openstack volume set --property csi.cleanupVolume=true ${VOLUME_ID}

# Delete PVC → triggers DeleteVolume → full cleanup
kubectl delete pvc migration-${VM_NAME}-vda-pvc
# Result: Shadow VM deleted + Cinder volume DELETED. No orphans.
```

---

## 5. Component Flow

### 5.1 End-to-End Workflow

#### 5.1.1 VMware → OpenStack (V2O) — CDI Multi-Phase Warm Migration

```
┌────────────────────────────────────────────────────────────────────────────────┐
│              END-TO-END V2O NFS-BACKED MIGRATION FLOW                          │
│              (CDI Multi-Phase Precopy + CSI NFS-Cinder Driver)                 │
└────────────────────────────────────────────────────────────────────────────────┘

 WRC Blueprint Orchestrator
 ═════════════════════════

 Stage 0: PRECHECK
    │  Validate CBT, inspect source VM
    │  Validate target OpenStack NFS backend
    ▼
 Stage 1: CLEANUP
    │  Remove previous migration artifacts
    ▼
 ┌─────────────────────────────────────────────────────────────┐
 │ CSI Volume Provisioning (via PVC + StorageClass)            │
 │                                                             │
 │  Blueprint: kubectl apply PVC (StorageClass: cinder-nfs)    │
 │  CSI CreateVolume:                                          │
 │    1. Create Cinder volume (--type netapp-nfs)              │
 │    2. Store csi.shadow_vm_id in Cinder metadata             │
 │    3. Create Shadow VM (triggers NFS attachment record)     │
 │    4. Stop Shadow VM (volume status: in-use)                │
 │                                                             │
 │  Result: PV bound to PVC, Cinder vol locked by Shadow VM    │
 └─────────────────────────────────────────────────────────────┘
    │
    ▼
 Stage 2: FULL COPY
    │  Create initial VMware snapshot
    │  CDI importer pod 1 on WRC K8S:
    │    CSI ControllerPublish → query attachment → NFS info
    │    CSI NodeStage → mount NFS on WRCP host
    │    CSI NodePublish → bind mount vol file into pod
    │    VDDK full disk copy → /dev/cdi-block-volume → NFS
    │  Pod 1 exits:
    │    CSI NodeUnpublish + NodeUnstage → umount NFS
    │    CSI ControllerUnpublish → NO-OP (Shadow VM persists)
    ▼
 Stage 3: PRECOPY (Loop — repeat N times)
    │  Create incremental snapshot
    │  CDI importer pod N on WRC K8S:
    │    CSI ControllerPublish → query SAME attachment → NFS info ✓
    │    CSI NodeStage → mount NFS again
    │    CSI NodePublish → bind mount again
    │    VDDK delta copy (changed blocks only) → NFS
    │  Pod N exits:
    │    CSI ControllerUnpublish → NO-OP (Shadow VM persists) ✓
    │  Repeat until cutover triggered
    ▼
 Stage 4: CUTOVER
    │  Power off source VM
    │  Final snapshot + final delta transfer (same pod pattern)
    │  Final CSI ControllerUnpublish → NO-OP
    ▼
 ┌─────────────────────────────────────────────────────────────┐
 │ Stage 5: DRIVER INJECTION (virt-v2v-in-place)               │
 │  (PVC still exists — NFS mount available via CSI)           │
 │                                                             │
 │  Blueprint launches helper pod on WRC K8S:                  │
 │    Pod spec references SAME PVC (volumeMode: Block)         │
 │    CSI ControllerPublish → query attachment → NFS info      │
 │    CSI NodeStage → mount NFS on WRCP host                   │
 │    CSI NodePublish → bind mount vol file into helper pod    │
 │                                                             │
 │  Helper pod runs virt-v2v-in-place:                         │
 │    - Injects virtio drivers into volume                     │
 │    - Fixes boot configuration for KVM/OpenStack             │
 │    - Writes changes directly to NFS volume file             │
 │                                                             │
 │  Helper pod exits:                                          │
 │    CSI NodeUnpublish + NodeUnstage → umount NFS             │
 │    CSI ControllerUnpublish → NO-OP                          │
 └─────────────────────────────────────────────────────────────┘
    │
    ▼
 ┌─────────────────────────────────────────────────────────────┐
 │ Stage 6: VOLUME RELEASE                                     │
 │                                                             │
 │  Blueprint: kubectl delete pvc                              │
 │  CSI DeleteVolume:                                          │
 │    1. Detach volume from Shadow VM                          │
 │    2. Wait for volume status → "available"                  │
 │    3. Delete Shadow VM                                      │
 │  Result: Cinder volume available, no attachments            │
 │          Volume already has virtio drivers injected         │
 └─────────────────────────────────────────────────────────────┘
    │
    ▼
 Stage 7: CREATE VM (OpenStack)
    │  openstack volume set --bootable ${VOLUME_ID}
    │  openstack server create --volume ${VOLUME_ID}
    │  VM boots from Cinder volume directly
    ▼
 ✓ MIGRATION COMPLETE
```

#### 5.1.2 OpenStack → OpenStack (O2O) — NBD Receiver + virsh blockcopy

```
┌────────────────────────────────────────────────────────────────────────────────┐
│              END-TO-END O2O NFS-BACKED MIGRATION FLOW                          │
│              (NBD Receiver Pod + virsh blockcopy + CSI NFS-Cinder Driver)       │
└────────────────────────────────────────────────────────────────────────────────┘

 WRC Blueprint Orchestrator
 ═════════════════════════

 Stage 0: PRECHECK
    │  Validate source VM, inspect disk layout
    │  Validate target OpenStack NFS backend
    ▼
 ┌─────────────────────────────────────────────────────────────┐
 │ CSI Volume Provisioning (via PVC + StorageClass)            │
 │                                                             │
 │  Blueprint: kubectl apply PVC (StorageClass: cinder-nfs)    │
 │  CSI CreateVolume:                                          │
 │    1. Create Cinder volume (--type netapp-nfs)              │
 │    2. Create Shadow VM + stop (volume: in-use)              │
 │                                                             │
 │  Result: PV bound to PVC, Cinder vol locked by Shadow VM    │
 └─────────────────────────────────────────────────────────────┘
    │
    ▼
 Stage 1: LAUNCH NBD RECEIVER
    │  Blueprint launches NBD receiver pod on WRC K8S:
    │    Pod spec references the PVC (volumeMode: Block)
    │    CSI ControllerPublish → query attachment → NFS info
    │    CSI NodeStage → mount NFS on WRCP host
    │    CSI NodePublish → bind mount vol file into pod
    │  NBD receiver pod starts:
    │    Exposes /dev/cdi-block-volume via NBD protocol
    │    Listens on nbd://<pod-ip>:10809/export
    ▼
 Stage 2: BLOCK MIRROR
    │  virsh blockcopy source-vm vda nbd://<nbd-receiver>:10809
    │    (source libvirt mirrors disk to NBD target → NFS volume)
    │  Mirror runs until ready for pivot
    ▼
 Stage 3: CUTOVER
    │  virsh blockjob --pivot (switch source to NBD/NFS target)
    │  Power off source VM
    ▼
 ┌─────────────────────────────────────────────────────────────┐
 │ Volume Release + Finalization                               │
 │                                                             │
 │  Blueprint: delete NBD receiver pod                         │
 │    CSI NodeUnpublish + NodeUnstage → umount NFS             │
 │    CSI ControllerUnpublish → NO-OP                          │
 │                                                             │
 │  Blueprint: kubectl delete pvc                              │
 │  CSI DeleteVolume:                                          │
 │    1. Detach from Shadow VM → delete Shadow VM              │
 │    2. Volume → "available"                                  │
 │                                                             │
 │  Blueprint (post-CSI):                                      │
 │    1. virt-v2v-in-place (if needed)                         │
 │    2. openstack volume set --bootable ${VOLUME_ID}          │
 └─────────────────────────────────────────────────────────────┘
    │
    ▼
 Stage 4: CREATE VM (OpenStack)
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

### 6.1 NFS Volume Discovery Script (Reference)

The following script demonstrates the NFS connection discovery logic that the CSI driver implements internally in `CreateVolume` and `ControllerPublishVolume`. This script is provided as a **reference** — in production, the CSI driver performs these steps automatically when a PVC is created.

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

### 6.2 StorageClass and PVC Definition

With the CSI driver managing Shadow VM lifecycle, PV/PVC provisioning is **dynamic** — the Blueprint only creates a PVC referencing a StorageClass. The CSI driver handles all OpenStack resource creation.

**StorageClass:**

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: cinder-nfs-migration
provisioner: cinder-nfs.csi.openstack.org
parameters:
  type: netapp-nfs       # Cinder volume type (must be NFS-backed)
  availability: nova     # Target availability zone
reclaimPolicy: Delete    # PVC deletion triggers DeleteVolume → Shadow VM cleanup
volumeBindingMode: Immediate
```

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
  annotations:
    cdi.kubevirt.io/storage.import.source: vddk
spec:
  accessModes:
    - ReadWriteOnce
  volumeMode: Block
  resources:
    requests:
      storage: ${DISK_SIZE}Gi
  storageClassName: cinder-nfs-migration
```

The CSI driver provisions the PV automatically with `VolumeContext` containing NFS connection info. The CO passes this context through `ControllerPublishVolume` → `NodeStageVolume` → `NodePublishVolume`. No static PV or manual NFS mount is needed for the V2O use case.

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
    # Dynamic PVC provisioned by cinder-nfs-migration StorageClass
    - name: cdi-data-vol
      persistentVolumeClaim:
        claimName: migration-${VM_NAME}-vda-pvc
    - name: vddk-vol-mount
      emptyDir: {}
```

### 6.4 Shadow VM Lifecycle Management

The Shadow VM is a temporary resource whose **sole purpose** is to trigger Cinder's NFS attachment record creation. Its lifecycle is fully managed by the CSI driver:

```
  CSI CreateVolume                              CSI DeleteVolume (PVC deleted)
  ══════════════                                ══════════════════════════════
  Created ──► Running ──► Stopped ──►  Migration runs  ──► Detached ──► Deleted
     │            │            │       (V2O: CDI pods     │             │
     └ Triggers   └ Attachment └ Vol    cycle; O2O: NBD   └ Volume      └ Full
       volume       record       stays  receiver pod      freed:        cleanup
       attachment   populated    in-    runs; Shadow VM   "available"
                                use     persists via
                                        no-op Unpublish)
```

**Key lifecycle invariants:**

| Phase | Shadow VM State | Volume State | Attachment Record |
|-------|----------------|--------------|-------------------|
| After `CreateVolume` | Stopped | `in-use` | Exists (NFS connection_info populated) |
| During V2O CDI precopy stages | Stopped | `in-use` | Persists (ControllerUnpublish = no-op) |
| During O2O NBD receiver pod | Stopped | `in-use` | Persists (long-running pod, single mount) |
| Between CDI stages (no pod) | Stopped | `in-use` | Persists (acts as lock) |
| After `DeleteVolume` (success) | Deleted | `available` | Removed |
| After `DeleteVolume` (failure) | Deleted | Deleted | Removed |

**Blueprint cleanup script (success path):**

```bash
# Migration complete — Blueprint deletes PVC
# CSI DeleteVolume automatically: detach → delete Shadow VM → volume "available"
kubectl delete pvc migration-${VM_NAME}-vda-pvc

# Volume is now ready for target VM creation
openstack volume set --bootable ${VOLUME_ID}
openstack server create --volume ${VOLUME_ID} \
  --flavor ${FLAVOR} --network ${NETWORK} target-${VM_NAME}
```

**Blueprint cleanup script (failure/cancellation):**

```bash
# Mark volume for full cleanup before deleting PVC
openstack volume set --property csi.cleanupVolume=true ${VOLUME_ID}

# Delete PVC — CSI DeleteVolume will delete Shadow VM AND the Cinder volume
kubectl delete pvc migration-${VM_NAME}-vda-pvc
# Result: no orphaned volumes or Shadow VMs
```

### 6.5 Driver Configuration — ConfigMap and Secret

The NFS-Cinder CSI driver follows the same **dual-config pattern** used by the existing `cinder.csi.openstack.org` driver in this project (see [manifests/cinder-csi-plugin/](../../../manifests/cinder-csi-plugin/)):

| Config Object | K8S Kind | Purpose | Contains |
|---|---|---|---|
| `cloud-config` | **Secret** | OpenStack authentication credentials | `cloud.conf` — Keystone auth-url, username, password, project-id, domain, region (same INI format as existing Cinder CSI) |
| `cinder-nfs-config` | **ConfigMap** | Shadow VM + NFS operational parameters | `driver.conf` — non-sensitive driver configuration: flavor, network, AZ, NFS mount options, timeouts |

**Why a ConfigMap for Shadow VM config (not Secret)?**

Shadow VM parameters (flavor ID, network ID, AZ) are **infrastructure identifiers**, not credentials. Placing them in a ConfigMap:
- Allows `kubectl edit configmap` for live reconfiguration without re-encoding base64
- Follows the Kubernetes convention of Secret for credentials, ConfigMap for configuration
- Matches the existing project pattern where `BlockStorageOpts` struct fields are non-sensitive operational knobs (see [`pkg/csi/cinder/openstack/openstack.go`](../../../pkg/csi/cinder/openstack/openstack.go) — `Config` struct)

#### 6.5.1 Configuration File Format (`driver.conf`)

The driver configuration uses the same INI-style format (`gcfg`) as the existing [`cloud.conf`](../../../pkg/csi/cinder/etc/cloud.conf) in this project. This ensures the driver can reuse the existing `gcfg` parsing infrastructure from `pkg/csi/cinder/openstack`.

```ini
# =============================================================================
# driver.conf — NFS-Cinder CSI Driver Configuration
# Mounted via ConfigMap: cinder-nfs-config
# Path: /etc/cinder-nfs/driver.conf
# =============================================================================

# -----------------------------------------------------------
# [ShadowVM] — Shadow VM lifecycle configuration
#
# The Shadow VM is a lightweight Nova instance whose sole
# purpose is to trigger Cinder's NFS attachment record
# creation. These parameters control how the Shadow VM is
# provisioned during the CreateVolume RPC.
# -----------------------------------------------------------
[ShadowVM]

# Nova flavor for the Shadow VM.
# Should be the smallest available flavor — the Shadow VM is
# stopped immediately after creation and performs zero I/O.
# Accepts either a flavor ID (UUID) or flavor name.
# Required.
flavor-ref = m1.small

# Network ID (UUID) to attach the Shadow VM to.
# This network must be accessible from the Nova compute host
# and reachable from the Cinder NFS backend. It does NOT need
# to be the storage network — the NFS mount is handled
# separately by the CSI Node plugin on WRCP worker hosts.
# Required.
network-id = 8e3f3c4a-1234-5678-abcd-9876543210ab

# Availability zone for the Shadow VM.
# Should match the AZ where the Cinder NFS backend resides
# to ensure volume attachment locality.
# Default: "" (Nova picks the AZ)
availability-zone = nova

# Name prefix for Shadow VM instances.
# CreateVolume generates names as: {prefix}-{req.Name}
# e.g., "shadow-migration-myvm-vda"
# Default: shadow
name-prefix = shadow

# Timeout (seconds) to wait for Shadow VM to reach ACTIVE
# state before being stopped.
# Default: 300
create-timeout = 300

# Timeout (seconds) to wait for Shadow VM to reach SHUTOFF
# state after stop request.
# Default: 120
stop-timeout = 120

# Optional: Security group ID(s) for the Shadow VM.
# Comma-separated list of security group IDs.
# Default: "" (Nova default security group)
security-groups =

# Optional: Image ID for the Shadow VM boot disk.
# Only required if the Shadow VM is NOT booted from the
# Cinder volume directly (boot-from-volume = true is default).
# Default: "" (boot from volume)
image-ref =

# -----------------------------------------------------------
# [NFS] — NFS mount configuration for CSI Node plugin
#
# Controls how the Node plugin mounts NFS exports on
# WRCP/WRC worker hosts during NodeStageVolume.
# -----------------------------------------------------------
[NFS]

# Default NFS mount options applied when the Cinder
# attachment record does not specify mount options.
# Override per-volume via StorageClass parameters.
# Default: rw,hard,intr
mount-options = rw,hard,intr,rsize=1048576,wsize=1048576

# Base path on WRCP worker hosts for NFS mount points.
# NodeStageVolume creates per-volume subdirectories under
# this path. Must be writable by the CSI Node plugin.
# Default: /var/lib/cinder-nfs
mount-base-path = /var/lib/cinder-nfs

# NFS version to use for mounting.
# Default: 4.1
nfs-version = 4.1

# -----------------------------------------------------------
# [Volume] — Volume lifecycle configuration
#
# Controls volume handling during CreateVolume and
# DeleteVolume RPCs.
# -----------------------------------------------------------
[Volume]

# Timeout (seconds) to wait for Cinder volume to reach
# "available" status after creation.
# Default: 300
create-timeout = 300

# Timeout (seconds) to wait for volume to reach "available"
# status after detaching from Shadow VM during DeleteVolume.
# Default: 120
detach-timeout = 120

# Default Cinder volume type.
# Can be overridden per-PVC via StorageClass parameters.
# Must be an NFS-backed volume type.
# Default: "" (use StorageClass parameter "type")
default-volume-type =

# Cinder volume metadata key prefix for CSI-managed metadata.
# The driver stores Shadow VM ID and cleanup flags in Cinder
# volume metadata using keys like: {prefix}.shadow_vm_id
# Default: csi
metadata-prefix = csi
```

#### 6.5.2 Go Config Struct

Following the pattern in [`pkg/csi/cinder/openstack/openstack.go`](../../../pkg/csi/cinder/openstack/openstack.go) where `Config` contains `BlockStorageOpts`:

```go
// Config for the NFS-Cinder CSI driver (parsed from driver.conf via gcfg)
type NfsCinderConfig struct {
    Global   map[string]*client.AuthOpts  // from cloud.conf (Secret)
    ShadowVM ShadowVMOpts                 // from driver.conf (ConfigMap)
    NFS      NFSOpts                      // from driver.conf (ConfigMap)
    Volume   VolumeOpts                   // from driver.conf (ConfigMap)
}

// ShadowVMOpts controls the Shadow VM lifecycle in CreateVolume/DeleteVolume
type ShadowVMOpts struct {
    FlavorRef        string `gcfg:"flavor-ref"`
    NetworkID        string `gcfg:"network-id"`
    AvailabilityZone string `gcfg:"availability-zone"`
    NamePrefix       string `gcfg:"name-prefix"`
    CreateTimeout    int    `gcfg:"create-timeout"`
    StopTimeout      int    `gcfg:"stop-timeout"`
    SecurityGroups   string `gcfg:"security-groups"`
    ImageRef         string `gcfg:"image-ref"`
}

// NFSOpts controls NFS mount behavior in NodeStageVolume/NodeUnstageVolume
type NFSOpts struct {
    MountOptions  string `gcfg:"mount-options"`
    MountBasePath string `gcfg:"mount-base-path"`
    NFSVersion    string `gcfg:"nfs-version"`
}

// VolumeOpts controls Cinder volume lifecycle
type VolumeOpts struct {
    CreateTimeout     int    `gcfg:"create-timeout"`
    DetachTimeout     int    `gcfg:"detach-timeout"`
    DefaultVolumeType string `gcfg:"default-volume-type"`
    MetadataPrefix    string `gcfg:"metadata-prefix"`
}
```

#### 6.5.3 ConfigMap Manifest

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: cinder-nfs-config
  namespace: kube-system
data:
  driver.conf: |
    [ShadowVM]
    flavor-ref = m1.small
    network-id = 8e3f3c4a-1234-5678-abcd-9876543210ab
    availability-zone = nova
    name-prefix = shadow
    create-timeout = 300
    stop-timeout = 120

    [NFS]
    mount-options = rw,hard,intr,rsize=1048576,wsize=1048576
    mount-base-path = /var/lib/cinder-nfs
    nfs-version = 4.1

    [Volume]
    create-timeout = 300
    detach-timeout = 120
    metadata-prefix = csi
```

#### 6.5.4 Secret Manifest (OpenStack Credentials)

Reuses the same format and pattern as the existing Cinder CSI driver's [`csi-secret-cinderplugin.yaml`](../../../manifests/cinder-csi-plugin/csi-secret-cinderplugin.yaml):

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: cinder-nfs-cloud-config
  namespace: kube-system
data:
  # Base64-encoded cloud.conf (same INI format as existing Cinder CSI)
  #
  # [Global]
  # auth-url = https://<keystone_ip>/identity/v3
  # username = cinder-nfs-service
  # password = <service_password>
  # domain-name = default
  # tenant-id = cc34b11ff95d42309081d0bd46c0ff89
  # region = RegionOne
  cloud.conf: W0dsb2JhbF0KYXV0aC11cmwgPSBodHRwczovLzxrZXlzdG9uZV9pcD4vaWRlbnRpdHkvdjMKdXNlcm5hbWUgPSBjaW5kZXItbmZzLXNlcnZpY2UKcGFzc3dvcmQgPSA8c2VydmljZV9wYXNzd29yZD4KZG9tYWluLW5hbWUgPSBkZWZhdWx0CnRlbmFudC1pZCA9IGNjMzRiMTFmZjk1ZDQyMzA5MDgxZDBiZDQ2YzBmZjg5CnJlZ2lvbiA9IFJlZ2lvbk9uZQo=
```

#### 6.5.5 Controller Deployment — Volume Mounts

The Controller Deployment mounts **both** the Secret (credentials) and ConfigMap (driver config), following the same mount pattern as the existing [`cinder-csi-controllerplugin.yaml`](../../../manifests/cinder-csi-plugin/cinder-csi-controllerplugin.yaml):

```yaml
# Excerpt from cinder-nfs-csi-controllerplugin.yaml
containers:
  - name: cinder-nfs-csi-plugin
    image: registry.k8s.io/provider-os/cinder-nfs-csi-plugin:v1.0.0
    args:
      - /bin/cinder-nfs-csi-plugin
      - "--endpoint=$(CSI_ENDPOINT)"
      - "--cloud-config=$(CLOUD_CONFIG)"         # OpenStack credentials (Secret)
      - "--driver-config=$(DRIVER_CONFIG)"        # Shadow VM + NFS config (ConfigMap)
      - "--v=1"
    env:
      - name: CSI_ENDPOINT
        value: unix://csi/csi.sock
      - name: CLOUD_CONFIG
        value: /etc/cloud-config/cloud.conf       # from Secret
      - name: DRIVER_CONFIG
        value: /etc/cinder-nfs/driver.conf         # from ConfigMap
    volumeMounts:
      - name: socket-dir
        mountPath: /csi
      - name: cloud-config                         # Secret: OpenStack credentials
        mountPath: /etc/cloud-config
        readOnly: true
      - name: driver-config                        # ConfigMap: Shadow VM + NFS config
        mountPath: /etc/cinder-nfs
        readOnly: true
volumes:
  - name: socket-dir
    emptyDir: {}
  - name: cloud-config                             # Same pattern as existing Cinder CSI
    secret:
      secretName: cinder-nfs-cloud-config
  - name: driver-config                            # New: ConfigMap for operational config
    configMap:
      name: cinder-nfs-config
```

#### 6.5.6 Node DaemonSet — Volume Mounts

The Node DaemonSet only needs the ConfigMap (for NFS mount options) — it does not call OpenStack APIs at all. However, mounting the Secret as well gives the option for `Probe` RPC to validate Keystone connectivity:

```yaml
# Excerpt from cinder-nfs-csi-nodeplugin.yaml
containers:
  - name: cinder-nfs-csi-plugin
    securityContext:
      privileged: true
      capabilities:
        add: ["SYS_ADMIN"]
    args:
      - /bin/cinder-nfs-csi-plugin
      - "--endpoint=$(CSI_ENDPOINT)"
      - "--provide-controller-service=false"
      - "--driver-config=$(DRIVER_CONFIG)"
      - "--v=1"
    env:
      - name: CSI_ENDPOINT
        value: unix://csi/csi.sock
      - name: DRIVER_CONFIG
        value: /etc/cinder-nfs/driver.conf
    volumeMounts:
      - name: socket-dir
        mountPath: /csi
      - name: kubelet-dir
        mountPath: /var/lib/kubelet
        mountPropagation: "Bidirectional"
      - name: driver-config
        mountPath: /etc/cinder-nfs
        readOnly: true
volumes:
  - name: driver-config
    configMap:
      name: cinder-nfs-config
```

#### 6.5.7 Configuration Validation and Defaults

The driver validates configuration at startup and applies defaults for optional fields:

| Field | Required | Default | Validation |
|-------|----------|---------|------------|
| `ShadowVM.flavor-ref` | **Yes** | — | Must resolve to a valid Nova flavor (UUID or name) |
| `ShadowVM.network-id` | **Yes** | — | Must be a valid Neutron network UUID |
| `ShadowVM.availability-zone` | No | `""` (Nova default) | If set, must match an existing Nova AZ |
| `ShadowVM.name-prefix` | No | `shadow` | Non-empty string, safe for Nova server names |
| `ShadowVM.create-timeout` | No | `300` | > 0 |
| `ShadowVM.stop-timeout` | No | `120` | > 0 |
| `ShadowVM.security-groups` | No | `""` | Comma-separated UUIDs; each must exist in Neutron |
| `ShadowVM.image-ref` | No | `""` | If set, must be a valid Glance image UUID |
| `NFS.mount-options` | No | `rw,hard,intr` | Valid NFS mount option string |
| `NFS.mount-base-path` | No | `/var/lib/cinder-nfs` | Absolute path, writable by Node plugin |
| `NFS.nfs-version` | No | `4.1` | `3`, `4`, `4.0`, `4.1`, or `4.2` |
| `Volume.create-timeout` | No | `300` | > 0 |
| `Volume.detach-timeout` | No | `120` | > 0 |
| `Volume.default-volume-type` | No | `""` | If set, must be a valid Cinder volume type |
| `Volume.metadata-prefix` | No | `csi` | Non-empty string |

**Startup validation flow:**

```
  Driver startup (main.go):
    1. Parse --cloud-config → load cloud.conf from Secret
       → Authenticate to Keystone (same as existing Cinder CSI)

    2. Parse --driver-config → load driver.conf from ConfigMap
       → Validate [ShadowVM]:
           flavor-ref: GET /v2/flavors/{id} or /v2/flavors?name=...
           network-id: GET /v2.0/networks/{id}
       → Apply defaults for optional fields
       → Log effective configuration

    3. Initialize Controller + Node servers with validated config
```

#### 6.5.8 StorageClass Parameter Overrides

Several ConfigMap defaults can be **overridden per-PVC** via StorageClass parameters. This allows different migration workloads to use different Shadow VM configurations without modifying the global ConfigMap:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: cinder-nfs-migration-az2
provisioner: cinder-nfs.csi.openstack.org
parameters:
  type: netapp-nfs                          # Cinder volume type (overrides Volume.default-volume-type)
  availability: az-2                        # Volume AZ (overrides ShadowVM.availability-zone)

  # Shadow VM overrides (optional — falls back to ConfigMap defaults)
  shadow-vm-flavor-ref: m1.tiny             # Override ShadowVM.flavor-ref
  shadow-vm-network-id: <other-net-uuid>    # Override ShadowVM.network-id

  # NFS overrides (optional)
  nfs-mount-options: "rw,hard,intr,nfsvers=3"  # Override NFS.mount-options
reclaimPolicy: Delete
volumeBindingMode: Immediate
```

**Parameter resolution order** (highest priority first):

```
  1. StorageClass parameters (per-PVC override)
  2. ConfigMap driver.conf (cluster-wide defaults)
  3. Hardcoded defaults in driver code (safety net)
```

This mirrors how the existing Cinder CSI driver handles the `type` and `availability` StorageClass parameters in [`controllerserver.go`](../../../pkg/csi/cinder/controllerserver.go) — the `CreateVolume` RPC reads `req.Parameters["type"]` and `req.Parameters["availability"]` from the StorageClass, falling back to cluster-level config.

#### 6.5.9 Configuration Diagram

```
┌─────────────────────────────────────────────────────────────────────────┐
│  Configuration Sources for NFS-Cinder CSI Driver                        │
└─────────────────────────────────────────────────────────────────────────┘

  K8S Secret                 K8S ConfigMap              K8S StorageClass
  ════════                   ═════════════              ════════════════
  cinder-nfs-cloud-config    cinder-nfs-config          cinder-nfs-migration
  ┌───────────────────┐      ┌───────────────────┐      ┌───────────────────┐
  │ cloud.conf        │      │ driver.conf       │      │ parameters:       │
  │                   │      │                   │      │   type: netapp-nfs│
  │ [Global]          │      │ [ShadowVM]        │      │   availability: ..│
  │ auth-url = ...    │      │ flavor-ref = ...  │      │   shadow-vm-... = │
  │ username = ...    │      │ network-id = ...  │      │   nfs-mount-... = │
  │ password = ...    │      │                   │      └─────────┬─────────┘
  │ tenant-id = ...   │      │ [NFS]             │                │
  │ region = ...      │      │ mount-options = ..│                │ Per-PVC
  └─────────┬─────────┘      │                   │                │ overrides
            │ Mounted at     │ [Volume]          │                │
            │ /etc/cloud-    │ create-timeout = .│                │
            │   config/      └─────────┬─────────┘                │
            │                          │ Mounted at               │
            │                          │ /etc/cinder-nfs/          │
            ▼                          ▼                          ▼
  ┌──────────────────────────────────────────────────────────────────────┐
  │  CSI Controller Plugin                                               │
  │                                                                      │
  │  Startup:                                                            │
  │    cloud.conf → Keystone auth → compute + blockstorage clients       │
  │    driver.conf → ShadowVMOpts + NFSOpts + VolumeOpts                 │
  │                                                                      │
  │  CreateVolume(req):                                                  │
  │    flavor  = req.Parameters["shadow-vm-flavor-ref"]                  │
  │              ?? config.ShadowVM.FlavorRef                            │
  │    network = req.Parameters["shadow-vm-network-id"]                  │
  │              ?? config.ShadowVM.NetworkID                            │
  │    volType = req.Parameters["type"]                                  │
  │              ?? config.Volume.DefaultVolumeType                      │
  └──────────────────────────────────────────────────────────────────────┘
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
| **Privileged Host Access** | Ability to run `mount` commands on WRCP/WRC worker hosts (for NFS mounting via CSI Node plugin). |
| **CSI Driver Deployment** | NFS-Cinder CSI driver (`cinder-nfs.csi.openstack.org`) deployed on WRC K8S cluster with Controller and Node plugins. |

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

1. **Multi-Disk Support** — Extend the workflow to handle VMs with multiple disks, creating one Cinder volume + NFS mount + PV/PVC per disk via multiple PVCs in a single StorageClass.

2. **NFS Mount DaemonSet** — Deploy a privileged DaemonSet on WRC workers that handles NFS mount/unmount operations via a gRPC or REST API, removing the need for direct host SSH access in the O2O use case.

3. **Support for Non-NFS Backends** — Investigate extending the approach to other Cinder backends (e.g., using `tgt`/iSCSI initiator on WRCP hosts for iSCSI-backed volumes).

4. **virt-v2v Driver Injection via NFS** — Perform virtio driver injection directly on the NFS-mounted volume file from the WRCP host, avoiding the need to re-mount NFS post-CSI for driver injection.

5. **Volume Cloning Support** — Add `CLONE_VOLUME` capability to enable clone-to-tenant-volume workflows directly within the CSI driver, eliminating the need for Blueprint to call `openstack volume create --source` separately.

6. **Automatic virt-v2v Integration** — Run virt-v2v as a post-migration CSI hook or sidecar, eliminating the Blueprint step between PVC deletion and VM creation.

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
