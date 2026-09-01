# RBD-Backed Cinder Volume for WRCP Migration — CSI Plugin Design

**Status:** Revised proposal (v2, post-review)
**Driver name:** `cinder-rbd.csi.windriver.com`
**Author:** Wind River Migration Framework Team
**Date:** 2026-08-31 (v1), revised 2026-09-01 (v2)

**Target baseline:** WRCP 24.09 and later (Kubernetes 1.29.2, Linux 6.6,
Rook Ceph 18.2.x)
**Node mounter:** Kernel RBD (`krbd`) — `rbd-nbd` is NOT part of the
initial design
**Volume mode:** Raw block only, `SINGLE_NODE_WRITER`
**Cinder API minimum:** microversion **3.44**

Sibling design to:
- `iscsi-backed-cinder-volume-for-wrcp-migration.md` (implemented on
  `wndrvr/cinder-iscsi-csi-plugin-impl`)
- `nfs-backed-cinder-volume-for-wrcp-migration.md`

> **Revision note (v2):** the initial proposal chose `rbd-nbd` as the
> node data path and mirrored the cinder-iscsi attachment lifecycle
> (delete attachment on every ControllerUnpublishVolume). Both were
> revised after environment validation on WRCP 24.09; see §14 for the
> full list of changes and rationale.

---

## 1. Executive decision

The plugin is a **Cinder-controlled, kernel-RBD CSI driver** that uses:

1. **Cinder attachments as the control-plane ownership reservation.**
2. **Kernel RBD (`krbd`) as the node data path.**
3. **Ceph exclusive locking during writable mappings.**
4. **A persistent Cinder attachment for the complete migration PVC
   lifetime, including gaps between CDI precopy pods.**

This matches the validated WRCP 24.09 environment, where the platform's
Ceph-CSI already maps RBD volumes through the kernel and active devices
appear as `/dev/rbdN`. The design does not depend on a host `rbd-nbd`
executable, an NBD kernel-device pool, or a long-running userspace
mapping daemon.

The initial implementation uses the Ceph identity returned by Cinder —
normally `client.cinder` — with a Kubernetes Secret containing the
matching key. A dedicated identity may be introduced later only under
the conditions in §6.

## 2. Problem statement

The migration workflow (V2O via KubeVirt CDI, O2O via the NBD pipeline)
provisions a target Cinder volume as a Kubernetes raw-block PVC.
Temporary NBD receiver pods write migration data to that PVC. CDI may
create and remove multiple pods across precopy and cutover phases.

The plugin therefore needs two independent lifecycles:

- **Control-plane lifecycle:** the target Cinder volume remains
  reserved for the migration from CSI `CreateVolume` until
  `DeleteVolume`.
- **Node data-path lifecycle:** a node maps the Ceph RBD image only
  while a migration pod stages and uses the volume.

Deleting the Cinder attachment during every `ControllerUnpublishVolume`
(as the cinder-iscsi plugin does) would incorrectly release the
control-plane reservation between CDI phases, creating an unsafe
`available` window in which Cinder/Nova consumers could claim the
volume. Conversely, retaining a kernel RBD mapping after
`NodeUnstageVolume` would violate CSI node lifecycle expectations.

The design keeps the Cinder attachment persistent while mapping and
unmapping the RBD image normally at the node. This is the NFS migration
driver's persistent-reservation invariant, without the Nova shadow VM:
the Cinder attachments API directly provides the RBD connection
information.

**Why the iSCSI plugin's semantics don't transfer:** iSCSI targets are
per-initiator, so the attachment must be deleted to terminate the
target. RBD connection information is node-agnostic (monitors +
pool/image), so a single reservation can span pod movements between
nodes.

## 3. Validated WRCP 24.09 baseline

The following facts were confirmed in the existing WRCP/WRO environment
and override the assumptions in the initial (v1) proposal:

