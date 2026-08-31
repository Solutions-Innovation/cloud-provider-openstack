# RBD-Backed Cinder Volume for WRCP Migration — CSI Plugin Proposal

**Status:** Proposal (pre-implementation)
**Driver name:** `cinder-rbd.csi.windriver.com`
**Author:** Wind River Migration Framework Team
**Date:** 2026-08-31

Sibling proposal to:
- `iscsi-backed-cinder-volume-for-wrcp-migration.md` (implemented on
  `wndrvr/cinder-iscsi-csi-plugin-impl`)
- `nfs-backed-cinder-volume-for-wrcp-migration.md`

---

## 1. Motivation

The migration framework (V2O via KubeVirt CDI, O2O via the NBD pipeline)
needs raw block access to Cinder volumes from WRCP worker hosts. The
existing `cinder-iscsi-csi-plugin` solves this for iSCSI-capable Cinder
backends (LVM, SolidFire, …). Many target clouds, however, back Cinder
with **Ceph RBD**, whose Cinder driver does not expose iSCSI targets —
`initialize_connection()` returns RBD-native connection info intended
for a Ceph client, not an iSCSI initiator.

This proposal adds a third migration CSI driver, `cinder-rbd`, that
reuses the validated cinder-iscsi architecture (Cinder v3 self-service
attachments, no Nova, no temporary/Shadow VM) and replaces the node-side
`iscsiadm` data path with `rbd-nbd`.

WRCP deployments host OpenStack on top of a platform-integrated Ceph
cluster: the monitors are directly reachable from worker hosts and the
Ceph client tooling (`ceph-common`, `rbd-nbd`) is already installed.
This makes the native RBD data path the natural fit.

## 2. Key feasibility question (validated by research, to be confirmed by Phase 0)

> Does the Cinder Ceph/RBD driver expose usable connection info through
> the attachments API **without attaching the volume to a temporary VM**?

**Answer: yes, with two caveats.** The Cinder v3 attachments API
(microversion >= 3.27) is backend-agnostic; `PUT /v3/attachments/{id}`
with a connector invokes the RBD driver's `initialize_connection()`
exactly as it does for LVM/iSCSI. This is the mechanism cinderlib and
Ember-CSI rely on for VM-less attachment. The returned payload is:

```json
{
  "driver_volume_type": "rbd",
  "data": {
    "name": "volumes/volume-<uuid>",
    "hosts": ["<mon1>", "<mon2>", "<mon3>"],
    "ports": ["6789", "6789", "6789"],
    "cluster_name": "ceph",
    "auth_enabled": true,
    "auth_username": "cinder",
    "secret_type": "ceph",
    "secret_uuid": "<libvirt-secret-uuid>",
    "volume_id": "<uuid>",
    "access_mode": "rw"
  }
}
```

**Caveat 1 — not iSCSI.** There is no portal/IQN/LUN. The node must
attach with a Ceph client (`rbd map` krbd or `rbd-nbd`). The existing
cinder-iscsi node service and its `driver_volume_type == "iscsi"`
validation cannot consume this; a new node data path is required.

**Caveat 2 — no credentials.** The cephx key is deliberately absent
from connection_info (CVE-2020-10755 removed the old `keyring` field).
`secret_uuid` references a libvirt secret that only exists on Nova
compute hosts — useless to us. The node plugin therefore needs a cephx
client keyring delivered out-of-band (Kubernetes Secret; see §6).

Phase 0 (§10) validates both caveats against a real WRCP cloud with
curl only, before any code is written.

## 3. Alternatives considered

| Approach | Verdict | Reason |
|---|---|---|
| **A. Native RBD sibling plugin (this proposal)** | **Selected** | WRCP hosts already run Ceph clients; no extra infrastructure; reuses 90% of the proven cinder-iscsi controller design |
| B. Cinder RBD-iSCSI driver (`cinder.volume.drivers.ceph.rbd_iscsi`) + ceph-iscsi gateways, reusing cinder-iscsi plugin unmodified | Rejected | Requires deploying and operating ceph-iscsi gateway infrastructure on the OpenStack side; ceph-iscsi is deprecated in recent Ceph releases; extra network hop on the data path |
| C. Generalize cinder-iscsi into a multi-protocol driver (node picks iscsiadm vs rbd by `driver_volume_type`) | Rejected (for now) | Keeps blast radius small and release cadence independent; the shared controller logic can be extracted into a common package later if a third backend appears |
| D. Upstream ceph-csi pointed directly at the Ceph cluster (bypassing Cinder) | Rejected | Volumes are Cinder-managed; migration handoff (retain → Blueprint boots target VM from the Cinder volume) requires the Cinder lifecycle |
| E. krbd (`rbd map`) instead of rbd-nbd on the node | Deferred | rbd-nbd chosen for full image-feature support independent of host kernel; krbd can be added later as a driver.conf option |

