# iSCSI-Backed Cinder Volume Direct Mount for WRCP/WRC Migration

> **Status**: Draft  
> **Version**: 0.1  
> **Date**: February 2026  
> **Authors**: WindRiver Migration Framework Team

---

## Table of Contents

- [iSCSI-Backed Cinder Volume Direct Mount for WRCP/WRC Migration](#iscsi-backed-cinder-volume-direct-mount-for-wrcpwrc-migration)
  - [Table of Contents](#table-of-contents)
  - [1. Problem Statement](#1-problem-statement)
    - [1.1 Background — NFS-Cinder CSI Driver](#11-background--nfs-cinder-csi-driver)
    - [1.2 Customer Requirement: iSCSI Storage Backend](#12-customer-requirement-iscsi-storage-backend)
    - [1.3 Network Isolation Problem (Same as NFS)](#13-network-isolation-problem-same-as-nfs)
  - [2. Proposed Solution](#2-proposed-solution)
    - [2.1 Key Insight: Cinder v3 Self-Service Attachments for iSCSI](#21-key-insight-cinder-v3-self-service-attachments-for-iscsi)
    - [2.2 Why No Shadow VM for iSCSI](#22-why-no-shadow-vm-for-iscsi)
    - [2.3 OpenStack Version Requirements](#23-openstack-version-requirements)
    - [2.4 Supported Migration Use Cases](#24-supported-migration-use-cases)
    - [2.5 Design Goals](#25-design-goals)
  - [3. Architecture Overview](#3-architecture-overview)
    - [3.1 NFS-Cinder CSI Architecture (Existing — for Comparison)](#31-nfs-cinder-csi-architecture-existing--for-comparison)
    - [3.2 Proposed Architecture (iSCSI Direct Attach on WRCP/WRC Host)](#32-proposed-architecture-iscsi-direct-attach-on-wrcpwrc-host)
    - [3.3 Architecture Comparison: NFS vs iSCSI CSI Driver](#33-architecture-comparison-nfs-vs-iscsi-csi-driver)
  - [4. Validation — CLI Proof of Concept](#4-validation--cli-proof-of-concept)
    - [4.1 Test Environment](#41-test-environment)
    - [4.2 Validated connection\_info Format (LVM iSCSI)](#42-validated-connection_info-format-lvm-iscsi)
    - [4.3 Volume Status Transitions (Validated)](#43-volume-status-transitions-validated)
    - [4.4 Key Findings](#44-key-findings)
    - [4.5 Pure Storage connection\_info (Expected)](#45-pure-storage-connection_info-expected)
  - [5. Detailed Design — CSI RPC Mapping](#5-detailed-design--csi-rpc-mapping)
    - [5.0 CSI Volume Lifecycle Reference](#50-csi-volume-lifecycle-reference)
    - [5.1 CSI Identity Service](#51-csi-identity-service)
    - [5.2 CSI Controller Service](#52-csi-controller-service)
      - [5.2.1 `CreateVolume` — Cinder Volume + Reserved Attachment](#521-createvolume--cinder-volume--reserved-attachment)
      - [5.2.2 `ControllerPublishVolume` — iSCSI Connection Discovery](#522-controllerpublishvolume--iscsi-connection-discovery)
      - [5.2.3 `ControllerUnpublishVolume` — Delete + Recreate Attachment](#523-controllerunpublishvolume--delete--recreate-attachment)
      - [5.2.4 `DeleteVolume` — Attachment Cleanup + Volume Release](#524-deletevolume--attachment-cleanup--volume-release)
    - [5.3 CSI Node Service](#53-csi-node-service)
      - [5.3.1 `NodeGetInfo` — WRCP Host Initiator Identity](#531-nodegetinfo--wrcp-host-initiator-identity)
      - [5.3.2 `NodeStageVolume` — iSCSI Login + Block Device Discovery](#532-nodestagevolume--iscsi-login--block-device-discovery)
      - [5.3.3 `NodePublishVolume` — Bind Mount Block Device into Pod](#533-nodepublishvolume--bind-mount-block-device-into-pod)
      - [5.3.4 `NodeUnpublishVolume` — Remove Pod Bind Mount](#534-nodeunpublishvolume--remove-pod-bind-mount)
      - [5.3.5 `NodeUnstageVolume` — iSCSI Logout](#535-nodeunstagevolume--iscsi-logout)
    - [5.4 Cinder Volume Status Lifecycle](#54-cinder-volume-status-lifecycle)
    - [5.5 End-to-End CSI RPC Call Sequence (CDI Multi-Phase Precopy)](#55-end-to-end-csi-rpc-call-sequence-cdi-multi-phase-precopy)
    - [5.6 Volume Finalization and VM Creation](#56-volume-finalization-and-vm-creation)
  - [6. Component Flow](#6-component-flow)
    - [6.1 End-to-End Workflow](#61-end-to-end-workflow)
      - [6.1.1 VMware → OpenStack (V2O) — CDI Multi-Phase Warm Migration](#611-vmware--openstack-v2o--cdi-multi-phase-warm-migration)
      - [6.1.2 OpenStack → OpenStack (O2O) — NBD Receiver + virsh blockcopy](#612-openstack--openstack-o2o--nbd-receiver--virsh-blockcopy)
    - [6.2 Data Path Visualization](#62-data-path-visualization)
  - [7. Implementation Details](#7-implementation-details)
    - [7.1 Driver Configuration Reference](#71-driver-configuration-reference)
  - [8. Network Architecture](#8-network-architecture)
    - [8.1 Network Topology](#81-network-topology)
    - [8.2 Network Requirements](#82-network-requirements)
  - [9. Prerequisites](#9-prerequisites)
  - [10. Risks and Mitigations](#10-risks-and-mitigations)
  - [11. Future Work](#11-future-work)
  - [Appendix A — iSCSI Initiator Configuration for Pure Storage](#appendix-a--iscsi-initiator-configuration-for-pure-storage)
    - [A.1 Background — iSCSI Initiator Identity](#a1-background--iscsi-initiator-identity)
    - [A.2 Backend Behavior — Auto-Register vs Pre-Register](#a2-backend-behavior--auto-register-vs-pre-register)
    - [A.3 Pure Storage — Required Pre-Registration Steps](#a3-pure-storage--required-pre-registration-steps)
    - [A.4 Automation with Ansible](#a4-automation-with-ansible)
    - [A.5 Troubleshooting — IQN Not Registered](#a5-troubleshooting--iqn-not-registered)
    - [A.6 RHOSO 18 Cinder Microversion Compatibility](#a6-rhoso-18-cinder-microversion-compatibility)
  - [12. References](#12-references)

---

## 1. Problem Statement

### 1.1 Background — NFS-Cinder CSI Driver

The [NFS-Cinder CSI Driver](nfs-backed-cinder-volume-for-wrcp-migration.md) (`cinder-nfs.csi.openstack.org`) was designed to solve the network isolation problem for VM migration by directly mounting NFS-backed Cinder volumes on WRCP/WRC worker hosts. It supports both VMware→OpenStack (V2O) and OpenStack→OpenStack (O2O) migration use cases.

The NFS driver relies on the fact that NFS-backed Cinder volumes are simply files on an NFS export that can be mounted by any host with network access to the NFS server. A **Shadow VM** pattern is used to trigger Cinder's NFS attachment record creation, which populates the `connection_info` with the NFS export path, volume filename, and mount options.

### 1.2 Customer Requirement: iSCSI Storage Backend

Customers are transitioning from **NetApp NFS** storage to **Pure Storage FlashArray with iSCSI**. The NFS-Cinder CSI driver cannot support iSCSI-backed Cinder volumes because:

1. **iSCSI volumes are not files on a shared filesystem** — they are block devices exported via iSCSI targets. There is no NFS export to mount.
2. **iSCSI requires initiator authentication** — the WRCP host must present its initiator IQN and (optionally) CHAP credentials to access the iSCSI target.
3. **iSCSI targets are per-attachment** — unlike NFS exports that persist regardless of who mounts them, iSCSI targets are created on-demand when Cinder creates an attachment and destroyed when the attachment is deleted.

A new CSI driver is needed that follows the same architectural pattern as the NFS driver but replaces the NFS mount path with an iSCSI initiator path.

### 1.3 Network Isolation Problem (Same as NFS)

The network isolation problem remains identical to the NFS case (see [NFS design doc, Section 1.2](nfs-backed-cinder-volume-for-wrcp-migration.md#12-network-isolation-problem)):

- CDI importer pods on an addon K8S cluster on the target OpenStack **cannot reach vCenter** on the management network.
- WRCP/WRC worker hosts have connectivity to **both** the management network (vCenter) and the storage network (iSCSI targets).
- The solution requires running CDI importer pods on the WRC K8S cluster with direct access to the Cinder volume via iSCSI.

```
┌─────────────────────┐                        ┌─────────────────────────────┐
│  Management Network │                        │  OpenStack Storage Network  │
│                     │         ✗ ISOLATED      │                             │
│  ┌───────────────┐  │      ◄─────────────►   │  ┌───────────────────────┐  │
│  │ VMware vCenter│  │                        │  │ Cinder iSCSI Target   │  │
│  │ (VDDK Source) │  │                        │  │ Pure Storage / LVM    │  │
│  └───────────────┘  │                        │  │ Portal: x.x.x.x:3260 │  │
│        │            │                        │  └───────────────────────┘  │
│        │            │                        │              │              │
└────────┼────────────┘                        └──────────────┼──────────────┘
         │                                                    │
         │  ✓ Reachable from WRCP                             │  ✓ Reachable from WRCP
         │                                                    │
┌────────┴────────────────────────────────────────────────────┴──────────────┐
│                         WRCP / WRC Platform                                │
│  ┌──────────────────────────────────────────────────────────────────────┐  │
│  │  CDI Importer Pod on WRC K8S                                        │  │
│  │  ✓ Can reach vCenter (VDDK)                                         │  │
│  │  ✓ Can reach iSCSI target via iscsiadm on WRCP host                 │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
└───────────────────────────────────────────────────────────────────────────┘
```

---

## 2. Proposed Solution

### 2.1 Key Insight: Cinder v3 Self-Service Attachments for iSCSI

The Cinder **v3 Attachments API** (microversion **3.27+**) supports creating volume attachments **without a Nova instance**. This means the WRCP host can directly request an iSCSI target export from Cinder by providing its initiator connector information (IQN, IP, hostname). Cinder communicates with the storage backend (LVM tgtd, Pure Storage FlashArray, etc.) to create the iSCSI target and returns the complete connection information.

**The flow is:**

```
  CSI Driver                              Cinder v3 API                    Storage Backend
  ──────────                              ──────────────                   ───────────────
       │                                        │                               │
       │  1. POST /v3/attachments               │                               │
       │     { volume_uuid: "xxx" }             │                               │
       ├───────────────────────────────────────►│                               │
       │  ◄── attachment_id, status: "reserved" │                               │
       │                                        │                               │
       │  2. PUT /v3/attachments/{id}           │                               │
       │     { connector: {                     │                               │
       │         initiator: "iqn.xxx",          │                               │
       │         ip: "10.0.0.1",                │   initialize_connection()     │
       │         host: "worker-3"               │──────────────────────────────►│
       │     }}                                 │                               │
       │                                        │  ◄── target_portal, target_iqn│
       │  ◄── connection_info:                  │      target_lun, CHAP creds   │
       │      { target_portal, target_iqn,      │                               │
       │        target_lun, auth_* }            │                               │
       │                                        │                               │
       │  3. WRCP host: iscsiadm login ─────────┼──────────────────────────────►│
       │     ← /dev/sdc (block device)          │                               │
       │                                        │                               │
       │  4. DELETE /v3/attachments/{id}        │                               │
       │     (cleanup)                          │   terminate_connection()      │
       ├───────────────────────────────────────►│──────────────────────────────►│
       │                                        │   iSCSI target removed        │
```

This approach **eliminates the need for a Shadow VM entirely** — the Cinder v3 Attachment API provides the same two capabilities:

| Capability | Shadow VM (NFS driver) | Cinder v3 Attachment (iSCSI driver) |
|-----------|----------------------|-------------------------------------|
| **Trigger `connection_info` creation** | Nova attach → Cinder populates NFS export info | `PUT /v3/attachments/{id}` with connector → Cinder populates iSCSI target info |
| **Volume lock (`in-use` status)** | Shadow VM attachment → `in-use` | Reserved attachment → `reserved`; completed attachment → `in-use` |

### 2.2 Why No Shadow VM for iSCSI

| Aspect | Shadow VM (NFS) | Cinder v3 Attachment (iSCSI) |
|--------|----------------|------------------------------|
| **OpenStack resources** | Nova VM (flavor, network, compute quota) | Cinder attachment record only |
| **Compute quota** | Consumes 1 instance + vCPU + RAM | Zero compute resources |
| **Lifecycle complexity** | Create → wait ACTIVE → stop → wait SHUTOFF → later detach + delete | Create attachment → update connector → later delete attachment |
| **Nova dependency** | Requires Nova API access | Cinder API only (no Nova) |
| **Cleanup risk** | Orphaned Shadow VMs if driver crashes | Minimal. The Cinder attachment is a lightweight metadata record (no compute resources). It is cleaned up by the standard K8S CSI pod/PVC lifecycle (see [K8S CSI Architecture Reference, §7](kubernetes-csi-architecture-reference.md#7-pod-lifecycle--volume-attachdetach-cycling)): when a migration pod is deleted, the AD Controller deletes the `VolumeAttachment` CR → external-attacher calls `ControllerUnpublishVolume` → driver deletes the Cinder attachment and recreates a reserved one for the next stage. When the PVC is finally deleted (migration complete), external-provisioner calls `DeleteVolume` → driver deletes any remaining attachment → volume becomes `available` and is handed off to Blueprint to create the target VM (Nova creates its own attachment to the compute host at that point). Even if the driver crashes mid-operation, only a stale `reserved` attachment record remains — no orphaned VMs or compute resources. |
| **Time to provision** | 30-120s (boot + stop VM) | <5s (API call only) |

### 2.3 OpenStack Version Requirements

The Cinder v3 self-service attachment API has specific version requirements:

| Feature | Minimum Microversion | Minimum OpenStack Release | Notes |
|---------|---------------------|--------------------------|-------|
| `POST /v3/attachments` (create without server) | **3.27** | **Queens (2018.1)** | Required. Allows attachment without Nova instance UUID. |
| `PUT /v3/attachments/{id}` (update connector) | **3.27** | **Queens (2018.1)** | Required. Sends initiator connector, returns `connection_info`. |
| `POST /v3/attachments/{id}/action` (`os-complete`) | **3.44** | **Stein (2019.1)** | Optional. Transitions attachment from reserved → attached. May not be required for iSCSI login — backend creates target during `PUT` connector. |

**Tested and validated on:** DevStack 2024.2 with LVM iSCSI backend (see [Section 4](#4-phase-0-validation--cli-proof-of-concept)).

> **TODO: Shadow VM Fallback**
>
> For OpenStack deployments older than Queens (pre-3.27) that do not support self-service
> attachments, implement a Shadow VM fallback path identical to the NFS driver. The Shadow VM
> would boot from the iSCSI volume, triggering Cinder to create the attachment record with
> `connection_info`. The CSI driver would then query the attachment to extract iSCSI target
> details.
>
> This fallback is deferred to a future release. The initial implementation requires
> Cinder microversion 3.27+ and will fail fast with a clear error if the target OpenStack
> does not support this microversion.

### 2.4 Supported Migration Use Cases

This design supports the **same two migration use cases** as the NFS-Cinder CSI driver:

| Use Case | Data Transfer Tool | Data Writer | iSCSI Mount Location | Description |
|----------|-------------------|-------------|---------------------|-------------|
| **VMware → OpenStack (V2O)** | CDI importer pod (VDDK multi-phase warm migration) | CDI pod on WRC K8S | WRCP/WRC worker host (via CSI Node RPCs) | CDI runs full copy + N precopy delta stages. Each stage may create/destroy importer pods. CSI must survive pod cycling without losing iSCSI target metadata. |
| **OpenStack → OpenStack (O2O)** | NBD receiver pod + `virsh blockcopy` | NBD receiver pod on WRC K8S (writes to PVC via NBD) | WRCP/WRC worker host (via CSI Node RPCs) | Blueprint launches a long-running NBD receiver pod that mounts the PVC/PV (backed by this CSI driver) and exposes it via the NBD protocol. Source libvirt `virsh blockcopy` targets the NBD endpoint to mirror the source VM's vda to the Cinder volume. |

### 2.5 Design Goals

| Goal | Description |
|------|-------------|
| **Support iSCSI-backed Cinder storage** | Enable migration workflows with Pure Storage FlashArray (iSCSI) and LVM iSCSI backends |
| **Eliminate Shadow VM dependency** | Use Cinder v3 self-service attachments instead of Shadow VMs — no Nova compute resources consumed |
| **Eliminate addon K8S cluster dependency** | Same as NFS driver — CDI runs on WRC K8S with direct iSCSI access |
| **Resolve network isolation** | CDI importer pods on WRCP/WRC have native access to the vCenter management network |
| **Support CDI multi-phase precopy** | iSCSI attachment must persist across CDI pod restarts between full copy and precopy stages |
| **Direct-to-Cinder writes** | Data written via iSCSI lands directly in the Cinder volume — zero copy at cutover |
| **Minimal cutover downtime** | Final delta sync + driver injection is all that remains in the cutover window |
| **Clean volume handoff** | After PVC deletion, the Cinder volume is `available` with no attachments — ready for Blueprint to create target VM |
| **Compatible with warm migration workflow** | Integrate with existing CDI multi-stage VDDK import and WRC blueprint orchestration |
| **Multi-worker cluster support** | CSI spec passes node identity through RPCs — driver correctly handles pod scheduling to any WRCP worker |

---

## 3. Architecture Overview

### 3.1 NFS-Cinder CSI Architecture (Existing — for Comparison)

**V2O (VMware → OpenStack) — CDI VDDK Import:**

```
┌──────────────────┐       ┌────────────────────────────────────────────────┐
│  VMware vCenter  │       │  Target OpenStack                              │
│  (Mgmt Network)  │       │                                                │
│  ┌────────────┐  │       │  ┌────────────────────────────────────┐        │
│  │ Source VM   │  │       │  │  Cinder (NFS Backend — NetApp)     │        │
│  │ VMDK Disks  │  │       │  │  NFS Export: 10.0.0.5:/vol/xxx     │        │
│  └────────────┘  │       │  └────────────────────────────────────┘        │
│        │ VDDK    │       │               │ NFS                            │
└────────┼─────────┘       └───────────────┼────────────────────────────────┘
         │                                 │
┌────────┴─────────────────────────────────┴────────────────────────────────┐
│                    WRCP / WRC Platform                                     │
│  ┌──────────────────────────────────────────────────────────────────────┐ │
│  │  CDI Importer Pod → mount -t nfs → volume file → Cinder volume      │ │
│  └──────────────────────────────────────────────────────────────────────┘ │
└──────────────────────────────────────────────────────────────────────────┘
```

**O2O (OpenStack → OpenStack) — NBD Receiver + virsh blockcopy:**

```
┌──────────────────────┐   ┌────────────────────────────────────────────────┐
│  Source OpenStack     │   │  Target OpenStack                              │
│  (Compute Host)       │   │                                                │
│  ┌────────────────┐   │   │  ┌────────────────────────────────────┐        │
│  │ Source VM       │   │   │  │  Cinder (NFS Backend — NetApp)     │        │
│  │ (libvirt/QEMU)  │   │   │  │  NFS Export: 10.0.0.5:/vol/xxx     │        │
│  │ vda (disk)      │   │   │  └────────────────────────────────────┘        │
│  └────────┬───────┘   │   │               │ NFS                            │
│           │ virsh     │   └───────────────┼────────────────────────────────┘
│           │ blockcopy │                   │
└───────────┼───────────┘                   │
            │ NBD                           │
┌───────────┴───────────────────────────────┴────────────────────────────────┐
│                    WRCP / WRC Platform                                     │
│  ┌──────────────────────────────────────────────────────────────────────┐  │
│  │  NBD Receiver Pod → mount -t nfs → volume file → Cinder volume      │  │
│  │  Listens on nbd://<pod-ip>:10809/export                             │  │
│  │  Source libvirt mirrors disk blocks → NBD → NFS → Cinder volume     │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
└───────────────────────────────────────────────────────────────────────────┘
```

### 3.2 Proposed Architecture (iSCSI Direct Attach on WRCP/WRC Host)

**V2O (VMware → OpenStack) — CDI VDDK Import:**

```
┌──────────────────┐       ┌────────────────────────────────────────────────┐
│  VMware vCenter  │       │  Target OpenStack                              │
│  (Mgmt Network)  │       │                                                │
│  ┌────────────┐  │       │  ┌────────────────────────────────────┐        │
│  │ Source VM   │  │       │  │  Cinder (iSCSI Backend)            │        │
│  │ VMDK Disks  │  │       │  │                                    │        │
│  └────────────┘  │       │  │  Pure Storage FlashArray            │        │
│        │         │       │  │  ──── OR ────                       │        │
│        │ VDDK    │       │  │  LVM iSCSI (tgtd/lioadm)           │        │
│        │         │       │  │                                    │        │
│        │         │       │  │  iSCSI Target:                     │        │
│        │         │       │  │  Portal: 10.0.0.1:3260             │        │
│        │         │       │  │  IQN: iqn.2010-10.org.openstack... │        │
│        │         │       │  │  LUN: 0                            │        │
│        │         │       │  └────────────────────────────────────┘        │
└────────┼─────────┘       │               │                                │
         │                 └───────────────┼────────────────────────────────┘
         │                                 │ iSCSI (TCP 3260)
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
│  │     │    │  iSCSI Session:                                  │    │     │
│  │     │    │  iscsiadm login → /dev/sdc (block device)        │    │     │
│  │     │    │                                                  │    │     │
│  │     │    │  /dev/disk/by-path/                              │    │     │
│  │     │    │    ip-10.0.0.1:3260-iscsi-iqn.xxx-lun-0 → sdc   │    │     │
│  │     │    └──────────────────────────────────────────────────┘    │     │
│  │     │                       │ Bind Mount                         │     │
│  │     │                       ▼                                    │     │
│  │     │    ┌──────────────────────────────────────────────────┐    │     │
│  │     ▼    │  CDI Importer Pod                                │    │     │
│  │  ┌──────────┐                                               │    │     │
│  │  │ VDDK     │  Writes to → /dev/cdi-block-volume            │    │     │
│  │  │ Client   │  (bind-mounted from iSCSI block device)       │    │     │
│  │  └──────────┘                                               │    │     │
│  │          │   └──────────────────────────────────────────────┘    │     │
│  └──────────┼──────────────────────────────────────────────────────┘     │
│             │                                                            │
│     ✓ Has access to vCenter (Mgmt Network)                               │
│     ✓ Has access to iSCSI storage (Storage Network, TCP 3260)            │
└──────────────────────────────────────────────────────────────────────────┘
```

**O2O (OpenStack → OpenStack) — NBD Receiver + virsh blockcopy:**

```
┌──────────────────────┐   ┌────────────────────────────────────────────────┐
│  Source OpenStack     │   │  Target OpenStack                              │
│  (Compute Host)       │   │                                                │
│  ┌────────────────┐   │   │  ┌────────────────────────────────────┐        │
│  │ Source VM       │   │   │  │  Cinder (iSCSI Backend)            │        │
│  │ (libvirt/QEMU)  │   │   │  │                                    │        │
│  │ vda (disk)      │   │   │  │  Pure Storage FlashArray            │        │
│  └────────┬───────┘   │   │  │  ──── OR ────                       │        │
│           │ virsh     │   │  │  LVM iSCSI (tgtd/lioadm)           │        │
│           │ blockcopy │   │  │                                    │        │
│           │           │   │  │  iSCSI Target:                     │        │
│           │           │   │  │  Portal: 10.0.0.1:3260             │        │
│           │           │   │  │  IQN: iqn.2010-10.org.openstack... │        │
│           │           │   │  │  LUN: 0                            │        │
│           │           │   │  └────────────────────────────────────┘        │
└───────────┼───────────┘   │               │                                │
            │ NBD           └───────────────┼────────────────────────────────┘
            │                               │ iSCSI (TCP 3260)
            │                               │
┌───────────┼───────────────────────────────┼────────────────────────────────┐
│           │           WRCP / WRC Platform  │                                │
│           │                               │                                │
│  ┌────────┼───────────────────────────────┼──────────────────────────┐     │
│  │        │         WRC K8S Cluster        │                          │     │
│  │        │                               ▼                          │     │
│  │        │    ┌──────────────────────────────────────────────────┐   │     │
│  │        │    │  WRCP/WRC Worker Host                            │   │     │
│  │        │    │                                                  │   │     │
│  │        │    │  iSCSI Session:                                  │   │     │
│  │        │    │  iscsiadm login → /dev/sdc (block device)        │   │     │
│  │        │    │                                                  │   │     │
│  │        │    │  /dev/disk/by-path/                              │   │     │
│  │        │    │    ip-10.0.0.1:3260-iscsi-iqn.xxx-lun-0 → sdc   │   │     │
│  │        │    └──────────────────────────────────────────────────┘   │     │
│  │        │                       │ Bind Mount                        │     │
│  │        │                       ▼                                   │     │
│  │        │    ┌──────────────────────────────────────────────────┐   │     │
│  │        ▼    │  NBD Receiver Pod                                │   │     │
│  │  ┌────────────┐                                               │   │     │
│  │  │ NBD Server │  /dev/nbd-block-volume                        │   │     │
│  │  │ Listens on │  (bind-mounted from iSCSI block device)       │   │     │
│  │  │ :10809     │  Exposes block device via NBD protocol        │   │     │
│  │  └────────────┘                                               │   │     │
│  │          │   └──────────────────────────────────────────────────┘   │     │
│  └──────────┼──────────────────────────────────────────────────────────┘     │
│             │                                                            │
│     ✓ Source libvirt mirrors disk → NBD → iSCSI block device → Cinder    │
│     ✓ Has access to iSCSI storage (Storage Network, TCP 3260)            │
└──────────────────────────────────────────────────────────────────────────┘
```

### 3.3 Architecture Comparison: NFS vs iSCSI CSI Driver

| Aspect | NFS Driver (`cinder-nfs.csi.openstack.org`) | iSCSI Driver (`cinder-iscsi.csi.openstack.org`) |
|--------|----------------------------------------------|--------------------------------------------------|
| **Volume lock mechanism** | Shadow VM (Nova instance) | Cinder v3 attachment (no Nova) |
| **Volume attach mechanism** | NFS mount on WRCP host | iSCSI initiator login on WRCP host |
| **Block device presentation** | Volume file on NFS export, bind-mounted | `/dev/sdX` block device, bind-mounted |
| **Data path** | CDI → NFS file → Cinder volume | CDI → iSCSI block device → Cinder volume |
| **Host dependencies** | `nfs-utils` / `nfs-common` | `open-iscsi` / `iscsiadm` |
| **OpenStack API dependency** | Nova + Cinder (Shadow VM) | Cinder only (v3 attachment, microversion 3.27+) |
| **Compute quota consumed** | 1 VM (Shadow VM, stopped) | Zero |
| **Provisioning time** | 30-120s (boot + stop Shadow VM) | <5s (API call) |
| **Multi-phase CDI support** | `ControllerUnpublishVolume` = no-op (Shadow VM persists) | `ControllerUnpublishVolume` = delete + recreate attachment for new node |
| **Cinder volume types** | NFS-backed only | iSCSI-backed only (LVM, Pure Storage, etc.) |
| **Access mode** | Shared filesystem (file-level access) | Exclusive block device access |

---

## 4. Validation — CLI Proof of Concept

The following Phase 0 validation was performed to prove the Cinder v3 self-service attachment → iSCSI login → block device theory before committing to implementation.

### 4.1 Test Environment

| Component | Details |
|-----------|---------|
| **OpenStack release** | DevStack 2024.2 |
| **Cinder backend** | LVM iSCSI (`lvmdriver-1`) with tgtd |
| **Cinder microversion** | 3.27 |
| **Host OS** | Ubuntu (devstack host, also acting as WRCP worker) |
| **Initiator IQN** | `iqn.2016-04.com.open-iscsi:1f98f85d8ef4` |

### 4.2 Validated connection\_info Format (LVM iSCSI)

**Step 1: Create Cinder volume**
```bash
openstack volume create --size 10 --type lvmdriver-1 --availability-zone nova iscsi-csi-test-vol
# Volume ID: bf39da68-f886-4733-8e21-d6099228429b
# Status: available
```

**Step 2: Create attachment (reserved, no connector, no server)**
```bash
curl -s -X POST "$CINDER_EP/attachments" \
  -H "X-Auth-Token: $TOKEN" \
  -H "Content-Type: application/json" \
  -H "OpenStack-API-Version: volume 3.27" \
  -d '{"attachment": {"volume_uuid": "bf39da68-f886-4733-8e21-d6099228429b"}}'
```

Response:
```json
{
    "attachment": {
        "id": "65506296-6e06-4fcb-8874-a4caf2ef4c20",
        "status": "reserved",
        "instance": null,
        "volume_id": "bf39da68-f886-4733-8e21-d6099228429b",
        "attached_at": "",
        "detached_at": "",
        "attach_mode": "null",
        "connection_info": {}
    }
}
```

Volume status after: **`reserved`**

**Step 3: Update attachment with connector**
```bash
curl -s -X PUT "$CINDER_EP/attachments/$ATTACH_ID" \
  -H "X-Auth-Token: $TOKEN" \
  -H "Content-Type: application/json" \
  -H "OpenStack-API-Version: volume 3.27" \
  -d '{
    "attachment": {
      "connector": {
        "initiator": "iqn.2016-04.com.open-iscsi:1f98f85d8ef4",
        "ip": "69.167.148.33",
        "host": "controller-0",
        "multipath": false,
        "platform": "x86_64",
        "os_type": "linux2"
      }
    }
  }'
```

Response — **`connection_info` with full iSCSI target details**:
```json
{
    "attachment": {
        "id": "65506296-6e06-4fcb-8874-a4caf2ef4c20",
        "status": "reserved",
        "instance": null,
        "volume_id": "bf39da68-f886-4733-8e21-d6099228429b",
        "attached_at": "",
        "detached_at": "",
        "attach_mode": "null",
        "connection_info": {
            "target_discovered": false,
            "target_portal": "69.167.149.97:3260",
            "target_iqn": "iqn.2010-10.org.openstack:volume-bf39da68-f886-4733-8e21-d6099228429b",
            "target_lun": 0,
            "volume_id": "bf39da68-f886-4733-8e21-d6099228429b",
            "auth_method": "CHAP",
            "auth_username": "Hkh2UcACt9zoUxYjnz4U",
            "auth_password": "trtMa3STYUiMJT7K",
            "encrypted": false,
            "qos_specs": null,
            "access_mode": "rw",
            "cacheable": false,
            "driver_volume_type": "iscsi",
            "attachment_id": "65506296-6e06-4fcb-8874-a4caf2ef4c20",
            "enforce_multipath": false
        }
    }
}
```

**Step 4: iSCSI discovery, CHAP auth, login — all successful**
```
$ sudo iscsiadm -m discovery -t sendtargets -p "69.167.149.97:3260"
69.167.149.97:3260,1 iqn.2010-10.org.openstack:volume-bf39da68-f886-4733-8e21-d6099228429b

$ sudo iscsiadm -m node -T "$TARGET_IQN" -p "$TARGET_PORTAL" --login
Login to [iface: default, target: iqn..., portal: 69.167.149.97,3260] successful.

$ ls -la /dev/disk/by-path/ip-69.167.149.97:3260-iscsi-iqn...-lun-0
lrwxrwxrwx 1 root root 9 ... -> ../../sdc

$ lsblk /dev/sdc
NAME MAJ:MIN RM SIZE RO TYPE MOUNTPOINT
sdc    8:32   0  10G  0 disk
```

**Step 5: I/O test — write and read successful**

Block device is fully functional for I/O. Write and readback verified.

### 4.3 Volume Status Transitions (Validated)

| Action | Volume Status | Notes |
|--------|--------------|-------|
| `POST /v3/volumes` (create) | `creating` → `available` | Normal Cinder lifecycle |
| `POST /v3/attachments` (no connector) | `available` → `reserved` | Attachment acts as lock |
| `PUT /v3/attachments/{id}` (set connector) | `reserved` | Backend creates iSCSI target. Status stays `reserved`. |
| `POST /v3/attachments/{id}/action` (`os-complete`) | `reserved` → `in-use` | Requires microversion **3.44**. Returns 404 on 3.27. |
| `iscsiadm --login` | `reserved` (or `in-use`) | iSCSI login works in both states |
| `iscsiadm --logout` | (no change) | Cinder is unaware of iSCSI session state |
| `DELETE /v3/attachments/{id}` | → `available` | Backend removes iSCSI target. Volume released. |

**Key finding:** iSCSI login works even when the volume is in `reserved` status (after `PUT` connector, before `os-complete`). The `os-complete` action (microversion 3.44) is optional for iSCSI functionality but recommended for correct volume status reporting.

### 4.4 Key Findings

| Finding | Impact |
|---------|--------|
| **Cinder v3 serverless attachment works** | No Shadow VM needed. `POST /v3/attachments` without `instance` field succeeds. |
| **`connection_info` populated on `PUT` connector** | All iSCSI target details (portal, IQN, LUN, CHAP) returned immediately. |
| **iSCSI target created during `PUT` connector** | Backend (LVM tgtd) creates the target on connector update, not on `os-complete`. |
| **`os-complete` requires microversion 3.44** | Returns 404 on 3.27. Not strictly required for iSCSI — login works without it. |
| **Volume status = `reserved` acts as lock** | Prevents accidental deletion (`openstack volume delete` on `reserved` → rejected). |
| **CHAP auth required (LVM)** | `auth_method: "CHAP"` with auto-generated username/password. |
| **Device path predictable** | `/dev/disk/by-path/ip-<portal>-iscsi-<iqn>-lun-<N>` — unique and deterministic. |
| **Data persists across attachment delete + recreate** | Verified: write data → delete attachment → create new attachment → re-login → data intact. Volume data lives on LVM LV, unaffected by attachment lifecycle. |

### 4.5 Pure Storage connection\_info (Expected)

> **TODO:** Validate against Pure Storage FlashArray in production environment.

Expected differences from LVM:

| Field | LVM iSCSI | Pure Storage (expected) |
|-------|-----------|------------------------|
| `target_portal` | Single portal | Single portal (primary) |
| `target_portals` | Not present | Array of portals (multipath) |
| `target_iqn` | Single IQN | Single IQN (primary) |
| `target_iqns` | Not present | Array of IQNs (multipath) |
| `target_lun` | `0` | Non-zero (Pure assigns dynamically) |
| `target_luns` | Not present | Array of LUNs (multipath) |
| `auth_method` | `"CHAP"` | `null` or `"CHAP"` (configurable) |
| `discard` | Not present | `true` (TRIM/UNMAP support) |

---

## 5. Detailed Design — CSI RPC Mapping

### 5.0 CSI Volume Lifecycle Reference

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

The table below summarizes how each CSI RPC maps to OpenStack operations for NFS vs iSCSI:

| CSI RPC | NFS Driver (`cinder-nfs.csi.openstack.org`) | iSCSI Driver (`cinder-iscsi.csi.openstack.org`) |
|---------|----------------------------------------------|--------------------------------------------------|
| **`CreateVolume`** | Cinder create vol + Shadow VM (Nova) + stop | Cinder create vol + create attachment (reserved, no connector) |
| **`DeleteVolume`** | Detach Shadow VM + delete Shadow VM | Delete remaining Cinder attachment + release volume to `available` (final cleanup; triggered by `external-provisioner` when PVC deleted) |
| **`ControllerPublishVolume`** | Query Shadow VM attachment → NFS export | Update attachment connector → iSCSI target info |
| **`ControllerUnpublishVolume`** | **No-op** (Shadow VM persists) | Delete attachment + recreate (reserved) for next stage |
| **`NodeStageVolume`** | `mount -t nfs` NFS export → staging | `iscsiadm` login → `/dev/sdX` block device |
| **`NodeUnstageVolume`** | `umount` NFS | `iscsiadm` logout + node DB cleanup |
| **`NodePublishVolume`** | Bind mount NFS volume file → target | Bind mount block device → target |
| **`NodeUnpublishVolume`** | `umount` bind mount | `umount` bind mount |
| **`NodeGetInfo`** | WRCP hostname + topology | WRCP hostname + initiator IQN + IP |

### 5.1 CSI Identity Service

**`GetPluginInfo`** returns:

| Field | Value |
|-------|-------|
| `name` | `cinder-iscsi.csi.openstack.org` |
| `vendor_version` | `1.0.0` |

**`GetPluginCapabilities`** advertises:

| Capability | Type | Rationale |
|------------|------|-----------|
| `CONTROLLER_SERVICE` | PluginCapability.Service | Driver implements Controller RPCs for volume provisioning and iSCSI connection discovery |
| `VOLUME_ACCESSIBILITY_CONSTRAINTS` | PluginCapability.Service | Volumes are only accessible from nodes with iSCSI storage network access |

**`Probe`** verifies:
- OpenStack credentials are valid (Keystone token obtainable)
- Cinder API supports microversion 3.27+
- iSCSI initiator tools (`iscsiadm`) are installed on the node (for Node plugin)
- `iscsid` service is running on the node

### 5.2 CSI Controller Service

The Controller plugin runs on the WRC K8S cluster. It communicates with the target OpenStack Cinder API (not Nova) over HTTPS.

**Controller Service Capabilities** (`ControllerGetCapabilities`):

| Capability | Supported | Notes |
|------------|-----------|-------|
| `CREATE_DELETE_VOLUME` | Yes | Provisions Cinder iSCSI volumes + attachment lifecycle |
| `PUBLISH_UNPUBLISH_VOLUME` | Yes | Discovers iSCSI connection info via Cinder v3 attachment connector |
| `LIST_VOLUMES` | Yes | Lists Cinder volumes |
| `EXPAND_VOLUME` | Yes | Delegates to Cinder `os-extend` API |
| `CREATE_DELETE_SNAPSHOT` | No | Not required for migration use case |
| `CLONE_VOLUME` | No | Not required for migration use case |

#### 5.2.1 `CreateVolume` — Cinder Volume + Reserved Attachment

**CSI Spec Reference:** *"A Controller Plugin MUST implement this RPC call if it has `CREATE_DELETE_VOLUME` controller capability."*

The `CreateVolume` RPC creates the Cinder volume and a **reserved attachment** (no connector). The reserved attachment acts as a volume lock — Cinder sets the volume status to `reserved`, preventing accidental deletion or double-attachment.

**K8S Calling Chain:** PVC created → `external-provisioner` sidecar detects unbound PVC → calls `CreateVolume` → PV created and bound to PVC.

**Request → Response Mapping:**

| CSI Field | Source / Value |
|-----------|----------------|
| `req.Name` | `migration-${VM_NAME}-${DISK_LABEL}` (idempotency key) |
| `req.CapacityRange.RequiredBytes` | Source VM disk size |
| `req.Parameters["type"]` | `pure-iscsi` or `lvmdriver-1` (StorageClass parameter) |
| `req.Parameters["availability"]` | Target AZ |
| `resp.Volume.VolumeId` | Cinder volume UUID |
| `resp.Volume.VolumeContext["attachment_id"]` | Cinder attachment UUID |

**Implementation Flow:**

```
┌────────────────────────────────────────────────────────────────────────────┐
│          CreateVolume RPC — Controller Plugin                              │
└────────────────────────────────────────────────────────────────────────────┘

  CSI Controller Plugin                     Target OpenStack (Cinder API)
       │                                         │
       │  1. Idempotency check:                   │
       │     cloud.GetVolumesByName(req.Name)     │
       │     → Cinder GET /v3/volumes?name=...    │
       ├────────────────────────────────────────► │
       │     If found: return existing vol + attachment
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
       │  ◄── VOLUME_ID                           │
       │                                          │
       │  3. Wait for volume available:           │
       │     WaitVolumeTargetStatus("available")   │
       │                                          │
       │  4. Create reserved attachment:          │
       │     cloud.CreateAttachment(VOLUME_ID)    │
       │     → POST /v3/attachments               │
       │       { volume_uuid: VOLUME_ID }         │
       │       (no instance, no connector)        │
       ├────────────────────────────────────────► │
       │  ◄── ATTACHMENT_ID                       │  Volume status → "reserved"
       │                                          │
       │  5. Store attachment_id in Cinder        │
       │     volume metadata:                     │
       │     cloud.SetVolumeMetadata(VOLUME_ID, { │
       │       "csi.attachment_id": ATTACHMENT_ID │
       │     })                                   │
       │     → Cinder PUT /v3/volumes/{id}/metadata
       ├────────────────────────────────────────► │
       │                                          │
       │  6. Return CreateVolumeResponse:         │
       │     Volume.VolumeId = VOLUME_ID          │
       │     Volume.VolumeContext = {             │
       │       "attachment_id": ATTACHMENT_ID     │
       │     }                                    │
```

**Why create the attachment in CreateVolume?**

- Acts as a **volume lock**: volume status changes to `reserved`, preventing deletion or double-allocation.
- Ensures an attachment record exists for `ControllerPublishVolume` to update with connector info.
- Lightweight: no Nova instance, no compute resources, <5s API call.

#### 5.2.2 `ControllerPublishVolume` — iSCSI Connection Discovery

**CSI Spec Reference:** *"This RPC will be called by the CO when it wants to place a workload that uses the volume onto a node."*

**K8S Calling Chain:** Pod scheduled to node → AD Controller creates `VolumeAttachment` CR → `external-attacher` sidecar calls `ControllerPublishVolume` with the node's identity (`NodeId` from `NodeGetInfo`).

In the NFS driver, this RPC queries the Shadow VM attachment for NFS export info. In the iSCSI driver, it **updates the Cinder attachment with the target node's initiator connector** — Cinder communicates with the storage backend to create the iSCSI target and returns the full connection info.

**Request → Response Mapping:**

| CSI Field | Source / Value |
|-----------|----------------|
| `req.VolumeId` | Cinder volume UUID |
| `req.NodeId` | `hostname;iqn.xxx;ip` (from `NodeGetInfo`) |
| `req.VolumeContext["attachment_id"]` | Attachment UUID (from `CreateVolume`) |
| `resp.PublishContext["target_portal"]` | `69.167.149.97:3260` |
| `resp.PublishContext["target_iqn"]` | `iqn.2010-10.org.openstack:volume-xxx` |
| `resp.PublishContext["target_lun"]` | `0` |
| `resp.PublishContext["auth_method"]` | `CHAP` |
| `resp.PublishContext["auth_username"]` | `Hkh2UcACt9zoUxYjnz4U` |
| `resp.PublishContext["auth_password"]` | `trtMa3STYUiMJT7K` |
| `resp.PublishContext["driver_volume_type"]` | `iscsi` |

**Implementation Flow:**

```
┌────────────────────────────────────────────────────────────────────────────┐
│      ControllerPublishVolume RPC — Controller Plugin                       │
└────────────────────────────────────────────────────────────────────────────┘

  CSI Controller Plugin                     Target OpenStack (Cinder API)
       │                                         │
       │  1. Parse node identity:                 │
       │     parts = split(req.NodeId, ";")       │
       │     host = parts[0]  ("worker-3")        │
       │     iqn  = parts[1]  ("iqn.xxx")         │
       │     ip   = parts[2]  ("10.0.0.103")      │
       │                                          │
       │  2. Get attachment_id from volume context │
       │     or volume metadata:                  │
       │     attachmentID = req.VolumeContext      │
       │       ["attachment_id"]                  │
       │     OR: cloud.GetVolume(req.VolumeId)     │
       │       → metadata["csi.attachment_id"]    │
       │                                          │
       │  3. Update attachment with connector:    │
       │     cloud.UpdateAttachmentConnector(      │
       │       attachmentID,                      │
       │       connector{                         │
       │         initiator: iqn,                  │
       │         ip: ip,                          │
       │         host: host,                      │
       │         multipath: false,                │
       │         platform: "x86_64",              │
       │         os_type: "linux2"                │
       │       }                                  │
       │     )                                    │
       │     → PUT /v3/attachments/{id}           │
       ├────────────────────────────────────────►│
       │                                          │  Cinder calls backend:
       │                                          │  initialize_connection()
       │                                          │  → Backend creates iSCSI
       │                                          │    target for this IQN
       │  ◄── connection_info:                    │
       │      {                                   │
       │        "target_portal": "x.x.x.x:3260", │
       │        "target_iqn": "iqn.xxx",          │
       │        "target_lun": 0,                  │
       │        "auth_method": "CHAP",            │
       │        "auth_username": "xxx",           │
       │        "auth_password": "xxx",           │
       │        "driver_volume_type": "iscsi"     │
       │      }                                   │
       │                                          │
       │  4. Optionally complete attachment:       │
       │     If microversion >= 3.44:             │
       │       POST /v3/attachments/{id}/action   │
       │       { "os-complete": {} }              │
       │     Volume status → "in-use"             │
       │                                          │
       │  5. Validate driver_volume_type:         │
       │     if != "iscsi" → INVALID_ARGUMENT     │
       │                                          │
       │  6. Return PublishContext with            │
       │     iSCSI connection details             │
```

**Key Design Decision:** The `publish_context` map returned here is passed by the CO (kubelet) to `NodeStageVolume` and `NodePublishVolume`. This is the standard CSI controller-to-node communication mechanism. The `publish_context` carries the iSCSI target details, including authentication credentials.

> **Security Note:** `auth_password` is passed in `publish_context` which is stored in the Kubernetes `VolumeAttachment` object. This is the same pattern used by other iSCSI CSI drivers (e.g., democratic-csi, HPE CSI). Access is restricted to the kubelet and CSI node plugin on the target node.

#### 5.2.3 `ControllerUnpublishVolume` — Delete + Recreate Attachment

**CSI Spec Reference:** *"This RPC is a reverse operation of ControllerPublishVolume."*

**K8S Calling Chain:** Pod deleted → kubelet calls `NodeUnpublishVolume` + `NodeUnstageVolume` → AD Controller deletes `VolumeAttachment` CR → `external-attacher` sidecar calls `ControllerUnpublishVolume`.

**NFS vs iSCSI — Critical Difference:**

In the NFS driver, `ControllerUnpublishVolume` is a **no-op** because:
- NFS mounts are shared — any host can mount the same export.
- The Shadow VM attachment persists, providing the same NFS info to any node.

For iSCSI, this is **not possible**. iSCSI targets are **per-initiator** — if CDI stage N ran on Worker-3 (IQN-A) and stage N+1 is scheduled to Worker-1 (IQN-B), the Cinder attachment connector must be updated with IQN-B. However, Cinder may not support updating a connector on an already-connected attachment.

**Solution: Delete the old attachment and recreate a reserved one.**

```
  CDI Stage N completes (on Worker-3):
    NodeUnpublishVolume    → umount bind mount
    NodeUnstageVolume      → iscsiadm logout
    ControllerUnpublishVolume:
      1. Delete current attachment (terminates iSCSI target for Worker-3)
      2. Create NEW reserved attachment (no connector)
      3. Store new attachment_id in Cinder volume metadata
      Volume status: reserved → available → reserved

  CDI Stage N+1 scheduled (on Worker-1):
    ControllerPublishVolume:
      1. Read new attachment_id from volume metadata
      2. Update with Worker-1's connector (IQN-B)
      3. Cinder creates NEW iSCSI target for Worker-1
    NodeStageVolume → iscsiadm login (Worker-1)
    NodePublishVolume → bind mount
```

**Implementation Flow:**

```
┌────────────────────────────────────────────────────────────────────────────┐
│      ControllerUnpublishVolume RPC — Controller Plugin                     │
└────────────────────────────────────────────────────────────────────────────┘

  CSI Controller Plugin                     Target OpenStack (Cinder API)
       │                                         │
       │  1. Get current attachment_id from       │
       │     volume metadata:                     │
       │     cloud.GetVolume(req.VolumeId)         │
       │     → metadata["csi.attachment_id"]      │
       ├────────────────────────────────────────► │
       │                                          │
       │  2. Delete current attachment:           │
       │     cloud.DeleteAttachment(              │
       │       attachmentID)                      │
       │     → DELETE /v3/attachments/{id}        │
       ├────────────────────────────────────────► │
       │                                          │  Backend: terminate_connection()
       │                                          │  → iSCSI target removed
       │                                          │  Volume status → "available"
       │                                          │
       │  3. Create new reserved attachment:      │
       │     cloud.CreateAttachment(              │
       │       req.VolumeId)                      │
       │     → POST /v3/attachments               │
       │       { volume_uuid: req.VolumeId }      │
       ├────────────────────────────────────────► │
       │  ◄── NEW_ATTACHMENT_ID                   │  Volume status → "reserved"
       │                                          │
       │  4. Update volume metadata:              │
       │     cloud.SetVolumeMetadata(             │
       │       req.VolumeId,                      │
       │       { "csi.attachment_id":             │
       │         NEW_ATTACHMENT_ID })              │
       ├────────────────────────────────────────► │
       │                                          │
       │  5. Return ControllerUnpublishVolumeResponse{}
```

**Volume status transition during `ControllerUnpublishVolume`:**
```
  reserved/in-use → (delete attachment) → available → (create attachment) → reserved
```

The `reserved` status between CDI stages still acts as a lock — the volume cannot be deleted or attached by another workflow while in `reserved` state.

**Idempotency:** If the old attachment is already deleted (e.g., driver crashed mid-operation), skip step 2 and proceed to step 3. If a reserved attachment already exists, reuse it.

#### 5.2.4 `DeleteVolume` — Attachment Cleanup + Volume Release

**CSI Spec Reference:** *"A Controller Plugin MUST implement this RPC call if it has CREATE_DELETE_VOLUME controller capability."*

This RPC is the **final cleanup point** in the migration lifecycle, triggered by the `external-provisioner` sidecar when the PVC is deleted (`reclaimPolicy: Delete`). Per-pod attachment cleanup between CDI stages is handled by `ControllerUnpublishVolume` (see §5.2.3). Behavior is controlled by Cinder volume metadata (`csi.cleanupVolume`):

| Scenario | `csi.cleanupVolume` metadata | `DeleteVolume` behavior |
|----------|------------------------------|------------------------|
| **Migration success** (default) | Not set or `"false"` | Delete attachment → volume becomes `available`. **Volume is NOT deleted.** Blueprint creates target VM. |
| **Migration failure / cancellation** | Blueprint sets `"true"` before deleting PVC | Delete attachment → **delete Cinder volume**. Full cleanup. |

**Implementation Flow — Success Path (default):**

```
┌────────────────────────────────────────────────────────────────────────────┐
│    DeleteVolume RPC — Success Path (volume released as "available")         │
└────────────────────────────────────────────────────────────────────────────┘

  CSI Controller Plugin                     Target OpenStack
       │                                         │
       │  1. Read volume metadata:                │
       │     cloud.GetVolume(req.VolumeId)         │
       │     → Get metadata["csi.attachment_id"]  │
       │     → Get metadata["csi.cleanupVolume"]  │
       ├────────────────────────────────────────► │
       │                                          │
       │  2. If volume not found:                 │
       │     → Return success (idempotent)        │
       │                                          │
       │  3. If attachment exists:                │
       │     cloud.DeleteAttachment(attachmentID) │
       │     → DELETE /v3/attachments/{id}        │
       ├────────────────────────────────────────► │
       │                                          │  Backend: terminate_connection()
       │                                          │  Volume status → "available"
       │                                          │
       │  4. Check cleanup mode:                  │
       │     cleanupVolume = metadata             │
       │       ["csi.cleanupVolume"]              │
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
       │         ["csi.attachment_id",            │
       │          "csi.cleanupVolume"])            │
       │                                          │
       │  5. Return DeleteVolumeResponse{}        │
```

**Volume Handoff to Blueprint:**

After `DeleteVolume` completes (success path), the Cinder volume is in `available` state with no attachments. The Blueprint can then:

```
  Blueprint (after PVC deletion):
    1. openstack volume set --bootable ${VOLUME_ID}
    2. openstack server create --volume ${VOLUME_ID} ...
    ✓ Target VM boots from the Cinder volume
    ✓ Nova creates its own iSCSI attachment with compute host's IQN
    ✓ Data written during migration is intact (persists on backend LV/FlashArray)
```

**Failure/Cancellation Cleanup:**

```bash
# Blueprint detects migration failure:
openstack volume set --property csi.cleanupVolume=true ${VOLUME_ID}

# Then deletes PVC — triggers DeleteVolume which fully cleans up
kubectl delete pvc migration-${VM_NAME}-vda-pvc
# → DeleteVolume: delete attachment + DELETE Cinder volume. No orphans.
```

### 5.3 CSI Node Service

The Node plugin runs on each **WRCP/WRC worker host** where CDI importer pods may be scheduled. It handles iSCSI login/logout and block device bind-mount operations.

**Node Service Capabilities** (`NodeGetCapabilities`):

| Capability | Supported | Notes |
|------------|-----------|-------|
| `STAGE_UNSTAGE_VOLUME` | Yes | iSCSI login/logout at staging phase (per-volume, per-node) |
| `GET_VOLUME_STATS` | Yes | Report block device stats |
| `EXPAND_VOLUME` | No | Volume expansion is handled controller-side via Cinder |

#### 5.3.1 `NodeGetInfo` — WRCP Host Initiator Identity

Each node plugin instance reports its identity including the iSCSI initiator IQN. This information is passed by the CO to `ControllerPublishVolume` as `req.NodeId`.

**Node ID Format:** `hostname;iqn;ip`

```
  Example: "worker-3;iqn.1993-08.org.debian:01:aabbccdd;10.0.0.103"
```

**Implementation:**

```
  NodeGetInfo():
    1. Read initiator IQN:
       iqn = parse(/etc/iscsi/initiatorname.iscsi)
       # e.g., "iqn.2016-04.com.open-iscsi:1f98f85d8ef4"

    2. Get hostname:
       host = os.Hostname()

    3. Get storage network IP (configurable interface):
       ip = getInterfaceIP(config.ISCSI.StorageInterface)
       # Default: primary interface IP

    4. Return NodeGetInfoResponse{
         NodeId: fmt.Sprintf("%s;%s;%s", host, iqn, ip)
         AccessibleTopology: {
           "topology.cinder-iscsi.csi.openstack.org/zone": config.Zone
         }
       }
```

**Multi-worker cluster:** Each WRCP worker has a unique IQN (assigned during `open-iscsi` installation). When the CO schedules a pod to Worker-3, it passes Worker-3's `NodeId` to `ControllerPublishVolume`. The controller creates an iSCSI target specifically for Worker-3's IQN — other workers cannot access it.

#### 5.3.2 `NodeStageVolume` — iSCSI Login + Block Device Discovery

**CSI Spec Reference:** *"This RPC is called by the CO prior to the volume being consumed by any workloads on the node."*

In the NFS driver, `NodeStageVolume` mounts the NFS export. In the iSCSI driver, it performs iSCSI discovery, CHAP authentication, login, and block device path resolution.

**Request → Action Mapping:**

| CSI Field | Source / Value |
|-----------|----------------|
| `req.VolumeId` | Cinder volume UUID |
| `req.PublishContext["target_portal"]` | `69.167.149.97:3260` (from `ControllerPublishVolume`) |
| `req.PublishContext["target_iqn"]` | `iqn.2010-10.org.openstack:volume-xxx` |
| `req.PublishContext["target_lun"]` | `0` |
| `req.PublishContext["auth_method"]` | `CHAP` |
| `req.PublishContext["auth_username"]` | CHAP username |
| `req.PublishContext["auth_password"]` | CHAP password |
| `req.StagingTargetPath` | `/var/lib/kubelet/plugins/kubernetes.io/csi/pv/.../globalmount` |

**Implementation Flow:**

```
┌────────────────────────────────────────────────────────────────────────────┐
│          NodeStageVolume RPC — Node Plugin (WRCP Worker Host)              │
└────────────────────────────────────────────────────────────────────────────┘

  WRCP/WRC Worker Host
  ┌──────────────────────────────────────────────────────────────────────┐
  │                                                                      │
  │  1. Parse publish_context:                                           │
  │     portal    = req.PublishContext["target_portal"]                   │
  │     iqn       = req.PublishContext["target_iqn"]                      │
  │     lun       = req.PublishContext["target_lun"]                      │
  │     authMethod= req.PublishContext["auth_method"]                     │
  │     username  = req.PublishContext["auth_username"]                   │
  │     password  = req.PublishContext["auth_password"]                   │
  │                                                                      │
  │  2. Idempotency check:                                               │
  │     if iSCSI session already active for this IQN+portal              │
  │       AND device path exists → return OK                             │
  │                                                                      │
  │  3. iSCSI discovery:                                                 │
  │     iscsiadm -m discovery -t sendtargets -p ${portal}                │
  │                                                                      │
  │  4. Set CHAP auth (if auth_method == "CHAP"):                        │
  │     iscsiadm -m node -T ${iqn} -p ${portal}                         │
  │       --op update -n node.session.auth.authmethod -v CHAP            │
  │     iscsiadm -m node -T ${iqn} -p ${portal}                         │
  │       --op update -n node.session.auth.username -v ${username}       │
  │     iscsiadm -m node -T ${iqn} -p ${portal}                         │
  │       --op update -n node.session.auth.password -v ${password}       │
  │                                                                      │
  │  5. iSCSI login:                                                     │
  │     iscsiadm -m node -T ${iqn} -p ${portal} --login                  │
  │                                                                      │
  │  6. Wait for block device (with timeout):                            │
  │     device_path = /dev/disk/by-path/                                 │
  │       ip-${portal}-iscsi-${iqn}-lun-${lun}                          │
  │     Poll until device_path exists (timeout: 30s)                     │
  │     Resolve: readlink -f ${device_path} → /dev/sdc                   │
  │                                                                      │
  │  7. Store device path at staging target path:                        │
  │     mkdir -p ${req.StagingTargetPath}                                │
  │     echo ${device_path} > ${req.StagingTargetPath}/devicepath        │
  │                                                                      │
  │  8. Return NodeStageVolumeResponse{}                                 │
  │                                                                      │
  └──────────────────────────────────────────────────────────────────────┘

  Result:
  ┌────────────────────┐        iSCSI         ┌─────────────────────────┐
  │ WRCP Worker Host   │◄────────────────────►│ Cinder iSCSI Target     │
  │ /dev/sdc (10G)     │   TCP 3260           │ (LVM tgtd / Pure)       │
  │                    │                      │                         │
  │ /dev/disk/by-path/ │                      │ IQN: iqn.xxx            │
  │   ip-x.x.x.x:3260-│                      │ LUN: 0                  │
  │   iscsi-iqn.xxx-   │                      │                         │
  │   lun-0 → sdc      │                      │                         │
  └────────────────────┘                      └─────────────────────────┘
```

**Comparison with NFS `NodeStageVolume`:**

| Step | NFS Driver | iSCSI Driver |
|------|-----------|-------------|
| Discovery | Parse `publish_context["nfs_export"]` | `iscsiadm -m discovery -t sendtargets` |
| Authentication | None (NFS ACL-based) | CHAP auth via `iscsiadm --op update` |
| Mount / Connect | `mount -t nfs -o opts export staging_path` | `iscsiadm --login` → kernel creates block device |
| Verification | `stat staging_path/volume_file` | Wait for `/dev/disk/by-path/...` to appear |
| Artifact at staging path | NFS mount point with volume file | File containing device path string |

#### 5.3.3 `NodePublishVolume` — Bind Mount Block Device into Pod

**CSI Spec Reference:** *"For volumes with an access type of block, the SP SHALL place the block device at target_path."*

**Implementation Flow:**

```
┌────────────────────────────────────────────────────────────────────────────┐
│        NodePublishVolume RPC — Node Plugin (WRCP Worker Host)              │
└────────────────────────────────────────────────────────────────────────────┘

  WRCP/WRC Worker Host
  ┌──────────────────────────────────────────────────────────────────────┐
  │                                                                      │
  │  1. Read device path from staging:                                   │
  │     device = readFile(${req.StagingTargetPath}/devicepath)           │
  │     # e.g., /dev/disk/by-path/ip-x.x.x.x:3260-iscsi-iqn.xxx-lun-0 │
  │                                                                      │
  │  2. Idempotency check:                                               │
  │     if target already bind-mounted → return OK                       │
  │                                                                      │
  │  3. For Block access type:                                           │
  │     a. Create target file if not exists:                             │
  │        touch ${req.TargetPath}                                       │
  │     b. Bind mount block device to target:                            │
  │        mount --bind ${device} ${req.TargetPath}                      │
  │                                                                      │
  │  4. Return NodePublishVolumeResponse{}                               │
  │                                                                      │
  └──────────────────────────────────────────────────────────────────────┘

  Data Path (inside CDI Importer Pod):
  ┌──────────────────────────────────────────────────────────────────────┐
  │  CDI Importer Pod                                                    │
  │                                                                      │
  │  /dev/cdi-block-volume                                               │
  │    │  (bind mount from /dev/disk/by-path/ip-...-iscsi-...-lun-0)    │
  │    │                                                                 │
  │    └─► VDDK Client writes VMDK data here                            │
  │        → write goes through bind mount                               │
  │        → lands on iSCSI block device                                 │
  │        → iSCSI write-through to Cinder volume on storage backend    │
  │        → Cinder volume updated in-place                              │
  └──────────────────────────────────────────────────────────────────────┘
```

#### 5.3.4 `NodeUnpublishVolume` — Remove Pod Bind Mount

```
  Node Plugin:
    1. umount ${req.TargetPath}     // remove bind mount
    2. Remove target file
    3. Return NodeUnpublishVolumeResponse{}
```

#### 5.3.5 `NodeUnstageVolume` — iSCSI Logout

```
  Node Plugin:
    1. Read device info from staging path:
       Read ${req.StagingTargetPath}/devicepath

    2. Parse target IQN and portal from device path:
       /dev/disk/by-path/ip-PORTAL-iscsi-IQN-lun-LUN

    3. iSCSI logout:
       iscsiadm -m node -T ${iqn} -p ${portal} --logout

    4. Clean up node DB entry:
       iscsiadm -m node -T ${iqn} -p ${portal} --op delete

    5. Remove staging directory artifacts:
       rm ${req.StagingTargetPath}/devicepath
       rmdir ${req.StagingTargetPath}

    6. Return NodeUnstageVolumeResponse{}
```

### 5.4 Cinder Volume Status Lifecycle

> **K8S CSI Trigger Reference:** `CreateVolume` and `DeleteVolume` are triggered by the `external-provisioner` sidecar (on PVC create/delete). `ControllerPublishVolume` and `ControllerUnpublishVolume` are triggered by the `external-attacher` sidecar (on pod schedule/delete, via the AD Controller's `VolumeAttachment` CR lifecycle). `NodeStageVolume`, `NodeUnstageVolume`, `NodePublishVolume`, and `NodeUnpublishVolume` are called by kubelet directly.

```
  Cinder Volume Status Timeline:

  CSI RPC / Action                    Volume Status    Reason
  ─────────────────────────────────── ────────────── ──────────────────────────
  Cinder POST /v3/volumes             creating →      Normal Cinder lifecycle
                                      available

  POST /v3/attachments (no connector) available →     Reserved attachment
                                      reserved        acts as volume lock

  PUT /v3/attachments/{id}             reserved        Backend creates iSCSI
    (set connector → iSCSI target)                     target. Status unchanged.

  POST .../action (os-complete)        reserved →      Volume fully attached
    (if microversion 3.44+)            in-use          (optional step)

  iscsiadm --login                     reserved/       WRCP host logs in to
    (WRCP host connects)               in-use          iSCSI target. Cinder
                                                       is unaware.

  CDI full copy / precopy              reserved/       Writes go iSCSI →
    (pod writes to block device)       in-use          storage backend.

  iscsiadm --logout                    reserved/       iSCSI session closed.
    (NodeUnstageVolume)                in-use          Cinder unaware.

  ControllerUnpublishVolume            reserved →      Delete old attachment.
    (delete + recreate attachment)     available →      Create new reserved.
                                      reserved

    ── CDI precopy cycles ──           reserved        Attachment recycled
                                                       between stages.

  DeleteVolume (PVC deleted)           reserved →      Delete attachment.
    (delete attachment)                available        Volume released.

  Blueprint: server create             available →     Nova creates its own
    --volume ${VOLUME_ID}              in-use          attachment to compute
                                                       host.
```

### 5.5 End-to-End CSI RPC Call Sequence (CDI Multi-Phase Precopy)

> **Component roles:** The "CO" column below represents side-car and kubelet components acting together. `CreateVolume`/`DeleteVolume` are issued by `external-provisioner` (PVC lifecycle). `ControllerPublishVolume`/`ControllerUnpublishVolume` are issued by `external-attacher` (triggered by the AD Controller's `VolumeAttachment` CR, which tracks pod-to-node assignment). `NodeStage`/`NodePublish`/`NodeUnpublish`/`NodeUnstage` are issued by kubelet. See [kubernetes-csi-architecture-reference.md](kubernetes-csi-architecture-reference.md) for the full calling chain.

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│   CSI RPC Call Sequence — CDI Multi-Phase Precopy Lifecycle (V2O)               │
│   iSCSI-Cinder CSI Driver                                                       │
└─────────────────────────────────────────────────────────────────────────────────┘

 CO (kubelet + sidecars)        Controller Plugin           Node Plugin
 ══════════════════════         ═════════════════           ═══════════
        │                              │                         │
  PVC Created                          │                         │
        │                              │                         │
  ┌─────┤  1. CreateVolume             │                         │
  │     ├─────────────────────────────►│                         │
  │     │                              │── Cinder create vol     │
  │     │                              │── Create attachment     │
  │     │                              │   (reserved, no conn.)  │
  │     │                              │── Set metadata:         │
  │     │                              │   csi.attachment_id     │
  │     │◄─────────────────────────────┤                         │
  │     │  VolumeId + VolumeContext    │                         │
  │     │                              │                         │
  │ ╔═══╪══ CDI Stage 1: Full Copy ════╪═════════════════════════╪═══╗
  │ ║   │                              │                         │   ║
  │ ║ Pod 1 Scheduled to Worker-3      │                         │   ║
  │ ║   │                              │                         │   ║
  │ ║   │  2. ControllerPublishVolume  │                         │   ║
  │ ║   ├─────────────────────────────►│                         │   ║
  │ ║   │  NodeId="worker-3;iqn-3;ip3"│                         │   ║
  │ ║   │                              │── Update attachment     │   ║
  │ ║   │                              │   connector (IQN-3)     │   ║
  │ ║   │                              │── Extract iSCSI info    │   ║
  │ ║   │◄─────────────────────────────┤                         │   ║
  │ ║   │  PublishContext (iSCSI info) │                         │   ║
  │ ║   │                              │                         │   ║
  │ ║   │  3. NodeStageVolume          │                         │   ║
  │ ║   ├──────────────────────────────┼────────────────────────►│   ║
  │ ║   │                              │                         │── iscsiadm
  │ ║   │                              │                         │   discovery
  │ ║   │                              │                         │── CHAP auth
  │ ║   │                              │                         │── iscsiadm
  │ ║   │                              │                         │   login
  │ ║   │                              │                         │── /dev/sdc
  │ ║   │                              │                         │   ║
  │ ║   │  4. NodePublishVolume        │                         │   ║
  │ ║   ├──────────────────────────────┼────────────────────────►│   ║
  │ ║   │                              │                         │── mount --bind
  │ ║   │                              │                         │   /dev/sdc →
  │ ║   │                              │                         │   target_path
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
  │ ║   │                              │                         │── iscsiadm
  │ ║   │                              │                         │   logout
  │ ║   │  7. ControllerUnpublishVol   │                         │   ║
  │ ║   ├─────────────────────────────►│                         │   ║
  │ ║   │                              │── Delete attachment     │   ║
  │ ║   │                              │── Create NEW reserved   │   ║
  │ ║   │                              │   attachment            │   ║
  │ ║   │                              │── Update metadata with  │   ║
  │ ║   │                              │   new attachment_id     │   ║
  │ ║   │◄─────────────────────────────┤                         │   ║
  │ ╚═══╪══════════════════════════════╪═════════════════════════╪═══╝
  │     │                              │                         │
  │     │  ── gap: no pod, no iSCSI session ──                   │
  │     │  ── Volume in "reserved" state (locked) ──             │
  │     │                              │                         │
  │ ╔═══╪══ CDI Stage 2: Precopy 1 ═══╪═════════════════════════╪═══╗
  │ ║   │                              │                         │   ║
  │ ║ Pod 2 Scheduled (may be different worker!)                 │   ║
  │ ║   │  2. ControllerPublishVolume  │                         │   ║
  │ ║   ├─────────────────────────────►│                         │   ║
  │ ║   │  NodeId="worker-1;iqn-1;ip1"│                         │   ║
  │ ║   │                              │── Update NEW attachment │   ║
  │ ║   │                              │   connector (IQN-1) ✓   │   ║
  │ ║   │◄─────────────────────────────┤                         │   ║
  │ ║   │                              │                         │   ║
  │ ║   │  3-4. NodeStage + Publish    │                         │   ║
  │ ║   ├──────────────────────────────┼────────────────────────►│   ║
  │ ║   │                              │                         │── iscsiadm
  │ ║   │                              │                         │   login
  │ ║   │                              │                         │── bind mount
  │ ║   │ CDI Delta Copy runs:         │                         │   ║
  │ ║   │ ┌───────────────────────┐    │                         │   ║
  │ ║   │ │ VDDK delta → block dev│    │                         │   ║
  │ ║   │ └───────────────────────┘    │                         │   ║
  │ ║   │                              │                         │   ║
  │ ║ Pod 2 completes                  │                         │   ║
  │ ║   │  5-7. Unpublish+Unstage+     │                         │   ║
  │ ║   │       CtrlUnpublish          │                         │   ║
  │ ║   │       (delete+recreate att.) │                         │   ║
  │ ╚═══╪══════════════════════════════╪═════════════════════════╪═══╝
  │     │                              │                         │
  │     │  ... repeat for precopy N ...│                         │
  │     │                              │                         │
  │ Migration complete!                │                         │
  │     │                              │                         │
  │ PVC Deleted (reclaimPolicy: Delete)│                         │
  │     │                              │                         │
  │     │  8. DeleteVolume             │                         │
  │     ├─────────────────────────────►│                         │
  │     │                              │── Delete attachment     │
  │     │                              │── Volume → "available"  │
  │     │                              │── Remove CSI metadata   │
  │     │                              │── (NOT deleted)         │
  │     │◄─────────────────────────────┤                         │
  │     │                              │                         │
  │ Blueprint takes over:              │                         │
  │   set bootable → server create     │                         │
  └─────┤                              │                         │
```

### 5.6 Volume Finalization and VM Creation

Volume finalization follows the same two-phase process as the NFS driver, but without NFS-specific steps.

**Phase 1: Driver Injection (Pre-CSI Cleanup — PVC Still Exists)**

The Blueprint launches a **helper pod** on the WRC K8S cluster that mounts the same PVC. This pod runs `virt-v2v-in-place` to inject virtio drivers. The CSI mount chain (iSCSI login → bind mount) provides access to the volume.

```
  WRC Blueprint → launch helper pod (same PVC, volumeMode: Block)
    CSI ControllerPublish → iSCSI info
    CSI NodeStage → iscsiadm login
    CSI NodePublish → bind mount /dev/sdc → pod

    Helper pod: virt-v2v-in-place on /dev/cdi-block-volume

    Pod exits → NodeUnpublish + NodeUnstage (logout) + ControllerUnpublish
```

**Phase 2: Volume Release + VM Creation (Post-CSI)**

```
  WRC Blueprint:
    1. kubectl delete pvc → CSI DeleteVolume → volume "available"
    2. openstack volume set --bootable ${VOLUME_ID}
    3. openstack server create --volume ${VOLUME_ID} ...
    ✓ Target VM boots from Cinder volume
```

---

## 6. Component Flow

### 6.1 End-to-End Workflow

#### 6.1.1 VMware → OpenStack (V2O) — CDI Multi-Phase Warm Migration

```
┌────────────────────────────────────────────────────────────────────────────────┐
│              END-TO-END V2O iSCSI-BACKED MIGRATION FLOW                        │
│              (CDI Multi-Phase Precopy + CSI iSCSI-Cinder Driver)               │
└────────────────────────────────────────────────────────────────────────────────┘

 WRC Blueprint Orchestrator
 ═════════════════════════

 Stage 0: PRECHECK
    │  Validate CBT, inspect source VM
    │  Validate target OpenStack iSCSI backend
    │  Validate Cinder microversion >= 3.27
    ▼
 Stage 1: CLEANUP
    │  Remove previous migration artifacts
    ▼
 ┌─────────────────────────────────────────────────────────────┐
 │ CSI Volume Provisioning (via PVC + StorageClass)            │
 │                                                             │
 │  Blueprint: kubectl apply PVC (StorageClass: cinder-iscsi)  │
 │  CSI CreateVolume:                                          │
 │    1. Create Cinder volume (--type pure-iscsi)              │
 │    2. Create reserved attachment (no connector)             │
 │    3. Store csi.attachment_id in Cinder metadata            │
 │                                                             │
 │  Result: PV bound to PVC, volume locked by attachment       │
 └─────────────────────────────────────────────────────────────┘
    │
    ▼
 Stage 2: FULL COPY
    │  Create initial VMware snapshot
    │  CDI importer pod on WRC K8S:
    │    CSI ControllerPublish → update attachment connector → iSCSI info
    │    CSI NodeStage → iscsiadm login → /dev/sdc
    │    CSI NodePublish → bind mount into pod
    │    VDDK full disk copy → /dev/cdi-block-volume → iSCSI
    │  Pod exits:
    │    CSI NodeUnpublish + NodeUnstage → iscsiadm logout
    │    CSI ControllerUnpublish → delete + recreate attachment
    ▼
 Stage 3: PRECOPY (Loop — repeat N times)
    │  Create incremental snapshot
    │  CDI importer pod N on WRC K8S:
    │    CSI ControllerPublish → update NEW attachment connector
    │    CSI NodeStage → iscsiadm login (new target for this pod's node)
    │    VDDK delta copy (changed blocks only) → iSCSI
    │  Pod exits → logout + attachment recycle
    │  Repeat until cutover triggered
    ▼
 Stage 4: CUTOVER
    │  Power off source VM
    │  Final snapshot + final delta transfer (same pattern)
    ▼
 Stage 5: DRIVER INJECTION (virt-v2v-in-place)
    │  Helper pod on WRC K8S (same PVC)
    │  CSI mounts iSCSI volume into helper pod
    │  virt-v2v injects virtio drivers
    ▼
 Stage 6: VOLUME RELEASE
    │  kubectl delete pvc → CSI DeleteVolume
    │  Delete attachment → volume "available"
    ▼
 Stage 7: CREATE VM (OpenStack)
    │  openstack volume set --bootable ${VOLUME_ID}
    │  openstack server create --volume ${VOLUME_ID}
    │  VM boots from Cinder volume directly
    ▼
 ✓ MIGRATION COMPLETE
```

#### 6.1.2 OpenStack → OpenStack (O2O) — NBD Receiver + virsh blockcopy

```
┌────────────────────────────────────────────────────────────────────────────────┐
│              END-TO-END O2O iSCSI-BACKED MIGRATION FLOW                        │
│              (NBD Receiver Pod + virsh blockcopy + CSI iSCSI-Cinder Driver)     │
└────────────────────────────────────────────────────────────────────────────────┘

 WRC Blueprint Orchestrator
 ═════════════════════════

 Stage 0: PRECHECK
    │  Validate source VM, inspect disk layout
    │  Validate target OpenStack iSCSI backend
    ▼
 ┌─────────────────────────────────────────────────────────────┐
 │ CSI Volume Provisioning — same as V2O                       │
 └─────────────────────────────────────────────────────────────┘
    │
    ▼
 Stage 1: LAUNCH NBD RECEIVER
    │  Blueprint launches NBD receiver pod on WRC K8S:
    │    Pod references PVC (volumeMode: Block)
    │    CSI ControllerPublish → iSCSI info
    │    CSI NodeStage → iscsiadm login
    │    CSI NodePublish → bind mount block device into pod
    │  NBD receiver pod starts:
    │    Exposes /dev/ndb-block-volume via NBD protocol
    │    Listens on nbd://<pod-ip>:10809/export
    ▼
 Stage 2: BLOCK MIRROR
    │  virsh blockcopy source-vm vda nbd://<nbd-receiver>:10809
    │    (source libvirt mirrors disk to NBD target → iSCSI volume)
    │  Mirror runs until ready for pivot
    ▼
 Stage 3: CUTOVER
    │  virsh blockjob --pivot
    │  Power off source VM
    ▼
 Volume Release + VM Creation (same as V2O stages 5-7)
    ▼
 ✓ MIGRATION COMPLETE
```

### 6.2 Data Path Visualization

```
VMware vCenter (Mgmt Network)          WRCP/WRC Worker Host              OpenStack Cinder (iSCSI Backend)
═══════════════════════════             ═══════════════════               ═════════════════════════════════

┌───────────────┐                   ┌──────────────────────┐          ┌──────────────────────────────┐
│  Source VM    │                   │  WRC K8S Cluster     │          │  iSCSI Storage Backend       │
│  ┌─────────┐ │    VDDK API       │                      │          │  (Pure Storage / LVM tgtd)   │
│  │ VMDK    │─┼───────────────►   │  ┌────────────────┐  │  iSCSI   │                              │
│  │ Disks   │ │                   │  │CDI Importer Pod│  │  Write   │  ┌──────────────────────────┐│
│  └─────────┘ │                   │  │                │──┼────────► │  │ Cinder Volume            ││
│              │                   │  │/dev/cdi-block- │  │ TCP 3260 │  │ (LUN on iSCSI target)    ││
│              │                   │  │    volume      │  │          │  │                          ││
│              │                   │  └────────────────┘  │          │  │ IQN: iqn.xxx             ││
│              │                   │                      │          │  │ LUN: 0                   ││
└──────────────┘                   └──────────────────────┘          │  └──────────────────────────┘│
                                                                    └──────────────────────────────┘
                                                                              │
                                                                              │ At cutover:
                                                                              ▼
                                                                    ┌──────────────────────────┐
                                                                    │  Target OpenStack VM     │
                                                                    │  boots from this volume  │
                                                                    │  (Nova iSCSI attachment)  │
                                                                    │  (zero additional copy)  │
                                                                    └──────────────────────────┘
```

---

## 7. Implementation Details

For complete implementation details — including package structure, Go interfaces,
configuration structs, CSI RPC implementation mapping, Kubernetes manifests,
Helm chart, StorageClass/PVC/CDI Pod specifications, and development phases — see
the companion implementation design document:

> **[iSCSI-Cinder CSI Implementation Design](iscsi-cinder-csi-implementation-design.md)**

### 7.1 Driver Configuration Reference

The iSCSI-Cinder CSI driver is configured via two files mounted into the controller
and node DaemonSet pods:

| File | Source | Kubernetes Object | Description |
|------|--------|-------------------|-------------|
| `cloud.conf` | OpenStack credentials | Secret (`csi-secret-cinderplugin-iscsi`) | Keystone auth — `[Global]` section with `auth-url`, `username`, `password`, etc. |
| `driver.conf` | Driver behavior tuning | ConfigMap (`csi-configmap-cinder-iscsi`) | `[ISCSI]` and `[Volume]` sections — see below |

#### `[ISCSI]` Section — iSCSI Initiator & Connector Options

These options control the iSCSI initiator behavior on Node RPCs (`NodeStageVolume` /
`NodeUnstageVolume`) and the connector fields sent to Cinder during
`ControllerPublishVolume`.

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `enable-multipath` | bool | `false` | Enable iSCSI multipath (DM-MPIO). When `true`, the connector advertises multipath support to Cinder and the Node RPCs use `multipathd`. |
| `chap-auth-enabled` | bool | `true` | Whether to use CHAP authentication for iSCSI sessions. |
| `login-timeout` | int | `30` | iSCSI login timeout in seconds (`iscsiadm --login --timeout`). |
| `device-wait-timeout` | int | `30` | Time in seconds to wait for the block device to appear after `iscsiadm --login`. |
| `iscsi-interface` | string | `"default"` | iSCSI interface to use (`iscsiadm -I`). |
| `storage-interface` | string | `""` | Network interface name for the storage network. Empty = use the host's primary IP. |
| `platform` | string | `"x86_64"` | Platform string sent in the Cinder attachment connector. |
| `os-type` | string | `"linux2"` | OS type string sent in the Cinder attachment connector. |

#### `[Volume]` Section — Cinder Volume Lifecycle Options

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `create-timeout` | int | `300` | Seconds to wait for a new Cinder volume to reach `available` status. |
| `detach-timeout` | int | `120` | Seconds to wait for attachment deletion to complete. |
| `default-volume-type` | string | `""` | Cinder volume type to use if not specified in the StorageClass. |
| `metadata-prefix` | string | `"csi"` | Prefix for CSI-managed Cinder volume metadata keys (e.g., `csi.attachment_id`). |
| `delete-volume-mode` | string | `"retain"` | Driver-level default for `DeleteVolume` behavior. `retain` = leave volume available for Blueprint; `delete` = fully delete volume. Per-volume `csi.cleanupVolume` metadata overrides this. |

#### Example `driver.conf`

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

> **Note:** The `delete-volume-mode` option uses a 3-tier precedence:
> 1. Per-volume `csi.cleanupVolume` metadata (`"true"` = delete) — highest priority
> 2. `delete-volume-mode` from `driver.conf`
> 3. Built-in default: `retain`
>
> See [Implementation Design §6.2](iscsi-cinder-csi-implementation-design.md#62-iscsi-specific-config-structs)
> for the full Go struct definitions and precedence rules.



---

## 8. Network Architecture

### 8.1 Network Topology

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
  │               │   │  ✓ Can reach iSCSI (storage) │       │                │
  │               │   └──────────────────────────────┘       │                │
  │               │                    │                      │                │
  │               └────────────────────┼──────────────────────┘                │
  │                                    │ iSCSI I/O                             │
  └────────────────────────────────────┼───────────────────────────────────────┘
                                       │
  ┌────────────────────────────────────┼───────────────────────────────────────┐
  │                Storage Network     │    (e.g., 192.168.57.0/24)            │
  │                                    │                                       │
  │   ┌──────────────────┐        ┌────┴───────────────────┐                   │
  │   │ OpenStack Cinder │        │  iSCSI Target          │                   │
  │   │ iSCSI Backend    │◄──────►│  Pure Storage /        │                   │
  │   │                  │        │  LVM tgtd              │                   │
  │   └──────────────────┘        │  Portal: x.x.x.x:3260 │                   │
  │                               └────────────────────────┘                   │
  │                                                                            │
  └────────────────────────────────────────────────────────────────────────────┘
```

### 8.2 Network Requirements

| Network Path | Protocol | Port | Purpose |
|-------------|----------|------|---------|
| WRCP Worker → iSCSI Target | iSCSI | **3260** | Block I/O data transfer (iscsiadm login + reads/writes) |
| WRCP Worker → vCenter | HTTPS | 443 | VDDK data transfer (CDI importer) |
| WRC Orchestrator → OpenStack API | HTTPS | 5000, 8776 | Keystone auth, Cinder volume/attachment API |
| WRCP Worker → OpenStack Keystone | HTTPS | 5000 | Authentication (CSI controller) |

> **Note:** Unlike the NFS driver, there is **no Nova API dependency** (port 8774) since the iSCSI driver uses Cinder v3 attachments instead of Shadow VMs.

---

## 9. Prerequisites

| Requirement | Details |
|-------------|---------|
| **OpenStack Cinder iSCSI Backend** | Cinder must be configured with an iSCSI-based volume type (Pure Storage FlashArray, LVM, etc.). The `driver_volume_type` in `connection_info` must be `iscsi`. |
| **Cinder Microversion 3.27+** | Required for self-service attachments without a Nova instance. Available since OpenStack Queens (2018.1). |
| **iSCSI Network Accessibility** | WRCP/WRC worker hosts must have TCP connectivity to the iSCSI target portal(s) on port **3260**. |
| **iSCSI Initiator Packages** | WRCP/WRC worker hosts must have `open-iscsi` installed (`iscsiadm`, `iscsid`). |
| **iscsid Service Running** | `iscsid` daemon must be running on each WRCP worker (`systemctl start iscsid`). |
| **Unique Initiator IQN per Worker** | Each WRCP worker must have a unique IQN in `/etc/iscsi/initiatorname.iscsi` (default after `open-iscsi` install). |
| **Management Network Access** | WRCP/WRC worker hosts must have connectivity to VMware vCenter on the management network for VDDK data transfer (V2O only). |
| **OpenStack Credentials** | Valid credentials with permissions for Cinder volume operations and attachment management. **No Nova permissions needed.** |
| **WRC K8S Cluster** | Existing WRC K8S cluster with CDI installed and operational. |
| **Privileged Host Access** | CSI Node DaemonSet pods must run as privileged to execute `iscsiadm` and access `/dev/disk/by-path/`. |

---

## 10. Risks and Mitigations

| Risk | Impact | Likelihood | Mitigation |
|------|--------|------------|------------|
| **Attachment connector update rejected** | Cannot switch iSCSI target to different worker between CDI stages | Medium | Use delete + recreate attachment pattern in `ControllerUnpublishVolume` (validated approach). |
| **Volume briefly `available` between CDI stages** | Possible race condition — another workflow could claim the volume | Low | Time window is <1s (delete + recreate attachment is atomic from driver's perspective). Volume name prefix (`migration-`) identifies it as in-use. |
| **iSCSI session stale after node restart** | Block device unavailable, migration stalls | Low | `NodeStageVolume` checks for stale sessions and re-establishes login. `iscsiadm -m node --logout` then re-login. |
| **CHAP credentials exposed in VolumeAttachment** | Credentials visible in Kubernetes `VolumeAttachment` object | Low | Standard pattern for iSCSI CSI drivers. Restrict VolumeAttachment RBAC to kubelet + CSI node plugin only. CHAP credentials rotate per-attachment (new creds on each attachment create). |
| **Pure Storage multipath handling** | Multiple portals/IQNs returned — driver must login to all for optimal I/O | Medium | Phase 1: single-path only (`multipath: false`). Phase 2: add `multipathd` integration. |
| **`os-complete` requires microversion 3.44** | Volume stays `reserved` instead of `in-use` | Low | iSCSI login works in `reserved` state (validated). `reserved` still acts as lock. Use 3.44 when available (best-effort). |
| **Non-iSCSI Cinder backends** | Design does not support NFS/FC backends | N/A | Separate driver (`cinder-nfs.csi.openstack.org`) exists for NFS. Document iSCSI-only limitation. |
| **Concurrent migration conflicts** | Two migrations creating attachments to same volume | Low | Cinder rejects second attachment on non-multiattach volume. |
| **Old OpenStack (pre-Queens)** | Cinder v3 attachment API not available | Medium | Fail fast with clear error. **TODO:** Shadow VM fallback for pre-Queens deployments. |

---

## 11. Future Work

1. **Pure Storage Multipath** — Enable `multipath: true` in connector, login to all `target_portals`/`target_iqns`, integrate with `multipathd` for device mapper (`/dev/dm-X`). This provides higher throughput and redundancy.

2. **Shadow VM Fallback for Pre-Queens OpenStack** — Implement a Shadow VM–based attachment path (identical to the NFS driver) for OpenStack deployments that do not support Cinder microversion 3.27. Configurable via `driver.conf` flag: `attachment-mode = v3-attachment | shadow-vm`.

3. **Multi-Disk Support** — Extend the workflow to handle VMs with multiple disks, creating one Cinder volume + attachment + PVC per disk.

4. **virt-v2v Driver Injection via iSCSI** — Perform virtio driver injection directly on the iSCSI-mounted volume from the WRCP host, avoiding the need for a separate helper pod step.

5. **Unified Migration CSI Driver** — Investigate unifying the NFS and iSCSI drivers into a single `cinder-migration.csi.openstack.org` driver with a strategy pattern at the node level, if the controller logic proves sufficiently similar.

6. **Automated Cutover Integration** — Run virt-v2v as a post-migration CSI hook or sidecar, eliminating the Blueprint step between PVC deletion and VM creation.

7. **Cinder v3 Attachment Connector Re-use Validation** — Test whether Cinder allows updating the connector on an existing attachment (changing the initiator IQN without delete + recreate). If supported, `ControllerUnpublishVolume` could be simplified to a no-op (same as NFS driver).

---

## Appendix A — iSCSI Initiator Configuration for Pure Storage

This appendix provides operational guidance for configuring iSCSI initiators on
Kubernetes worker nodes when the Cinder backend is **Pure Storage FlashArray**.
This is a Day 1 setup task performed once per cluster, not a per-migration activity.

### A.1 Background — iSCSI Initiator Identity

Every Linux host running an iSCSI initiator has a globally unique **iSCSI Qualified
Name (IQN)** stored locally:

```
$ cat /etc/iscsi/initiatorname.iscsi
InitiatorName=iqn.1994-05.com.redhat:worker-node-01
```

Key facts:

| Property | Detail |
|----------|--------|
| **Generated by** | `iscsi-initiator-utils` (RHEL/CentOS) or `open-iscsi` (Ubuntu) at package install |
| **Format** | `iqn.<year>-<month>.<reversed-domain>:<unique-id>` |
| **Scope** | Per-host, stored locally in `/etc/iscsi/initiatorname.iscsi` |
| **Uniqueness** | Must be unique across all hosts — duplicate IQNs cause **storage corruption** |
| **Central registry** | None — each host manages its own IQN |

The iSCSI-Cinder CSI driver reads this IQN during `ControllerPublishVolume` and sends
it to Cinder as `connector.Initiator` in the attachment update call. This tells the
storage backend which host is requesting access to the volume.

### A.2 Backend Behavior — Auto-Register vs Pre-Register

What happens when Cinder calls `initialize_connection` on the backend depends on the
storage vendor:

| Backend | Admin Pre-Registration? | Behavior |
|---------|------------------------|----------|
| **LVM/LIO (reference)** | No | ACL created automatically for the IQN |
| **NetApp ONTAP** | No | igroup auto-created on first attach |
| **Dell PowerStore** | No | Host object auto-created on first attach |
| **Ceph iSCSI (tcmu)** | No | Gateway ACL created dynamically |
| **HPE 3PAR** | Depends on config | Can auto-register or require manual VLUN |
| **Pure Storage FlashArray** | **Yes** | Admin must create Host object with IQN before first use |

### A.3 Pure Storage — Required Pre-Registration Steps

Pure Storage FlashArray requires that every initiator IQN be registered as a **Host**
object before Cinder can map volumes to it. If the IQN is not registered,
`initialize_connection` returns an error and `ControllerPublishVolume` fails with
gRPC `Internal`.

**Step 1 — Collect IQNs from all worker nodes:**

```bash
# Run on each worker node, or use Ansible/SSH loop:
for node in worker-{01..05}; do
  echo "$node: $(ssh $node cat /etc/iscsi/initiatorname.iscsi | grep InitiatorName)"
done
```

Example output:
```
worker-01: InitiatorName=iqn.1994-05.com.redhat:worker-01
worker-02: InitiatorName=iqn.1994-05.com.redhat:worker-02
worker-03: InitiatorName=iqn.1994-05.com.redhat:worker-03
```

**Step 2 — Create Host objects on FlashArray:**

Using the Pure Storage CLI (`purecli`) or REST API:

```bash
# Pure Storage CLI
purecli host create --name worker-01 --iqn iqn.1994-05.com.redhat:worker-01
purecli host create --name worker-02 --iqn iqn.1994-05.com.redhat:worker-02
purecli host create --name worker-03 --iqn iqn.1994-05.com.redhat:worker-03
```

Or via the FlashArray REST API (v2):

```bash
curl -X POST "https://flasharray.example.com/api/2.x/hosts" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "names": ["worker-01"],
    "iqns": ["iqn.1994-05.com.redhat:worker-01"]
  }'
```

**Step 3 — (Optional) Create a Host Group:**

Group all WRCP worker nodes for shared access policies:

```bash
purecli hgroup create --name wrcp-k8s-workers
purecli hgroup setattr --name wrcp-k8s-workers --hostlist worker-01,worker-02,worker-03
```

**Step 4 — Verify Pure Storage sees the hosts:**

```bash
purecli host list
# Should show all worker nodes with their IQNs and "iscsi" connection type
```

### A.4 Automation with Ansible

For production clusters, automate host registration with a playbook:

```yaml
# pure_storage_register_hosts.yml
- name: Register K8s worker IQNs on Pure Storage FlashArray
  hosts: k8s_workers
  gather_facts: false
  tasks:
    - name: Read iSCSI initiator name
      slurp:
        src: /etc/iscsi/initiatorname.iscsi
      register: initiator_file

    - name: Parse IQN
      set_fact:
        host_iqn: >-
          {{ (initiator_file.content | b64decode | regex_search('InitiatorName=(.+)', '\1'))[0] | trim }}

    - name: Create host on FlashArray
      purestorage.flasharray.purefa_host:
        fa_url: "{{ pure_fa_url }}"
        api_token: "{{ pure_api_token }}"
        name: "{{ inventory_hostname }}"
        iqn: "{{ host_iqn }}"
        state: present
      delegate_to: localhost

    - name: Add host to host group
      purestorage.flasharray.purefa_hg:
        fa_url: "{{ pure_fa_url }}"
        api_token: "{{ pure_api_token }}"
        name: wrcp-k8s-workers
        host:
          - "{{ inventory_hostname }}"
        state: present
      delegate_to: localhost
      run_once: false
```

### A.5 Troubleshooting — IQN Not Registered

If the CSI driver logs show:

```
ControllerPublishVolume: failed to update attachment att-xxx connector:
  failed to update attachment att-xxx connector: Bad Request (HTTP 400)
```

or the Cinder volume log shows:

```
PureISCSIDriver: Host with IQN iqn.1994-05.com.redhat:worker-04 not found
```

**Resolution:** Register the missing host IQN on the FlashArray (Step 2 above),
then retry the PVC/CDI import. No driver restart is needed — the next
`ControllerPublishVolume` call will succeed.

### A.6 RHOSO 18 Cinder Microversion Compatibility

RHOSO 18.0 is based on OpenStack **Caracal (2024.1)** and supports Cinder
microversions up to **3.70**. All microversions required by this driver are
supported:

| Microversion | Introduced | Release | Purpose | RHOSO 18? |
|---|---|---|---|---|
| **3.27** | Queens (2018) | Feb 2018 | Self-service attachments (no Nova) | **Yes** |
| **3.34** | Rocky (2018) | Aug 2018 | Server-side volume name filtering | **Yes** |
| **3.42** | Rocky (2018) | Aug 2018 | Online volume resize (in-use extend) | **Yes** |
| **3.44** | Rocky (2018) | Aug 2018 | `os-complete` attachment action | **Yes** |

For reference, here is the Red Hat OpenStack distribution lineage:

| Distribution | OpenStack Release | Max Cinder MV | 3.27? | 3.44? |
|---|---|---|---|---|
| RHOSP 13 | Queens | 3.43 | Yes | **No** |
| RHOSP 16.x | Train | 3.59 | Yes | Yes |
| RHOSP 17.x | Wallaby | 3.64 | Yes | Yes |
| **RHOSO 18.0** | **Caracal** | **3.70** | **Yes** | **Yes** |

The `DiscoverCinderCapabilities` startup probe confirms microversion support at
driver initialization. On RHOSO 18 it will always pass. The only edge case is
migrating from RHOSP 13, where 3.44 (`os-complete`) is unavailable — the driver
handles this gracefully by skipping the `CompleteAttachment` call when
`SupportsV344 == false`.

---

## 12. References

- [NFS-Backed Cinder Volume Design for WRCP Migration](nfs-backed-cinder-volume-for-wrcp-migration.md) — companion design doc for NFS-backed storage
- [O2O Warm Migration Solution Design (iSCSI, non-CSI)](../../../vm-migration-wrc/doc/o2o-warm-migration-solution-design.md) — deprecated O2O design using direct iSCSI attach to hypervisor
- [OpenStack Cinder v3 Attachments API](https://docs.openstack.org/api-ref/block-storage/v3/#volume-attachments)
- [Kubernetes CSI Specification](https://github.com/container-storage-interface/spec/blob/master/spec.md)
- [CDI Multi-stage VDDK Import](https://github.com/kubevirt/containerized-data-importer)
- [Open-iSCSI Administration](https://github.com/open-iscsi/open-iscsi)
- [Pure Storage Cinder Driver — iSCSI](https://docs.openstack.org/cinder/latest/configuration/block-storage/drivers/pure-storage-driver.html)
- [OpenStack Cinder Microversion History](https://docs.openstack.org/cinder/latest/contributor/api_microversion_history.html)