| Area | Validated result | Design consequence |
|---|---|---|
| Platform | WRCP 24.09, Kubernetes 1.29.2, Linux 6.6 | WRCP 24.09 is the minimum baseline |
| Ceph topology | WRCP Kubernetes and WRO Cinder use the same Rook-managed Ceph cluster | Cinder connection info identifies a locally reachable image |
| Ceph version | Rook Ceph 18.2.2; Ceph-CSI container tools 18.2.1 | Bundle a qualified Ceph 18.2.x `rbd` client in the plugin image |
| Existing K8s path | Ceph-CSI uses `krbd`; active devices are `/dev/rbdN` | Use `krbd` as the default mounter |
| Host tooling | Host `rbd` is Ceph 14.2.22; host `rbd-nbd` is absent | Do not depend on host Ceph user-space tools |
| NBD resources | Kernel NBD module exposes a finite 16-device pool | Avoid consuming NBD devices for the storage mapping |
| Ceph identities | Cinder uses `client.cinder`; Ceph-CSI uses separate CSI identities | The node key must match Cinder's returned `auth_username` |
| Cinder backend | Active volume type is `ceph-rook-store` | Do not hard-code an inactive `rbd1` type |
| Cinder response | Attachment update returns **flat** RBD connection fields | Parse the flat schema; tolerate nested `data` only for compatibility |
| Image naming | `name` is `cinder-volumes/<cinder-volume-uuid>` — no `volume-` prefix | Use Cinder's exact `name`; split only the first `/` |

## 4. Goals and non-goals

**Goals**
- Provision or adopt a Cinder RBD-backed volume as a Kubernetes
  raw-block PV.
- Keep the Cinder volume reserved throughout a multi-stage migration.
- Map the exact Cinder-selected RBD image on a WRCP worker using krbd.
- Preserve CSI idempotency across retries, pod replacement, plugin
  restart, and partial failures.
- Enforce one writable node mapping while the volume is actively staged.
- Retain the Cinder volume by default after the migration PVC is
  deleted (Blueprint handoff).
- Avoid Nova, a shadow VM, host `rbd-nbd`, and dynamically copied Ceph
  credentials.

**Non-goals**
- General-purpose replacement for the upstream Cinder CSI plugin.
- Filesystem-mode PVCs, RWX, snapshots, clones, online expansion,
  topology-aware provisioning (see §12).
- Cross-Ceph-cluster replication.
- Automatic support for RBD image features the WRCP kernel cannot map.
- Permanent hard fencing against privileged actors that bypass Cinder
  with direct Ceph credentials (see §13).

## 5. Architecture

```text
                         WRCP Kubernetes

  CDI / migration controller
             |
             | PVC, ControllerPublish, NodeStage
             v
  +-----------------------------+
  | cinder-rbd CSI controller   |
  |                             |
  | - Cinder volume lifecycle   |
  | - persistent attachment     |
  | - attachment reconciliation |
  +-------------+---------------+
                |
                | Cinder v3 API, microversion >= 3.44
                v
  +-----------------------------+
  | WRO Cinder                  |
  | RBD volume driver           |
  +-------------+---------------+
                |
                | connection_info:
                | cluster, monitors, pool/image,
                | auth_username, secret UUID
                v
  +-----------------------------+
  | Shared Rook Ceph cluster    |
  | pool: cinder-volumes        |
  +-------------+---------------+
                ^
                | krbd map --exclusive
                |
  +-------------+---------------+
  | cinder-rbd CSI node         |
  |                             |
  | bundled Ceph 18.2.x `rbd`   |
  | host kernel RBD module      |
  +-------------+---------------+
                |
                | /dev/rbdN exposed as CSI block device
                v
       NBD receiver / CDI importer pod
```

### Separation of control and data planes

**Control plane:** the Cinder attachment is a persistent ownership
reservation. It starts during `CreateVolume` and ends during
`DeleteVolume`. `ControllerUnpublishVolume` does not delete it.

**Data plane:** the node plugin maps the RBD image with krbd during
`NodeStageVolume` and unmaps it during `NodeUnstageVolume`.
`NodePublishVolume`/`NodeUnpublishVolume` only create and remove the
raw-block bind target required by kubelet.

### Connection information contract

The validated attachment-update response is **flat**:

```json
{
  "driver_volume_type": "rbd",
  "cluster_name": "ceph",
  "name": "cinder-volumes/<cinder-volume-uuid>",
  "auth_enabled": true,
  "auth_username": "cinder",
  "secret_type": "ceph",
  "secret_uuid": "<ceph-fsid>",
  "volume_id": "<cinder-volume-uuid>",
  "hosts": ["10.107.190.121", "10.106.210.60", "10.98.180.79"],
  "ports": ["6789", "6789", "6789"]
}
```

The plugin must:
- Treat `name` as authoritative and split only the first `/` into pool
  and image. Never add a `volume-` prefix or assume the pool name.
