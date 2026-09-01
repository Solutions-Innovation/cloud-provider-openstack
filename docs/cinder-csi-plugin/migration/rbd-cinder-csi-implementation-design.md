# RBD-Backed Cinder CSI Plugin — Detailed Implementation Design

| Field          | Value                                                                                                                     |
|----------------|---------------------------------------------------------------------------------------------------------------------------|
| **Authors**    | Wind River Migration Framework Team                                                                                        |
| **Status**     | Draft — pairs with a *Proposed* design; no RBD driver code exists yet                                                      |
| **Created**    | 2026-09-01                                                                                                                 |
| **Depends On** | [Cinder RBD CSI Plugin for WRCP Migration](rbd-backed-cinder-volume-for-wrcp-migration.md) (the *proposal*; referred to below as **P§n**) |
| **Related**    | [iSCSI-Cinder CSI Implementation Design](iscsi-cinder-csi-implementation-design.md) (**implemented sibling — the reuse baseline**), [NFS-Cinder CSI Implementation Design](nfs-cinder-csi-implementation-design.md) |
| **Reference**  | [Kubernetes CSI Architecture Reference](kubernetes-csi-architecture-reference.md)                                          |
| **Repository** | `kubernetes/cloud-provider-openstack`                                                                                      |
| **Driver name**| `cinder-rbd.csi.windriver.com`                                                                                             |
| **Design branch** | `wndrvr/cinder-rbd-csi-plugin-design`                                                                                   |

**Reading contract.** This document is the *implementation* counterpart of the
proposal. The proposal answers *what and why*; this document answers *which
package, which type, which file, which phase*. Every normative statement here
carries a status marker:

| Marker | Meaning |
|---|---|
| **[V]** | **Validated** on the WRCP 24.09 lab. Treat as fact. |
| **[R]** | **Reused** from the implemented `cinder-iscsi` driver at a cited file. Treat as existing code. |
| **[P]** | **Proposed** implementation behavior. Not written yet. |
| **[Q]** | **Open contract / qualification-gated.** Must be answered by a lab measurement before the corresponding code is frozen. |

Nothing in `pkg/csi/cinder-rbd/` exists today. Statements marked **[P]** describe
code to be written; they must not be quoted as available behavior.

---

## Table of Contents