## 4. Architecture

```
                        ┌────────────────────────────────────────┐
                        │ Controller Deployment (kube-system)    │
 CreateVolume  ────────▶│  Cinder volume + reserved attachment   │──── Cinder v3 API
 ControllerPublish ────▶│  UpdateAttachmentConnector             │     (mv >= 3.27,
 ControllerUnpublish ──▶│  DeleteAttachment + clear metadata     │      no Nova)
 DeleteVolume  ────────▶│  retain (default) | delete             │
                        └────────────────────────────────────────┘
                                     │ publish_context:
                                     │ pool_image, mon_hosts,
                                     │ cluster_name, auth_username
                                     ▼
                        ┌────────────────────────────────────────┐
                        │ Node DaemonSet (per worker host)       │
 NodeStage    ─────────▶│  rbd-nbd map pool/image → /dev/nbdX    │──── Ceph mons
 NodePublish  ─────────▶│  bind-mount /dev/nbdX → volumeDevices  │     (direct, cephx
 NodeUnstage  ─────────▶│  rbd-nbd unmap                         │      key from Secret)
                        └────────────────────────────────────────┘
```

Identical to cinder-iscsi in every dimension except the node data path:
- Block-only (`volumeMode: Block`), `SINGLE_NODE_WRITER`, pods use
  `volumeDevices`.
- Controller capabilities: `CREATE_DELETE_VOLUME`, `PUBLISH_UNPUBLISH_VOLUME`.
- Node capabilities: `STAGE_UNSTAGE_VOLUME`, `GET_VOLUME_STATS`.
- `csi.attachment_id` in Cinder volume metadata is the single source of
  truth; on-demand attachment creation in ControllerPublishVolume with
  one 404 retry; ControllerUnpublishVolume deletes the attachment and
  clears metadata (keeps Cinder volume status truthful: `in-use` ↔
  `available`).
- DeleteVolume default `retain` (attachment deleted, CSI metadata
  stripped, volume left `available` for the migration Blueprint);
  per-volume `csi.cleanupVolume` override; `delete-volume-mode` config.
- Startup `DiscoverCinderCapabilities` hard-fails below microversion
  3.27; `CompleteAttachment` (3.44) best-effort.

## 5. Package structure

```
pkg/csi/cinder-rbd/                # package name: rbd
├── driver.go                      # cinder-rbd.csi.windriver.com, capabilities
├── controllerserver.go            # ~copied from cinder-iscsi; RBD connector + validation
├── nodeserver.go                  # NodeGetInfo/Stage/Unstage/Publish/Unpublish/Stats
├── identityserver.go              # mode-aware Probe (Cinder caps | rbd-nbd check)
├── rbdnbd.go                      # RBDConnector interface + rbd-nbd implementation
├── rbdnbd_mock.go                 # testify mock
├── connectioninfo.go              # publish-context keys, ParseNodeID (2-field), validation
├── server.go / utils.go           # copied
└── openstack/                     # copied from cinder-iscsi/openstack, RBD conn parsing
    ├── openstack.go               # IOpenStackRBD, RBDOpts/VolumeOpts
    ├── openstack_volumes.go
    ├── openstack_attachments.go   # parseRBDConnectionInfo instead of iSCSI
    └── openstack_mock.go

cmd/cinder-rbd-csi-plugin/main.go
charts/cinder-rbd-csi-plugin/
manifests/cinder-rbd-csi-plugin/
examples/cinder-rbd-csi-plugin/
.github/workflows/cinder-rbd-csi-release.yaml
```

## 6. Controller design deltas (vs cinder-iscsi)

### 6.1 Node ID and connector
Node ID simplifies to `hostname;ip` (no IQN — the host may not have
open-iscsi configured at all). The connector sent to
`UpdateAttachmentConnector`:

```json
{
  "host": "<hostname>",
  "ip": "<storage-network-ip>",
  "platform": "x86_64",
  "os_type": "linux2",
  "multipath": false,
  "do_local_attach": true
}
```

`do_local_attach: true` signals a bare-metal/local consumer (the
cinderlib convention). Phase 0 confirms the WRCP Cinder RBD driver
accepts this connector shape.

### 6.2 Connection info validation and publish context
`ValidateRBDConnectionInfo` requires `driver_volume_type == "rbd"`,
non-empty `name` (`pool/image`) and `hosts[]`. A non-RBD backend fails
publish with a clear error (fail-fast on misconfiguration).

PublishContext keys (consumed by NodeStageVolume):