- Validate `driver_volume_type == "rbd"`.
- Pair `hosts[n]` with `ports[n]`.
- Treat `secret_uuid` as an identifier (it equals the Ceph FSID here),
  never as a key.
- Accept a nested `data` object only as a backward-compatible alternate
  representation.
- **Persist the normalized connection data** (volume metadata +
  controller cache) because later attachment GET responses may omit it.
- Never log a Ceph key or generated keyring.

## 6. Ceph authentication model

### Initial implementation

A namespace-scoped Kubernetes Secret contains the key for the Ceph
identity returned by Cinder:

```text
auth_username  = cinder
keyring entity = client.cinder
```

The Secret is created and rotated by the platform operator. The plugin
must not retrieve credentials from a Cinder pod, copy host keyrings, or
infer the key from `secret_uuid`. The controller validates that the
configured Secret identity matches the returned `auth_username`; a
mismatch is a hard failure.

**Why not a dedicated least-privilege identity initially:** the krbd
map `--id` and the keyring must belong to the *same* identity.
Supplying a `client.wrcp-csi` key while mapping as `cinder` fails
authentication.

### Future dedicated identity

`client.wrcp-cinder-rbd-csi` is acceptable only when all hold:
- explicit configuration overrides the connection username,
- the supplied key belongs to that exact identity,
- the identity has the required capabilities on the Cinder pool,
- exclusive-lock and blocklist recovery have been tested,
- the boundary is documented and accepted by the Ceph operator.

## 7. Kernel RBD node data path

### Why krbd

- It is already the active WRCP Ceph-CSI data path (validated).
- It creates a kernel-owned `/dev/rbdN` device with **no userspace
  daemon** — mappings inherently survive plugin pod restarts, removing
  the entire daemon-lifecycle problem class.
- It does not consume the finite `/dev/nbdN` pool.
- The host has no `rbd-nbd` binary; the host's Ceph 14 CLI must not be
  used. The plugin image bundles a qualified Ceph 18.2.x `rbd` CLI; the
  mapping itself is implemented by the host kernel module.

`rbd-nbd` remains a future, explicitly configured fallback for an image
feature krbd cannot map. It must never be selected automatically.

### Map operation

The node plugin writes a temporary volume-specific `ceph.conf` (monitor
addresses from connection info) and a keyring from the Secret, then:

```bash
rbd device map \
  --device-type krbd \
  --exclusive \
  --cluster ceph \
  --id cinder \
  --conf    /run/cinder-rbd-csi/<volume-id>/ceph.conf \
  --keyring /run/cinder-rbd-csi/<volume-id>/keyring \
  cinder-volumes/<image-name>
```

`--exclusive` requires the image's `exclusive-lock` feature (default
for Cinder-created images on Ceph 18); lock contention behavior is a
Phase 0 gate.

The implementation must not rely on the `/dev/rbdN` number remaining
constant. It records the returned path but reconciles ownership through
`rbd device list --format json` and sysfs identity before reusing or
unmapping a device.

Before publishing a writable device it verifies:
- mapped cluster/FSID matches the configured cluster,
- mapped pool and image exactly match Cinder's `name`,
- the mapping is exclusive and no conflicting writable client holds the
  lock,
- the block device is present with the expected size.

If exclusive mapping or lock acquisition fails, `NodeStageVolume`
fails. No silent fallback to non-exclusive mapping.

## 8. CSI lifecycle

### CreateVolume
1. Resolve the Cinder volume type from the StorageClass (the active
   WRCP type is `ceph-rook-store`; never hard-code).
2. Create the Cinder volume or reconcile an existing operation using
   the CSI request identity.
3. Wait until the volume is usable.
4. Create a **persistent attachment reservation** without a connector,
   using a **stable migration-owner UUID** as the attachment's
   `instance_uuid` (no Nova instance exists — Phase 0 gate).
5. Record driver ownership metadata as a recovery hint (§9).
6. Return the Cinder volume UUID as the CSI volume ID.

### ControllerPublishVolume
Under a per-volume process-safe lock:
1. **List** Cinder attachments for the volume (the list is the source
   of truth, not metadata).
2. Reconcile the attachment owned by this driver/migration owner;
   create it if `CreateVolume` failed before reservation completed.
3. Reject a conflicting non-driver attachment (operator resolution
   required).
4. Update the owned attachment with the node connector **only if
   connection information has not yet been obtained**.
5. **Require successful attachment completion** (microversion ≥ 3.44).
   `CompleteAttachment` is not best-effort; a deployment that cannot
   support 3.44 is not production-qualified.
