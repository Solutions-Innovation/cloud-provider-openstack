# Architecture & Design Decisions

## Problem statement

The existing Cinder CSI driver (`cinder.csi.openstack.org`) requires an addon K8S cluster
on target OpenStack for volume attachment via Nova API. This fails because:

1. **Network isolation:** VMware vCenter runs on a management network isolated from
   OpenStack tenant VM networks. CDI importer pods on addon K8S cannot reach vCenter.
2. **WRCP/WRC dual access:** The WRCP/WRC platform has connectivity to BOTH the
   management network (vCenter) and the storage network (NFS backend).

## Solution: NFS direct mount via Shadow VM

When Cinder uses NFS-based storage (e.g., NetApp NFS), volumes are **files on NFS exports**.
Any host that can mount the NFS export can access the volume — including WRCP/WRC workers.

**Flow:**
1. Provision Cinder volume on target OpenStack
2. Create a Shadow VM to trigger NFS attachment record creation
3. Query NFS connection info from the Cinder attachment properties
4. Mount NFS export directly on WRCP/WRC worker host
5. Bind-mount the volume file as a block device into CDI importer pod
6. CDI writes directly to the Cinder volume file over NFS

## Why a separate package (not extension)

Every CSI RPC has fundamentally incompatible code paths between block-attach and NFS:

| Dimension | Existing Cinder CSI | NFS-Cinder Requirement | Compatible? |
|-----------|---------------------|------------------------|:-----------:|
| Driver name | `cinder.csi.openstack.org` (hardcoded) | `cinder-nfs.csi.openstack.org` | No |
| ControllerPublish | Nova `AttachVolume` (block device) | Query `connection_info` for NFS export | No |
| CreateVolume | Cinder POST only | Cinder POST + Shadow VM create/attach/stop | No |
| NodeStageVolume | `getDevicePath` + `FormatAndMount` | `mount -t nfs` | No |
| IOpenStack interface | Block-attach methods (25 methods) | Shadow VM lifecycle + connection_info | No |
| Access mode | `SINGLE_NODE_WRITER` | `MULTI_NODE_MULTI_WRITER` | No |

**Follows Manila CSI precedent** — `pkg/csi/manila/` is a fully separate package with its
own driver, servers, binary, and manifests in the same monorepo.

## Two supported use cases

| Use Case | Consumer Pod | Pod Lifecycle | Data Flow |
|----------|-------------|---------------|-----------|
| **V2O** (VMware→OpenStack) | CDI importer | Multi-phase (pods cycle between full copy + precopy stages) | VDDK → block device → NFS → Cinder volume |
| **O2O** (OpenStack→OpenStack) | NBD receiver | Long-running single pod | virsh blockcopy → NBD → NFS → Cinder volume |

Both use cases share the same CSI driver. The difference is only the consumer pod.

## Architecture comparison

| Aspect | Current (Addon K8S) | Proposed (NFS Direct Mount) |
|--------|--------------------|-----------------------------|
| K8S cluster for data path | Dedicated addon K8S on target OpenStack | WRC K8S cluster (existing) |
| Volume attach mechanism | Nova API block attach | NFS mount on WRCP host |
| Block device presentation | virtio block device (`/dev/vdb`) | Volume file on NFS export, bind-mounted |
| vCenter connectivity | Blocked (network isolation) | WRCP has management network access |
| Cinder volume types | Any (iSCSI, FC, NFS) | **NFS-backed only** |
| Infrastructure overhead | Requires addon K8S cluster VMs | No additional infrastructure |

## Two decoupled data planes

```
Cinder Control Plane:                NFS Data Plane:
Shadow VM ──attach──► Volume         WRCP Host ──NFS mount──► Same files
(stopped, idle)       (in-use)       (active, writing)        on NFS server
```

This is safe because:
1. Shadow VM is stopped — zero I/O on the volume
2. NFS is a shared filesystem — multiple clients can mount the same export
3. Single writer — only the CDI pod writes to the specific volume file
4. No filesystem corruption — volume file is raw block image, not mounted as FS by Shadow VM

## Volume finalization (two-phase)

**Phase 1 — Driver injection (PVC still exists):**
Blueprint launches a helper pod on WRC K8S that mounts the same PVC. Runs
`virt-v2v-in-place` to inject virtio drivers. Full CSI mount chain available.

**Phase 2 — Volume release + VM creation (PVC deleted):**
`DeleteVolume` detaches from Shadow VM, deletes Shadow VM. Volume becomes `available`.
Blueprint then: `openstack volume set --bootable` → `openstack server create --volume`.