- [1. Summary](#1-summary)
  - [1.1 Conclusion](#11-conclusion)
  - [1.2 What is reused verbatim, adapted, and newly written](#12-what-is-reused-verbatim-adapted-and-newly-written)
- [2. Baseline analysis — the implemented `cinder-iscsi` driver](#2-baseline-analysis--the-implemented-cinder-iscsi-driver)
  - [2.1 Package layout of the baseline](#21-package-layout-of-the-baseline)
  - [2.2 Baseline controller behavior worth inheriting](#22-baseline-controller-behavior-worth-inheriting)
  - [2.3 Baseline node behavior that must NOT be inherited](#23-baseline-node-behavior-that-must-not-be-inherited)
  - [2.4 Baseline defects and dead knobs not to replicate](#24-baseline-defects-and-dead-knobs-not-to-replicate)
- [3. Architectural assessment — extend or fork](#3-architectural-assessment--extend-or-fork)
- [4. Driver comparison matrix](#4-driver-comparison-matrix)
- [5. Folder structure](#5-folder-structure)
  - [5.1 New directories and files](#51-new-directories-and-files)
  - [5.2 Files to modify in the existing tree](#52-files-to-modify-in-the-existing-tree)
- [6. Interface design](#6-interface-design)
  - [6.1 `IOpenStackRBD`](#61-iopenstackrbd)
  - [6.2 RBD connection information and normalization](#62-rbd-connection-information-and-normalization)
  - [6.3 The Cinder connector — CLOSED](#63-the-cinder-connector--closed)
  - [6.4 Node ID contract — CLOSED](#64-node-id-contract--closed)
  - [6.5 `publish_context` key set](#65-publish_context-key-set)
  - [6.6 `RBDMapper` — the node data-path abstraction](#66-rbdmapper--the-node-data-path-abstraction)
  - [6.7 `CephCredentialProvider` and runtime credential files](#67-cephcredentialprovider-and-runtime-credential-files)
  - [6.8 Staging record and map-intent format](#68-staging-record-and-map-intent-format)
  - [6.9 Configuration structs and proposal-to-config mapping](#69-configuration-structs-and-proposal-to-config-mapping)
  - [6.10 Cinder attachment lifecycle state machine](#610-cinder-attachment-lifecycle-state-machine)
- [7. Prerequisites and runtime validation](#7-prerequisites-and-runtime-validation)
  - [7.1 Environment prerequisites](#71-environment-prerequisites)
  - [7.2 Startup validation vs. per-RPC validation](#72-startup-validation-vs-per-rpc-validation)
  - [7.3 Microversion detection](#73-microversion-detection)
  - [7.4 Identity gate — the five checks before a writable map](#74-identity-gate--the-five-checks-before-a-writable-map)
- [8. CSI RPC implementation map](#8-csi-rpc-implementation-map)
  - [8.1 Identity service](#81-identity-service)
  - [8.2 Controller service](#82-controller-service)
  - [8.3 Node service](#83-node-service)
  - [8.4 gRPC error code mapping](#84-grpc-error-code-mapping)
- [9. Recovery and reconciliation design](#9-recovery-and-reconciliation-design)
  - [9.1 Node startup reconciliation](#91-node-startup-reconciliation)
  - [9.2 Failure matrix to code mapping](#92-failure-matrix-to-code-mapping)
- [10. Build, packaging, and deployment](#10-build-packaging-and-deployment)
  - [10.1 Makefile changes](#101-makefile-changes)
  - [10.2 Dockerfile stage](#102-dockerfile-stage)
  - [10.3 Node DaemonSet host access](#103-node-daemonset-host-access)
  - [10.4 Helm chart and manifests](#104-helm-chart-and-manifests)
  - [10.5 StorageClass, PVC, and CDI StorageProfile](#105-storageclass-pvc-and-cdi-storageprofile)
  - [10.6 Metrics and logging redaction](#106-metrics-and-logging-redaction)
  - [10.7 Release workflow](#107-release-workflow)
- [11. Testing strategy](#11-testing-strategy)
- [12. Development phases](#12-development-phases)
  - [Phase 0 — Qualification gate (blocking)](#phase-0--qualification-gate-blocking)
  - [Phase 1 — Scaffold, interfaces, buildable binary](#phase-1--scaffold-interfaces-buildable-binary)
  - [Phase 2 — OpenStack layer and controller service](#phase-2--openstack-layer-and-controller-service)
  - [Phase 3 — Node data path (krbd)](#phase-3--node-data-path-krbd)
  - [Phase 4 — Recovery, reconciliation, fault injection](#phase-4--recovery-reconciliation-fault-injection)
  - [Phase 5 — Packaging, chart, manifests, release](#phase-5--packaging-chart-manifests-release)
  - [Phase 6 — Migration workflow integration and full qualification](#phase-6--migration-workflow-integration-and-full-qualification)
- [13. Traceability matrix](#13-traceability-matrix)
- [14. Open contracts and decision log](#14-open-contracts-and-decision-log)
- [Appendix A: iSCSI vs. RBD at a glance](#appendix-a-iscsi-vs-rbd-at-a-glance)
- [Appendix B: File-level reuse decision matrix](#appendix-b-file-level-reuse-decision-matrix)
- [Appendix C: Full `driver.conf` reference](#appendix-c-full-driverconf-reference)
- [Appendix D: Invariants — the "never" list](#appendix-d-invariants--the-never-list)
- [References](#references)

---

## 1. Summary

This document specifies how to implement `cinder-rbd.csi.windriver.com`: a
block-only CSI driver that provisions Ceph RBD-backed Cinder volumes, reserves
them with **Cinder v3 self-service attachment records** (microversion ≥ 3.27,
no Nova), and maps them on WRCP worker nodes with **exclusive kernel RBD
(`krbd`)**.

The driver exists to serve VM migration into WRCP. A migration workflow
(KubeVirt CDI for VMware-to-OpenStack, an NBD pipeline for
OpenStack-to-OpenStack) writes a source disk image into a raw-block PVC. CDI
creates and deletes several pods across precopy and cutover, so the volume must
return to Cinder `available` between pods, and the Wind River migration
Blueprint must find the volume intact after the PVC is deleted.

Two properties drive nearly every decision below:

1. **The control plane is already solved.** The implemented `cinder-iscsi`
   driver contains a working Cinder attachment state machine — on-demand
   record creation, attachment ID in Cinder volume metadata, single 404
   replacement retry, delete-on-unpublish, and retained Cinder volumes.
   The RBD controller is a port, not a design exercise. **[R]**
2. **The data plane is entirely new.** There is no RBD analogue of
   `iscsiadm`, IQNs, CHAP, sessions, or `/dev/disk/by-path`. The node path is a
   credentialed, identity-verified, exclusive kernel map whose authoritative
   state lives in the kernel and in Ceph, not in a file the plugin wrote. **[P]**

### 1.1 Conclusion

**A new package `pkg/csi/cinder-rbd/` is required. Neither the upstream
`pkg/csi/cinder/` driver nor the implemented `pkg/csi/cinder-iscsi/` driver can
be extended in place.**

| Dimension | `cinder-csi` (upstream) | `cinder-iscsi` (implemented) | `cinder-rbd` requirement | Extend upstream? | Extend iSCSI? |
|---|---|---|---|:--:|:--:|
| Driver name | `cinder.csi.openstack.org` (const) | `cinder-iscsi.csi.windriver.com` (const) | `cinder-rbd.csi.windriver.com` (const) | **No** | **No** |
| CSIDriver object | separate | separate | separate — needs its own registration socket | **No** | **No** |
| Volume reservation | Nova attachment | Cinder v3 attachment record | Cinder v3 attachment record | **No** | **Yes (port)** |
| Node data path | kernel block via Nova | `iscsiadm` login → `/dev/disk/by-path/...` | `rbd device map --device-type krbd --exclusive` → `/dev/rbdN` | **No** | **No** |
| Node identity | Nova instance UUID | `hostname;iqn;ip` | bare hostname; no initiator identity **[V]** | **No** | **No** |
| Node credentials | none | optional CHAP from `connection_info` | mandatory Ceph key from an operator-managed Secret, never in `connection_info` | **No** | **No** |
| Single-writer enforcement | CSI access mode | CSI access mode + per-initiator target | CSI access mode + **Ceph exclusive-lock** | **No** | **No** |
| Host packages | none | `open-iscsi` | Ceph 18.2.x `rbd` CLI in the plugin image | **No** | **No** |
| Volume mode | Block + Filesystem | Block only | Block only | **No** | **Yes** |
| Restart survival of node state | n/a | iscsiadm node DB | kernel-owned map, no userspace process | **No** | **No** |

Extending `cinder-iscsi` would mean a runtime-switched driver name, a
runtime-switched node ID format, two mutually exclusive credential models, and
two incompatible device abstractions behind one CSIDriver object. The monorepo
precedent is already a fork per backend (`pkg/csi/manila/`,
`pkg/csi/cinder-nfs/`, `pkg/csi/cinder-iscsi/`); this follows it.

### 1.2 What is reused verbatim, adapted, and newly written

| Layer | Source | Treatment | Est. |
|---|---|---|---|
| gRPC server, endpoint parsing, `logGRPC` | `pkg/csi/cinder-iscsi/server.go` | **copy**, change package name only | ~120 LOC |
| Capability factories, `New*Server`, `RunServicesInitialized` | `pkg/csi/cinder-iscsi/utils.go` | **copy**, retarget types | ~135 LOC |
| Cinder volume CRUD, status waiter, metadata read-modify-write | `openstack/openstack_volumes.go` | **copy**, rename receiver | ~300 LOC |
| Cinder attachment CRUD, microversion discovery | `openstack/openstack_attachments.go` | **copy** the request plumbing, **replace** `parseISCSIConnectionInfo` | ~320 LOC |
| Provider init, gcfg loading, metrics registration | `openstack/openstack.go` | **adapt** — new interface, new opts structs | ~400 LOC |
| Controller RPC flows | `controllerserver.go` | **adapt** — same state machine, RBD validation | ~640 LOC |
| Identity server (mode-aware Probe) | `identityserver.go` | **adapt** — node readiness probes the `rbd` CLI | ~110 LOC |
| Driver struct, capabilities, wiring | `driver.go` | **rewrite** | ~230 LOC |
| Connection info normalization + publish context | `connectioninfo.go` | **new** | ~250 LOC |
| `RBDMapper` (krbd, sysfs, `rbd device list`) | — | **new** | ~600 LOC |
| Ceph credential provider + runtime `ceph.conf`/keyring | — | **new** | ~250 LOC |
| Staging record store + startup reconciler | — | **new** | ~400 LOC |
| Node RPC flows | `nodeserver.go` | **rewrite** on top of the above | ~550 LOC |

Roughly 45 % of the driver is a mechanical port of proven code; the node data
path and its recovery logic are the genuinely new engineering and carry the
qualification risk.

---

## 2. Baseline analysis — the implemented `cinder-iscsi` driver

All facts in this section were read from the tree on branch
`wndrvr/cinder-rbd-csi-plugin-design` and are marked **[R]**. They define the
port surface.

### 2.1 Package layout of the baseline

```text
pkg/csi/cinder-iscsi/                 # package iscsi
├── driver.go              (202)  Driver, DriverOpts, NewDriver, Setup{Controller,Node}Service, Run
├── controllerserver.go    (634)  Create/Delete/Publish/Unpublish/Validate/GetCapabilities
├── nodeserver.go          (510)  NodeGetInfo, Stage/Unstage, Publish/Unpublish, Stats
├── identityserver.go      (109)  GetPluginInfo, mode-aware Probe, GetPluginCapabilities
├── connectioninfo.go      (110)  PublishContext keys, ParseNodeID, BuildPublishContext, Validate*
├── iscsi.go               (417)  ISCSIInitiator iface + iscsiadm impl, device path build/parse
├── iscsi_mock.go           (73)  testify mock
├── server.go              (119)  NonBlockingGRPCServer (copy of pkg/csi/cinder/server.go)
├── utils.go               (134)  capability factories, New*Server, ParseEndpoint, logGRPC
├── controllerserver_test.go (833)
├── nodeserver_test.go       (960)
└── openstack/                        # package openstack
    ├── openstack.go            (400)  IOpenStackISCSI, opts structs, provider init, microversions
    ├── openstack_volumes.go    (300)  volume CRUD, WaitVolumeTargetStatus, metadata RMW
    ├── openstack_attachments.go (318)  attachment CRUD, DiscoverCinderCapabilities, conn parse
    └── openstack_mock.go       (199)  testify mock

cmd/cinder-iscsi-csi-plugin/main.go   (116)  cobra entry point, mode flags
```

Shared monorepo imports actually used: `pkg/util/mount` (`IMount`,
`GetMountProvider`, `DeviceStats`), `pkg/util/errors` (`IsNotFound`),
`pkg/util/RoundUpSize`, `pkg/version`, `pkg/client` (auth), `pkg/metrics`.
**Nothing is imported from `pkg/csi/cinder`** — `server.go` and `utils.go` are
file-level copies. The RBD driver copies them again. **[R]**

### 2.2 Baseline controller behavior worth inheriting

These are the mechanisms the RBD controller ports. Each is real code today.

| Mechanism | Baseline implementation | Port note |
|---|---|---|
| Reserved attachment create | `reservedAttachmentCreateOpts{VolumeUUID}` — a custom opts type, because upstream gophercloud `CreateOpts` serializes `"instance_uuid":""` and Cinder returns 400 | copy verbatim |
| Attachment ID persistence | Cinder volume metadata, key composed by `metadataKey(prefix, suffix)`; default prefix `csi` → `csi.attachment_id` | prefix default becomes `csi.rbd` (P§9) |
| Metadata write | read-modify-write via `GetVolume` + `volumes.Update{Metadata:merged}`; there is no metadata sub-resource call | copy mechanism; make persistence failure fatal for newly created RBD attachments |
| Publish recovery | metadata is the **only** source of the current attachment ID; PV `volumeContext` is deliberately not a fallback because it goes stale | copy verbatim, same comment |
| Stale attachment | `UpdateAttachmentConnector` → `cpoerrors.IsNotFound` → create replacement, persist metadata, retry update **once** | copy verbatim |
| Completion | `CompleteAttachment` short-circuits when `!cinderCaps.SupportsV344`; errors are logged and swallowed | copy helper semantics; invoke only after normalized RBD connection validation |
| Delete idempotency | `DeleteAttachment` swallows HTTP 404; `DeleteVolume` swallows 404 | copy verbatim |
| Retain-by-default | baseline can delete by policy; RBD initially retains unconditionally because the controller cannot prove absence of host krbd maps | adapt; automatic delete is qualification-gated |
| Microversion discovery | `attachments.List(limit 1)` against a 3.27-pinned client (fatal on failure), then a 3.44-pinned client (advisory) | copy verbatim |
| Per-microversion client | `blockStorageClient(mv)` builds a fresh `ServiceClient` per call because gophercloud's `Microversion` field is mutable shared state | copy verbatim |
| Status waiter | `WaitVolumeTargetStatus` derives backoff steps from the timeout, fails fast on `error*` states | copy verbatim |
| Block-only rejection | every capability-bearing RPC rejects `VolumeCapability.GetMount() != nil` | copy verbatim |

### 2.3 Baseline node behavior that must NOT be inherited

`iscsi.go` and the iSCSI half of `nodeserver.go` have **no RBD analogue**. Do
not port:

- `ISCSIInitiator` and every `iscsiadm` invocation, including the
  `--op new` existence pre-check and the separate `login_timeout` update call.
- `BuildDevicePath` / `ParseDevicePath` / `devicePathRe` and the
  `/dev/disk/by-path/ip-…-iscsi-…-lun-N` convention.
- `ReadInitiatorIQN`, the `hostname;iqn;ip` node ID, and `GetInterfaceIP` as an
  *initiator* identity source.
- CHAP handling and the treatment of `connection_info` as a credential carrier.
  For RBD this is forbidden: Ceph keys never travel in `connection_info`
  (CVE-2020-10755) and never enter `publish_context`.
- The "device path file is the staging truth" pattern. The *file* is reused as
  a mechanism (see §6.8) but is demoted to a hint. The node-scoped intent is
  authoritative for ownership; the kernel and Ceph are authoritative for live
  identity.

One node-layer idea *is* portable: `WaitForDevice`'s bounded stat-poll loop, and
the practice of making the device-path prefix a package `var` so tests can
redirect it to `t.TempDir()` instead of touching real `/dev` entries. **[R]**

### 2.4 Baseline defects and dead knobs not to replicate

| Baseline issue | Decision for RBD |
|---|---|
| `Makefile` declares `test-cinder-iscsi-csi-sanity` but `tests/sanity/cinder-iscsi/` does not exist | either create `tests/sanity/cinder-rbd/` in Phase 5 or omit the target; do not add a dangling target |
| `CinderCapabilities.MaxMicroversion` is declared and never assigned | drop the field or populate it |
| `ISCSIOpts.CHAPAuthEnabled`, `ISCSIInterface`, `VolumeOpts.DetachTimeout` are parsed and never read | do not create dead knobs; every field in `RBDOpts` must have a reader |
| Documented struct-comment "defaults" are not enforced in Go (only a few are) | centralize defaulting in one `applyDefaults()` per opts struct, and unit-test it |
| `topologyKey` declared but unused; `NodeGetInfo` returns no topology | omit the constant until topology is actually implemented |
| Only two `_test.go` files; `openstack/`, `driver.go`, `iscsi.go` have no direct tests | require tests for `connectioninfo.go`, `rbdmapper`, credential provider, and the reconciler |
| **`Run()` passes the concrete `*controllerServer`/`*nodeServer` into `csi.ControllerServer`/`csi.NodeServer` parameters.** A nil pointer in an interface is non-nil, so `server.go`'s `if cs != nil` guard passes, the service is registered with a nil receiver, and the first controller RPC **segfaults the process**. Confirmed live on the RBD scaffold before fixing (§12 Phase 1 findings); the iSCSI baseline shares the defect, where it would kill a privileged node DaemonSet | convert to interfaces through an explicit nil check (`Driver.csiServices`) and cover it with a regression test |
| Three stale tests encoded the pre-`ea84cbb8` "attachment rotation" design; the first panicked and **aborted the test binary**, masking every test after it (14 of 81 ran) | **Fixed 2026-09-01** during Phase 2: `TestControllerPublishVolume_Success` now proves the stale PV `volumeContext` is ignored in favour of metadata; the two `ControllerUnpublish` tests now expect delete-and-clear with no replacement record. Suite is 81/81 green |
| `openstack.go:352` returned a capitalized error string, the only `golangci-lint` finding in the whole repository, so `make check` failed repo-wide | **Fixed 2026-09-01**: lowercased. Repo-wide lint is now clean |

---

## 3. Architectural assessment — extend or fork

The decision is a fork with disciplined code copying, not a shared abstraction
layer. The reasoning, stated once:

**Why not a shared `pkg/csi/cinderbase/` extracted from `cinder-iscsi`?**
Tempting, and it would remove the ~1 400 LOC of duplicated controller and
OpenStack plumbing. Rejected for this release because:

1. `cinder-iscsi` is on a separate delivery branch
   (`wndrvr/cinder-iscsi-csi-plugin-impl`) with its own release tags. Extracting
   a shared package forces a coordinated refactor of a driver that is already
   in migration use, on a branch that must stay releasable.
2. The connection-info type is the natural generic parameter, and Go generics
   over a `parse → validate → publish_context` pipeline buys little compared to
   two ~250-line concrete implementations.
3. The RBD connector and node-ID contracts were open until Phase 0 closed
   them (§6.3–6.4); extracting a shared interface before they were measured
   would have frozen the wrong abstraction. They are now settled, so this
   objection has weakened — but objections 1 and 2 still hold.

**Revisit trigger:** after the RBD driver passes Phase 6 qualification and both
drivers' controller code has converged for one release, extract
`pkg/csi/cinderattach/` covering `openstack_volumes.go`,
`openstack_attachments.go`, `server.go`, `utils.go`, and the metadata helpers.
That is deliberately deferred, not forgotten. Record it as a follow-up issue in
Phase 5.

**Reusable-as-is components (import, do not copy):** `pkg/client`,
`pkg/metrics`, `pkg/util/errors`, `pkg/util/mount`, `pkg/util`, `pkg/version`,
`pkg/csi/csi.go`.

---

## 4. Driver comparison matrix

| Aspect | `cinder-csi` | `cinder-nfs` | `cinder-iscsi` | **`cinder-rbd`** |
|---|---|---|---|---|
| Volume lock | Nova attachment | Shadow VM | Cinder attachment record | Cinder attachment record **+ Ceph exclusive lock** |
| Nova dependency | yes | yes | **no** | **no** |
| Controller publish | Nova `AttachVolume` | read Shadow VM `connection_info` | update attachment connector → iSCSI target | update attachment connector → **RBD pool/image/monitors** |
| Controller unpublish | Nova detach | no-op | delete attachment record | delete attachment record |
| Node stage | `FormatAndMount` | `mount -t nfs` | `iscsiadm --login` | `rbd device map --device-type krbd --exclusive` |
| Node unstage | unmount | umount | logout + node DB delete | identity-verified flush + `rbd device unmap` |
| Device identity | `/dev/vdX` | n/a | portal + IQN + LUN | **Ceph FSID + pool + image** |
| Node credentials | none | none | optional CHAP (from Cinder) | **mandatory Ceph key (from operator Secret)** |
| Access mode | `SINGLE_NODE_WRITER` | `MULTI_NODE_MULTI_WRITER` | `SINGLE_NODE_WRITER` | `SINGLE_NODE_WRITER` |
| Volume mode | Block + FS | FS | Block only | **Block only** |
| external-attacher | yes | no | yes | **yes** |
| Delete default | delete | delete | **retain** | **retain only initially** |
| Node state owner | kubelet | kernel mount | iscsiadm DB | **kernel (`/sys/bus/rbd`)** |

---

## 5. Folder structure

### 5.1 New directories and files

```text
pkg/csi/cinder-rbd/                      # package name: rbd
├── driver.go                    # Driver, DriverOpts, NewDriver, Setup*Service, Run          [P]
├── controllerserver.go          # Controller RPCs — ported attachment state machine          [P]
├── nodeserver.go                # Node RPCs — stage/unstage over RBDMapper                   [P]
├── identityserver.go            # Identity RPCs, mode-aware Probe (rbd CLI check on node)    [P]
├── server.go                    # NonBlockingGRPCServer — copied from cinder-iscsi           [R]
├── utils.go                     # capability factories, New*Server, ParseEndpoint, logGRPC   [R]
├── connectioninfo.go            # RBD conn-info normalization, publish context, node ID      [P]
├── connectioninfo_test.go       #   table-driven: flat, nested, conflicting, malformed        [P]
├── rbd.go                       # RBDMapper interface + types (ImageIdentity, MappedDevice)  [P]
├── rbd_cli.go                   # rbdCLIMapper — bundled `rbd` CLI implementation            [P]
├── rbd_sysfs.go                 # /sys/bus/rbd identity reader                               [P]
├── rbd_mock.go                  # testify mock for RBDMapper                                 [P]
├── rbd_cli_test.go              # exec-faked map/unmap/list parsing                          [P]
├── credentials.go               # CephCredentialProvider, ceph.conf + keyring materialization [P]
├── credentials_test.go          # file modes, redaction, entity/auth_username mismatch        [P]
├── staging.go                   # staging record encode/decode, atomic write, dir layout     [P]
├── reconcile.go                 # startup reconciliation: records × live maps × sysfs        [P]
├── reconcile_test.go            # intent-owned reuse / mark-unstaged / isolate-conflict cases [P]
├── controllerserver_test.go     # mirrors the iSCSI suite, RBD assertions                    [P]
├── nodeserver_test.go           # stage/unstage/publish/unpublish with mocks                 [P]
└── openstack/                            # package name: openstack
    ├── openstack.go             # IOpenStackRBD, RBDOpts/VolumeOpts, provider init           [P]
    ├── openstack_volumes.go     # volume CRUD + metadata RMW — copied                        [R]
    ├── openstack_attachments.go # attachment CRUD + microversion discovery — copied          [R]
    ├── rbdconnection.go         # parseRBDConnectionInfo (replaces parseISCSIConnectionInfo)  [P]
    ├── rbdconnection_test.go    # flat/nested/conflict/missing-field fixtures                 [P]
    └── openstack_mock.go        # testify mock for IOpenStackRBD                              [P]

cmd/cinder-rbd-csi-plugin/
└── main.go                      # cobra entry point, mirrors the iSCSI flag set              [P]

charts/cinder-rbd-csi-plugin/            # mirrors charts/cinder-iscsi-csi-plugin/            [P]
├── Chart.yaml                   # name: openstack-cinder-rbd-csi
├── values.yaml
├── README.md
├── .helmignore
├── testdata/
│   ├── legacy-values-no-cacert.yaml
│   └── cacert-enabled-values.yaml
└── templates/
    ├── _helpers.tpl             # cinder-rbd-csi.{name,fullname,labels,...,cacert}
    ├── NOTES.txt
    ├── cinder-rbd-csi-driver.yaml
    ├── controllerplugin-deployment.yaml
    ├── controllerplugin-rbac.yaml
    ├── nodeplugin-daemonset.yaml
    ├── nodeplugin-rbac.yaml
    ├── driverconfig.yaml        # ConfigMap: driver.conf ([RBD], [Volume])
    ├── secret.yaml              # cloud.conf secret
    ├── cephsecret.yaml          # OPTIONAL creation of the Ceph credential Secret
    ├── storageclass.yaml
    └── storageprofile.yaml      # CDI StorageProfile (Block + RWO)

manifests/cinder-rbd-csi-plugin/                                                              [P]
├── csi-cinder-rbd-driver.yaml
├── csi-secret-cinder-rbd-plugin.yaml
├── cinder-rbd-csi-controllerplugin.yaml
├── cinder-rbd-csi-controllerplugin-rbac.yaml
├── cinder-rbd-csi-nodeplugin.yaml
├── cinder-rbd-csi-nodeplugin-rbac.yaml
└── cdi-storageprofile-patch.yaml

examples/cinder-rbd-csi-plugin/                                                               [P]
├── raw-block-pvc.yaml
├── raw-block-pod.yaml
├── cdi-datavolume.yaml
└── demo-walkthrough.md

hack/verify-cinder-rbd-chart.sh          # chart render checks, mirrors the iSCSI script      [P]
.github/workflows/cinder-rbd-csi-release.yaml   # image + Helm OCI release                    [P]
docs/cinder-csi-plugin/migration/
└── rbd-cinder-csi-operator-runbook.md   # conflicting records, isolated maps, key rotation   [P]
```

Note the deliberate omission: there is **no** `tests/sanity/cinder-rbd/` entry
above unless Phase 5 actually writes it (§2.4).

### 5.2 Files to modify in the existing tree

| File | Change | Phase |
|---|---|---|
| `Makefile` | add `cinder-rbd-csi-plugin` to `IMAGE_NAMES` (after line 45) and `BUILD_CMDS` (after line 55); add a `gox` line near line 204 | 1 |
| `Dockerfile` | add a `cinder-rbd-csi-plugin` stage with Ceph 18.2.x tooling (§10.2) | 1 |
| `go.mod` | no change — same module, no new direct dependency required for the CLI-based mapper | — |
| `OWNERS` | add reviewers/approvers for `pkg/csi/cinder-rbd/` | 1 |
| `.golangci.yaml` | no change expected; keep the `//revive:disable:unexported-return` pragma around the `New*Server` constructors as in the baseline | 1 |
| `docs/cinder-csi-plugin/migration/rbd-backed-cinder-volume-for-wrcp-migration.md` | add a `Related` link to this document | 1 |

---


## 6. Interface design

### 6.1 `IOpenStackRBD`

The interface is the iSCSI interface with the iSCSI-specific return type
replaced and the dead snapshot stubs removed. **[P]**

```go
// pkg/csi/cinder-rbd/openstack/openstack.go

type IOpenStackRBD interface {
    // ── Volume operations (Cinder) ────────────────────────────────────────
    CreateVolume(ctx context.Context, opts *volumes.CreateOpts,
        schedulerHints volumes.SchedulerHintOptsBuilder) (*volumes.Volume, error)
    DeleteVolume(ctx context.Context, volumeID string) error
    GetVolume(ctx context.Context, volumeID string) (*volumes.Volume, error)
    GetVolumesByName(ctx context.Context, name string) ([]volumes.Volume, error)
    WaitVolumeTargetStatus(ctx context.Context, volumeID string,
        tStatus []string, timeoutSeconds int) error
    SetVolumeMetadata(ctx context.Context, volumeID string, metadata map[string]string) error
    DeleteVolumeMetadata(ctx context.Context, volumeID string, keys []string) error

    // ── Cinder v3 self-service attachment records (no Nova) ───────────────
    CreateAttachment(ctx context.Context, volumeID string) (string, error)
    UpdateAttachmentConnector(ctx context.Context, attachmentID string,
        connector *AttachmentConnector) (*RBDConnectionInfo, error)
    CompleteAttachment(ctx context.Context, attachmentID string) error
    GetAttachment(ctx context.Context, attachmentID string) (*Attachment, error)
    ListAttachmentsByVolume(ctx context.Context, volumeID string) ([]Attachment, error)
    DeleteAttachment(ctx context.Context, attachmentID string) error

    // ── Discovery & configuration ─────────────────────────────────────────
    DiscoverCinderCapabilities(ctx context.Context) (*CinderCapabilities, error)
    GetRBDOpts() RBDOpts
    GetVolumeOpts() VolumeOpts
    GetCinderCapabilities() *CinderCapabilities
}
```

Differences from `IOpenStackISCSI`, each with a reason:

| Change | Reason |
|---|---|
| `ExpandVolume` removed | expansion is a non-goal (P§4, P§12); do not advertise `EXPAND_VOLUME` |
| `CreateSnapshot`/`DeleteSnapshot`/`GetSnapshotByID` removed | the baseline versions are `not implemented` stubs that exist only to satisfy the interface; snapshots are a non-goal |
| `ListAttachmentsByVolume` **added** | detects an attachment record that cannot be attributed safely when metadata is missing. Because a reserved record created with only `volume_uuid` carries no driver marker, listing is a fail-closed conflict check, never an automatic-adoption mechanism. |
| `UpdateAttachmentConnector` returns `*RBDConnectionInfo` | different backend schema |

`ListAttachmentsByVolume` is implemented with
`attachments.List(client, attachments.ListOpts{VolumeID: volumeID})` against a
3.27-pinned client. The result is not described as "driver-owned": Cinder
provides no ownership marker for the connector-less record shape used here.
See §9.2 for the fail-closed recovery rule.

### 6.2 RBD connection information and normalization

The validated Cinder response is **flat**, with **16 fields** **[V]**
(re-measured in Phase 0 task P0-1 on 2026-09-01; no nested `data` object was
returned):

```json
{
  "driver_volume_type": "rbd",
  "cluster_name": "ceph",
  "name": "cinder-volumes/3018df26-0ba3-45a3-adfd-4a84ed59fff1",
  "auth_enabled": true,
  "auth_username": "cinder",
  "secret_type": "ceph",
  "secret_uuid": "c5f7876d-258c-4152-b26a-a3ab532fda28",
  "volume_id": "3018df26-0ba3-45a3-adfd-4a84ed59fff1",
  "attachment_id": "8d6cd8d7-1e6f-4bfd-8753-42332b4bc42d",
  "hosts": ["10.107.190.121", "10.106.210.60", "10.98.180.79"],
  "ports": ["6789", "6789", "6789"],
  "access_mode": "rw",
  "discard": true,
  "encrypted": false,
  "cacheable": false,
  "qos_specs": null
}
```

Two types: a wire type that mirrors Cinder exactly, and a normalized type the
rest of the driver consumes. Keeping them separate is what prevents the
"invented prefix" and "nested overrides top-level" classes of bug. **[P]**

```go
// pkg/csi/cinder-rbd/openstack/rbdconnection.go

const DriverVolumeTypeRBD = "rbd"

// rbdConnectionWire mirrors the Cinder attachment connection_info payload.
// Field set confirmed against the live backend in Phase 0 (P0-1).
type rbdConnectionWire struct {
    DriverVolumeType string   `json:"driver_volume_type"`
    ClusterName      string   `json:"cluster_name"`
    Name             string   `json:"name"`        // "<pool>/<image>" — authoritative
    AuthEnabled      bool     `json:"auth_enabled"`
    AuthUsername     string   `json:"auth_username"`
    SecretType       string   `json:"secret_type"`
    SecretUUID       string   `json:"secret_uuid"` // Ceph FSID — an identifier, NOT a key
    VolumeID         string   `json:"volume_id"`
    AttachmentID     string   `json:"attachment_id"`
    Hosts            []string `json:"hosts"`
    Ports            []string `json:"ports"`
    AccessMode       string   `json:"access_mode"`
    Discard          bool     `json:"discard"`
    Encrypted        bool     `json:"encrypted"`
    Cacheable        bool     `json:"cacheable"`  // present in the live response
    // qos_specs is intentionally NOT decoded: it is null on this backend and
    // its schema is backend-defined. Decoding it would invite a type panic.
}

// RBDConnectionInfo is the normalized, validated form used by the driver.
type RBDConnectionInfo struct {
    DriverVolumeType string      // always "rbd"
    ClusterName      string      // e.g. "ceph"
    ClusterFSID      string      // from secret_uuid
    Pool             string      // first path segment of Name
    Image            string      // remainder of Name, verbatim
    AuthEnabled      bool
    AuthUsername     string      // e.g. "cinder"; the required keyring entity
    Monitors         []MonAddr   // hosts[n] paired with ports[n]
    VolumeID         string
    AccessMode       string
}

type MonAddr struct { Host string; Port string }

func (m MonAddr) String() string // net.JoinHostPort — IPv6-safe
```

`parseRBDConnectionInfo(raw map[string]any) (*RBDConnectionInfo, error)`
algorithm, in order: **[P]**

1. Build the effective field map: start from the top-level map; if a nested
   `data` object is present, use it **only to fill fields absent at the top
   level**. If a field is present in both and the values differ, **reject** the
   response — do not prefer either. (Skill rule: nested must never override
   validated top-level fields.)
2. Read `driver_volume_type` from the effective map. Reject unless it equals
   `"rbd"`. This ordering is what permits a nested-only compatibility payload
   while keeping top-level values authoritative.
3. `name` is mandatory. Split on the **first** `/` only:
   `pool, image, found := strings.Cut(name, "/")`. Reject when `!found`, or when
   either side is empty. **Never** prepend `volume-`, never assume the pool.
   An image name containing further `/` characters keeps them.
4. `hosts` and `ports` are mandatory, non-empty, and equal length. Pair
   positionally into `Monitors`. Reject on length mismatch — silently truncating
   would produce a partially reachable monitor list.
5. `secret_uuid` → `ClusterFSID`. It is an identifier. It is never used as a
   key, never logged as sensitive, and never used to look up a key.
6. Require `auth_enabled == true` and a non-empty `auth_username`. The initial
   implementation supports only the validated CephX model; accepting an
   unauthenticated payload would contradict the mandatory credential path.
7. `cluster_name` is optional in the payload; when present it must match
   `[RBD] expected-cluster-name` if that is configured.
8. `secret_type` is recorded for diagnostics only. It must not cause a lookup.
9. Reject any response carrying a `keyring`, `key`, `secret`, or `password`
   field. Such a field indicates a backend regression of the class fixed by
   CVE-2020-10755; the driver fails loudly rather than consuming it, and logs
   only the *field name*.

Validation of the normalized struct is separate, so the parser stays
side-effect-free and the validator can also run on a struct rebuilt from
`publish_context` on the node:

```go
// pkg/csi/cinder-rbd/connectioninfo.go
func ValidateRBDConnectionInfo(ci *openstack.RBDConnectionInfo, opts openstack.RBDOpts) error
```

It enforces: `DriverVolumeType == "rbd"`; `AuthEnabled == true`; non-empty
`ClusterFSID`, `Pool`, `Image`, and `AuthUsername`; at least one monitor with
both host and port; and, when configured,
`ClusterFSID == opts.ExpectedFSID` and
`ClusterName == opts.ExpectedClusterName`.

### 6.3 The Cinder connector — CLOSED

**Status: [V] — measured on the WRCP 24.09 lab, 2026-09-01 (Phase 0 task P0-1).**

The deployed Cinder RBD backend requires exactly **one** connector property:
`host`. Measured behavior:

| Connector sent | Result |
|---|---|
| `{}` | **HTTP 400** — `Invalid input for field/attribute connector. Value: {}. {} does not have enough properties` |
| `{"host":"controller-0"}` | **Accepted.** Returned complete flat RBD `connection_info` (all 16 fields), volume → `attaching` |

No `initiator`, `ip`, `platform`, `os_type`, `multipath`, or `do_local_attach` is
required. The iSCSI connector fields have no meaning for RBD and are not sent.

```go
// AttachmentConnector — frozen by lab measurement, not by analogy with iSCSI.
// Cinder rejects an empty connector, so Host is mandatory and is sufficient.
type AttachmentConnector struct {
    Host string `json:"host"` // worker hostname from NodeGetInfo  [V]
}
```

Implementation rule: build the JSON body from a `map[string]any` containing only
`host`, exactly as the baseline hand-builds its connector map. Do **not** add
speculative fields — each one is an untested compatibility risk against a
backend that has been shown to need none of them. If a future backend requires
more, extend `AttachmentConnector` and re-measure; do not pre-populate.

### 6.4 Node ID contract — CLOSED

**Status: [V] — follows from P0-1; the connector needs only `host`.**

`NodeId = <hostname>`, from `os.Hostname()`. There is no initiator identity to
carry and the backend does not require `ip`, so the iSCSI `hostname;iqn;ip`
format is not used.

```go
const nodeIDSeparator = ";"

// ParseNodeID accepts "<host>" and, for forward compatibility, "<host>;<ip>".
// ip is "" in the 1-field form, which is the form this driver emits.
func ParseNodeID(nodeID string) (host, ip string, err error)
func BuildNodeID(host, ip string) string // ip == "" ⇒ "<host>"
```

`ParseNodeID` still tolerates the 2-field form from day one. That is deliberate:
if a future backend requires `ip`, nodes already registered with the 1-field ID
keep working through the upgrade. `NodeGetInfo` emits the 1-field form.

- `AccessibleTopology` is nil — topology-aware provisioning is a non-goal (P§4).
- `MaxVolumesPerNode` comes from `[RBD] max-volumes-per-node`, default `0`
  (unlimited) until Phase 6 measures the krbd mapping limit (P§12). **[Q6]**

### 6.5 `publish_context` key set

`publish_context` crosses from controller to node through the Kubernetes
`VolumeAttachment` object, which is **world-readable to anyone with RBAC on the
resource**. It therefore carries identifiers only. **[P]**

| Key | Source | Example |
|---|---|---|
| `driver_volume_type` | `DriverVolumeType` | `rbd` |
| `cluster_name` | `ClusterName` | `ceph` |
| `cluster_fsid` | `ClusterFSID` (from `secret_uuid`) | `c5f7876d-…` |
| `pool` | `Pool` | `cinder-volumes` |
| `image` | `Image` | `8ee6132d-c940-47b0-9df5-a9b7ecba2d2f` |
| `monitors` | `Monitors`, comma-separated `host:port` | `10.107.190.121:6789,10.106.210.60:6789,…` |
| `auth_enabled` | `AuthEnabled` | `true` |
| `auth_username` | `AuthUsername` | `cinder` |
| `volume_id` | `VolumeID` | Cinder volume UUID |
| `access_mode` | `AccessMode` when present | `rw` |
| `attachment_id` | current Cinder attachment ID | for correlation in node logs only |

Forbidden in `publish_context`, permanently: any Ceph key, keyring content,
`secret_type`-driven credential material, or the contents of the credential
Secret. `attachment_id` is included for log correlation and **must not** be
treated by the node as authoritative for anything.

`BuildPublishContext(ci *openstack.RBDConnectionInfo, attachmentID string) map[string]string`
and its inverse `ParsePublishContext(map[string]string) (*openstack.RBDConnectionInfo, string, error)`
live in `connectioninfo.go`; the inverse re-runs `ValidateRBDConnectionInfo` so
the node never trusts a partially populated context.

### 6.6 `RBDMapper` — the node data-path abstraction

This is the layer with no baseline analogue. It is an interface so the node
server is unit-testable without a kernel. **[P]**

```go
// pkg/csi/cinder-rbd/rbd.go

// ImageIdentity is the tuple that must match before any map is reused,
// published, or unmapped.
type ImageIdentity struct {
    ClusterFSID string // authoritative
    ClusterName string // convenience/diagnostic
    Pool        string
    Image       string
}

type MapRequest struct {
    Identity    ImageIdentity
    Monitors    []openstack.MonAddr
    UserID      string        // == connection_info.auth_username, e.g. "cinder"
    ConfPath    string        // generated ceph.conf
    KeyringPath string        // generated keyring, mode 0400
    Exclusive   bool          // ALWAYS true for writable maps
    ReadOnly    bool
    Timeout     time.Duration
}

type MappedDevice struct {
    ID          int    // N in /dev/rbdN
    Pool        string
    Namespace   string
    Image       string
    Snap        string
    DevicePath  string // "/dev/rbdN"
    ClusterFSID string // from sysfs cluster_fsid — confirmed present  [V]
}

type RBDMapper interface {
    // Map performs an exclusive kernel map and returns the created device.
    // It must NOT retry with --exclusive removed under any circumstances.
    Map(ctx context.Context, req MapRequest) (MappedDevice, error)

    // Unmap releases a device. Callers must have verified identity first.
    Unmap(ctx context.Context, devicePath string, timeout time.Duration) error

    // ListMapped returns all kernel RBD mappings on this host.
    ListMapped(ctx context.Context) ([]MappedDevice, error)

    // VerifyIdentity confirms that devicePath currently maps want.
    // It reads /sys/bus/rbd and cross-checks ListMapped.
    VerifyIdentity(ctx context.Context, devicePath string, want ImageIdentity) error

    // DeviceSize returns the block device size in bytes.
    DeviceSize(ctx context.Context, devicePath string) (int64, error)

    // Flush issues a buffer flush before unmap.
    Flush(ctx context.Context, devicePath string) error

    // CheckClient reports the bundled rbd CLI version; used by the node Probe.
    CheckClient(ctx context.Context) (string, error)
}
```

`rbd_cli.go` implements it over the bundled CLI using `k8s.io/utils/exec` (so
tests can substitute `testingexec`, as the baseline does for `iscsiadm`):

```bash
# Map — the exact validated form (P§7, lab-verified)
rbd device map \
  --device-type krbd \
  --exclusive \
  --cluster <cluster_name> \
  --id <auth_username> \
  --conf    /run/cinder-rbd-csi/<volume-id>/ceph.conf \
  --keyring /run/cinder-rbd-csi/<volume-id>/keyring \
  <pool>/<image>

# Inventory — machine-readable, the basis of all reconciliation
rbd device list --format json

# Unmap
rbd device unmap /dev/rbdN
```

Rules encoded in the implementation, not left to callers:

1. **No non-exclusive fallback.** `Map` returns an error when `--exclusive`
   fails. There is no code path that removes the flag. A unit test asserts the
   flag is present in every generated argv, and a second test asserts no retry
   occurs on lock failure.
2. **`/dev/rbdN` is not an identity.** `Map`'s returned path is recorded but
   every later use goes through `VerifyIdentity`. Device numbers are recycled
   after restarts.
3. **`ListMapped` is the inventory of record**, parsed from
   `rbd device list --format json` (fields `id`, `pool`, `namespace`, `name`,
   `snap`, `device`). Absence of a staging record is never proof of absence of a
   mapping.
4. **sysfs cross-check.** `VerifyIdentity` reads `/sys/bus/rbd/devices/<N>/`
   and compares against `want`. **[V] Measured on WRCP 24.09 (Linux 6.6,
   P0-3): `cluster_fsid` IS exposed**, so the FSID check is direct — no
   transitive `ceph.conf` argument is needed. Verified attribute set:

   ```text
   block  client_addr  client_id  cluster_fsid  config_info  current_snap
   features  image_id  major  minor  name  parent  pool  pool_id  pool_ns
   power  refresh  size  snap_id  subsystem  uevent
   ```

   Identity is read from `cluster_fsid`, `pool`, `name`; `pool_id`, `image_id`,
   `size`, and `features` are recorded for diagnostics. Note there is **no**
   `snap` attribute — it is `current_snap` (and `snap_id`); a namespace is
   `pool_ns`. Sample from a live Cinder-created mapping:

   ```text
   cluster_fsid = c5f7876d-258c-4152-b26a-a3ab532fda28
   pool         = cinder-volumes      pool_id  = 7
   name         = 3018df26-0ba3-45a3-adfd-4a84ed59fff1
   image_id     = 3337ebc9501c13      size     = 1073741824
   features     = 0x000000000000003d  # layering|exclusive-lock|object-map|fast-diff|deep-flatten
   ```

5. **Always pass `--conf` to every `rbd` invocation.** **[V] P0-6 hazard:** the
   WRCP host ships `/etc/ceph/ceph.conf` as an unsubstituted StarlingX template
   containing `fsid = %CLUSTER_UUID%`. Any `rbd` command that falls back to it
   — including `rbd device list` and `rbd device unmap`, which need no cluster
   credentials — emits `parse error setting 'fsid' to '%CLUSTER_UUID%'` on
   stderr. Consequences for the implementation:
   - `Map`, `Unmap`, and `ListMapped` all pass `--conf <generated ceph.conf>`.
     `ListMapped` and `Unmap` therefore require a generated conf to exist, so
     the reconciler writes a minimal cluster-only conf at startup rather than
     relying on per-volume confs.
   - **stderr content is not a failure signal.** Decide success from exit status
     and from parsed stdout only. A command that printed a parse error and
     exited 0 succeeded. Log stderr at a low verbosity level.

6. **Exclusive-lock observability.** Before publishing a writable device,
   `rbd status <pool>/<image>` confirms no conflicting writable client holds the
   lock. **[V] P0-4 measured output shape** while mapped:

   ```text
   Watchers:
       watcher=192.168.206.2:0/76629272 client.56193456 cookie=18446462598732840961
   There is 1 exclusive lock on this image.
   Locker           ID                         Address
   client.56193456  auto 18446462598732840961  192.168.206.2:0/76629272
   ```

   After unmap it reports `Watchers: none`. A conflicting holder fails
   `NodeStageVolume` with `FailedPrecondition` and the error names the locker.

7. **Bounded operations.** `Map` and `Unmap` honor
   `[RBD] map-timeout` / `unmap-timeout` (default 120 s each, P§11) via
   `exec.CommandContext`. An unmap timeout leaves staging state intact and
   returns a retryable error.

### 6.7 `CephCredentialProvider` and runtime credential files

The credential model is fixed by the proposal: the operator duplicates the
existing platform `client.cinder` key into a namespace-scoped Kubernetes Secret;
the plugin never derives, copies, or discovers a key at runtime **[V]** (P§6,
lab-verified that `openstack/cinder-volume-rbd-keyring` is byte-identical to
`ceph auth get client.cinder`).

**How the node plugin obtains the Secret — three options, one recommendation:**

| Option | Mechanism | Assessment |
|---|---|---|
| A | StorageClass `csi.storage.k8s.io/node-stage-secret-name` → key arrives in `NodeStageVolumeRequest.Secrets` | **Rejected.** Puts the key into a CSI RPC payload; the baseline's `logGRPC` uses `protosanitizer`, which strips known secret fields, but any future non-sanitized log or trace re-exposes it. Also couples credential rotation to StorageClass edits. |
| B | Node plugin reads the Secret through the Kubernetes API | Deferred. Needs a client-go dependency, a `get` RBAC rule on one named Secret, and a cache/refresh policy. |
| C | **Secret projected as a read-only volume in the node DaemonSet**, plugin reads files at stage time | **Recommended.** No API client, no RBAC on Secrets, no key in any RPC. kubelet refreshes projected Secret contents in place, so re-reading on every `NodeStageVolume` picks up a rotated key **without a pod restart** — which is exactly the operator procedure in P§6 step 6. |

Chosen: **option C** for Phase 3, with option B recorded as a future
enhancement if a namespace other than the plugin's own must be read
(decision log D-2).

```go
// pkg/csi/cinder-rbd/credentials.go

type CephCredential struct {
    UserID string // entity WITHOUT the "client." prefix, e.g. "cinder"
    key    string // never exported, never logged, never String()-ed
}

type CephCredentialProvider interface {
    // Load reads the credential and fails if its entity does not match
    // wantUserID (== connection_info.auth_username).
    Load(ctx context.Context, wantUserID string) (*CephCredential, error)
}

// fileCredentialProvider reads the projected Secret mounted at credentialPath.
type fileCredentialProvider struct{ dir string } // e.g. /etc/cinder-rbd-csi/ceph
```

Expected Secret keys, matching the operator procedure in P§6 step 3:
`userID` (default `cinder`) and `userKey`. Mounted as
`/etc/cinder-rbd-csi/ceph/userID` and `/etc/cinder-rbd-csi/ceph/userKey`,
`defaultMode: 0400`, `readOnly: true`.

**Entity match is mandatory and pre-flight.** If
`credential.UserID != connInfo.AuthUsername`, `NodeStageVolume` fails
immediately with `FailedPrecondition` and a message naming both values —
because krbd requires `--id` and the keyring entity to be the *same* identity
(P§6, "Why not a dedicated least-privilege identity initially"). This must fail
before any map attempt, so the failure is a clear configuration error rather
than an opaque Ceph auth error.

**Runtime materialization.** Per volume, under a private plugin-owned tmpfs-
or emptyDir-backed directory:

```text
/run/cinder-rbd-csi/<volume-id>/
├── ceph.conf   0600   [global] mon_host = <mon1:port>,<mon2:port>,…
│                      fsid    = <cluster_fsid>
│                      auth_cluster_required = cephx  (when auth_enabled)
│                      auth_service_required = cephx
│                      auth_client_required  = cephx
└── keyring     0400   [client.<userID>]
                           key = <redacted>
```

Rules: directory `0700`, owned by the plugin; files written via
create-temp-then-`rename` so a partial file is never readable; removed after a
successful unmap and after a failed map attempt; **never** written under a
kubelet-visible staging path, `/etc/ceph`, or any host path shared with other
workloads. The generated `ceph.conf` carries the FSID that was validated
against `expected-fsid`, which is what makes the transitive FSID argument in
§6.6 rule 4 sound.

The design explicitly does **not** require host `/etc/ceph` credentials or the
host `rbd` binary (P§11).

### 6.8 Staging record and map-intent format

The node-scoped record is the driver's ownership evidence for a kernel map; the
kernel and Ceph remain authoritative for the map's current identity (P§10).
The record is written **before** the map side effect so a process crash can
never produce a driver-created but unowned mapping. **[P]**

```json
{
  "schema":        1,
  "phase":         "staged",
  "volume_id":     "8ee6132d-c940-47b0-9df5-a9b7ecba2d2f",
  "attachment_id": "5c1e…",
  "cluster_name":  "ceph",
  "cluster_fsid":  "c5f7876d-258c-4152-b26a-a3ab532fda28",
  "pool":          "cinder-volumes",
  "image":         "8ee6132d-c940-47b0-9df5-a9b7ecba2d2f",
  "monitors":      ["10.107.190.121:6789", "10.106.210.60:6789", "10.98.180.79:6789"],
  "auth_username": "cinder",
  "device_path":   "/dev/rbd5",
  "device_id":     5,
  "exclusive":     true,
  "map_generation": 3,
  "size_bytes":    21474836480,
  "staged_at":     "2026-09-01T10:14:22Z",
  "driver":        "cinder-rbd.csi.windriver.com"
}
```

- Before `rbd device map`, write
  `/var/lib/cinder-rbd-csi/staged/<volume-id>.json` with phase
  `map-pending`, the expected identity, monitors, attachment ID, and no device
  path. This durable intent is the proof that a later matching map was created
  by this driver.
- After map and identity verification succeed, update the node-scoped record to
  phase `staged` with the device fields, then write the same finalized record to
  `<stagingTargetPath>/rbd-staging.json`.
- Every write uses create-temp → `fsync` → `rename` → `fsync(dir)`, mode
  `0600`.
- `map_generation` increments on every successful map of the same volume on this
  node; it makes "the map I recorded" distinguishable from "a later map of the
  same image" in logs and metrics.
- `schema` and `phase` are checked on read. An unknown value is isolated and
  reported; it is never permission to adopt or unmap a live device.
- The record contains **no credential material** — only `auth_username`.
- `device_path` is a hint. Any consumer calls `VerifyIdentity` before acting.

The staging-path copy is a kubelet-lifecycle hint. The node-scoped record is
the ownership index used for startup enumeration. A disagreement is resolved
by combining the node-scoped intent with live kernel/sysfs identity; kernel
identity alone proves what a map is, but not that this driver owns it.

### 6.9 Configuration structs and proposal-to-config mapping

The proposal's §11 configuration sketch mixes CLI flags, driver constants, and
per-backend options. This section reconciles it with the baseline's actual
mechanism: `cloud.conf` (a Secret, `[Global]` auth) plus `driver.conf` (a
ConfigMap, backend and volume options), both passed via repeatable
`--cloud-config`, parsed with `gcfg.FatalOnly(gcfg.ReadInto(...))` so a node-mode
process tolerating only `driver.conf` still starts. **[R]**

| Proposal §11 line | Implementation home | Note |
|---|---|---|
| `[Global] cloud-config=` | `--cloud-config` CLI flag (repeatable) | not a config-file key |
| `[Global] driver-name=` | Go constant `driverName` | **deviation**: not configurable, matching the baseline; a mutable driver name breaks CSIDriver/CSINode registration |
| `[Global] cinder-min-microversion=3.27` | Go constant `MvSelfServiceAttach` | **deviation**: a hard requirement, not a tunable; `DiscoverCinderCapabilities` fails startup if unsupported |
| `[Global] retain-volume=true` | `[Volume] delete-volume-mode=retain` | **deviation**: only `retain` is accepted initially; `delete` and the per-volume `cleanupVolume` override fail closed until Q8 |
| `[RBD] mounter=krbd` | `RBDOpts.Mounter` | only `krbd` is accepted in this release; any other value fails startup |
| `[RBD] exclusive=true` | `RBDOpts.Exclusive` | accepted for explicitness; setting `false` is **rejected at startup**, because a writable non-exclusive map is forbidden by P§7 |
| `[RBD] expected-cluster-name=ceph` | `RBDOpts.ExpectedClusterName` | |
| `[RBD] expected-fsid=<fsid>` | `RBDOpts.ExpectedFSID` | environment-specific identifier, not a credential |
| `[RBD] ceph-client-version-major=18` | `RBDOpts.CephClientVersionMajor` | checked at startup against `rbd --version` |
| `[RBD] credential-secret-name/-namespace` | chart values that render the projected Secret volume | **deviation**: the plugin consumes `[RBD] credential-path`; see §6.7 option C |
| `[RBD] map-timeout/unmap-timeout` | `RBDOpts.MapTimeout` / `UnmapTimeout` | |

```go
// pkg/csi/cinder-rbd/openstack/openstack.go

type RBDCinderConfig struct {
    Global map[string]*client.AuthOpts // cloud.conf
    RBD    RBDOpts                     // driver.conf [RBD]
    Volume VolumeOpts                  // driver.conf [Volume]
}

type RBDOpts struct {
    Mounter                string `gcfg:"mounter"`                   // default "krbd"; only "krbd" accepted
    Exclusive              bool   `gcfg:"exclusive"`                 // default true; false rejected
    ExpectedClusterName    string `gcfg:"expected-cluster-name"`     // default "ceph"
    ExpectedFSID           string `gcfg:"expected-fsid"`             // required in production
    CephClientVersionMajor int    `gcfg:"ceph-client-version-major"` // default 18
    CredentialPath         string `gcfg:"credential-path"`           // default /etc/cinder-rbd-csi/ceph
    RuntimeDir             string `gcfg:"runtime-dir"`               // default /run/cinder-rbd-csi
    StateDir               string `gcfg:"state-dir"`                 // default /var/lib/cinder-rbd-csi
    MapTimeout             string `gcfg:"map-timeout"`               // default "120s"
    UnmapTimeout           string `gcfg:"unmap-timeout"`             // default "120s"
    DeviceWaitTimeout      string `gcfg:"device-wait-timeout"`       // default "60s"
    MaxVolumesPerNode      int64  `gcfg:"max-volumes-per-node"`      // default 0 (unlimited)  [Q6]
}

type VolumeOpts struct {
    CreateTimeout     int    `gcfg:"create-timeout"`      // default 300 s
    DetachTimeout     int    `gcfg:"detach-timeout"`      // default 120 s — MUST be read (unlike baseline)
    DefaultVolumeType string `gcfg:"default-volume-type"` // e.g. ceph-rook-store; never hard-coded
    MetadataPrefix    string `gcfg:"metadata-prefix"`     // default "csi.rbd"
    DeleteVolumeMode  string `gcfg:"delete-volume-mode"`  // only "retain" until Q8 closes
}
```

Every field above has a reader in the driver; `applyDefaults()` on each struct is
the single place defaults live, and it is unit-tested (§2.4).

**Metadata keys.** With `metadata-prefix` defaulting to `csi.rbd`:

| Key | Purpose |
|---|---|
| `csi.rbd.attachment_id` | current Cinder attachment ID (P§9) |
| `csi.rbd.cleanupVolume` | future delete request; rejected until Q8 closes |

The proposal mentions `csi.cleanupVolume` in P§8 and `csi.rbd.attachment_id` in
P§9. This document resolves the inconsistency in favor of the prefixed form for
both keys, so a single `metadata-prefix` governs all driver-owned metadata and
the two sibling drivers cannot collide on one volume. Recorded as decision D-3.

### 6.10 Cinder attachment lifecycle state machine

Identical in shape to the baseline; the RBD-specific addition is the Ceph lock
on the node side. **[P]**

```text
                    CreateVolume
   NEW ─────────────────────────────────► VOLUME_AVAILABLE
                                              │
                     CreateAttachment (volume_uuid only, no instance_uuid)
                     volume: available → reserved                        [V]
                                              ▼
                                          RESERVED
                                              │
                     UpdateAttachmentConnector({"host": <node>}) → connection_info
                     volume: reserved → attaching                        [V]
                     attachment: reserved → attaching                    [V]
                                              ▼
                                         ATTACHING
                                              │
                     CompleteAttachment (mv 3.44, HTTP 204) — OPTIONAL
                     volume → in-use, attachment → attached              [V]
                                              ▼
                     NodeStage: exclusive krbd map (Ceph lock held)
                                              ▼
                                     POD_USING_VOLUME
                                              │
                     NodeUnpublish → NodeUnstage (unmap, lock released)
                     ControllerUnpublish → DeleteAttachment + clear metadata
                     volume: in-use → available                          [V]
                                              ▼
                                      VOLUME_AVAILABLE
                          ┌───────────────────┴───────────────────┐
             next migration pod                            DeleteVolume
       (ControllerPublish creates a NEW record,                   │
        with a DIFFERENT attachment ID)  [V]     ┌────────────────┴───────────┐
                          │                      ▼                            ▼
                          ▼              RETAINED_VOLUME (default)     DELETED_VOLUME
                      RESERVED           metadata stripped,            explicit cleanup
                                         volume left available         only
```

Every transition marked **[V]** was observed in Phase 0 (P0-7, P0-8).

`attaching` is a real intermediate state: after the connector update the volume
is **no longer** `reserved` and **not yet** `in-use`. Code that waits for
`in-use` would hang whenever microversion 3.44 is unavailable and
`CompleteAttachment` is skipped — so no publish path waits on `in-use`.

Two invariants the code must make impossible to violate:

- **No Cinder attachment record and no Ceph lock are held between migration
  pods.** The `available` window is intentional (P§13).
- **Normal teardown releases the node before the controller.**
  `NodeUnstageVolume` ordinarily unmaps before `ControllerUnpublishVolume`
  deletes the record. This is an orchestration expectation, not a universal
  guarantee: forced detach and an unreachable node can bypass node cleanup.
  The controller therefore never treats Cinder `available` as proof that no
  kernel map exists.

---


## 7. Prerequisites and runtime validation

### 7.1 Environment prerequisites

| Prerequisite | Where enforced | Failure mode |
|---|---|---|
| Cinder API supports microversion 3.27 | controller startup, `DiscoverCinderCapabilities` | process exits with a message naming `3.27` **[R]** |
| Cinder volume type exists (e.g. `ceph-rook-store`) | `CreateVolume` via StorageClass `type` parameter | `InvalidArgument` from Cinder, surfaced verbatim; never hard-coded **[V]** |
| Bundled `rbd` CLI present and major version matches | node startup + `Probe` | node reports not-ready |
| `rbd` kernel module loadable, `/sys/bus/rbd` present | node startup | node reports not-ready |
| Ceph credential Secret projected and readable | node startup (existence) + `NodeStageVolume` (entity match) | not-ready / `FailedPrecondition` |
| Monitors reachable from the node | `NodeStageVolume` (map attempt) | `Unavailable`, retryable |
| Kubernetes ≥ 1.29 with `volumeMode: Block` PVCs | deployment | n/a |
| CDI StorageProfile declares Block + RWO for the StorageClass | operator/chart | CDI creates Filesystem PVCs → driver rejects → DataVolume hangs Pending **[R]** |
| Node plugin privileged (or equivalent caps) with `/dev`, `/sys` access | DaemonSet spec | map fails |

Explicitly **not** prerequisites (P§11): host PID namespace, host
`/usr/bin/rbd*`, host `/etc/ceph`, Nova, a shadow VM, `open-iscsi`.

The baseline node DaemonSet sets `hostPID: true`; the RBD node plugin does not
need it and must not request it. If a Phase 3 measurement shows otherwise, that
is a decision requiring explicit sign-off, not a quiet template edit.

### 7.2 Startup validation vs. per-RPC validation

Split by cost and volatility, following the baseline's pattern of caching Cinder
capabilities once at provider creation. **[P]**

**Controller startup (fail fast, exit non-zero):**
1. Parse `cloud.conf` + `driver.conf`; reject `mounter != "krbd"` and
   `exclusive == false`.
2. Build the Cinder v3 client only — no Nova client.
3. `DiscoverCinderCapabilities`: 3.27 mandatory, 3.44 advisory. Cache.
4. Register metrics under `cinder-rbd-csi`.

**Node startup (mark not-ready, keep running so kubelet retries):**
1. `rbd --version` → parse major version, compare with
   `ceph-client-version-major`.
2. `/sys/bus/rbd` exists (or `modprobe rbd` succeeds).
3. Credential directory exists and `userID`/`userKey` are readable.
4. `runtime-dir` and `state-dir` are writable, mode `0700`.
5. Run startup reconciliation (§9.1) **before** serving the first RPC.

**Per-RPC (cheap, must be re-checked every time):**
- Block-mode and access-mode checks on every capability-bearing RPC.
- `publish_context` completeness and `ValidateRBDConnectionInfo`.
- Credential entity vs. `auth_username`.
- Expected FSID / cluster name.
- Live map identity before reuse, publish, or unmap.

Rationale for keeping the FSID check per-RPC rather than startup-only: a
monitor-set change or a re-pointed Cinder backend must be caught at the moment a
device would be mapped, not at a startup that happened days earlier.

### 7.3 Microversion detection

Ported unchanged from the baseline, with the constants trimmed to what the RBD
driver actually uses. **[R]**

```go
const (
    MvSelfServiceAttach = "3.27" // mandatory — self-service attachment records
    MvAttachComplete    = "3.44" // optional  — POST os-complete
)
```

`DiscoverCinderCapabilities` probes with
`attachments.List(client, attachments.ListOpts{Limit: 1})` against a 3.27-pinned
client (failure ⇒ startup error naming `3.27`), then repeats against a
3.44-pinned client and records `SupportsV344` without failing. Result cached on
the provider. `CompleteAttachment` short-circuits when `SupportsV344` is false
and its errors are logged, never returned — completion is optional because
self-service attachment records and connection discovery need only 3.27 (P§8).

`MvServerSideNameFilter` (3.34) and `MvOnlineResize` (3.42) from the baseline are
dropped: name filtering is not used, and expansion is a non-goal.
`CinderCapabilities.MaxMicroversion` is either populated from the API root
document or removed — the baseline's unassigned field is not carried over (§2.4).

### 7.4 Identity gate — the five checks before a writable map

Consolidated here because it is the single most important correctness rule in
the driver, and every reviewer should be able to find it in one place. Before
`NodeStageVolume` exposes a writable device (P§7), all five must pass: **[P]**

| # | Check | Source of truth | Failure code |
|---|---|---|---|
| 1 | mapped cluster FSID matches configured/expected FSID | sysfs `cluster_fsid` — **[V]** confirmed present on Linux 6.6 (§6.6 rule 4) | `FailedPrecondition` |
| 2 | mapped pool and image match Cinder's `name` exactly | `rbd device list --format json` + sysfs `pool`/`name` | `FailedPrecondition` |
| 3 | mapping is exclusive and no conflicting writable client holds the lock | `rbd status` — output shape **[V]** in §6.6 rule 6 | `FailedPrecondition` |
| 4 | block device exists with the expected size | `stat` + `BLKGETSIZE64` / `DeviceSize` | `Internal` (retryable) |
| 5 | credential entity equals `connection_info.auth_username` | credential provider + publish context | `FailedPrecondition` |

If any check fails, `NodeStageVolume` fails. There is no partial success, no
read-only downgrade, and no non-exclusive fallback.

---

## 8. CSI RPC implementation map

### 8.1 Identity service

| RPC | Behavior |
|---|---|
| `GetPluginInfo` | returns `cinder-rbd.csi.windriver.com` and `fqVersion` (`{Version}@{CPO version}`); `Unavailable` if either is empty **[R]** |
| `GetPluginCapabilities` | `CONTROLLER_SERVICE` only. No `VOLUME_ACCESSIBILITY_CONSTRAINTS` (no topology), no `VolumeExpansion` |
| `Probe` | mode-aware, see below |

`Probe` **[P]**, mirroring the baseline's structure including its wiring-bug
guard:

```text
if cloud == nil:                       # node-only mode
    if mapper == nil:                  # wiring bug guard
        return Ready=false, log a warning
    if mapper.CheckClient(ctx) fails:  # rbd CLI missing/wrong major version
        return Ready=false
    if credentials unreadable:         # projected Secret not mounted yet
        return Ready=false
    return Ready=true
else:                                  # controller (or all-in-one) mode
    caps := cloud.GetCinderCapabilities()
    if caps == nil or !caps.SupportsV327:
        return Ready=false, log a warning
    return Ready=true
```

`Probe` always returns a nil error and expresses readiness through
`wrapperspb.BoolValue`, as the baseline does — returning a gRPC error makes
kubelet's liveness handling noisier without adding information.

### 8.2 Controller service

**Advertised capabilities** (P§12): `CREATE_DELETE_VOLUME`,
`PUBLISH_UNPUBLISH_VOLUME`. Access mode: `SINGLE_NODE_WRITER`. Volume mode:
Block only.

Not advertised: `LIST_VOLUMES`, `EXPAND_VOLUME`, `CREATE_DELETE_SNAPSHOT`,
`CLONE_VOLUME`, `VOLUME_CONDITION`, multi-node access modes.

#### CreateVolume

K8s chain: PVC created → `external-provisioner` → `CreateVolume` → PV bound.

```text
Input: name, capacity_range, volume_capabilities, parameters{type, availability}
│
├─ 1. Validate: name non-empty; capabilities non-empty
│       REJECT any capability with GetMount() != nil        → InvalidArgument
│       REQUIRE GetBlock() != nil                            → InvalidArgument
│       REJECT access modes other than SINGLE_NODE_WRITER    → InvalidArgument
├─ 2. Size: default 1 GiB, else util.RoundUpSize(required, 1 GiB)
├─ 3. volType := parameters["type"]; if empty → VolumeOpts.DefaultVolumeType
│       never hard-code "ceph-rook-store"                          [V]
├─ 4. Idempotency: cloud.GetVolumesByName(name)
│       1 match  → size differs? AlreadyExists
│                  metadata has attachment_id? return the volume
│                  otherwise list attachments:
│                    any record → FailedPrecondition; ownership is ambiguous
│                    none → continue at step 7 and persist a new attachment ID
│       >1 match → Internal
├─ 5. cloud.CreateVolume({Name, Size, VolumeType, AvailabilityZone})
├─ 6. cloud.WaitVolumeTargetStatus(id, ["available"], VolumeOpts.CreateTimeout)
├─ 7. cloud.CreateAttachment(id)          # reserved: no connector, no instance
├─ 8. cloud.SetVolumeMetadata(id, {"csi.rbd.attachment_id": attachmentID})
│       failure is FATAL: delete the new attachment, then delete the new volume;
│       return the persistence error plus any cleanup errors
│
Output: Volume{
          VolumeId:      <cinder volume uuid>,
          CapacityBytes: sizeBytes,
          VolumeContext: {"attachment_id": <id>},   # informational only; goes stale
        }
```

Rollback: any failure after step 5 attempts cleanup of objects created by this
request before returning. If a new attachment exists, delete it before deleting
a new volume; an idempotency retry never deletes a pre-existing volume. A
metadata failure is not reported as success because a connector-less attachment
has no ownership marker that a later list operation could attribute safely.

RBD-specific notes: no RBD image feature negotiation happens here — the image is
created by Cinder's driver, and `exclusive-lock` is a default feature of
Cinder-created images on Ceph 18 **[V]**. Phase 0 confirms the feature is
present; the driver does not attempt to enable or disable image features
(P§4 non-goal: "automatic support for RBD image features the WRCP kernel cannot
map").

#### DeleteVolume

K8s chain: PVC deleted → `external-provisioner` → `DeleteVolume` → PV removed.
This is the migration handoff point.

```text
Input: volume_id
│
├─ 1. cloud.GetVolume(id) → NotFound ⇒ return success (idempotent)
├─ 2. Read metadata: attachmentID, cleanupVolume
├─ 3. if attachmentID != "": cloud.DeleteAttachment(attachmentID)   (404 tolerated)
├─ 4. Wait until the volume leaves in-use/reserved
│       cloud.WaitVolumeTargetStatus(id, ["available","error"], VolumeOpts.DetachTimeout)
├─ 5. if cleanupVolume=="true" or DeleteVolumeMode=="delete":
│       return FailedPrecondition; automatic physical deletion is disabled
│       until Q8 defines a cross-node proof that no krbd map remains
├─ 6. RETAIN: cloud.DeleteVolumeMetadata(id, ["csi.rbd.attachment_id",
│            "csi.rbd.cleanupVolume"])   # strip CSI ownership only
│
Output: DeleteVolumeResponse{}
```

Unlike the baseline, the initial RBD driver does not physically delete a
Cinder volume. The controller cannot inspect node kernel maps, and Cinder
`available` only proves that no Cinder attachment record remains. Treating it
as a map proxy would make forced-detach and unreachable-node paths unsafe.
Physical deletion remains an operator/Blueprint action after independent map
verification until Q8 is closed. `DetachTimeout` — dead config in the baseline
— is read here.

Retain path handoff (unchanged from the iSCSI workflow):

```text
Blueprint, after PVC deletion:
  openstack volume set --bootable <volume-id>
  openstack server create --volume <volume-id> …
  → Nova/Cinder create their own attachment; the target VM boots from the volume
```

#### ControllerPublishVolume

K8s chain: pod scheduled → AD controller creates `VolumeAttachment` →
`external-attacher` → `ControllerPublishVolume(node_id)`.

```text
Input: volume_id, node_id, volume_capability
│
├─ 1. Validate volume_id, node_id, capability; reject Mount; require Block
├─ 2. host, ip := ParseNodeID(node_id)                                   [Q §6.4]
├─ 3. cloud.GetVolume(volume_id)  → NotFound ⇒ NotFound
├─ 4. attachmentID := metadata["csi.rbd.attachment_id"]
│       PV VolumeContext is NEVER used as a fallback (P§14, stale after unpublish)
├─ 5. if attachmentID == "":
│       ListAttachmentsByVolume(volume_id)
│         any record → FailedPrecondition; ownership is ambiguous, mutate none
│         no records → CreateAttachment(volume_id), then SetVolumeMetadata(...)
│           metadata failure → delete the new record and return error
├─ 6. existing := cloud.GetAttachment(attachmentID)
│       usable connection_info → normalize and continue at step 8
│       reserved/no connection_info → continue with connector update
│       NotFound → create replacement, persist metadata, continue
├─ 7. connector := buildConnector(host, ip, RBDOpts)                     [Q §6.3]
│     connInfo, err := cloud.UpdateAttachmentConnector(attachmentID, connector)
│       err is NotFound ⇒ create replacement record, persist metadata,
│                          retry the update EXACTLY ONCE
├─ 8. ValidateRBDConnectionInfo(connInfo, rbdOpts)
│       enforces driver_volume_type=="rbd", pool/image present,
│       ≥1 monitor, CephX/auth_username present, FSID/cluster match
├─ 9. if caps.SupportsV344: cloud.CompleteAttachment(attachmentID)  # best effort
│
Output: PublishContext{ driver_volume_type, cluster_name, cluster_fsid,
                        pool, image, monitors, auth_enabled, auth_username,
                        volume_id, access_mode, attachment_id }
        # no credential material, ever
```

Validation precedes completion so malformed or contradictory connection
information never advances the Cinder attachment state. `GetAttachment`
provides the idempotent response-loss path: a retry reuses already stored,
validated connection information rather than updating a completed record.
Every newly created or replacement attachment must be persisted to metadata
before use. If persistence fails, delete that new record and return the error;
if cleanup cannot be confirmed, a later retry fails closed when listing reveals
the unattributable record.
Each later CDI phase takes this path and receives fresh connection information
from a newly created record (P§8).

#### ControllerUnpublishVolume

Ordinary K8s chain: pod deleted → `NodeUnpublishVolume` →
`NodeUnstageVolume` → `VolumeAttachment` deleted → `external-attacher` →
`ControllerUnpublishVolume`. Forced detach and an unreachable node can bypass
the ordinary node-cleanup ordering; controller detach therefore does not prove
that the host map is gone.

**Not a no-op**, and **not** a rotation. The record is deleted and no
replacement is created; the next publish creates one on demand. This is what
returns the volume to `available` between CDI phases (P§2, P§13).

```text
Input: volume_id, node_id
│
├─ 1. cloud.GetVolume(volume_id) → NotFound ⇒ success (idempotent)
├─ 2. attachmentID := metadata["csi.rbd.attachment_id"]
│       empty ⇒ nothing to delete; fall through to step 4
├─ 3. cloud.DeleteAttachment(attachmentID)          (404 tolerated)
├─ 4. cloud.DeleteVolumeMetadata(volume_id, ["csi.rbd.attachment_id"])
│       failure → return retryable error so the stale ID is not concealed
├─ 5. Wait for the volume to reach "available" (bounded by DetachTimeout);
│       on timeout return Aborted so the attacher retries — the next CDI pod
│       must not be published while the volume is still reserved/in-use
│
Output: ControllerUnpublishVolumeResponse{}
```

If the Cinder API is unavailable, this RPC stays pending and is retried by the
external-attacher; the node-side unmap may already have completed
independently (P§10, last row). That asymmetry is intentional and must not be
"fixed" by having the node call Cinder.

#### ValidateVolumeCapabilities

Confirms the volume exists, then returns a `Confirmed` response only for
`Block` + `SINGLE_NODE_WRITER`. Mount capabilities and other access modes
produce a `Message` (not a gRPC error), matching the baseline and the CSI spec.

#### ControllerExpandVolume / snapshots / clones

`Unimplemented`. Not advertised. Do not add speculative implementations
(P§4, P§12).

### 8.3 Node service

**Advertised capabilities:** `STAGE_UNSTAGE_VOLUME`, `GET_VOLUME_STATS`.
Not advertised: `EXPAND_VOLUME`, `VOLUME_CONDITION`.

#### NodeGetInfo

```text
├─ hostname := os.Hostname()                       (injectable: hostnameFunc)
├─ NodeId := hostname                             # 1-field form; no ip needed [V]
├─ MaxVolumesPerNode := RBDOpts.MaxVolumesPerNode  # 0 = unlimited until measured [Q6]
└─ AccessibleTopology: nil                         # topology is a non-goal
```

Injection point on `nodeServer`, following the baseline pattern so this is
unit-testable: `hostnameFunc`. No `getInterfaceIPFunc` is needed — the
connector requires only `host` (§6.3).

#### NodeStageVolume

```text
Input: volume_id, staging_target_path, volume_capability, publish_context
│
├─ 1. Validate: volume_id, staging_target_path non-empty
│       REJECT GetMount() != nil; REQUIRE GetBlock() != nil    → InvalidArgument
│       REQUIRE SINGLE_NODE_WRITER                              → InvalidArgument
├─ 2. ci, attachmentID := ParsePublishContext(req.PublishContext)
│       then ValidateRBDConnectionInfo(ci, rbdOpts)             → InvalidArgument
├─ 3. cred := credentials.Load(ctx, ci.AuthUsername)
│       entity mismatch                                         → FailedPrecondition
├─ 4. Load the node-scoped ownership record for volume_id:
│       valid map-pending/staged record with the same identity → may reconcile
│       record identity mismatch → FailedPrecondition; mutate nothing
│       no record + matching live map → ISOLATE as unowned; never adopt/unmap
├─ 5. If an owned live map exists, VerifyIdentity:
│       ok       → finalize/repair records, goto step 10
│       mismatch → ISOLATE: fail FailedPrecondition, do NOT unmap
├─ 6. Atomically write the node-scoped map intent (phase=map-pending)
│       BEFORE invoking rbd; failure → return without a map side effect
├─ 7. Materialize credentials: <runtime-dir>/<volume-id>/{ceph.conf,keyring}
│       dir 0700, conf 0600, keyring 0400, temp+rename
├─ 8. dev := mapper.Map({identity, monitors, userID, conf, keyring,
│                        Exclusive: true, Timeout: map-timeout})
│       exclusive/lock failure → FailedPrecondition, NO fallback   (P§7)
│       monitor unreachable    → Unavailable (retryable)
├─ 9. Identity gate, all five checks of §7.4
│       any failure → best-effort unmap of the device WE just created,
│                     remove credentials and intent only after absence confirmed;
│                     otherwise retain intent and return a retryable error
├─ 10. Atomically finalize the node-scoped record (phase=staged), then write
│        the staging-path copy; increment map_generation
│        persistence failure after map → unmap; retain intent if cleanup uncertain
├─ 11. Do NOT create the bind target here — that is NodePublishVolume's job
│
Output: NodeStageVolumeResponse{}
```

The pre-map intent closes the crash window between the kernel side effect and
record persistence. Kernel identity is authoritative for *what* a map is; the
node-scoped intent is authoritative for whether this driver owns it. A matching
pool/image without that ownership evidence is never adopted, including in
node-only mode where platform Ceph-CSI shares the same kernel map inventory.

#### NodePublishVolume

```text
Input: volume_id, staging_target_path, target_path, volume_capability
│
├─ 1. Validate; reject Mount; require Block
├─ 2. Idempotency: Mounter.IsLikelyNotMountPointAttach(target_path)
├─ 3. Read the staging record; resolve device_path
├─ 4. mapper.VerifyIdentity(device_path, identityFromRecord)   → FailedPrecondition
│       (a recycled /dev/rbdN must never be bind-mounted into a pod)
├─ 5. Mounter.MakeFile(target_path)      # replaces a kubelet-created directory
├─ 6. Mounter.Mounter().Mount(realDevicePath, target_path, "", []string{"bind"})
│       on failure remove the file created in step 5
│
Output: NodePublishVolumeResponse{}
```

No Cinder interaction, no Ceph interaction. `pkg/util/mount` is imported as-is,
including the `MakeFile` behavior that replaces a kubelet-created directory at
the block publish path with a regular file. **[R]**

#### NodeUnpublishVolume

`Mounter.UnmountPath(target_path)`; missing path is success. No Cinder or Ceph
interaction. **[R]**

#### NodeUnstageVolume

```text
Input: volume_id, staging_target_path
│
├─ 1. Verify no publish target still references the staged device
│       (scan the driver's own publish bookkeeping / mountpoints)
│       still referenced → FailedPrecondition (retryable by kubelet ordering)
├─ 2. Read the node-scoped ownership record, using the staging copy as a hint:
│       neither exists → success; this driver has no owned map to unstage
│       map-pending/staged exists → use its full identity for reconciliation
├─ 3. If a candidate device exists: mapper.VerifyIdentity(dev, want)
│       mismatch → ISOLATE: log, emit metric, keep both records,
│                  return FailedPrecondition and require operator resolution
│       (a recycled /dev/rbdN must never be unmapped on the strength of a
│        recorded device number alone — P§10)
├─ 4. mapper.Flush(dev)                       # blockdev --flushbufs
├─ 5. mapper.Unmap(dev, unmap-timeout)
│       timeout/busy → KEEP staging state, return Aborted (retryable)   (P§10)
├─ 6. Confirm absence: dev no longer in ListMapped()
├─ 7. Remove <runtime-dir>/<volume-id>/
├─ 8. Remove the staging record and the node-scoped index entry
│
Output: NodeUnstageVolumeResponse{}
```

No Cinder call is made here. The node never deletes the Cinder attachment
record — that is `ControllerUnpublishVolume`'s job, and keeping the split is what
makes the "Cinder API unavailable during unstage" row of P§10 tractable.

#### NodeGetVolumeStats

`os.Stat(volume_path)` → `NotFound` if absent → `Mounter.GetDeviceStats`. For a
raw block volume, return a single `VolumeUsage{Total: bytes, Unit: BYTES}`; no
inode statistics exist for block devices. **[R]**

#### NodeExpandVolume

`Unimplemented`. Expansion is a non-goal.

### 8.4 gRPC error code mapping

Consistency here is what makes CSI sidecar retry behavior predictable, so it is
specified rather than left to each RPC's author. **[P]**

| Condition | Code | Sidecar effect |
|---|---|---|
| Missing/malformed request field, mount capability, wrong access mode | `InvalidArgument` | no retry — surfaced on the PVC/pod |
| Volume absent in Cinder on publish | `NotFound` | no retry |
| Volume absent on delete/unpublish/unstage | `OK` (idempotent) | done |
| Existing volume, different size | `AlreadyExists` | no retry |
| Expected FSID/cluster mismatch, pool/image mismatch, credential entity mismatch, exclusive-lock denied, conflicting live map, ambiguous attachment records | `FailedPrecondition` | retried, but requires operator action to succeed — always accompanied by a metric and a log naming the conflict |
| Monitor unreachable, Cinder API 5xx/timeout | `Unavailable` | retried with backoff |
| Unmap timeout, volume still `in-use` when it must be `available`, publish target still referenced | `Aborted` | retried with backoff |
| Unexpected internal failure, size verification failure | `Internal` | retried |
| Snapshots, clones, expansion | `Unimplemented` | no retry |

Rule: never return `OK` to hide a partial side effect. Returning a retryable
code and leaving durable state intact is always preferred over "clean up
aggressively and report success".

---

## 9. Recovery and reconciliation design

### 9.1 Node startup reconciliation

Runs before the node server accepts its first RPC (P§10). Pure function of
(records, live maps, sysfs) → actions, which is what makes it testable without a
kernel. **[P]**

```text
records := loadAll(state-dir)                  # driver-owned staging records
live    := mapper.ListMapped()                 # kernel inventory

for each record r in records:
    d := live.findByPoolImage(r.pool, r.image)
    case d == nil:
        → if phase is map-pending, confirm absence, then
          remove the record and leftover credential files.
        → if phase is staged, mark it unstaged and remove stale state.
          A later NodeStageVolume will map afresh.
    case VerifyIdentity(d, r.identity) == ok:
        → ADOPT: refresh r.device_path/device_id from d (the number may have
          changed), finalize a map-pending record, and keep map_generation.
          Do not recreate publish bind targets; kubelet replays NodePublishVolume.
    case identity mismatch:
        → ISOLATE: do not unmap, do not adopt. Emit
          cinder_rbd_csi_isolated_mappings metric, log pool/image/device and
          both identities, and refuse to serve this volume until an operator
          resolves it. Continue processing other records.

for each live map d NOT covered by any record:
    → unowned candidate: report and leave untouched. Never adopt or unmap it.
```

The "never unmap an unrecognized device" rule is absolute. A device the driver
does not understand may belong to platform Ceph-CSI, which uses the same kernel
RBD path on the same nodes **[V]**. Unmapping it would fault a platform
workload.

This rule is the same in split and all-in-one deployments. A Cinder attachment
record proves reservation, not ownership of a particular host kernel map. Only
the pre-map node intent closes that attribution gap.

### 9.2 Failure matrix to code mapping

Every row of P§10 mapped to the component that implements it. This table is the
Phase 4 work list.

| P§10 failure | Implementing component | Mechanism |
|---|---|---|
| Volume created, reservation not created | `controllerserver.CreateVolume` | retry finds the volume by name (request identity), then creates the reservation |
| Reservation created, metadata write failed | `CreateVolume` / `ControllerPublishVolume` | delete the newly created record and return the error; if cleanup cannot be confirmed, a later list detects the unattributable record and fails `FailedPrecondition` for operator resolution |
| Attachment update succeeded, RPC response lost | `ControllerPublishVolume` | `GetAttachment(attachmentID)` returns the stored `connection_info`; normalize it instead of re-updating |
| Map succeeded, final staging record write failed | `NodeStageVolume` steps 6–10 | pre-map intent proves ownership; unmap the new device and remove intent only after absence is confirmed, otherwise retain intent for retry reconciliation |
| Plugin restart with intent-owned live map | `reconcile.go` | the same verified kernel mapping is reused, no remap |
| Node crash | `reconcile.go` + `ControllerPublishVolume` | ownership intents × live maps × sysfs on the node; metadata + `GetAttachment` on the controller, with listing used only for conflict detection |
| Exclusive map denied | `rbd_cli.Map` | error propagated as `FailedPrecondition`; no fallback path exists in the code |
| Conflicting Cinder attachment record | `ControllerPublishVolume` | fail without mutating any record; metric `duplicate_attachment_records`; runbook entry |
| Unmap timeout | `NodeUnstageVolume` step 5 | keep staging state, return `Aborted` |
| Cinder API unavailable during unstage | node/controller split | node unmaps; `ControllerUnpublishVolume` remains pending and is retried by external-attacher |
| Credential rotated mid-flight | `credentials.Load` on each stage | projected Secret is re-read per stage; existing maps are unaffected (the kernel holds the session) |
| Conflicting live map at unstage | `NodeUnstageVolume` step 3 | isolate, retain state, and return `FailedPrecondition`; never unmap on a device number alone |

---


## 10. Build, packaging, and deployment

### 10.1 Makefile changes

Three additions, all following the existing generic rules — no new build rule is
needed because `$(BUILD_CMDS): $(SOURCES)` already compiles `cmd/$@/main.go`. **[R]**

```make
# Makefile:43  IMAGE_NAMES ?= …  (add after cinder-iscsi-csi-plugin, line 45)
				cinder-rbd-csi-plugin \

# Makefile:53  BUILD_CMDS ?= …   (add after cinder-iscsi-csi-plugin, line 55)
				cinder-rbd-csi-plugin \

# Makefile:204 build-cross — add one gox line
	CGO_ENABLED=0 gox -parallel=$(GOX_PARALLEL) -output="_dist/{{.OS}}-{{.Arch}}/{{.Dir}}" \
		-osarch='$(TARGETS)' $(GOFLAGS) $(if $(TAGS),-tags '$(TAGS)',) \
		-ldflags '$(GOX_LDFLAGS)' $(GIT_HOST)/$(BASE_DIR)/cmd/cinder-rbd-csi-plugin/
```

Verification: `make cinder-rbd-csi-plugin` produces a static binary;
`make unit` picks up `pkg/csi/cinder-rbd/...` automatically (`-tags=unit` over
all packages except `sanity`/`tests`); `make check` runs golangci-lint v2.3.1 —
keep the `//revive:disable:unexported-return` pragma around the `New*Server`
constructors, as the baseline does, or the lint fails.

A `test-cinder-rbd-csi-sanity` target is added **only if** Phase 5 creates
`tests/sanity/cinder-rbd/`. The baseline has a dangling target of this kind
(§2.4); do not reproduce it.

### 10.2 Dockerfile stage

The plugin image must carry a **qualified Ceph 18.2.x `rbd` CLI**, because the
WRCP host CLI is Ceph 14.2.22 while the cluster runs Rook Ceph 18.2.2 **[V]**
(P§3, P§14). The kernel performs the mapping; the CLI only issues it.

Three packaging options; Phase 1 picks one and records it as decision D-4:

| Option | Approach | Trade-off |
|---|---|---|
| **A (recommended)** | `debian-base:bookworm` + the upstream Ceph *reef* apt repository, installing a **pinned** `ceph-common=18.2.x-…` | Small image, explicit version pin, one external repo to trust and mirror. Debian bookworm's own `ceph-common` is 16.x and therefore unusable. |
| B | multi-stage copy of `rbd` plus its shared-library closure out of `quay.io/ceph/ceph:v18.2.2`, in the style of `tools/csi-deps.sh` | Smallest result, but a fragile library closure to maintain per Ceph patch release. |
| C | base the node image directly on `quay.io/ceph/ceph:v18.2.2` | Exactly matches the cluster tooling and needs no repo trust decision, but a much larger image and a base the project does not otherwise consume. |

Option A, sketched:

```dockerfile
##
## cinder-rbd-csi-plugin (development)
##
FROM ${DEBIAN_IMAGE} AS cinder-rbd-csi-plugin

# [V] P0-9: download.ceph.com/debian-reef publishes a bookworm suite whose
# current ceph-common is 18.2.8-1~bpo12+1. Client 18.2.8 against the lab's
# 18.2.2 cluster is the same 18.2 line and is supported.
ARG CEPH_PKG_VERSION=18.2.8-1~bpo12+1
# Pinned upstream Ceph reef repository; mirror internally for production builds.
RUN clean-install ca-certificates gnupg curl \
 && curl -fsSL https://download.ceph.com/keys/release.asc \
      -o /etc/apt/trusted.gpg.d/ceph.asc \
 && echo "deb https://download.ceph.com/debian-reef bookworm main" \
      > /etc/apt/sources.list.d/ceph.list \
 && clean-install ceph-common="${CEPH_PKG_VERSION}" \
                  mount util-linux \
 && rbd --version

COPY --from=builder /build/cinder-rbd-csi-plugin /bin/cinder-rbd-csi-plugin
COPY --from=certs /etc/ssl/certs /etc/ssl/certs

LABEL name="cinder-rbd-csi-plugin" \
      license="Apache Version 2.0" \
      maintainers="Kubernetes Authors" \
      description="Cinder RBD CSI Plugin" \
      distribution-scope="public" \
      summary="Cinder RBD CSI Plugin for Ceph RBD-backed Cinder volumes" \
      help="none"

CMD ["/bin/cinder-rbd-csi-plugin"]
```

Notes:

- **[V] P0-9:** `debian-reef` publishes `bookworm`, `focal`, and `jammy`; the
  bookworm `ceph-common` is `18.2.8-1~bpo12+1`. The container base is bookworm
  even though the WRCP *hosts* are Debian 11 bullseye — container userspace is
  independent of the host, and only the kernel (6.6, shared) does the mapping.
  Fallback option C image is available as `quay.io/ceph/ceph:v18.2.8`.
- `rbd --version` in the build is a cheap guard that the pin resolved to 18.2.x.
- **[V] P0-10: the `rbd` module is already loaded on WRCP 24.09** (`lsmod`
  shows `rbd` with refcount 10, `libceph` beneath it), because platform
  Ceph-CSI uses krbd. `/lib/modules` therefore does **not** need mounting and
  `kmod` is not required. "rbd module preloaded" becomes a documented
  prerequisite (§7.1) rather than a runtime `modprobe`.
- One image serves both controller and node, as with the baseline. The
  controller does not use `rbd`; carrying it costs image size but keeps the
  chart, the release workflow, and the version story single-track. Splitting into
  two images is a possible Phase 5 optimization, not a requirement.
- A distroless production image is a follow-up, mirroring the baseline's
  documented "3-step distroless" TODO.

### 10.3 Node DaemonSet host access

Derived from the baseline DaemonSet with every iSCSI-specific path removed and
the RBD-specific ones added. **[P]**

| Mount | Host path | Mode | Why |
|---|---|---|---|
| `socket-dir` | `{kubeletDir}/plugins/cinder-rbd.csi.windriver.com` | `DirectoryOrCreate` | CSI socket |
| `registration-dir` | `{kubeletDir}/plugins_registry/` | `Directory` | node-driver-registrar |
| `kubelet-dir` | `{kubeletDir}` | `Directory`, `mountPropagation: Bidirectional` | staging/publish paths |
| `dev-dir` | `/dev` | `Directory`, `HostToContainer` | `/dev/rbdN` visibility |
| `sys-dir` | `/sys` | `Directory` | `/sys/bus/rbd` identity checks — **read-write**, because `rbd device map/unmap` writes to `/sys/bus/rbd/add`/`remove` |
| `run-dir` | *no host path* — `emptyDir{medium: Memory}` at `/run/cinder-rbd-csi` | private | generated `ceph.conf` + keyring; memory-backed so keys never touch disk |
| `state-dir` | `/var/lib/cinder-rbd-csi` | `DirectoryOrCreate` | staging record index, must survive plugin restarts |
| `ceph-credentials` | *Secret projection* at `/etc/cinder-rbd-csi/ceph` | `readOnly`, `defaultMode: 0400` | operator-managed `client.cinder` key (§6.7 option C) |
| `driver-config` | ConfigMap `driver.conf`, `subPath` | `readOnly` | `[RBD]`/`[Volume]` options |
| `lib-modules` *(conditional)* | `/lib/modules` | `readOnly` | only if `modprobe rbd` is needed (§10.2) |

Removed relative to the baseline: `/etc/iscsi`, `/var/lib/iscsi`,
`/run/lock/iscsi`, and `hostPID: true`.

Container security context: `privileged: true` with `SYS_ADMIN`, as the baseline
uses. P§11 allows an equivalent validated capability set instead; determining the
minimal set for krbd is a Phase 6 hardening task, not a Phase 3 blocker.
`hostNetwork: true` and `dnsPolicy: ClusterFirstWithHostNet` are retained so the
node can reach Ceph monitor IPs on the storage network.

> **Security note.** This DaemonSet is privileged and mounts `/dev` and `/sys`
> read-write on every node it runs on. That is inherent to kernel RBD mapping and
> matches what platform Ceph-CSI already does on the same nodes, but it means
> access to the driver's ServiceAccount and to its credential Secret must be
> restricted to the driver itself (P§11). The RBAC templates must not grant
> broad `secrets` read access; with option C the node plugin needs **no** Secret
> RBAC at all.

The controller Deployment mirrors the baseline: `csi-attacher`,
`csi-provisioner` (`--extra-create-metadata`, `--leader-election`,
`--timeout={{ .Values.timeout }}`), `livenessprobe`, and the plugin with
`--provide-node-service=false`, two `--cloud-config` values (`cloud.conf` from a
Secret, `driver.conf` from the ConfigMap), and the optional CA bundle via the
`cinder-rbd-csi.cacert` helper.

### 10.4 Helm chart and manifests

Chart `charts/cinder-rbd-csi-plugin/`, chart name `openstack-cinder-rbd-csi`.
Structure and values keys mirror the iSCSI chart so operators see one pattern
across the sibling drivers. **[P]**

```yaml
# charts/cinder-rbd-csi-plugin/values.yaml (excerpt)
csi:
  plugin:
    image:
      repository: ghcr.io/solutions-innovation/cinder-rbd-csi-plugin
      pullPolicy: Always
      tag:                       # defaults to .Chart.AppVersion
  attacher:
    image: registry.k8s.io/sig-storage/csi-attacher:v4.10.0
  provisioner:
    image: registry.k8s.io/sig-storage/csi-provisioner:v5.3.0
    topology: "false"
  nodeDriverRegistrar:
    image: registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.15.0
  livenessprobe:
    image: registry.k8s.io/sig-storage/livenessprobe:v2.17.0

cephCredential:
  # The operator duplicates the platform client.cinder key into this Secret.
  # create: false means "the Secret already exists"; the chart only projects it.
  create: false
  secretName: cinder-rbd-ceph-client
  userIDKey: userID
  userKeyKey: userKey

driverConfig:
  enabled: true
  name: cinder-rbd-driver-config
  rbd:
    expectedClusterName: ceph
    expectedFsid: ""            # REQUIRED per deployment; chart fails if empty
    cephClientVersionMajor: 18
    mapTimeout: 120s
    unmapTimeout: 120s
  volume:
    defaultVolumeType: ceph-rook-store
    metadataPrefix: csi.rbd
    deleteVolumeMode: retain

storageClass:
  enabled: true
  name: cinder-rbd-migration
  isDefault: false
  reclaimPolicy: Delete
  volumeBindingMode: Immediate
  parameters:
    type: ceph-rook-store       # read from the StorageClass; never hard-coded [V]

storageProfile:
  enabled: true
  claimPropertySets:
    - accessModes: [ReadWriteOnce]
      volumeMode: Block
```

Chart-level guards, verified by `hack/verify-cinder-rbd-chart.sh`:

1. `driverConfig.rbd.expectedFsid` empty ⇒ `fail` with a message pointing at
   `ceph fsid`. An unset FSID would silently disable identity check #1 of §7.4.
2. `cephCredential.create: true` renders a Secret only from values the operator
   supplies out-of-band; the chart must never embed a key in a committed values
   file, and `helm template` output is checked for a `userKey` literal.
3. The cacert helper indirection is enforced exactly as in the iSCSI script: no
   template other than `_helpers.tpl` may dereference `.Values.cacert`, and
   rendering with the legacy no-cacert values must not emit a `cacert` volume.
4. `exclusive: false` in rendered `driver.conf` ⇒ `fail`. The chart refuses to
   render a configuration the driver would reject at startup.

Static manifests under `manifests/cinder-rbd-csi-plugin/` mirror the chart for
non-Helm deployments, including the `CSIDriver` object:

```yaml
apiVersion: storage.k8s.io/v1
kind: CSIDriver
metadata:
  name: cinder-rbd.csi.windriver.com
spec:
  attachRequired: true          # external-attacher drives ControllerPublish
  podInfoOnMount: false
  volumeLifecycleModes: [Persistent]
```

### 10.5 StorageClass, PVC, and CDI StorageProfile

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: cinder-rbd-migration
provisioner: cinder-rbd.csi.windriver.com
parameters:
  type: ceph-rook-store        # Cinder volume type, from the StorageClass  [V]
  availability: nova
reclaimPolicy: Delete          # Delete → DeleteVolume → RETAIN by default
volumeBindingMode: Immediate
allowVolumeExpansion: false    # expansion is a non-goal
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: migration-target-disk
spec:
  accessModes: [ReadWriteOnce]
  volumeMode: Block            # MANDATORY — Filesystem is rejected
  resources:
    requests:
      storage: 20Gi
  storageClassName: cinder-rbd-migration
---
apiVersion: cdi.kubevirt.io/v1beta1
kind: StorageProfile
metadata:
  name: cinder-rbd-migration   # MUST equal the StorageClass name
spec:
  claimPropertySets:
    - accessModes: [ReadWriteOnce]
      volumeMode: Block
```

The StorageProfile is not optional. CDI defaults an unknown provisioner to
`volumeMode: Filesystem`, this driver rejects Filesystem, and the DataVolume's
PVC then hangs `Pending` with no obvious cause. It must be reapplied whenever the
StorageClass is recreated. **[R]** (P§11)

Consuming pods use `volumeDevices`, never `volumeMounts`:

```yaml
spec:
  containers:
    - name: writer
      volumeDevices:
        - name: target
          devicePath: /dev/target
  volumes:
    - name: target
      persistentVolumeClaim:
        claimName: migration-target-disk
```

### 10.6 Metrics and logging redaction

Metrics registered under `cinder-rbd-csi` via `pkg/metrics`, exposed on
`--http-endpoint`. Cinder calls reuse the baseline's
`metrics.NewMetricContext(resource, op)` + `mc.ObserveRequest(err)` wrapper,
which yields latency and error counters for free. **[R]**

RBD-specific series to add (P§12): **[P]**

| Metric | Type | Labels |
|---|---|---|
| `cinder_rbd_csi_map_duration_seconds` | histogram | `result` |
| `cinder_rbd_csi_unmap_duration_seconds` | histogram | `result` |
| `cinder_rbd_csi_exclusive_lock_failures_total` | counter | — |
| `cinder_rbd_csi_attachment_records_created_total` | counter | `reason` (`create_volume`, `on_demand`, `replacement`) |
| `cinder_rbd_csi_attachment_records_deleted_total` | counter | — |
| `cinder_rbd_csi_duplicate_attachment_records_total` | counter | — |
| `cinder_rbd_csi_isolated_mappings` | gauge | — |
| `cinder_rbd_csi_orphaned_staging_records` | gauge | — |
| `cinder_rbd_csi_staged_volumes` | gauge | — |
| `cinder_rbd_csi_volumes_retained_total` / `_deleted_total` | counter | — |

Permitted identifiers in logs and metrics (P§12): CSI volume ID, Cinder volume
UUID, Cinder attachment ID, node ID, Ceph cluster FSID, pool, image, device path,
map generation, lifecycle state.

Redaction rules, enforced by construction rather than by reviewer vigilance:

1. `CephCredential.key` is unexported and the type implements
   `String() string` returning `"<redacted>"`, so it cannot be accidentally
   `%v`-printed.
2. The credential provider is the only code that reads `userKey`; it never
   returns the raw string to callers other than the keyring writer.
3. The keyring writer takes an `io.Writer` and the credential, and is the single
   place the key is formatted.
4. A unit test asserts that a rendered log line for a full stage operation
   contains none of: the key bytes, the string `keyring =`, or the contents of
   the generated keyring file.
5. `logGRPC` keeps `protosanitizer` as the baseline does, which matters because
   `NodeStageVolumeRequest.Secrets` would be logged otherwise — even though
   option C means the driver does not use that field.

### 10.7 Release workflow

`.github/workflows/cinder-rbd-csi-release.yaml`, structurally identical to the
iSCSI workflow: **[P]**

- Triggers: push to the implementation branch (`wndrvr/cinder-rbd-csi-plugin-impl`)
  and tags `wndrvr_v*`.
- Env: `REGISTRY=ghcr.io`,
  `IMAGE_NAME=solutions-innovation/cinder-rbd-csi-plugin`,
  `HELM_CHART_DIR=charts/cinder-rbd-csi-plugin`,
  `HELM_OCI_REPO=oci://ghcr.io/solutions-innovation/charts`.
- Job 1 `build-and-push-image`: derive version (tag `wndrvr_v1.0.0` → image tag
  `wndrvr_v1.0.0`, build version `v1.0.0`; branch → `dev-<sha>`), buildx,
  `docker/build-push-action` with `target: cinder-rbd-csi-plugin`,
  `platforms: linux/amd64`, `build-args: VERSION=…`.
- Job 2 `helm-package`: rewrite `version`/`appVersion` in `Chart.yaml` and the
  `tag:` sentinel in `values.yaml`, `helm lint`,
  `bash hack/verify-cinder-rbd-chart.sh`, `helm package`, `helm push` to OCI.
- Job 3 `create-release`: on tags only, attach the packaged chart.

Additional gate not present in the iSCSI workflow: the image job asserts
`docker run --rm <image> rbd --version` reports major version 18, so a
repository drift cannot silently ship a Ceph 16 client.

---

## 11. Testing strategy

Unit tests must run on a developer laptop with no Ceph, no kernel RBD module,
and no OpenStack. Everything that touches the kernel or the network sits behind
`RBDMapper`, `CephCredentialProvider`, `IOpenStackRBD`, or `mount.IMount`. **[P]**

| Layer | Approach | Key cases |
|---|---|---|
| `rbdconnection_test.go` | table-driven fixtures | flat payload **[V]**; nested-only payload; nested filling an absent field; nested **conflicting** with top level ⇒ reject; `name` without `/`; `name` with multiple `/` (image keeps them); empty pool or image; `hosts`/`ports` length mismatch; `auth_enabled: false` ⇒ reject; missing `auth_username`; a payload containing a `keyring` field ⇒ reject; IPv6 monitor addresses |
| `connectioninfo_test.go` | round-trip | `BuildPublishContext` → `ParsePublishContext` identity; assert **no** key contains credential material; `ParseNodeID` 1-field and 2-field forms; malformed node IDs |
| `rbd_cli_test.go` | `k8s.io/utils/exec/testing` | argv always contains `--exclusive`, `--device-type krbd`, correct `--id`/`--conf`/`--keyring`; **`--conf` present on `list` and `unmap` too** (§6.6 rule 5); `rbd device list --format json` parsing incl. empty output and namespaces; **stderr `parse error` noise with exit 0 ⇒ success**; map failure on lock contention produces **no** second invocation; timeout propagation via `CommandContext` |
| `rbd_sysfs_test.go` | fake sysfs tree in `t.TempDir()` | identity match; pool match but image mismatch ⇒ error; FSID mismatch ⇒ error; a **missing** `cluster_fsid` attribute must **fail closed**, never be skipped |
| `credentials_test.go` | temp dirs | entity/`auth_username` mismatch ⇒ error before any map; file modes 0700/0600/0400; temp+rename atomicity; `String()` redaction; log-scrub assertion |
| `staging_test.go` | temp dirs | pre-map intent precedes mapper call; atomic phase transitions; unknown schema/phase ⇒ isolate; `map_generation` increment |
| `reconcile_test.go` | pure inputs | adopt only with an ownership intent; finalize `map-pending`; mark unstaged; identity mismatch ⇒ isolate **and no unmap call**; every unrecorded live map ⇒ report, never adopt/unmap |
| `controllerserver_test.go` | `OpenStackRBDMock` | mirrors the baseline cases plus: metadata write failure deletes the new attachment and fails; any unattributable listed record ⇒ `FailedPrecondition`; stored connection info recovers a lost response without a second update; validation precedes completion; 404-on-update ⇒ exactly one replacement + one retry; retain-only delete behavior |
| `nodeserver_test.go` | `RBDMapperMock`, `mount.MountMock` | mount capability rejection; matching map without intent is never adopted; map intent is durable before mapper invocation; final-record failure triggers verified unmap or retained intent; all five identity-gate failures; unstage mismatch returns `FailedPrecondition` and keeps state; unmap timeout keeps state; publish verifies identity before bind |
| Chart | `helm lint` + `hack/verify-cinder-rbd-chart.sh` | empty FSID fails; `exclusive: false` fails; no key literal in rendered output; cacert helper indirection |
| CSI sanity | `tests/sanity/cinder-rbd/` (optional, Phase 5) | only if a fake mapper + fake Cinder are wired; otherwise omit rather than leave a dangling Makefile target |
| Lab E2E | WRCP 24.09 (see the lab-environment reference) | the 15 requirements of P§15 and the 19-item qualification reference |

Hard test-hygiene rules, learned from the baseline: **no test may create,
modify, or remove anything under `/dev`, `/sys`, or `/etc/ceph`.** Device path
prefixes and sysfs roots are package `var`s so tests redirect them to
`t.TempDir()`. On a developer host with live Ceph mappings, violating this
deletes real kernel state.

---

## 12. Development phases

Each phase states its inputs from the proposal, its tasks with target files, and
**exit criteria that must be demonstrable**. A phase is not complete because its
code compiles; it is complete when its exit criteria are shown.

### Phase 0 — Qualification gate (blocking)

**Goal:** answer the open contracts and satisfy the proposal's mandatory
pre-implementation requirements. P§15 states that requirements 2, 3, 5, 8, 9, and
10 are mandatory *before implementation work proceeds beyond qualification*.
No Go code in `pkg/csi/cinder-rbd/` is written until this phase closes, except
throwaway probe scripts.

| Task | Description | Answers |
|---|---|---|
| P0-1 | Update a Cinder attachment record with progressively minimal connectors; determine the required property set | §6.3 connector contract, decision D-1 |
| P0-2 | Confirm the node-ID content the connector actually needs (hostname only, or hostname + storage IP) | §6.4 node-ID contract |
| P0-3 | Inspect `/sys/bus/rbd/devices/<N>/` on the WRCP 24.09 kernel; record which attributes exist, especially `cluster_fsid` | §6.6 rule 4, §7.4 check 1 |
| P0-4 | Confirm `exclusive-lock` is present on Cinder-created images and that a second writable map is rejected | P§15.5 |
| P0-5 | Duplicate the platform `client.cinder` key into a test Secret; verify entity and FSID against `secret_uuid` | P§15.2 |
| P0-6 | Map the exact returned pool/image with the duplicated key; write, read, flush, unmap, remap; verify integrity | P§15.3, P§15.4 |
| P0-7 | Create a reserved attachment record with no Nova server; update it at 3.27; complete it at 3.44 | P§15.8, P§15.9 |
| P0-8 | Delete the record and confirm the volume returns to `available`; create a second record for the same volume | P§15.10 |
| P0-9 | Identify a Ceph 18.2.x `rbd` package source and pin it; confirm `rbd --version` in a scratch image | §10.2, decision D-4 |
| P0-10 | Confirm the `rbd` kernel module is preloaded on WRCP workers; decide whether `/lib/modules` must be mounted | §10.2, §10.3 |

**Exit criteria**
- P0-1–P0-10 answered, with commands and outputs recorded in the qualification
  reference (secrets redacted).
- §6.3, §6.4, §6.6 rule 4, §7.4 check 1, and §10.2 in **this document** updated
  from **[Q]** to **[V]** or to a concrete design decision.
- Decisions D-1, D-2, D-4 recorded in §14.

**Gate:** Phase 2 code review may not begin while the connector or node-ID
contract is still open. **This gate is now satisfied** — both closed in §6.3
and §6.4 by P0-1/P0-2.

**Phase 0 status: COMPLETE** (2026-09-01). Evidence in §14.

### Phase 1 — Scaffold, interfaces, buildable binary

**Goal:** a binary that starts, serves the identity service on a unix socket,
and reports readiness correctly in both modes.

| Task | Description | Files |
|---|---|---|
| 1.1 | Package skeleton, `Driver`, `DriverOpts`, `NewDriver`, capability lists | `driver.go` |
| 1.2 | Copy gRPC infrastructure, retarget types, keep the revive pragma | `server.go`, `utils.go` |
| 1.3 | Define `IOpenStackRBD`, `RBDOpts`, `VolumeOpts`, `applyDefaults()` | `openstack/openstack.go` |
| 1.4 | Define `RBDMapper`, `ImageIdentity`, `MapRequest`, `MappedDevice` | `rbd.go` |
| 1.5 | Define `CephCredentialProvider` | `credentials.go` |
| 1.6 | Identity server with mode-aware `Probe` incl. the nil-mapper guard | `identityserver.go` |
| 1.7 | cobra entry point, flags mirroring the iSCSI set | `cmd/cinder-rbd-csi-plugin/main.go` |
| 1.8 | Mocks for `IOpenStackRBD` and `RBDMapper` | `openstack/openstack_mock.go`, `rbd_mock.go` |
| 1.9 | Makefile, Dockerfile stage, OWNERS, cross-link the proposal | `Makefile`, `Dockerfile`, `OWNERS` |
| 1.10 | `applyDefaults` unit tests | `openstack/openstack_test.go` |

**Phase 1 status: COMPLETE** (2026-09-01). 109 tests/subtests pass, lint clean,
verified against a live gRPC socket. Findings recorded below.

**Exit criteria**
- `make cinder-rbd-csi-plugin`, `make unit`, `make check` all pass.
- `docker build --target cinder-rbd-csi-plugin` succeeds and `rbd --version`
  in the image reports 18.2.x.
- The binary starts in node-only mode with no Cinder credentials and reports
  `Ready=false` with a clear reason; it reports `Ready=true` once a fake
  credential dir and a stub mapper are present.
- No dead config field: a test enumerates `RBDOpts`/`VolumeOpts` fields and
  asserts each is read somewhere (or the field is removed).

**Phase 1 findings**

1. **Typed-nil crash (fixed).** Reproduced live: a controller RPC to a
   node-only plugin segfaulted the process. Root cause and fix in §2.4 and
   Appendix D item 16; regression test
   `TestCSIServices_TypedNilInterface`. The iSCSI baseline still has it.
2. **`unused` linter vs. phase scaffolding.** golangci-lint v2's default
   `unused` linter rejects unexported placeholders declared for a later phase.
   Constants and helpers must therefore be introduced by the phase that first
   reads them — which is the same discipline as the no-dead-config rule.
   `metadataKey` was retained because a test exercises decision D-3.
3. **Docker image unverified.** No container runtime is available in the
   development environment, so the Dockerfile stage and the pinned
   `ceph-common=18.2.8-1~bpo12+1` install are **not** yet build-tested. This
   remains an open Phase 1 exit item, carried into Phase 5.
4. **`OWNERS` unchanged.** Neither sibling driver has a per-package `OWNERS`,
   and inventing approver names would be wrong. Deferred to a human decision.

### Phase 2 — OpenStack layer and controller service

**Goal:** the full Cinder attachment state machine, including RBD connection
normalization and every recovery boundary the controller owns.

| Task | Description | Files |
|---|---|---|
| 2.1 | Copy volume CRUD, status waiter, metadata read-modify-write | `openstack/openstack_volumes.go` |
| 2.2 | Copy attachment CRUD + microversion discovery; add `ListAttachmentsByVolume` as conflict detection, never ownership inference | `openstack/openstack_attachments.go` |
| 2.3 | Implement `parseRBDConnectionInfo` per §6.2, all nine rules | `openstack/rbdconnection.go` |
| 2.4 | Provider init: Cinder-only client, 3.27 startup gate, metrics registration, `mounter`/`exclusive` validation | `openstack/openstack.go` |
| 2.5 | `BuildPublishContext` / `ParsePublishContext` / `ValidateRBDConnectionInfo` / node-ID helpers | `connectioninfo.go` |
| 2.6 | `CreateVolume` (§8.2) | `controllerserver.go` |
| 2.7 | `DeleteVolume` retain-only path, cleanup-request rejection, and `DetachTimeout` wait | `controllerserver.go` |
| 2.8 | `ControllerPublishVolume` incl. fail-closed metadata handling, stored-response recovery, 404 replacement + single retry, validation before optional completion | `controllerserver.go` |
| 2.9 | `ControllerUnpublishVolume` incl. the wait-for-`available` step | `controllerserver.go` |
| 2.10 | `ValidateVolumeCapabilities`, `ControllerGetCapabilities`, `Unimplemented` stubs | `controllerserver.go` |
| 2.11 | Controller + connection-info unit tests | `controllerserver_test.go`, `openstack/rbdconnection_test.go`, `connectioninfo_test.go` |
| 2.12 | Cinder metrics for attachment create/delete/duplicate | `openstack/openstack_attachments.go` |

**Phase 2 status: CODE COMPLETE** (2026-09-01). 240 tests/subtests pass, lint
clean repo-wide, cross-compiles for amd64/arm/arm64. The lab-integration exit
criteria below still require a deployed image and remain open until Phase 5.

**Exit criteria**
- Against the lab Cinder: a PVC produces a Cinder volume plus a reserved
  attachment record, and `csi.rbd.attachment_id` appears in volume metadata.
- `ControllerPublishVolume` returns a `publish_context` whose `pool`, `image`,
  `cluster_fsid`, and `monitors` match the values `openstack volume ...`
  reports for the same volume — verified field by field, not just "non-empty".
- `publish_context` contains no credential material (asserted in a test).
- Deleting the record returns the volume to `available` **[V]**, and a second
  publish creates a new record.
- With metadata deliberately cleared, zero listed records permits creation;
  one or more unattributable records fails `FailedPrecondition` without
  mutating any of them.
- PVC deletion leaves the Cinder volume `available` with CSI metadata stripped.
  A physical-delete request fails closed until Q8 supplies a cross-node no-map
  proof.

**Phase 2 findings and corrections**

1. **Microversion 3.34 is not needed after all.** §7.3 claimed name filtering
   was unused, but `CreateVolume` idempotency needs it. Rather than add a
   microversion dependency, `GetVolumesByName` requests the server-side filter
   and then **re-filters exactly on the client**. This is a correctness fix, not
   tidiness: a backend that ignores or loosens the filter would make
   `CreateVolume` see several "matches" for a unique name and fail, or adopt an
   unrelated volume. The same local re-filter is applied in
   `ListAttachmentsByVolume`, where adopting another volume's record would
   attach the wrong image.
2. **`LimitBytes` was unimplemented in the baseline.** Cinder allocates whole
   GiB, so a PVC requesting 1 GiB + 1 byte rounds up to 2 GiB and can exceed an
   explicit `limit_bytes`. The CSI spec requires `OUT_OF_RANGE`; the baseline
   silently over-allocates. The RBD driver returns `OutOfRange`.
3. **32-bit narrowing guarded.** `Makefile` cross-compiles to `arm`, where `int`
   is 32 bits, so the GiB count is range-checked before the `int64 → int`
   conversion instead of being converted blind as the baseline does.
4. **Unknown `delete-volume-mode` is now rejected.** The baseline treats any
   non-`delete` value as retain, silently accepting a typo such as `delet`.
   `VolumeOpts.Validate` rejects it, so a misconfigured cleanup policy fails at
   startup rather than at PVC deletion.
5. **`attaching` handled.** No publish or delete path waits for `in-use`; the
   waits target `available`, and `DeleteVolume`/`ControllerUnpublishVolume`
   return retryable `Aborted` rather than proceeding while a mapping may remain.

### Phase 3 — Node data path (krbd)

**Goal:** exclusive kernel mapping with the full identity gate, and raw-block
exposure to a pod.

| Task | Description | Files |
|---|---|---|
| 3.1 | `rbdCLIMapper`: `Map`, `Unmap`, `ListMapped`, `Flush`, `DeviceSize`, `CheckClient` | `rbd_cli.go` |
| 3.2 | sysfs identity reader; handle the `cluster_fsid` outcome from Q3 | `rbd_sysfs.go` |
| 3.3 | `VerifyIdentity` combining `ListMapped` + sysfs + lock state | `rbd_cli.go` |
| 3.4 | `fileCredentialProvider`; `ceph.conf`/keyring materialization with temp+rename and strict modes | `credentials.go` |
| 3.5 | Pre-map ownership intent, staged record, atomic phase transitions, node-scoped index | `staging.go` |
| 3.6 | `NodeGetInfo` with the Phase 0 node-ID format and injection points | `nodeserver.go` |
| 3.7 | `NodeStageVolume` incl. intent-owned live-map idempotency and the five-check gate | `nodeserver.go` |
| 3.8 | `NodeUnstageVolume` incl. isolate-on-mismatch and timeout handling | `nodeserver.go` |
| 3.9 | `NodePublishVolume` / `NodeUnpublishVolume` raw-block bind | `nodeserver.go` |
| 3.10 | `NodeGetVolumeStats`; `NodeExpandVolume` unimplemented | `nodeserver.go` |
| 3.11 | Node DaemonSet host access per §10.3 (dev deployment) | `manifests/…/cinder-rbd-csi-nodeplugin.yaml` |
| 3.12 | Node unit tests, incl. the log-redaction assertion | `nodeserver_test.go`, `rbd_cli_test.go`, `credentials_test.go`, `rbd_sysfs_test.go`, `staging_test.go` |
| 3.13 | Map/unmap latency and exclusive-lock-failure metrics | `rbd_cli.go` |

**Phase 3 status: CODE COMPLETE** (2026-09-01). 359 tests/subtests pass, race
detector clean, lint clean repo-wide, cross-compiles amd64/arm/arm64. The
on-cluster exit criteria below need a deployed image and remain open until
Phase 5.

**Phase 3 findings**

1. **`sysfs` has no `snap` attribute.** It is `current_snap` (plus `snap_id`),
   and a namespace is `pool_ns`. §6.6 rule 4 had the name wrong; using it would
   have silently skipped a check. A `current_snap` of `-` means no snapshot, not
   a snapshot literally named `-`.
2. **`--conf` is needed on `rbd status` too**, not only map/list/unmap. Any
   invocation reaching the host config triggers the `%CLUSTER_UUID%` parse
   error, so `clusterArgs` centralises the flags and every call site uses it.
3. **A cluster-scoped `ceph.conf` is required at node startup.** Reconciliation
   runs before any volume is staged, so `ListMapped` and `Unmap` cannot rely on
   a per-volume conf existing. `prepareNodeRuntime` writes one, carrying the
   configured FSID and no key material.
4. **`DeviceSize` prefers `blockdev --getsize64` over sysfs `size`.** The sysfs
   attribute is in 512-byte sectors on some kernels; treating it as bytes would
   fail the size check for every volume. sysfs is the fallback.
5. **Failure signatures must be matched on text.** The CLI has no distinct exit
   codes for lock-denied versus busy versus not-mapped, and krbd reports an
   already-mapped image as `(30) Read-only file system`. The patterns are narrow
   and each is covered by a test using real captured output.
6. **A staging-record write failure after a successful map is not rolled back.**
   Unmapping would discard working state; the mapping is adopted by pool/image on
   the next stage instead. The RPC still fails so kubelet retries.

**Exit criteria**
- A raw-block pod on WRCP 24.09 writes to and reads from the volume; the data is
  verified out-of-band (`rbd export` checksum or a direct read on another host
  after unmap).
- `rbd device list` shows exactly one exclusive mapping while the pod runs, and
  none after the pod is deleted.
- A second writable map of the same image is rejected **[V]**, and
  `NodeStageVolume` fails `FailedPrecondition` rather than degrading.
- Each of the five identity-gate checks is failed deliberately (wrong FSID,
  wrong pool, held lock, missing device, wrong entity) and each produces the
  documented code and a log naming the mismatch.
- A generated keyring exists only under the memory-backed runtime dir with mode
  0400 and is gone after unstage; a full-stage log capture contains no key
  material.
- Kernel maps survive a node-plugin pod restart (the mapping is kernel-owned;
  no remap occurs).

### Phase 4 — Recovery, reconciliation, fault injection

**Goal:** every row of P§10 demonstrated, not merely coded.

| Task | Description | Files |
|---|---|---|
| 4.1 | Startup reconciler per §9.1 | `reconcile.go` |
| 4.2 | Wire reconciliation to run before the node serves its first RPC | `driver.go` |
| 4.3 | Orphan/conflict reporting, isolation state, metrics | `reconcile.go`, `nodeserver.go` |
| 4.4 | Fault-injection harness: fail after each external side effect | `*_test.go` + a lab script |
| 4.5 | Repeated publish/unpublish cycles across two workers | lab script |
| 4.6 | Operator runbook: conflicting records, isolated maps, key rotation, stuck unmap | `docs/…/rbd-cinder-csi-operator-runbook.md` |
| 4.7 | Reconciler and recovery unit tests | `reconcile_test.go` |

**Exit criteria**
- Every row of the §9.2 table has a passing test or a recorded lab
  demonstration.
- Node-plugin restart with a live mapping reuses the same kernel mapping and
  refreshes a changed `/dev/rbdN` number without remapping.
- Node reboot leaves no orphaned staging records and no isolated mappings after
  the first successful stage.
- An artificially mismatched live mapping is isolated: not adopted, **not
  unmapped**, reported in `cinder_rbd_csi_isolated_mappings`, and covered by a
  runbook procedure.
- Killing the plugin after intent persistence but before final record
  persistence recovers only the intent-owned matching map on the next stage.
- Source-key rotation into the Secret is picked up on the next stage without a
  pod restart; an intentionally wrong key fails with a clear entity/auth error.

### Phase 5 — Packaging, chart, manifests, release

**Goal:** an installable, releasable artifact with the guards that keep
misconfiguration from becoming silent unsafety.

| Task | Description | Files |
|---|---|---|
| 5.1 | Helm chart with all templates and helpers | `charts/cinder-rbd-csi-plugin/` |
| 5.2 | Chart verification script incl. the FSID and `exclusive` guards | `hack/verify-cinder-rbd-chart.sh` |
| 5.3 | Static manifests incl. CSIDriver and RBAC | `manifests/cinder-rbd-csi-plugin/` |
| 5.4 | CDI StorageProfile template + standalone patch | `templates/storageprofile.yaml`, `manifests/…/cdi-storageprofile-patch.yaml` |
| 5.5 | Examples and a demo walkthrough | `examples/cinder-rbd-csi-plugin/` |
| 5.6 | Release workflow incl. the `rbd --version` gate | `.github/workflows/cinder-rbd-csi-release.yaml` |
| 5.7 | Metrics exposure and a dashboard/alert starting point | chart, runbook |
| 5.8 | Optional CSI sanity suite, or explicitly none (no dangling target) | `tests/sanity/cinder-rbd/` |
| 5.9 | File the deferred `pkg/csi/cinderattach/` extraction follow-up (§3) | issue tracker |

**Exit criteria**
- `helm lint` and `hack/verify-cinder-rbd-chart.sh` pass; rendering with an
  empty `expectedFsid` or `exclusive: false` fails with an actionable message.
- A clean-cluster install from the chart alone reaches a running controller and
  node plugin with `Probe` ready on both.
- The release workflow produces an image and an OCI chart, and the image gate
  confirms Ceph 18.2.x.
- The documented operator procedure (duplicate the key, set the FSID, install,
  test a map) is executed by someone who did not write the code.

### Phase 6 — Migration workflow integration and full qualification

**Goal:** close every remaining item of P§15 and prove the real workflows.

| Task | Description |
|---|---|
| 6.1 | CDI DataVolume on the RBD StorageClass with the StorageProfile applied |
| 6.2 | Multi-phase precopy: verify each importer pod deletion deletes the record and returns the volume to `available`, and that the next pod creates a new record |
| 6.3 | Cutover: final delta, pod movement between workers, remap on a different node |
| 6.4 | O2O/NBD workflow end to end |
| 6.5 | Blueprint handoff: PVC deletion retains the volume; `server create --volume` boots the target VM |
| 6.6 | Disruption tests: Cinder API outage, Ceph monitor disruption, node reboot mid-copy |
| 6.7 | Measure the krbd mapping limit; set `MaxVolumesPerNode` |
| 6.8 | Security review: minimal capability set instead of blanket `privileged`; confirm no secret leakage in logs, CSI responses, annotations, or metadata |
| 6.9 | Scale/soak: repeated create/publish/unpublish/delete cycles for orphan and leak detection |
| 6.10 | Sign-off: mark the P§15 list complete and update the qualification reference |

**Exit criteria**
- All 15 P§15 requirements and all 19 items of the qualification reference are
  demonstrated and recorded.
- A full V2O migration and a full O2O migration complete with data integrity
  verified on the target VM.
- No orphaned mappings, staging records, attachment records, or generated
  keyrings remain after the soak run.
- The driver's status in the skill references is updated from *proposed* to
  *qualified*, with the exact platform baseline stated.

---


## 13. Traceability matrix

Every proposal section maps to an implementation artifact and a phase. Use this
table in review to confirm nothing in the proposal is unimplemented and nothing
in the implementation is unproposed.

| Proposal section | Requirement | Implementation artifact | Phase |
|---|---|---|---|
| P§1 | Cinder attachment record reserves the volume | `openstack_attachments.go`, `controllerserver.go` | 2 |
| P§1 | krbd node mapping | `rbd_cli.go`, `nodeserver.go` | 3 |
| P§1 | Ceph exclusive lock while writable | `rbd_cli.Map` with mandatory `--exclusive` | 3 |
| P§1 | Record lifecycle tied to each migration pod | `ControllerPublishVolume` / `ControllerUnpublishVolume` | 2 |
| P§1, P§6 | Cinder's own identity + operator-duplicated Secret | `credentials.go`, chart `cephCredential` | 3, 5 |
| P§2 | Volume returns to `available` between pods | `ControllerUnpublishVolume` + wait-for-available | 2 |
| P§2 | Normal teardown releases the node map before controller detach; forced detach is an exception | lifecycle E2E plus forced-detach test | 6 |
| P§3 | Volume type from the StorageClass, never hard-coded | `CreateVolume` `parameters["type"]` → `DefaultVolumeType` | 2 |
| P§3, P§5 | Flat connection schema; nested only for compatibility | `parseRBDConnectionInfo` rules 1–2 | 2 |
| P§3, P§5 | `name` authoritative, split first `/`, no `volume-` prefix | `parseRBDConnectionInfo` rule 3 | 2 |
| P§4 | Raw block only, `SINGLE_NODE_WRITER` | capability lists + rejection in every capability RPC | 1, 2, 3 |
| P§4 | Retain-only initially | `DeleteVolume`; physical deletion gated by Q8 | 2, 6 |
| P§4 | No Nova, no shadow VM, no host Ceph deps | Cinder-only client; bundled CLI; no `/etc/ceph` mount | 1, 2, 3 |
| P§5 | `publish_context` carries normalized non-secret fields | §6.5 key set, `BuildPublishContext` | 2 |
| P§5 | `secret_uuid` is an identifier, never a key | `ClusterFSID` field + redaction tests | 2 |
| P§5 | Never log a key or keyring | §10.6 rules 1–5 | 3 |
| P§6 | Entity must equal `auth_username` | `credentials.Load(wantUserID)` pre-flight | 3 |
| P§6 | No runtime key discovery from Cinder pods or host keyrings | file-only credential provider | 3 |
| P§6 | Rotation by re-copying the Secret | projected Secret re-read per stage | 3, 4 |
| P§7 | Bundled Ceph 18.2.x CLI | Dockerfile stage + release gate | 1, 5 |
| P§7 | `--exclusive` mandatory, no fallback | `rbd_cli.Map` rule 1 + tests | 3 |
| P§7 | Reconcile identity via kernel/sysfs and ownership via pre-map intent | `ListMapped`, `rbd_sysfs.go`, `staging.go`, `VerifyIdentity` | 3 |
| P§7 | Five pre-publish verifications | §7.4 identity gate | 3 |
| P§8 | `CreateVolume` with mandatory attachment-ID persistence | §8.2 `CreateVolume` | 2 |
| P§8 | `ControllerPublishVolume` incl. stored-response recovery and one 404 replacement | §8.2 `ControllerPublishVolume` | 2 |
| P§8 | `ControllerUnpublishVolume` deletes the attachment and clears metadata | §8.2 `ControllerUnpublishVolume` | 2 |
| P§8 | `NodeStageVolume` persists ownership intent before mapping | §8.3 `NodeStageVolume` | 3 |
| P§8 | `NodeUnstageVolume` only removes an intent-owned, identity-verified map | §8.3 `NodeUnstageVolume` | 3 |
| P§8 | `DeleteVolume` retains until cross-node no-map proof exists | §8.2 `DeleteVolume`, Q8 | 2, 6 |
| P§8 | Controller state machine | §6.10 | 2 |
| P§9 | `csi.rbd.attachment_id` in Cinder metadata; PV context stale | `metadataKey` with prefix `csi.rbd`; no volumeContext fallback | 2 |
| P§9 | Missing metadata ⇒ fail on unattributable records or create and persist; stale ⇒ replace once | `ControllerPublishVolume` steps 5–7 | 2 |
| P§10 | Durable ownership intent plus kernel identity | `staging.go`, `reconcile.go` | 3, 4 |
| P§10 | Startup reconciliation, isolate conflicts | §9.1 | 4 |
| P§10 | Twelve-row failure matrix | §9.2 | 4 |
| P§11 | Privileged node access, `/dev` + `/sys`, private runtime dir | §10.3 | 3, 5 |
| P§11 | No host PID, no host `rbd`, no host `/etc/ceph` | §7.1, §10.3 | 3 |
| P§11 | Restrictive keyring permissions, removed when unused | §6.7, §8.3 unstage step 7 | 3 |
| P§11 | Configuration surface | §6.9 mapping table, chart `driverConfig` | 1, 5 |
| P§11 | CDI StorageProfile required | §10.5, chart `storageprofile.yaml` | 5 |
| P§12 | Advertised capabilities only | §8.1, §8.2, §8.3 capability lists | 1 |
| P§12 | `MaxVolumesPerNode` reflects validated limits | `RBDOpts.MaxVolumesPerNode`, measured in Phase 6 | 6 |
| P§12 | Non-secret identifiers in logs/metrics; recommended metric set | §10.6 | 3, 5 |
| P§13 | No record and no lock between pods | §6.10 invariants; E2E observation | 2, 6 |
| P§14 | Alternatives — bundled CLI, on-demand records, Cinder identity, metadata not PV context, exclusive lock | §3, §6.7, §6.9, §8.2 | — |
| P§15 | Qualification requirements | Phase 0 (mandatory subset) and Phase 6 (remainder) | 0, 6 |
| P§16 | Four-part implementation plan | Phases 1–2 (controller), 3 (node), 4 (recovery), 5–6 (qualification/packaging) | all |
| P§16 | Package layout mirrors cinder-iscsi with an `RBDMapper` node layer | §5.1, §6.6 | 1 |

Mapping of the proposal's own four-step plan to the phases here:

| P§16 step | Phases | Note |
|---|---|---|
| 1. Controller foundation | 1 + 2 | split so the scaffold is reviewable before the state machine |
| 2. Kernel RBD node path | 3 | |
| 3. Recovery and lifecycle | 4 | separated from Phase 3 because recovery is where the risk is |
| 4. Qualification | 0 + 5 + 6 | the mandatory subset is pulled **forward** into Phase 0, per P§15's own gating statement |

---

## 14. Open contracts and decision log

Items still marked **[Q]** anywhere in this document are listed here. Each must
be closed by Phase 0 or by the phase named.

| ID | Open contract | Status | Closes in |
|---|---|---|---|
| Q1 | Cinder connector property set (§6.3) | **CLOSED [V]** — `{"host": …}` only; `{}` rejected | Phase 0 / P0-1 |
| Q2 | Node-ID content and format (§6.4) | **CLOSED [V]** — bare hostname, 1-field | Phase 0 / P0-1,2 |
| Q3 | sysfs `cluster_fsid` on Linux 6.6 (§6.6 rule 4) | **CLOSED [V]** — present; direct FSID check | Phase 0 / P0-3 |
| Q4 | Ceph 18.2.x package source and pin (§10.2) | **CLOSED [V]** — `ceph-common=18.2.8-1~bpo12+1`, `debian-reef/bookworm` | Phase 0 / P0-9 |
| Q5 | `/lib/modules` needed for `modprobe rbd` (§10.3) | **CLOSED [V]** — no; module preloaded (refcount 10) | Phase 0 / P0-10 |
| Q6 | krbd mapping limit → `MaxVolumesPerNode` | **OPEN** | Phase 6 |
| Q7 | Minimal Linux capability set vs. blanket `privileged` (§10.3) | **OPEN** | Phase 6 |
| Q8 | **Cross-host** exclusive-lock rejection (P0-4 tested same-host only) | **OPEN** | Phase 6 |

### Phase 0 evidence summary (executed 2026-09-01, WRCP 24.09 lab)

| Item | Result |
|---|---|
| P0-1 connector | `{}` → HTTP 400 "does not have enough properties"; `{"host":"controller-0"}` → accepted, 16-field flat `connection_info` |
| P0-2 node ID | bare hostname sufficient; no `ip`/`initiator`/`platform`/`os_type` |
| P0-3 sysfs | `cluster_fsid` present (= `c5f7876d-…`); no `snap` attr (it is `current_snap`/`snap_id`); `pool_ns` for namespaces |
| P0-4 exclusive lock | Cinder image features `layering, exclusive-lock, object-map, fast-diff, deep-flatten`; krbd 6.6 maps all of them; 2nd map rejected, no 2nd device; `rbd status` shows 1 exclusive lock + locker, `Watchers: none` after unmap |
| P0-5 credential | `openstack/cinder-volume-rbd-keyring` present, type `kubernetes.io/rbd`; `client.cinder` caps `mon/osd = profile rbd`; FSID matches `secret_uuid` |
| P0-6 data path | map → `/dev/rbd5`; 4 MiB write, flush, md5 readback identical; unmap released lock; remap preserved data; final unmap clean |
| P0-7 attachment | reserved record from `volume_uuid` alone, `instance: None`; 3.27 update OK; 3.44 complete → HTTP 204, volume `in-use`, attachment `attached` |
| P0-8 lifecycle | DELETE → volume `available`; second record created with a **different** ID → `reserved` |
| P0-9 packaging | `debian-reef` has `bookworm`; `ceph-common 18.2.8-1~bpo12+1`; fallback `quay.io/ceph/ceph:v18.2.8` |
| P0-10 kernel | `rbd` loaded (refcount 10) over `libceph`; `/sys/bus/rbd` present; 5 live Ceph-CSI maps in pool `kube-rbd` |
| Cleanup | scratch volume deleted, keyring shredded, zero residual `cinder-volumes` mappings |

Incidental findings that changed the design:

1. **Broken host `ceph.conf`.** `/etc/ceph/ceph.conf` on WRCP contains
   `fsid = %CLUSTER_UUID%`. Every `rbd` call must pass `--conf`, including
   `device list` and `device unmap`, and stderr parse-error noise must not be
   read as failure (§6.6 rule 5, decision D-11).
2. **`attaching` is a real state.** The connector update moves the volume to
   `attaching`, not straight to `in-use` (§6.10). No publish path may wait on
   `in-use`, since `CompleteAttachment` is optional.
3. **Three undocumented response fields** — `cacheable`, `qos_specs`, `discard`
   — are present. `qos_specs` is deliberately not decoded (§6.2).
4. **Pool separation is real:** Cinder uses `cinder-volumes` (pool_id 7),
   platform Ceph-CSI uses `kube-rbd` (pool_id 2), same cluster. This makes the
   "never unmap an unrecognized device" rule concrete, not theoretical.
5. The lab `openstack` CLI is too old for `volume attachment` subcommands;
   attachment work needs raw API calls with an `OpenStack-API-Version` header.
   Worth noting in the operator runbook.

Decisions taken in this document, recorded so later phases do not silently
re-litigate them:

| ID | Decision | Rationale |
|---|---|---|
| D-1 | Build the connector from a `map[string]any` containing only Phase 0-proven fields; do not copy iSCSI connector fields by analogy | unnecessary fields are a silent compatibility risk; the iSCSI fields are meaningless for RBD |
| D-2 | Node plugin reads the Ceph credential from a **projected Secret volume** (§6.7 option C), not from CSI secrets or the Kubernetes API | keeps the key out of every RPC payload, needs no Secret RBAC, and supports rotation without a pod restart |
| D-3 | Both driver metadata keys use the configurable prefix: `csi.rbd.attachment_id` and `csi.rbd.cleanupVolume` | resolves the P§8/P§9 inconsistency; one prefix governs all driver-owned metadata and sibling drivers cannot collide on one volume |
| D-4 | Ceph tooling via a pinned upstream *reef* package on the Debian base (§10.2 option A), with option C as fallback | smallest change to the existing build with an explicit version pin |
| D-5 | Driver name and minimum microversion are Go constants, not configuration | a mutable driver name breaks CSIDriver/CSINode registration; 3.27 is a hard requirement, not a preference |
| D-6 | `exclusive: false` and `mounter != krbd` are rejected at startup and at chart render time | the only safe writable configuration is exclusive krbd; making the unsafe value unrepresentable is better than documenting it |
| D-7 | `ListAttachmentsByVolume` is a fail-closed conflict detector, never attachment-ownership inference | connector-less reserved records have no reliable driver marker |
| D-8 | Expansion, snapshots, clones, topology, and filesystem mode are `Unimplemented` and unadvertised — no stub methods that pretend otherwise | the baseline's `not implemented (Phase 2)` snapshot stubs are dead weight |
| D-9 | Extraction of a shared `pkg/csi/cinderattach/` package is deferred until after Phase 6 (§3) | the iSCSI driver is in use on a releasable branch; the RBD contracts are not yet frozen |
| D-10 | No deployment mode adopts or unmaps an unrecorded live mapping (§9.1) | Cinder state cannot attribute a host kernel map, and platform Ceph-CSI shares the same kernel path |
| D-11 | Persist a node-scoped `map-pending` intent before `rbd device map` | kernel/sysfs proves map identity, while the intent proves driver ownership across crashes |
| D-12 | Validate normalized connection information before optional completion | malformed connection data must not advance the Cinder attachment state |
| D-13 | Ship retain-only deletion until Q8 closes | Cinder status does not prove that every host has released its krbd map |

---

## Appendix A: iSCSI vs. RBD at a glance

```text
cinder-iscsi (implemented)                cinder-rbd (proposed)
──────────────────────────                ─────────────────────
cinder-iscsi.csi.windriver.com            cinder-rbd.csi.windriver.com
Cinder v3 attachment record               Cinder v3 attachment record        ← same
metadata csi.attachment_id                metadata csi.rbd.attachment_id     ← same shape
on-demand record creation                 on-demand record creation          ← same
404 ⇒ replace + retry once                404 ⇒ replace + retry once         ← same
delete record on unpublish                delete record on unpublish         ← same
retain volume by default                  retain volume by default           ← same
block-only, SINGLE_NODE_WRITER            block-only, SINGLE_NODE_WRITER     ← same
CDI StorageProfile required               CDI StorageProfile required        ← same
─────────────────────────────────────────────────────────────────────────────
iscsiadm discovery/login/logout            rbd device map --exclusive / unmap
portal + IQN + LUN                         Ceph FSID + pool + image
/dev/disk/by-path/ip-…-lun-N               /dev/rbdN (number is NOT identity)
optional CHAP from connection_info         mandatory Ceph key from operator Secret
per-initiator target = exclusivity         Ceph exclusive-lock = exclusivity
iscsiadm node DB is node state             kernel /sys/bus/rbd is node state
node ID hostname;iqn;ip                    node ID hostname (bare)            [V]
devicepath file = staging truth            intent = ownership; kernel = identity
open-iscsi host package                    bundled Ceph 18.2.x rbd CLI
/etc/iscsi, /var/lib/iscsi, /run/lock      /dev, /sys (rw), private tmpfs runtime dir
hostPID: true                              hostPID not required
```

---

## Appendix B: File-level reuse decision matrix

| Baseline file (`pkg/csi/cinder-iscsi/…`) | Reuse | Strategy for `pkg/csi/cinder-rbd/` |
|---|:--:|---|
| `server.go` | 📋 | copy; change package name only |
| `utils.go` | 📋 | copy; retarget `New*Server` types; keep the revive pragma |
| `driver.go` | 📝 | rewrite: name, capabilities, `SetupNodeService` wires `RBDMapper` + credentials + reconciler |
| `identityserver.go` | 📝 | adapt: node readiness probes the `rbd` CLI and credential presence |
| `controllerserver.go` | 📝 | adapt: fail-closed attachment state machine; RBD validation; retain-only deletion |
| `nodeserver.go` | 🆕 | rewrite over `RBDMapper`; intent-owned live-map idempotency; five-check gate |
| `connectioninfo.go` | 📝 | adapt: RBD key set, node-ID helpers, normalized-struct validation |
| `iscsi.go` | ❌ | not applicable; replaced by `rbd.go` + `rbd_cli.go` + `rbd_sysfs.go` |
| `iscsi_mock.go` | 📋 | pattern only; new `rbd_mock.go` |
| `openstack/openstack.go` | 📝 | adapt: `IOpenStackRBD`, `RBDOpts`, trimmed microversions, centralized defaults |
| `openstack/openstack_volumes.go` | 📋 | copy; rename receiver |
| `openstack/openstack_attachments.go` | 📋 | copy request plumbing incl. `reservedAttachmentCreateOpts`; add `ListAttachmentsByVolume`; replace the connection parser |
| `openstack/openstack_mock.go` | 📋 | copy pattern for the new interface |
| `controllerserver_test.go` | 📋 | copy structure; RBD assertions; add recovery cases |
| `nodeserver_test.go` | 🆕 | new; keep the `t.TempDir()` discipline and injection-point pattern |
| — | 🆕 | `rbd.go`, `rbd_cli.go`, `rbd_sysfs.go`, `credentials.go`, `staging.go`, `reconcile.go` and their tests |
| `pkg/util/mount` | ✅ | import — bind mount, `MakeFile`, `GetDeviceStats` |
| `pkg/util/errors` | ✅ | import — `IsNotFound` |
| `pkg/util` | ✅ | import — `RoundUpSize` |
| `pkg/client` | ✅ | import — OpenStack auth |
| `pkg/metrics` | ✅ | import — metric contexts |
| `pkg/version` | ✅ | import |
| `pkg/csi/csi.go` | ✅ | import |
| `Makefile:100-101` sanity target pattern | ❌ | do not copy a target without a backing suite |

Legend: ✅ import · 📋 copy · 📝 rewrite/adapt · 🆕 new · ❌ not used

---

## Appendix C: Full `driver.conf` reference

```ini
# ConfigMap: cinder-rbd-driver-config, key driver.conf
# Passed to the plugin as an additional --cloud-config file.
# Authentication lives separately in cloud.conf ([Global], from a Secret).

[RBD]
# Node mapping method. Only "krbd" is accepted in this release; any other
# value fails startup.
mounter = krbd

# Mandatory exclusive mapping. "false" is rejected at startup AND at chart
# render time — a writable non-exclusive map is not a supported configuration.
exclusive = true

# Ceph cluster identity. expected-fsid is environment-specific and is an
# identifier, not a credential. It is REQUIRED in production: leaving it empty
# disables identity check #1 of the pre-publish gate.
expected-cluster-name = ceph
expected-fsid = c5f7876d-258c-4152-b26a-a3ab532fda28

# Bundled Ceph client major version; checked against `rbd --version` at startup.
ceph-client-version-major = 18

# Directory where the operator-managed Ceph credential Secret is projected.
# Expected files: userID, userKey (mode 0400, readOnly).
credential-path = /etc/cinder-rbd-csi/ceph

# Private, memory-backed directory for generated ceph.conf and keyring files.
runtime-dir = /run/cinder-rbd-csi

# Durable node-scoped staging index; must survive plugin restarts.
state-dir = /var/lib/cinder-rbd-csi

# Bounded map/unmap/device-appearance operations.
map-timeout = 120s
unmap-timeout = 120s
device-wait-timeout = 60s

# 0 = unlimited until the platform mapping limit is measured (Phase 6).
max-volumes-per-node = 0


[Volume]
# Seconds to wait for a new volume to reach "available".
create-timeout = 300

# Seconds to wait for a volume to leave in-use/reserved during unpublish/delete.
detach-timeout = 120

# Cinder volume type used when the StorageClass omits "type".
# The active WRCP type is ceph-rook-store; it is never hard-coded in Go.
default-volume-type = ceph-rook-store

# Prefix for all driver-owned Cinder volume metadata keys:
#   <prefix>.attachment_id, <prefix>.cleanupVolume
metadata-prefix = csi.rbd

# "retain" keeps the Cinder volume after PVC deletion so the Wind River
# migration Blueprint can attach it to the target VM. It is the only accepted
# initial value. "delete" and <prefix>.cleanupVolume=true fail closed until Q8
# defines a cross-node proof that no krbd map remains.
delete-volume-mode = retain
```

---

## Appendix D: Invariants — the "never" list

A reviewer should be able to reject a change by pointing at one line here.

1. **Never** map writable without `--exclusive`, and never retry a failed
   exclusive map without it.
2. **Never** unmap or bind-mount a device based on a recorded `/dev/rbdN` number
   without a live identity verification.
3. **Never** adopt or unmap a device without a valid node-scoped ownership
   intent — platform Ceph-CSI shares the same kernel path on the same nodes.
4. **Never** treat a missing staging record as proof that no mapping exists;
   it proves only that this driver has no recorded authority over a live map.
5. **Never** put a Ceph key, keyring, or Secret content into `publish_context`,
   volume metadata, annotations, logs, metrics, or an error message.
6. **Never** derive a Ceph key from `secret_uuid`, read it from a running Cinder
   pod, or copy a host keyring at runtime.
7. **Never** map with an `--id` that differs from the keyring entity, or from
   `connection_info.auth_username`.
8. **Never** add a `volume-` prefix to the image name, assume a pool name, or
   split `name` on anything but the **first** `/`.
9. **Never** let a nested `data` object override a validated top-level field, and
   never accept a response where the two conflict.
10. **Never** use the attachment ID from the PV's immutable volume context after
    an unpublish — Cinder volume metadata is the only current source.
11. **Never** hard-code the Cinder volume type; read it from the StorageClass.
12. **Never** accept a Filesystem volume capability or an access mode other than
    `SINGLE_NODE_WRITER`.
13. **Never** physically delete the Cinder volume until Q8 provides a race-safe
    cross-node proof that no krbd mapping remains; retain is the initial
    migration contract.
14. **Never** call Cinder from the node plugin, and never delete a Cinder
    attachment record from `NodeUnstageVolume`.
15. **Never** return `OK` to conceal a partial side effect; return a retryable
    code and leave durable state intact.
16. **Never** assign a concrete `*controllerServer` or `*nodeServer` to a
    `csi.ControllerServer`/`csi.NodeServer` interface without a nil check — a
    typed nil is a non-nil interface and registers a service that segfaults on
    first call.
17. **Never** describe anything in `pkg/csi/cinder-rbd/` as implemented or
    production-qualified until Phase 6 sign-off.

---

## References

- Proposal: [Cinder RBD CSI Plugin for WRCP Migration](rbd-backed-cinder-volume-for-wrcp-migration.md)
- Implemented sibling: [iSCSI-Cinder CSI Implementation Design](iscsi-cinder-csi-implementation-design.md)
- [NFS-Cinder CSI Implementation Design](nfs-cinder-csi-implementation-design.md)
- [Kubernetes CSI Architecture Reference](kubernetes-csi-architecture-reference.md)
- Cinder attachments API: https://docs.openstack.org/api-ref/block-storage/v3/#attachments
- Ceph `rbd` command reference: https://docs.ceph.com/en/reef/man/8/rbd/
- Ceph exclusive locks: https://docs.ceph.com/en/reef/rbd/rbd-exclusive-locks/
- Ceph user management: https://docs.ceph.com/en/latest/rados/operations/user-management/
- CVE-2020-10755 — keyring removed from RBD `connection_info`