6. Normalize, persist, and return non-secret connection fields in
   `publish_context`.

For subsequent CDI phases, return the existing normalized connection
information. **Do not create one attachment per pod.** (RBD connection
info is node-agnostic, so a connector recorded from an earlier node
remains functionally valid.)

### ControllerUnpublishVolume
**Return success without deleting the persistent attachment.** CDI may
delete one migration pod and create another during precopy/cutover;
releasing the attachment here would open an unsafe `available` window.

### NodeStageVolume
1. Validate block mode and `SINGLE_NODE_WRITER`.
2. Load and validate `publish_context`.
3. Reconcile any existing mapping for the same cluster/pool/image
   (idempotency by live-map identity, not by staging file).
4. If absent, map with `krbd --exclusive` (§7).
5. Verify mapping identity and size.
6. Atomically write the staging record: volume ID, FSID, pool, image,
   device path, map generation.

### NodePublishVolume / NodeUnpublishVolume
Standard raw-block target exposure/removal; verify the target resolves
to the staged mapping. No Cinder interaction.

### NodeUnstageVolume
1. Verify no remaining publish targets reference the staged device.
2. Reconcile the current map by cluster/pool/image; confirm the device
   still belongs to this volume (a recycled `/dev/rbdN` number must
   never be unmapped without identity verification).
3. Unmap; confirm absence; remove the staging record.

A missing staging file is **not** proof that a mapping is absent —
always reconcile live kernel maps.

### DeleteVolume
Under the per-volume lock:
1. Confirm no node mapping remains, or return a retryable error.
2. List and reconcile all attachments; delete **only** driver-owned
   ones.
3. Wait until the volume leaves `in-use`/`reserved`.
4. Default: **retain** the volume, removing only CSI ownership
   metadata (Blueprint boots the target VM from it). Delete the volume
   only when the explicit cleanup policy is enabled (per-volume
   `csi.cleanupVolume` override or driver-level mode, as in the iSCSI
   plugin).

### Controller state machine

```text
NEW
 | Create Cinder volume
 v
VOLUME_AVAILABLE
 | Create persistent attachment reservation
 v
RESERVED
 | Attachment update + complete, capture connection_info
 v
ATTACHED_CONTROL_PLANE  <── ControllerUnpublish: no state change
 |                          NodeStage/Unstage: data path only
 | DeleteVolume
 v
RELEASING ──> RETAINED_VOLUME (default)
        └───> DELETED_VOLUME (explicit cleanup only)
```

## 9. Attachment ownership and reconciliation

Volume metadata is a **hint, not the source of truth**:

```text
csi.rbd.driver        = cinder-rbd.csi.windriver.com
csi.rbd.owner_uuid    = <stable-migration-owner-uuid>
csi.rbd.attachment_id = <last-known-attachment-uuid>
csi.rbd.request_id    = <create-volume-idempotency-key>
```

For every mutating controller RPC the plugin: acquires the per-volume
lock → lists the actual attachment collection → classifies attachments
(owned / conflicting / stale / unrelated) → uses metadata only to
accelerate matching → repairs missing metadata → removes duplicate
owned attachments only when no node mapping depends on them → never
deletes an unrelated attachment automatically.

The controller runs **startup reconciliation** for volumes carrying
this driver's ownership metadata.

## 10. Node state and crash recovery

The node plugin keeps a durable staging record under the CSI plugin
directory; it aids recovery but never overrides live kernel and Ceph
evidence.

**Startup reconciliation:** for each driver-owned staging record,
compare with `rbd device list --format json`; verify pool/image via
sysfs; recreate a missing target link when the map is valid; mark an
absent mapping unstaged; **quarantine** (never blindly unmap) a device
whose identity conflicts with the record. Unrecorded but matching live
maps are adopted only after Cinder attachment and ownership validation.

| Failure | Required behavior |
|---|---|
| Volume created, reservation not created | Retry `CreateVolume`; find by request identity, create reservation |
| Reservation created, metadata write failed | List attachments, reconstruct metadata |
| Attachment update ok, RPC response lost | Find existing attachment, recover normalized connection data |
| Map ok, staging record write failed | Detect map by pool/image, adopt after ownership validation |
| Plugin restart with live map | Reconcile and reuse the same kernel mapping |
| Node crash | Reconcile kernel map, staging records, Cinder attachment |
| Exclusive map denied | Fail `NodeStageVolume`; never fall back to non-exclusive |
| Conflicting Cinder attachment | Fail safely; operator resolution |
| Unmap timeout | Keep staging state, return retryable failure |
| Cinder API down during unstage | Unmap may proceed; persistent attachment reconciled later |

