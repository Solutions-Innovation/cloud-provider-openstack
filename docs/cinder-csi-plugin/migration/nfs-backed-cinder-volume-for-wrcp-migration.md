# Design Proposal: NFS-Backed Cinder Volume Direct Mount for WRCP/WRC Migration

> **Status**: Draft  
> **Version**: 0.1  
> **Date**: February 2026  
> **Authors**: WindRiver Migration Framework Team

---

## Table of Contents

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
- [4. Detailed Design](#4-detailed-design)
  - [4.1 Phase 1 — Cinder Volume Provisioning via Shadow VM](#41-phase-1--cinder-volume-provisioning-via-shadow-vm)
  - [4.2 Phase 2 — NFS Connection Discovery](#42-phase-2--nfs-connection-discovery)
  - [4.3 Phase 3 — NFS Mount on WRCP/WRC Worker Host](#43-phase-3--nfs-mount-on-wrcpwrc-worker-host)
  - [4.4 Phase 4 — Bind Mount into CDI Importer Pod](#44-phase-4--bind-mount-into-cdi-importer-pod)
  - [4.5 Phase 5 — Volume Finalization and VM Creation](#45-phase-5--volume-finalization-and-vm-creation)
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

## 4. Detailed Design

### 4.1 Phase 1 — Cinder Volume Provisioning via Shadow VM

A **Shadow VM** is created on the target OpenStack to trigger Cinder to provision the NFS-backed volume and establish a proper volume attachment record with NFS connection properties.

```
┌────────────────────────────────────────────────────────────────────────────┐
│                    PHASE 1: Shadow VM Provisioning                         │
└────────────────────────────────────────────────────────────────────────────┘

  WRC Orchestrator                          Target OpenStack
       │                                         │
       │  1. openstack volume create              │
       │     --size ${DISK_SIZE}                  │
       │     --type netapp-nfs                    │
       │     --bootable                           │
       ├────────────────────────────────────────► │
       │                                          │  Cinder provisions NFS volume
       │                                          │  on NetApp backend
       │  2. openstack server create              │
       │     --volume ${VOLUME_ID}                │
       │     shadow-${VM_NAME}                    │
       ├────────────────────────────────────────► │
       │                                          │  Nova creates VM, attaches vol
       │                                          │  → Attachment record created
       │  3. openstack server stop                │     with NFS Properties
       │     shadow-${VM_NAME}                    │
       ├────────────────────────────────────────► │
       │                                          │  Shadow VM stopped
       │                                          │  Volume + Attachment persist
```

**Why a Shadow VM?**

- Cinder's NFS driver populates the `volume attachment` properties (export path, volume filename, mount options) **only when a volume is attached to an instance**.
- The Shadow VM is a lightweight instance (`m1.small`) whose sole purpose is to trigger this attachment record creation.
- Once stopped, the Shadow VM consumes negligible resources but keeps the attachment record intact for querying.

### 4.2 Phase 2 — NFS Connection Discovery

Query the Cinder volume attachment to extract the NFS export connection info:

```
┌────────────────────────────────────────────────────────────────────────────┐
│                    PHASE 2: NFS Connection Discovery                       │
└────────────────────────────────────────────────────────────────────────────┘

  WRC Orchestrator                          Target OpenStack (Cinder API)
       │                                         │
       │  1. openstack volume attachment list     │
       │     --volume-id ${VOLUME_ID}             │
       ├────────────────────────────────────────► │
       │                                          │
       │  ◄── ATTACHMENT_ID                       │
       │                                          │
       │  2. openstack volume attachment show     │
       │     ${ATTACHMENT_ID}                     │
       ├────────────────────────────────────────► │
       │                                          │
       │  ◄── Properties JSON:                    │
       │      {                                   │
       │        "export": "192.168.57.105:/trident_pvc_xxx",
       │        "name": "volume-ba833668-xxx",    │
       │        "options": null,                  │
       │        "format": "raw",                  │
       │        "driver_volume_type": "nfs",      │
       │        "mount_point_base": "/opt/stack/data/cinder/mnt"
       │      }                                   │
```

**Extracted Information:**

| Field | Example Value | Usage |
|-------|---------------|-------|
| `export` | `192.168.57.105:/trident_pvc_xxx` | NFS server and export path for mount |
| `name` | `volume-ba833668-xxx` | Volume filename within the NFS export |
| `format` | `raw` | Confirms volume format (must be `raw` for block mode) |
| `driver_volume_type` | `nfs` | Validates this is an NFS-backed volume |
| `options` | `null` (defaults to `rw,hard,intr`) | NFS mount options |

### 4.3 Phase 3 — NFS Mount on WRCP/WRC Worker Host

Mount the NFS export on the WRCP/WRC worker host where the CDI importer pod will be scheduled:

```
┌────────────────────────────────────────────────────────────────────────────┐
│              PHASE 3: NFS Mount on WRCP/WRC Worker Host                    │
└────────────────────────────────────────────────────────────────────────────┘

  WRCP/WRC Worker Host
  ┌──────────────────────────────────────────────────────────────────────┐
  │                                                                      │
  │  1. Create mount point directory                                     │
  │     mkdir -p /var/lib/cinder-nfs/migration-${VM_NAME}-vda            │
  │                                                                      │
  │  2. Mount NFS export                                                 │
  │     mount -t nfs -o rw,hard,intr \                                   │
  │       192.168.57.105:/trident_pvc_xxx \                              │
  │       /var/lib/cinder-nfs/migration-${VM_NAME}-vda                   │
  │                                                                      │
  │  3. Verify volume file exists                                        │
  │     ls -la /var/lib/cinder-nfs/migration-${VM_NAME}-vda/             │
  │       └── volume-ba833668-xxx   (raw volume file, ${DISK_SIZE}G)     │
  │                                                                      │
  │  VOLUME_PATH=/var/lib/cinder-nfs/migration-${VM_NAME}-vda/          │
  │              volume-ba833668-xxx                                      │
  │                                                                      │
  └──────────────────────────────────────────────────────────────────────┘

  Storage Network:
  ┌────────────────────┐         NFS          ┌─────────────────────────┐
  │ WRCP Worker Host   │◄───────────────────►│ NFS Server              │
  │ (NFS Client)       │   192.168.57.x net  │ (NetApp / Cinder NFS)   │
  └────────────────────┘                      └─────────────────────────┘
```

**Mount Point Convention:** `/var/lib/cinder-nfs/migration-${VM_NAME}-${DISK_LABEL}/`

This path avoids potential SELinux issues and follows the convention used by Cinder NFS driver mounts.

### 4.4 Phase 4 — Bind Mount into CDI Importer Pod

The CDI importer pod is configured to mount the NFS volume file as a block device using Kubernetes' `hostPath` volume with bind mount:

```
┌────────────────────────────────────────────────────────────────────────────┐
│              PHASE 4: Bind Mount into CDI Importer Pod                     │
└────────────────────────────────────────────────────────────────────────────┘

  WRC K8S Cluster
  ┌──────────────────────────────────────────────────────────────────────┐
  │                                                                      │
  │  Option A: Static PV/PVC with hostPath (Recommended for MVP)         │
  │  ──────────────────────────────────────────────────────               │
  │                                                                      │
  │  PersistentVolume:                                                   │
  │    spec:                                                             │
  │      hostPath:                                                       │
  │        path: /var/lib/cinder-nfs/migration-vm1-vda/volume-xxx        │
  │        type: FileOrCreate                                            │
  │      volumeMode: Block                                               │
  │      nodeAffinity: (pin to worker with NFS mount)                    │
  │                                                                      │
  │  PersistentVolumeClaim:                                              │
  │    spec:                                                             │
  │      volumeMode: Block                                               │
  │      accessModes: [ReadWriteOnce]                                    │
  │      storageClassName: "" (static binding)                           │
  │                                                                      │
  │  CDI Importer Pod:                                                   │
  │    volumeDevices:                                                    │
  │      - name: cdi-data-vol                                            │
  │        devicePath: /dev/cdi-block-volume                             │
  │    volumes:                                                          │
  │      - name: cdi-data-vol                                            │
  │        persistentVolumeClaim:                                        │
  │          claimName: migration-vm1-vda-pvc                            │
  │                                                                      │
  │  Option B: Direct hostPath volume in Pod spec                        │
  │  ────────────────────────────────────────────                        │
  │                                                                      │
  │  CDI Importer Pod:                                                   │
  │    volumes:                                                          │
  │      - name: cdi-data-vol                                            │
  │        hostPath:                                                     │
  │          path: /var/lib/cinder-nfs/migration-vm1-vda/volume-xxx      │
  │          type: FileOrCreate                                          │
  │                                                                      │
  └──────────────────────────────────────────────────────────────────────┘
```

**Data Flow in Container:**

```
NFS Server                 WRCP Worker Host                CDI Importer Pod
─────────                  ────────────────                ────────────────
NFS Export                 NFS Mount                       Bind Mount
 └─ volume-xxx    ◄─────►  /var/lib/cinder-nfs/...  ────►  /dev/cdi-block-volume
    (raw file)                └─ volume-xxx                 (block device)
                               (raw file)
                                                            VDDK Client writes
                                                            directly to this
                                                            → NFS write-through
                                                            → Cinder volume updated
```

### 4.5 Phase 5 — Volume Finalization and VM Creation

After all CDI data transfers (full copy + precopy deltas + final cutover delta) are complete:

```
┌────────────────────────────────────────────────────────────────────────────┐
│              PHASE 5: Volume Finalization & VM Creation                    │
└────────────────────────────────────────────────────────────────────────────┘

  WRC Orchestrator                          Target OpenStack
       │                                         │
       │  1. Unmount NFS on WRCP host             │
       │     umount /var/lib/cinder-nfs/...       │
       │                                          │
       │  2. Run virt-v2v-in-place                │
       │     (inject virtio drivers into          │
       │      the volume via NFS re-mount         │
       │      or via Shadow VM)                   │
       │                                          │
       │  3. Detach volume from Shadow VM         │
       │     openstack server remove volume       │
       │     shadow-${VM_NAME} ${VOLUME_ID}       │
       ├────────────────────────────────────────► │
       │                                          │
       │  4. Delete Shadow VM                     │
       │     openstack server delete              │
       │     shadow-${VM_NAME}                    │
       ├────────────────────────────────────────► │
       │                                          │
       │  5. Set volume bootable                  │
       │     openstack volume set --bootable      │
       │     ${VOLUME_ID}                         │
       ├────────────────────────────────────────► │
       │                                          │
       │  6. Create target VM from volume         │
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
