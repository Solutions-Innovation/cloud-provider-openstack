# Kubernetes CSI Architecture Reference

> **Purpose:** General reference for understanding how Kubernetes implements the CSI (Container Storage Interface) specification — component roles, calling sequences, data persistence, and deployment topology.

---

## Table of Contents

1. [CSI Spec vs Kubernetes Reality](#1-csi-spec-vs-kubernetes-reality)
2. [Component Roles](#2-component-roles)
3. [Full Calling Sequence — PVC to Pod Mount](#3-full-calling-sequence--pvc-to-pod-mount)
4. [Data Persistence — Where CSI Responses Are Stored](#4-data-persistence--where-csi-responses-are-stored)
5. [publish_context Data Flow](#5-publish_context-data-flow)
6. [Deployment Topology](#6-deployment-topology)
7. [Pod Lifecycle — Volume Attach/Detach Cycling](#7-pod-lifecycle--volume-attachdetach-cycling)

---

## 1. CSI Spec vs Kubernetes Reality

The CSI specification defines a single entity called **CO (Container Orchestrator)** that calls all CSI RPCs. In Kubernetes, the CO role is **split across multiple components**:

| CSI Spec Concept | Kubernetes Implementation |
|------------------|--------------------------|
| CO calls `CreateVolume` / `DeleteVolume` | **external-provisioner** sidecar |
| CO calls `ControllerPublishVolume` / `ControllerUnpublishVolume` | **external-attacher** sidecar (triggered by AD Controller creating `VolumeAttachment` CRs) |
| CO calls `NodeStageVolume` / `NodeUnstageVolume` | **kubelet** (Volume Manager) |
| CO calls `NodePublishVolume` / `NodeUnpublishVolume` | **kubelet** (Volume Manager) |
| CO calls `NodeGetInfo` | **node-driver-registrar** sidecar (at startup) |

**Key insight:** kubelet **never** calls Controller RPCs (`ControllerPublishVolume`, `CreateVolume`, etc.). It only calls Node RPCs (`NodeStageVolume`, `NodePublishVolume`, etc.). The Controller RPCs are handled by sidecars running alongside the CSI Controller Plugin.

---

## 2. Component Roles

### 2.1 AD Controller (Attach/Detach Controller)

The AD Controller is a **built-in Kubernetes controller loop** inside `kube-controller-manager` (control plane). It is **not** a CSI component — it's core Kubernetes.

- **What it does:** Watches for Pods scheduled to nodes that need volumes. Creates/deletes `VolumeAttachment` custom resource objects in the K8S API.
- **What it does NOT do:** It never talks to CSI drivers directly. It only creates/deletes K8S `VolumeAttachment` objects.

```
kube-controller-manager (control plane)
  └── AD Controller (one of many built-in controllers)
        │
        │  Watches: Pods scheduled to nodes that reference PVCs
        │  Creates: VolumeAttachment CR in K8S API
        │
        │  Does NOT call CSI RPCs.
        │  Only manages K8S VolumeAttachment objects.
```

### 2.2 external-provisioner (Sidecar)

- **Runs in:** Same pod as CSI Controller Plugin
- **Watches:** PVC objects in K8S API
- **Action:** When it sees an unbound PVC targeting its StorageClass, it calls `CreateVolume` via gRPC on the CSI Controller Plugin. Takes the response and creates a PV object, then binds PVC↔PV.
- **Reverse:** When PVC is deleted (with `reclaimPolicy: Delete`), calls `DeleteVolume` on the CSI Controller Plugin and deletes the PV.

### 2.3 external-attacher (Sidecar)

- **Runs in:** Same pod as CSI Controller Plugin
- **Watches:** `VolumeAttachment` objects in K8S API (created by AD Controller)
- **Action:** When it sees a new `VolumeAttachment`, it calls `ControllerPublishVolume` via gRPC on the CSI Controller Plugin. Stores the returned `publish_context` in `VolumeAttachment.status.attachmentMetadata`.
- **Reverse:** When `VolumeAttachment` is marked for deletion, calls `ControllerUnpublishVolume`.

### 2.4 kubelet (Volume Manager)

- **Runs on:** Every worker node
- **Watches:** `VolumeAttachment.status.attached == true`
- **Action:** Calls `NodeStageVolume` and `NodePublishVolume` via gRPC on the CSI Node Plugin (local unix socket). Passes `publish_context` (from `VolumeAttachment.status.attachmentMetadata`) and `volumeContext` (from `PV.spec.csi.volumeAttributes`) to these RPCs.

### 2.5 node-driver-registrar (Sidecar)

- **Runs in:** Same pod as CSI Node Plugin (DaemonSet, every node)
- **Action:** At startup, calls `NodeGetInfo` on the CSI Node Plugin and registers the driver with kubelet. Creates/updates the `CSINode` CR with the node's ID and topology.

---

## 3. Full Calling Sequence — PVC to Pod Mount

```
User creates PVC (storageClass: my-csi-driver)
    │
    ▼
┌─────────────────────────────────────────────────────────────────────┐
│  external-provisioner sidecar (watches PVC objects)                 │
│                                                                     │
│  Sees unbound PVC → gRPC call to CSI Controller Plugin:             │
│    CreateVolume(name, capacityRange, parameters, ...)               │
│                                                                     │
│  Gets back: CreateVolumeResponse {                                  │
│    Volume.VolumeId:      "vol-xxx"                                  │
│    Volume.VolumeContext: { key1: val1, key2: val2 }                 │
│    Volume.CapacityBytes: 107374182400                               │
│  }                                                                  │
│                                                                     │
│  Creates PV object in K8S API (see Section 4)                       │
│  Binds PVC ↔ PV                                                     │
└─────────────────────────────────────────────────────────────────────┘
    │
    ▼  PVC is now Bound to PV

User/Controller creates Pod referencing the PVC
    │
    ▼
┌─────────────────────────────────────────────────────────────────────┐
│  kube-scheduler                                                     │
│  Schedules Pod to Node X                                            │
└─────────────────────────────────────────────────────────────────────┘
    │
    ▼
┌─────────────────────────────────────────────────────────────────────┐
│  AD Controller (inside kube-controller-manager)                     │
│                                                                     │
│  Sees: Pod on Node X needs VolumeId "vol-xxx"                       │
│  Creates VolumeAttachment CR:                                       │
│    {                                                                │
│      spec:                                                          │
│        attacher: "my-csi-driver"                                    │
│        nodeName: "node-x"                                           │
│        source:                                                      │
│          persistentVolumeName: "pv-xxx"                              │
│    }                                                                │
└─────────────────────────────────────────────────────────────────────┘
    │
    ▼
┌─────────────────────────────────────────────────────────────────────┐
│  external-attacher sidecar (watches VolumeAttachment objects)       │
│                                                                     │
│  Sees new VolumeAttachment → reads PV to get volumeId + context     │
│  gRPC call to CSI Controller Plugin:                                │
│    ControllerPublishVolume(                                         │
│      volumeId:      "vol-xxx"         ← from PV.spec.csi.volumeHandle
│      nodeId:        "node-x-id"       ← from CSINode CR            │
│      volumeContext: { key1: val1 }    ← from PV.spec.csi.volumeAttributes
│    )                                                                │
│                                                                     │
│  Gets back: ControllerPublishVolumeResponse {                       │
│    PublishContext: { nfs_export: "...", nfs_volume_file: "..." }     │
│  }                                                                  │
│                                                                     │
│  Updates VolumeAttachment:                                          │
│    status.attached = true                                           │
│    status.attachmentMetadata = publish_context map                   │
└─────────────────────────────────────────────────────────────────────┘
    │
    ▼  VolumeAttachment is now "attached"

┌─────────────────────────────────────────────────────────────────────┐
│  kubelet on Node X (Volume Manager)                                 │
│                                                                     │
│  Sees: volume attached, needs staging/publishing                    │
│  gRPC calls to CSI Node Plugin (local unix socket):                 │
│                                                                     │
│  Step 1: NodeStageVolume(                                           │
│    volumeId:         "vol-xxx"                                      │
│    publishContext:   { nfs_export: "..." }    ← from VolumeAttachment
│    volumeContext:    { key1: val1 }           ← from PV             │
│    stagingTargetPath: "/var/lib/kubelet/plugins/.../globalmount"     │
│  )                                                                  │
│                                                                     │
│  Step 2: NodePublishVolume(                                         │
│    volumeId:         "vol-xxx"                                      │
│    publishContext:   { ... }                                        │
│    stagingTargetPath: "..."                                         │
│    targetPath: "/var/lib/kubelet/pods/<pod-uid>/volumes/..."        │
│  )                                                                  │
└─────────────────────────────────────────────────────────────────────┘
    │
    ▼  Volume is mounted into Pod — container starts
```

---

## 4. Data Persistence — Where CSI Responses Are Stored

### 4.1 `CreateVolumeResponse` → PV Object

The `external-provisioner` translates the CSI response into a K8S PersistentVolume:

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: pvc-7f8a9b2c-...               # auto-generated
  annotations:
    pv.kubernetes.io/provisioned-by: my-csi-driver
spec:
  capacity:
    storage: 100Gi                       # ← resp.Volume.CapacityBytes
  accessModes: [ReadWriteOnce]
  claimRef:
    name: my-pvc                         # ← bound to the PVC
    namespace: default
  csi:
    driver: my-csi-driver

    volumeHandle: "vol-xxx"              # ← resp.Volume.VolumeId

    volumeAttributes:                    # ← resp.Volume.VolumeContext (entire map)
      key1: "val1"
      key2: "val2"

  storageClassName: my-storage-class
  persistentVolumeReclaimPolicy: Delete
```

**Mapping table:**

| `CreateVolumeResponse` field | Persisted in PV field | Who reads it later |
|---|---|---|
| `Volume.VolumeId` | `spec.csi.volumeHandle` | external-attacher → `ControllerPublishVolume(req.VolumeId)` |
| `Volume.CapacityBytes` | `spec.capacity.storage` | Informational |
| `Volume.VolumeContext` (entire map) | `spec.csi.volumeAttributes` | external-attacher → `ControllerPublishVolume(req.VolumeContext)`, kubelet → `NodeStageVolume(req.VolumeContext)` |
| `Volume.AccessibleTopology` | `spec.nodeAffinity` | kube-scheduler uses for pod placement |

### 4.2 `ControllerPublishVolumeResponse` → VolumeAttachment Status

The `external-attacher` stores the `publish_context` in the `VolumeAttachment` CR:

```yaml
apiVersion: storage.k8s.io/v1
kind: VolumeAttachment
metadata:
  name: csi-abc123...
spec:
  attacher: my-csi-driver
  nodeName: node-x
  source:
    persistentVolumeName: pv-xxx
status:
  attached: true
  attachmentMetadata:                    # ← resp.PublishContext (entire map)
    nfs_export: "192.168.57.105:/trident_pvc_xxx"
    nfs_volume_file: "volume-ba833668-xxx"
    nfs_mount_options: "rw,hard,intr"
```

### 4.3 Complete K8S Object Map

| K8S Object | Created by | What it stores | Lifetime |
|---|---|---|---|
| **PVC** | User / operator | Storage request (size, storageClass, accessModes) | Until user deletes it |
| **PV** | external-provisioner | `volumeHandle` (volume ID) + `volumeAttributes` (volume context) | Bound to PVC lifetime (with `Delete` reclaim policy) |
| **VolumeAttachment** | AD Controller (spec) + external-attacher (status) | Trigger for ControllerPublish; stores `publish_context` in `.status.attachmentMetadata` | Created when pod scheduled, deleted when pod removed from node |
| **CSINode** | node-driver-registrar | Node ID, topology keys, driver info per node | Exists as long as node exists |
| **CSIDriver** | Cluster admin (manifest) | Driver-level settings: `attachRequired`, `podInfoOnMount`, `fsGroupPolicy` | Cluster-scoped, permanent |

---

## 5. publish_context Data Flow

The most critical data handoff in the CSI architecture:

```
ControllerPublishVolume returns publish_context
        │
        │ stored in
        ▼
VolumeAttachment.status.attachmentMetadata   (in etcd via K8S API)
        │
        │ read by
        ▼
kubelet (Volume Manager on the target node)
        │
        │ passed as parameter to
        ▼
NodeStageVolume(req.PublishContext)    and    NodePublishVolume(req.PublishContext)
```

This is the CSI spec's intended mechanism for **controller-to-node communication**. The Controller Plugin returns an opaque map of strings, and the Node Plugin receives it. They never communicate directly — the `VolumeAttachment` CR is the intermediary.

### What data flows where (summary):

```
                          ┌──────────────┐
                          │ StorageClass │
                          │  parameters  │
                          └──────┬───────┘
                                 │
                                 ▼
                     ┌───────────────────────┐
   PVC created ────► │    CreateVolume()      │
                     │                       │
                     │  Returns:             │
                     │   VolumeId ──────────────────┐
                     │   VolumeContext ─────────────┐│
                     └───────────────────────┘      ││
                                                    ││
                     ┌───────────────────────┐      ││
                     │   PV object (etcd)    │ ◄────┘│
                     │                       │       │
                     │  .csi.volumeHandle ◄──────────┘
                     │  .csi.volumeAttributes ◄──┘
                     └───────────┬───────────┘
                                 │
                    ┌────────────┼────────────────┐
                    │            │                 │
                    ▼            ▼                 ▼
         ┌──────────────┐  ┌─────────────┐  ┌──────────────┐
         │ ControllerPub│  │NodeStageVol  │  │NodePublishVol│
         │  lishVolume() │  │  ume()      │  │  ume()       │
         │              │  │              │  │              │
         │ req.VolumeId │  │req.VolumeCtx │  │req.VolumeCtx │
         │ req.VolCtx   │  │req.PubCtx ◄──── from VolumeAttachment
         │ req.NodeId   │  │              │  │req.PubCtx    │
         │              │  │              │  │              │
         │ Returns:     │  └──────────────┘  └──────────────┘
         │  PubCtx ─────────► VolumeAttachment.status
         └──────────────┘      .attachmentMetadata
```

---

## 6. Deployment Topology

### 6.1 CSI Controller Plugin — Deployment (NOT DaemonSet)

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: csi-controller
spec:
  replicas: 1                    # or 2 with leader election
  template:
    spec:
      containers:
        # Your CSI driver — Controller mode
        - name: csi-plugin
          args: ["--endpoint=unix:///csi/csi.sock", "--role=controller"]

        # Sidecar: watches PVCs → calls CreateVolume/DeleteVolume
        - name: external-provisioner
          image: registry.k8s.io/sig-storage/csi-provisioner:v4.0.0

        # Sidecar: watches VolumeAttachments → calls ControllerPublish/Unpublish
        - name: external-attacher
          image: registry.k8s.io/sig-storage/csi-attacher:v4.5.0

        # Sidecar: watches PVCs for resize → calls ControllerExpandVolume
        - name: external-resizer
          image: registry.k8s.io/sig-storage/csi-resizer:v1.10.0

      volumes:
        - name: socket-dir
          emptyDir: {}            # shared unix socket between sidecars and plugin
```

**Internal architecture of the Controller Deployment Pod:**

The Controller Deployment Pod contains **3+ containers** that share a single unix socket. The sidecars are generic, off-the-shelf K8S SIG-Storage images — they know nothing about Cinder or NFS. Your CSI driver (e.g., `cinder-nfs.csi.openstack.org`) is the only container that contains domain-specific logic.

```
┌─────────────────────────────────────────────────────────────────────────────┐
│  Controller Deployment Pod                                                  │
│                                                                             │
│  ┌─────────────────────────┐                                                │
│  │ external-provisioner    │   Generic sidecar (registry.k8s.io)            │
│  │                         │   - Watches: PVC objects in K8S API            │
│  │  Unbound PVC detected ──┼──────────┐                                     │
│  │  PVC deleted ───────────┼──────────┐│                                    │
│  └─────────────────────────┘          ││                                    │
│                                       ││ gRPC (CreateVolume / DeleteVolume) │
│  ┌─────────────────────────┐          ││                                    │
│  │ external-attacher       │          ││                                    │
│  │                         │          ││                                    │
│  │  VolumeAttachment ──────┼──────────┐│                                    │
│  │  created / deleted      │          │││ gRPC (ControllerPublish/Unpublish)│
│  └─────────────────────────┘          │││                                   │
│                                       │││                                   │
│  ┌─────────────────────────┐          │││                                   │
│  │ external-resizer        │          │││                                   │
│  │                         │          │││                                   │
│  │  PVC resize request ────┼──────────┐│││ gRPC (ControllerExpandVolume)    │
│  └─────────────────────────┘          ││││                                  │
│                                       ││││                                  │
│                          unix socket: ││││  /csi/csi.sock                   │
│                          (emptyDir)   ││││                                  │
│                                       ▼▼▼▼                                  │
│  ┌──────────────────────────────────────────────────────────────────────┐    │
│  │  CSI Controller Plugin  (YOUR DRIVER CODE)                          │    │
│  │  e.g., cinder-nfs.csi.openstack.org                                 │    │
│  │                                                                      │    │
│  │  gRPC server listening on /csi/csi.sock                              │    │
│  │                                                                      │    │
│  │  Implements:                                                         │    │
│  │    CreateVolume()              → Cinder POST /v3/volumes             │    │
│  │                                  + Nova POST /v2/servers (Shadow VM) │    │
│  │    DeleteVolume()              → Nova delete Shadow VM               │    │
│  │                                  + Cinder detach / optional delete   │    │
│  │    ControllerPublishVolume()   → Cinder GET /v3/attachments/{id}     │    │
│  │                                  (extract NFS connection_info)       │    │
│  │    ControllerUnpublishVolume() → No-op (Shadow VM persists)          │    │
│  │    ControllerExpandVolume()    → Cinder POST os-extend               │    │
│  │                                                                      │    │
│  │  External API calls:                                                 │    │
│  │    ┌────────────────┐  ┌────────────────┐  ┌──────────────────┐     │    │
│  │    │ Keystone API   │  │  Cinder API    │  │   Nova API       │     │    │
│  │    │ (auth tokens)  │  │  (volumes,     │  │   (Shadow VM     │     │    │
│  │    │                │  │   attachments) │  │    lifecycle)    │     │    │
│  │    └────────────────┘  └────────────────┘  └──────────────────┘     │    │
│  │         ▲                    ▲                    ▲                  │    │
│  │         └────────────────────┴────────────────────┘                  │    │
│  │                    HTTPS to Target OpenStack                         │    │
│  └──────────────────────────────────────────────────────────────────────┘    │
└─────────────────────────────────────────────────────────────────────────────┘
```

**Key points:**

1. **Single unix socket** — All sidecars and the CSI driver share `/csi/csi.sock` via an `emptyDir` volume. Sidecars are gRPC clients; the CSI driver is the gRPC server.

2. **Sidecars are generic** — `external-provisioner`, `external-attacher`, and `external-resizer` are standard images from `registry.k8s.io/sig-storage/`. They translate K8S API events (PVC created, VolumeAttachment created, PVC resized) into gRPC calls. They have zero knowledge of Cinder, NFS, or Shadow VMs.

3. **CSI driver is your domain logic** — The `cinder-nfs` driver is the only container that knows how to talk to OpenStack APIs. It implements the CSI Controller gRPC service interface and translates CSI RPCs into Cinder/Nova API calls.

4. **Outbound HTTPS only** — The CSI driver talks to the target OpenStack over HTTPS. It does not need to run on the target OpenStack — it just needs network access to Keystone, Cinder, and Nova API endpoints. This is why the Controller Deployment can run on the WRC K8S cluster.

### 6.2 CSI Node Plugin — DaemonSet (every worker node)

```yaml
apiVersion: apps/v1
kind: DaemonSet
metadata:
  name: csi-node
spec:
  template:
    spec:
      containers:
        # Your CSI driver — Node mode
        - name: csi-plugin
          args: ["--endpoint=unix:///csi/csi.sock", "--role=node"]
          securityContext:
            privileged: true      # needs host mount access

        # Sidecar: registers driver with kubelet
        - name: node-driver-registrar
          image: registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.10.0

      volumes:
        - name: kubelet-dir
          hostPath:
            path: /var/lib/kubelet
        - name: registration-dir
          hostPath:
            path: /var/lib/kubelet/plugins_registry
```

### 6.3 Topology Summary

| Component | K8S Workload | Replicas | Where it runs | CSI RPCs it handles |
|-----------|-------------|----------|---------------|---------------------|
| **CSI Controller Plugin** + sidecars | **Deployment** | 1 (or 2 HA) | Any node | `CreateVolume`, `DeleteVolume`, `ControllerPublishVolume`, `ControllerUnpublishVolume`, `ControllerExpandVolume`, `ListVolumes`, `ValidateVolumeCapabilities` |
| **CSI Node Plugin** + registrar | **DaemonSet** | 1 per node | Every worker node that needs to mount volumes | `NodeStageVolume`, `NodeUnstageVolume`, `NodePublishVolume`, `NodeUnpublishVolume`, `NodeGetInfo`, `NodeGetCapabilities` |
| **AD Controller** | Built-in in `kube-controller-manager` | 1 (leader-elected) | Control plane | N/A (manages VolumeAttachment CRs, not CSI RPCs) |
| **external-provisioner** | Sidecar in Controller Deployment | — | Same pod as Controller | Calls `CreateVolume`, `DeleteVolume` |
| **external-attacher** | Sidecar in Controller Deployment | — | Same pod as Controller | Calls `ControllerPublishVolume`, `ControllerUnpublishVolume` |
| **node-driver-registrar** | Sidecar in Node DaemonSet | — | Same pod as Node | Calls `NodeGetInfo` at startup |

### 6.4 Communication Paths

```
┌─────────────────────────────────────────────────────────────────────┐
│  Control Plane                                                      │
│                                                                     │
│  ┌────────────────────────┐                                         │
│  │ kube-controller-manager│                                         │
│  │  └── AD Controller     │ ── creates/deletes ──► VolumeAttachment │
│  │  └── PV Controller     │ ── binds ──► PVC ↔ PV                   │
│  └────────────────────────┘                                         │
│                                                                     │
│  ┌────────────────────────┐                                         │
│  │ kube-scheduler         │ ── schedules pod → node                 │
│  └────────────────────────┘                                         │
└─────────────────────────────────────────────────────────────────────┘
         │                              │
         │ K8S API (etcd)               │ K8S API
         ▼                              ▼
┌────────────────────────────┐  ┌────────────────────────────────────┐
│ Controller Deployment Pod  │  │ Node DaemonSet Pod (on each node)  │
│                            │  │                                    │
│ ┌──────────────────┐       │  │ ┌──────────────────────┐           │
│ │external-provisioner       │  │ │node-driver-registrar │           │
│ │ watches: PVC     │       │  │ │ calls NodeGetInfo    │           │
│ └────────┬─────────┘       │  │ └──────────┬───────────┘           │
│          │ gRPC            │  │            │ gRPC                  │
│ ┌────────┴─────────┐       │  │ ┌──────────┴───────────┐           │
│ │external-attacher │       │  │ │                      │           │
│ │ watches:         │       │  │ │  CSI Node Plugin     │           │
│ │ VolumeAttachment │       │  │ │  NodeStageVolume()   │           │
│ └────────┬─────────┘       │  │ │  NodePublishVolume() │           │
│          │ gRPC            │  │ │                      │ ◄── kubelet
│ ┌────────┴─────────┐       │  │ └──────────────────────┘   (gRPC) │
│ │                  │       │  │                                    │
│ │ CSI Controller   │       │  └────────────────────────────────────┘
│ │ Plugin           │       │
│ │ CreateVolume()   │       │
│ │ CtrlPublishVol() │       │
│ │ DeleteVolume()   │       │
│ └──────────────────┘       │
└────────────────────────────┘
```

---

## 7. Pod Lifecycle — Volume Attach/Detach Cycling

When a pod using a CSI volume is deleted and a new pod is created (e.g., CDI precopy stage cycling), the full volume lifecycle fires:

### 7.1 Pod Deletion Sequence

```
Pod deleted on Node X:
  1. kubelet → NodeUnpublishVolume()     (unmount bind mount from pod path)
  2. kubelet → NodeUnstageVolume()       (unmount from staging/global path)
  3. AD Controller deletes VolumeAttachment CR
  4. external-attacher → ControllerUnpublishVolume()
  5. VolumeAttachment CR deleted from etcd
```

### 7.2 New Pod Creation Sequence

```
New Pod scheduled to Node X (or Node Y):
  1. AD Controller creates new VolumeAttachment CR
  2. external-attacher → ControllerPublishVolume()
  3. external-attacher updates VolumeAttachment.status (attached=true, metadata=publish_context)
  4. kubelet → NodeStageVolume(publishContext from VolumeAttachment.status)
  5. kubelet → NodePublishVolume()
  6. Pod container starts with volume mounted
```

### 7.3 PVC Deletion Sequence

```
PVC deleted:
  1. If pod still running → pod must be deleted first (steps 7.1)
  2. PV reclaim policy = Delete → external-provisioner triggers:
     external-provisioner → DeleteVolume(volumeId from PV.spec.csi.volumeHandle)
  3. PV object deleted from etcd
```

### 7.4 Important: PV Survives Pod Cycling

The PV (and its `volumeHandle` + `volumeAttributes`) persists in etcd **as long as the PVC exists**. Pod deletion/recreation does not affect the PV. Only PVC deletion (with `reclaimPolicy: Delete`) triggers `DeleteVolume` and PV removal.

```
PVC lifecycle:        ════════════════════════════════════════════════►
PV lifecycle:         ════════════════════════════════════════════════►
                         (created by provisioner, deleted with PVC)

Pod 1 lifecycle:      ══════════╗
VolumeAttachment 1:   ══════════╝  (deleted when pod 1 removed)

Pod 2 lifecycle:                  ══════════╗
VolumeAttachment 2:               ══════════╝  (new VA for pod 2)

Pod 3 lifecycle:                              ══════════╗
VolumeAttachment 3:                           ══════════╝
```

---

## References

- [CSI Specification](https://github.com/container-storage-interface/spec/blob/master/spec.md)
- [Kubernetes CSI Developer Documentation](https://kubernetes-csi.github.io/docs/)
- [Kubernetes CSI Sidecar Containers](https://kubernetes-csi.github.io/docs/sidecar-containers.html)
- [VolumeAttachment API](https://kubernetes.io/docs/reference/kubernetes-api/config-and-storage-resources/volume-attachment-v1/)
- [CSINode API](https://kubernetes.io/docs/reference/kubernetes-api/config-and-storage-resources/csi-node-v1/)
- [CSIDriver API](https://kubernetes.io/docs/reference/kubernetes-api/config-and-storage-resources/csi-driver-v1/)