## 11. Security, deployment, configuration

**Node DaemonSet requires:** privileged container (or equivalently
validated capability/device policy), host `/dev`, required `/sys`
access for RBD identity reconciliation, kubelet plugin/pod-volume
paths, a writable private runtime directory for generated Ceph config
and staging state, and Secret access limited to the driver service
account.

**Not required** (unlike the v1 rbd-nbd design): host PID namespace for
a mapper daemon, host `/usr/bin/rbd*`, host `/etc/ceph` credentials,
`/dev/nbd*`, NBD module configuration.

Generated keyrings use restrictive permissions and are removed when no
longer needed. Credentials never appear in `publish_context`, logs,
annotations, or volume metadata.

**Configuration example:**

```ini
[Global]
cloud-config=/etc/kubernetes/cloud.conf
driver-name=cinder-rbd.csi.windriver.com
cinder-min-microversion=3.44
retain-volume=true

[RBD]
mounter=krbd
exclusive=true
expected-cluster-name=ceph
expected-fsid=<per-deployment Ceph FSID>
ceph-client-version-major=18
credential-secret-name=cinder-rbd-ceph-client
credential-secret-namespace=kube-system
map-timeout=120s
unmap-timeout=120s
```

The FSID is an environment-specific identifier (not a credential),
configured per deployment.

**CDI StorageProfile:** as with the iSCSI plugin, CDI must be patched
per StorageClass (`claimPropertySets: [{accessModes: [ReadWriteOnce],
volumeMode: Block}]`) or DataVolume PVCs default to Filesystem and hang
Pending.

## 12. Supported capabilities

Initial: `CREATE_DELETE_VOLUME`, `PUBLISH_UNPUBLISH_VOLUME`,
`SINGLE_NODE_WRITER`, raw block mode. Expansion only if separately
validated.

Not initially advertised: filesystem volumes, snapshots, clones,
multi-node writer/reader, online expansion, topology-aware provisioning.

`NodeGetInfo.MaxVolumesPerNode` reflects platform constraints; NBD
device limits are irrelevant under krbd.

**Observability:** logs/metrics use non-secret identifiers only (CSI
volume ID, Cinder volume/attachment UUIDs, owner UUID, node ID, FSID,
pool/image, device path, state). Recommended metrics: Cinder API
latency/failures, reconciliation outcomes, duplicate/conflicting
attachments, map/unmap latency, exclusive-lock failures, orphaned
maps/records, volumes retained vs deleted.

## 13. Residual fencing limitation

The persistent Cinder attachment fences all consumers that obey Cinder.
The krbd exclusive lock fences second writable Ceph clients **while a
node mapping is active**. Between CDI stages, `NodeUnstageVolume`
removes the mapping, so no Ceph-level lock remains — a privileged actor
with direct Ceph credentials could bypass Cinder during that gap. This
is an explicit trust-boundary limitation; an uninterrupted Ceph-level
lock across inter-pod gaps would require a durable lock owner or
persistent mapping service and must not be hidden inside the CSI node
lifecycle.

## 14. Changes from the initial (v1) proposal

| v1 design area | v2 revision | Reason |
|---|---|---|
| `rbd-nbd` as data path | Kernel RBD (`krbd`) default; rbd-nbd future opt-in only | Host has no `rbd-nbd`; Ceph-CSI already uses krbd; removes daemon lifecycle entirely |
| Host tooling assumed | Bundle qualified Ceph 18.2.x `rbd` CLI in image | Host CLI is Ceph 14 |
| `nsenter` host-namespace daemon spawn | Removed | No daemon exists under krbd |
| NBD module init container, device pool concerns | Removed | Not applicable to krbd |
| Attachment deleted on every unpublish (iSCSI semantics) | One persistent attachment until `DeleteVolume` | Prevents unsafe `available` window between CDI phases; RBD info is node-agnostic |
| `CompleteAttachment` best-effort (mv 3.44 optional) | Required; minimum microversion 3.44 | Persistent reservation must reach `in-use`; validated environment supports it |
| Nested `data` connection_info assumed | Flat schema is authoritative; nested tolerated for compat | Validated response is flat |
| Image `volumes/volume-<uuid>` | Exact `name`, validated form `cinder-volumes/<uuid>` | Validated; never synthesize prefixes |
| Dedicated `client.wrcp-csi` key | `client.cinder` key initially; dedicated identity gated (§6) | map `--id` and key must be the same identity |
| Metadata `attachment_id` as source of truth | Attachment list under per-volume lock; metadata as hint | Long-lived attachments need reconciliation |
| Fencing via CSI access mode only | krbd `--exclusive` + lock-contention testing | Real single-writer enforcement |
| Missing staging file = unmapped | Reconcile live kernel maps by FSID/pool/image | Staging records can be lost while maps persist |