| Key | Source | Example |
|---|---|---|
| `pool_image` | `data.name` | `volumes/volume-bf39da68-...` |
| `mon_hosts` | `data.hosts` + `data.ports`, joined | `10.0.0.1:6789,10.0.0.2:6789` |
| `cluster_name` | `data.cluster_name` | `ceph` |
| `auth_enabled` | `data.auth_enabled` | `true` |
| `auth_username` | `data.auth_username` | `cinder` |
| `driver_volume_type` | fixed | `rbd` |

**No credentials flow through the publish context** (improvement over
iSCSI's CHAP-in-publishContext): the cephx key lives only in a
node-side Secret.

## 7. Node design

### 7.1 RBDConnector interface (`rbdnbd.go`)

```go
type RBDConnector interface {
    Map(ctx context.Context, poolImage, monHosts, user, keyringPath, confPath string) (device string, err error)
    Unmap(ctx context.Context, device string) error
    FindMapped(ctx context.Context, poolImage string) (device string, found bool, err error)
    CheckRbdNbd(ctx context.Context) error   // probe: binary present + nbd module loaded
}
```

Concrete implementation shells out to `rbd-nbd` via `k8s.io/utils/exec`
(same pattern as `iscsiadmInitiator`); unit tests inject a testify mock.

### 7.2 NodeStageVolume
1. Validate; **block-only enforcement** (reject Mount capability).
2. Parse `pool_image`, `mon_hosts`, `auth_username` from publish context.
3. Idempotency: `FindMapped(pool/image)` (`rbd-nbd list-mapped`) — if
   mapped and the device node exists, rewrite `<staging>/devicepath`
   and return success.
4. Generate a minimal per-volume `ceph.conf` under the staging dir
   (mon hosts from publish context); keyring read from the mounted
   Secret path (`[RBD] keyring-path`, default `/etc/ceph-csi/keyring`).
5. `rbd-nbd map <pool/image> --id <user> --conf <conf> [extra args]`
   executed **in the host namespace** (see §7.4) → `/dev/nbdX`.
6. Wait for the device node (reuse `WaitForDevice` polling,
   `device-wait-timeout`). On failure: best-effort `Unmap` before
   returning (the Logout+DeleteNode pairing rule, adapted).
7. Persist device path to `<stagingTargetPath>/devicepath` (same file
   contract as cinder-iscsi).

### 7.3 Other node RPCs
- **NodeUnstage:** read devicepath (ENOENT → idempotent success) →
  `Unmap` → remove staging files.
- **NodePublish/Unpublish/GetVolumeStats:** byte-identical to
  cinder-iscsi (bind-mount to the `volumeDevices` target file, reuse
  the patched `pkg/util/mount.MakeFile` that replaces kubelet-created
  directories; block stats report Total only).
- **NodeGetInfo:** `hostname;ip`; IP from `[RBD] storage-interface`
  NIC or primary route (logic reused from iSCSI).
- **NodeExpandVolume:** Unimplemented (`rbd-nbd` picks up Cinder
  os-extend resizes; revisit if online resize is needed).

### 7.4 rbd-nbd daemon lifecycle (the one genuinely new risk)
Each mapping is backed by a long-lived userspace `rbd-nbd` process. If
that process runs inside the plugin container, **a DaemonSet pod
restart or upgrade kills every mapped volume on the node**, breaking
running migration pods with I/O errors (the well-known ceph-csi
failure mode).

Mitigation:
- **Spawn in the host namespace:** the node pod already runs
  `privileged: true` + `hostPID: true` (as cinder-iscsi does); execute
  `nsenter --target 1 --mount --net -- rbd-nbd map ...` so the daemon
  is parented to the host, surviving plugin restarts. `FindMapped` and
  `Unmap` run through the same nsenter wrapper.
- **Node prerequisites:** `nbd` kernel module (init container runs
  `modprobe nbd`), `rbd-nbd` binary on the host (already shipped by
  WRCP), staging-time existence check in Probe.
- **Recovery runbook:** if an rbd-nbd daemon dies (OOM/crash), the
  device returns EIO; recovery is pod reschedule → unstage/stage cycle
  remaps the image. Documented in the troubleshooting guide.

## 8. Configuration

### cloud.conf — unchanged (Keystone credentials, Secret-mounted).

### driver.conf — `[Volume]` unchanged; `[ISCSI]` replaced by `[RBD]`:

```ini
[RBD]
map-timeout         = 30                       ; seconds, rbd-nbd map
device-wait-timeout = 30                       ; seconds, wait for /dev/nbdX
keyring-path        = /etc/ceph-csi/keyring    ; cephx keyring (mounted Secret)
rbd-nbd-args        =                          ; optional, e.g. --try-netlink
storage-interface   =                          ; NIC for NodeGetInfo IP; empty = primary

[Volume]
create-timeout      = 300
detach-timeout      = 120
default-volume-type =
metadata-prefix     = csi
delete-volume-mode  = retain
```

### Ceph credentials Secret (`cinder-rbd-cephx`, kube-system)
A **dedicated least-privilege cephx client** (not the host's admin
key), e.g.:

```
ceph auth get-or-create client.wrcp-csi \
  mon 'profile rbd' osd 'profile rbd pool=volumes'
```

Stored as a Secret with a `keyring` key, mounted read-only into the
node DaemonSet at `/etc/ceph-csi/`. Key rotation = Secret update +
rolling restart of the DaemonSet (keys are read at map time, not
cached; existing mappings keep their session).

## 9. Deployment

Mirrors cinder-iscsi manifests/chart with these deltas:

| Item | cinder-iscsi | cinder-rbd |
|---|---|---|
| Node host mounts | `/etc/iscsi`, `/var/lib/iscsi`, `/run/lock/iscsi`, `/dev`, `/var/lib/kubelet` | `/dev`, `/var/lib/kubelet` + Secret `cinder-rbd-cephx` |
| Init container | — | `modprobe nbd` |
| Host prerequisites | open-iscsi, iscsid, initiatorname | ceph-common, rbd-nbd, nbd module (all WRCP-native) |
| Sidecars | provisioner v5.3.0, attacher v4.10.0, livenessprobe v2.17.0, registrar v2.15.0 | identical |
| Security context | privileged, hostPID, hostNetwork | identical |
| CDI StorageProfile patch | required | required (same reason: block-only) |
| CI | `wndrvr_v*` tags → ghcr.io image + OCI chart | cloned workflow, image `ghcr.io/solutions-innovation/cinder-rbd-csi-plugin` |

The demo walkthrough, examples (`pvc-only.yaml`, `block/block.yaml`),
and troubleshooting guide are cloned and adapted (`iscsiadm -m session`
→ `rbd-nbd list-mapped`, by-path device → `/dev/nbdX`).

## 10. Risks and open questions

| # | Risk / question | Mitigation / resolution path |
|---|---|---|
| 1 | WRCP Cinder RBD driver rejects the bare-metal connector or returns unexpected connection_info | **Phase 0 spike is the gate** — no code before it passes |
| 2 | rbd-nbd daemon death → EIO on mapped device | host-namespace spawn (§7.4); recovery runbook |
| 3 | Double-map from two nodes (RBD does not enforce per-initiator exclusivity like iSCSI targets) | CSI `SINGLE_NODE_WRITER` + external-attacher; optional `rbd status` watcher check before map |
| 4 | cephx key compromise (node Secret) | least-privilege `profile rbd` client scoped to the volumes pool; RBAC on the Secret |
| 5 | Ceph mon addresses in connection_info unreachable from workers | WRCP topology makes mons local; Phase 0 confirms reachability |
| 6 | Image features unsupported by mapper | rbd-nbd supports all features (reason it was chosen over krbd) |
| 7 | Cinder metadata loss orphans attachment | same on-demand-create + 404-retry design as cinder-iscsi |

## 11. Phased implementation plan

- **Phase 0 — feasibility spike (gate, ~1 day, zero code):** on a real
  WRCP cloud with curl/openstack CLI: create volume → `POST
  /v3/attachments` (reserve, no server) → `PUT` with the §6.1 connector
  → inspect connection_info → create `client.wrcp-csi` cephx user →
  manual `rbd-nbd map` → dd read/write → `rbd-nbd unmap` → `DELETE
  /v3/attachments/{id}` → confirm volume returns to `available`.
  Output: a curl walkthrough doc (like the iSCSI one) + go/no-go.
- **Phase 1 — controller:** `pkg/csi/cinder-rbd/` controller +
  openstack package (copied from cinder-iscsi, RBD deltas), unit tests.
- **Phase 2 — node:** `RBDConnector` + node RPCs + nsenter host-spawn,
  unit tests with mock.
- **Phase 3 — deploy:** manifests, Helm chart, CDI StorageProfile,
  examples, chart verify script.
- **Phase 4 — release & E2E:** CI workflow, staging deployment,
  demo-walkthrough validation, troubleshooting guide.

## 12. References

- Existing implementation: `pkg/csi/cinder-iscsi/` on
  `wndrvr/cinder-iscsi-csi-plugin-impl`; skill
  `.github/skills/cinder-iscsi-csi/`
- Cinder attachments API (mv 3.27/3.44):
  https://docs.openstack.org/api-ref/block-storage/v3/#attachments
- Cinder RBD driver: `cinder/volume/drivers/rbd.py`
  (`initialize_connection`)
- CVE-2020-10755 — keyring removed from RBD connection_info
- cinderlib / Ember-CSI — prior art for VM-less Cinder attachment
- ceph-csi rbd-nbd restart problem — prior art for §7.4