## 15. Phase 0 qualification gates

### Already validated
WRCP 24.09 krbd data path; active type `ceph-rook-store`;
`driver_volume_type=rbd`; flat connection info; image name
`<pool>/<uuid>` without prefix; `auth_username=cinder`; `secret_uuid` =
Ceph FSID; no host `rbd-nbd`; Ceph 18.2.x tools available in Ceph-CSI
image; shared Ceph cluster between WRCP and Cinder.

### Must pass before production
1. Build a minimal node image with the qualified Ceph 18.2.x `rbd` CLI.
2. Map a Cinder image using the exact returned connection fields and
   the operator-provided `client.cinder` Secret.
3. Write, read, flush, unmap, remap, verify integrity.
4. Confirm `--exclusive` blocks a second writable client.
5. Recovery after node-plugin restart with a live map.
6. Recovery after node reboot.
7. Attachment creation without a real Nova server using the stable
   migration-owner UUID.
8. Attachment update + completion with microversion ≥ 3.44.
9. Persistent attachment survives multiple CDI publish/unpublish cycles.
10. Fault injection after each external side effect; verify idempotent
    recovery.
11. Deliberate duplicate owned attachments; safe reconciliation.
12. Credential rotation and insufficient-capability failures.
13. No Secret material in logs, CSI responses, or metadata.
14. Full precopy + cutover workflow with pod movement between nodes.

**Gates 2, 4, 7, 8, 9 are blocking.**

## 16. Implementation plan

1. **Controller foundation** — reuse the cinder-iscsi Cinder client and
   CSI scaffolding; strict RBD connection normalization; require
   microversion 3.44; per-volume locking; attachment-list
   reconciliation; persistent reservation semantics.
2. **Kernel RBD node path** — node image with Ceph 18.2.x tools;
   generated `ceph.conf` + protected keyring handling; exclusive
   mapping; live-map/sysfs reconciliation; raw-block stage/publish.
3. **Recovery and lifecycle** — durable staging records; controller and
   node startup reconciliation; fault injection around every side
   effect; retain-by-default deletion.
4. **Qualification** — all Phase 0 gates; multi-stage CDI migration
   tests; restart/reboot/reschedule/Cinder-outage/mon-disruption tests;
   operator runbook for conflicting attachments and quarantined maps.

Package layout mirrors the cinder-iscsi plugin (`pkg/csi/cinder-rbd/`,
`cmd/cinder-rbd-csi-plugin/`, chart, manifests, examples, release
workflow) with the node layer replaced by an `RBDMapper` abstraction
(`Map`/`Unmap`/`ListMapped`/`VerifyIdentity`) over the bundled `rbd`
CLI.

## 17. References

- Initial (v1) proposal: this file's git history on
  `wndrvr/cinder-rbd-csi-plugin-design`
- iSCSI sibling: `pkg/csi/cinder-iscsi/` on
  `wndrvr/cinder-iscsi-csi-plugin-impl`; skill
  `.github/skills/cinder-iscsi-csi/`
- NFS sibling: `nfs-backed-cinder-volume-for-wrcp-migration.md`; skill
  `.github/skills/nfs-cinder-csi/`
- Cinder attachments API:
  https://docs.openstack.org/api-ref/block-storage/v3/#attachments
- Ceph RBD command reference: https://docs.ceph.com/en/reef/man/8/rbd/
- Ceph exclusive locks:
  https://docs.ceph.com/en/reef/rbd/rbd-exclusive-locks/
- Ceph user management:
  https://docs.ceph.com/en/latest/rados/operations/user-management/
- Ceph-CSI RBD-NBD design (prior art for why krbd was preferred):
  https://github.com/ceph/ceph-csi/blob/devel/docs/design/proposals/rbd-nbd.md
- CVE-2020-10755 — keyring removed from RBD connection_info
