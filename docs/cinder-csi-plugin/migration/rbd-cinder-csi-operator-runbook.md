# Cinder RBD CSI Plugin — Operator Runbook

| Field | Value |
|---|---|
| **Driver** | `cinder-rbd.csi.windriver.com` |
| **Status** | Pre-production. The driver is not yet qualified (see the implementation design, Phase 6). |
| **Design** | [Detailed implementation design](rbd-cinder-csi-implementation-design.md) · [Proposal](rbd-backed-cinder-volume-for-wrcp-migration.md) |
| **Platform baseline** | WRCP 24.09, Kubernetes 1.29, Linux 6.6, Rook Ceph 18.2.x |

This runbook covers the conditions the driver deliberately refuses to resolve on
its own. In every one of them the driver has chosen to stop rather than guess,
because the wrong guess would corrupt data or fault an unrelated workload.

**The single most important rule:** the driver never unmaps a kernel RBD device
it cannot positively identify as its own. Platform Ceph-CSI maps images through
the same kernel interface on the same nodes, so an unverified unmap can break a
platform workload. If a procedure below tells you to unmap manually, verify the
image identity first.

---

## 0. Orientation

### Which pool belongs to whom

On the validated platform two clients share the kernel RBD path:

| Pool | Owner | Image name shape |
|---|---|---|
| `cinder-volumes` | Cinder, and therefore this driver | `<cinder-volume-uuid>` |
| `kube-rbd` | platform Ceph-CSI | `csi-vol-<uuid>` |

A mapping in `kube-rbd` is **never** this driver's. Leave it alone.

### Reading the kernel's view

```bash
# Every kernel RBD mapping on the node, with pool and image.
rbd device list --format json --conf /run/cinder-rbd-csi/ceph.conf

# The authoritative identity of one device, straight from the kernel.
N=5
for a in cluster_fsid pool pool_id name image_id size features; do
  printf '%-14s = %s\n' "$a" "$(cat /sys/bus/rbd/devices/$N/$a 2>/dev/null)"
done
```

`--conf` is required on every `rbd` invocation, including those that need no
credentials. The WRCP host ships `/etc/ceph/ceph.conf` as an unsubstituted
template whose `fsid` is the literal `%CLUSTER_UUID%`; without `--conf` the CLI
reads it and prints `parse error setting 'fsid' to '%CLUSTER_UUID%'`. That
message on **stderr with exit status 0 is harmless noise**, not a failure.

### The driver's own state

```bash
# Node-scoped staging index: one file per staged volume.
ls -l /var/lib/cinder-rbd-csi/staged/
cat /var/lib/cinder-rbd-csi/staged/<volume-id>.json

# Generated Ceph config and keyring, per volume. The keyring is mode 0400.
ls -l /run/cinder-rbd-csi/<volume-id>/
```

Never paste the contents of a `keyring` file into a ticket, log, or chat.

### Metrics worth alerting on

| Series | Meaning | Action |
|---|---|---|
| `cinder_rbd_csi_isolated_mappings > 0` | a node refuses to serve one or more volumes | §1 |
| `cinder_rbd_csi_duplicate_attachment_records_total` increasing | a volume has more than one Cinder attachment record | §2 |
| `cinder_rbd_csi_exclusive_lock_failures_total` increasing | another writable client holds the Ceph lock | §3 |
| `cinder_rbd_csi_unrecognized_mappings > 0` | a driver-owned pool has a mapping with no record | §4 |
| `cinder_rbd_csi_orphaned_staging_records > 0` after startup | records were discarded; normal after a reboot | none if it settles at 0 |

---

## 1. Isolated mapping

**Symptom.** `NodeStageVolume` fails with:

```
volume <id> is isolated on this node and will not be served: device /dev/rbdN
occupies <pool>/<image> but failed identity verification ...
```

and `cinder_rbd_csi_isolated_mappings` is non-zero. The pod stays in
`ContainerCreating`.

**What it means.** A live kernel mapping occupies the pool and image the driver
expected, but its identity does not match — most often the cluster FSID differs,
or sysfs and `rbd device list` disagree. The driver has **not** adopted it and
has **not** unmapped it.

**Why the driver stops.** Adopting the device could expose an unrelated image to
the pod. Unmapping it could fault whichever client actually owns it. Neither is
recoverable automatically.

### Diagnosis

```bash
# 1. What does the driver expect?
cat /var/lib/cinder-rbd-csi/staged/<volume-id>.json

# 2. What does the kernel actually have on that device?
N=<device-number-from-the-error>
for a in cluster_fsid pool name image_id; do
  printf '%-12s = %s\n' "$a" "$(cat /sys/bus/rbd/devices/$N/$a)"
done

# 3. What does Cinder say the volume should be?
openstack volume show <cinder-volume-uuid> -f value -c id -c status
```

Compare the three. The likely cases:

| Finding | Cause | Go to |
|---|---|---|
| `cluster_fsid` differs from `[RBD] expected-fsid` | the node is talking to a different Ceph cluster, or `expected-fsid` is wrong | §1a |
| pool/image match but `image_id` differs | the Cinder volume was deleted and recreated under the same name | §1b |
| the device is in `kube-rbd` | platform Ceph-CSI mapping; the record is stale | §1c |
| sysfs and `rbd device list` disagree | kernel state is inconsistent | §1d |

### 1a. FSID mismatch

Confirm the cluster identity, then fix the configuration rather than the mapping:

```bash
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph fsid
```

If the reported FSID is correct and `driver.conf` is wrong, correct
`[RBD] expected-fsid` in the ConfigMap and restart the node plugin. If the FSID
is genuinely unexpected, **stop** — the Cinder backend may have been re-pointed
at another cluster, which is a platform-level problem, not a driver one.

### 1b. Stale record after volume recreation

The record refers to an image that no longer exists under that identity. Confirm
the Cinder volume is gone or recreated, then discard the record and let the
driver map afresh:

```bash
# Verify the device is NOT the image the record claims before touching anything.
cat /sys/bus/rbd/devices/$N/image_id

# Remove only the driver's own record. This does not unmap anything.
rm /var/lib/cinder-rbd-csi/staged/<volume-id>.json
```

Then delete the pod so kubelet retries staging. The isolation set is in-memory,
so restarting the node plugin also clears it:

```bash
kubectl -n <namespace> delete pod -l app=cinder-rbd-csi-nodeplugin --field-selector spec.nodeName=<node>
```

### 1c. Record points at a foreign mapping

Delete the driver's record as in §1b. **Do not unmap the device** — it belongs to
platform Ceph-CSI, and unmapping it will break that workload.

### 1d. Inconsistent kernel state

sysfs and the CLI disagreeing indicates a kernel or udev problem rather than a
driver one. Drain the node and reboot it. Do not attempt a manual unmap: the
device identity cannot be established, which is precisely the condition that
makes unmapping unsafe.

---

## 2. Duplicate or conflicting Cinder attachment records

**Symptom.** `ControllerPublishVolume` fails with:

```
volume <id> has N attachment records (<ids>); refusing to guess which one to use
— operator resolution required
```

**What it means.** The driver found more than one attachment record for a volume
whose metadata carried no attachment ID. Picking one could attach a record
another actor owns.

### Diagnosis

The lab `openstack` CLI is too old for `volume attachment` subcommands, so use
the API directly:

```bash
source /var/home/sysadmin/openrc.os
TOK=$(openstack token issue -f value -c id)
PROJ=$(openstack token issue -f value -c project_id)
BASE="http://cinder.openstack.svc.cluster.local/v3/$PROJ"

curl -s -H "X-Auth-Token: $TOK" -H 'OpenStack-API-Version: volume 3.27' \
  "$BASE/attachments?volume_id=<volume-id>" | python3 -m json.tool
```

For each record note `id`, `status`, and `instance`. Then decide:

| Record shape | Interpretation |
|---|---|
| `status: reserved`, `instance: null` | created by this driver and unused |
| `status: attached`, `instance: null` | this driver, in use by a migration pod |
| `instance` is a UUID | **Nova owns this.** Do not delete it. |

### Resolution

1. If any record has a non-null `instance`, stop. A compute instance is using
   the volume; deleting the record would break it. Establish why a migration PVC
   targets a Nova-attached volume before continuing.
2. Otherwise, identify which record corresponds to the running migration pod by
   matching the node in the pod's `VolumeAttachment` against the record.
3. Delete the surplus records:

   ```bash
   curl -s -o /dev/null -w '%{http_code}\n' -X DELETE \
     -H "X-Auth-Token: $TOK" -H 'OpenStack-API-Version: volume 3.27' \
     "$BASE/attachments/<surplus-attachment-id>"
   ```

4. Write the surviving record's ID into the volume metadata so the driver adopts
   it rather than creating another:

   ```bash
   openstack volume set --property csi.rbd.attachment_id=<surviving-id> <volume-id>
   ```

5. Confirm the volume settles, then let the attacher retry.

---

## 3. Exclusive lock held by another client

**Symptom.** `NodeStageVolume` fails with:

```
exclusive lock on <pool>/<image>@<fsid> is held by [client.NNN@<addr>];
refusing to map without exclusivity
```

**What it means.** Another writable client holds the Ceph exclusive lock. This is
the single-writer guarantee working correctly, not a driver defect. There is no
fallback: the driver will never map non-exclusively.

### Diagnosis

```bash
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- \
  rbd status cinder-volumes/<image>
```

Read the `Watchers` block and the locker address. Map the address back to a node:

```bash
kubectl get nodes -o wide | grep <address-from-rbd-status>
```

### Resolution

| Holder | Action |
|---|---|
| a node running a migration pod for this volume | expected. Wait for the previous pod to terminate; the lock releases on unmap. |
| a node with no such pod | a leaked mapping. Follow §4 on that node. |
| a Nova compute host | the volume is attached to a VM. Detach it in Nova before migrating. |
| nobody, yet the map still fails | the lock may be stale after an ungraceful shutdown. See below. |

A stale lock can be blocklisted and broken, but this is a **destructive
operation on Ceph state**. Only do it after confirming no client is writing:

```bash
# Confirm there are no watchers at all first.
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- rbd status cinder-volumes/<image>

# Then, and only then, with a Ceph operator present:
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- \
  rbd lock ls cinder-volumes/<image>
# rbd lock rm <image> <lock-id> <locker>   # requires explicit sign-off
```

---

## 4. Unrecognized mapping in a driver-owned pool

**Symptom.** `cinder_rbd_csi_unrecognized_mappings` is non-zero, and the node
plugin logged:

```
reconcile: reported /dev/rbdN (cinder-volumes/<image>): live mapping in a
driver-owned pool has no staging record ...
```

**What it means.** A mapping exists in a pool this driver uses, but nothing on
the node claims it. In a split controller/node deployment the node plugin has no
Cinder client and therefore **cannot prove ownership**, so it reports and leaves
it alone. This is the deliberate safety asymmetry described in the design.

### Resolution

1. Establish whether the image belongs to a Cinder volume this driver owns:

   ```bash
   openstack volume show <image-name-is-the-volume-uuid> \
     -f value -c id -c status -c properties
   ```

   Driver-owned volumes carry a `csi.rbd.attachment_id` property.

2. Check whether any pod on the node still uses it:

   ```bash
   kubectl get volumeattachment -o wide | grep <volume-id>
   grep -rl '<volume-id>' /var/lib/kubelet/pods/*/volumeDevices/ 2>/dev/null
   ```

3. If no pod uses it and the volume is driver-owned, the mapping is a leak from
   an ungraceful shutdown. Unmap it manually **after** verifying identity:

   ```bash
   N=<device-number>
   # These three must match the Cinder volume before you proceed.
   cat /sys/bus/rbd/devices/$N/cluster_fsid
   cat /sys/bus/rbd/devices/$N/pool
   cat /sys/bus/rbd/devices/$N/name

   blockdev --flushbufs /dev/rbd$N
   rbd device unmap /dev/rbd$N --conf /run/cinder-rbd-csi/ceph.conf
   ```

4. If the volume is not driver-owned, leave it and investigate who created it.

---

## 5. Ceph credential rotation

The operator duplicates the platform `client.cinder` key into the plugin's
namespace-scoped Secret. There is no automatic synchronization.

### Rotating

```bash
# 1. Read the current platform key (do not echo it into a shared terminal).
kubectl -n openstack get secret cinder-volume-rbd-keyring \
  -o jsonpath='{.data.key}' | base64 -d > /tmp/newkey
chmod 600 /tmp/newkey

# 2. Replace the plugin Secret.
kubectl -n <plugin-namespace> create secret generic <plugin-secret-name> \
  --from-literal=userID=cinder \
  --from-file=userKey=/tmp/newkey \
  --dry-run=client -o yaml | kubectl apply -f -

# 3. Shred the temporary copy.
shred -u /tmp/newkey
```

**No pod restart is required.** kubelet refreshes the projected Secret in place
and the driver re-reads it on every `NodeStageVolume`. Existing mappings are
unaffected: the kernel already holds its session.

### Verifying

```bash
# The entity must match what Cinder returns as auth_username.
kubectl -n <plugin-namespace> get secret <plugin-secret-name> \
  -o jsonpath='{.data.userID}' | base64 -d; echo

# The FSID must match the cluster.
kubectl -n rook-ceph exec deploy/rook-ceph-tools -- ceph fsid
```

A mismatch produces, at stage time:

```
ceph credential entity does not match Cinder auth_username: configured "X",
Cinder returned "cinder"
```

This fails **before** any map attempt, by design: krbd requires `--id` and the
keyring entity to be the same identity, and an opaque Ceph authentication error
would be far harder to diagnose.

---

## 6. Volume stuck detaching between migration phases

**Symptom.** `ControllerUnpublishVolume` returns `Aborted`:

```
volume <id> has not returned to available after detaching
```

**What it means.** The attachment record was deleted but Cinder has not moved the
volume back to `available`. The next migration pod must not be published while
the volume is still `reserved` or `in-use`, so the driver refuses to report
success and the external-attacher retries.

### Diagnosis

```bash
openstack volume show <volume-id> -f value -c status
```

| Status | Action |
|---|---|
| `available` | resolved; the retry will succeed |
| `detaching` | in progress; wait |
| `in-use` or `reserved` | a record still exists — check §2 |
| `error_deleting` | Cinder-side failure; escalate to the storage team |

Confirm from the node side that nothing is mapped:

```bash
rbd device list --format json --conf /run/cinder-rbd-csi/ceph.conf \
  | python3 -c 'import sys,json;print([e["device"] for e in json.load(sys.stdin) if e["pool"]=="cinder-volumes"])'
```

An empty list plus a stuck Cinder status means the problem is in Cinder, not the
driver.

---

## 7. Node plugin not ready

`Probe` reports not-ready for exactly four reasons. Check in this order:

```bash
kubectl -n <namespace> logs -l app=cinder-rbd-csi-nodeplugin -c cinder-rbd-csi-plugin --tail=50
```

| Log line | Cause | Fix |
|---|---|---|
| `rbd client check failed: ... executable file not found` | the image lacks the bundled Ceph client | wrong image or a broken build |
| `bundled client major version is N, expected 18` | image/config mismatch | align the image with `[RBD] ceph-client-version-major` |
| `ceph credential unavailable` | the Secret is not projected or is empty | §5 |
| `node-only mode but RBD mapper not configured` | a wiring defect, not a configuration problem | file a bug; include the startup log |

A node runtime preparation failure (`node runtime preparation failed`) leaves the
plugin running but staging will fail. Check that `[RBD] runtime-dir` and
`state-dir` are writable and that the runtime volume is mounted.

---

## 8. What never to do

1. **Never `rbd device unmap` a device without first reading its
   `cluster_fsid`, `pool` and `name` from sysfs.** Device numbers are reused;
   `/dev/rbd5` today is not `/dev/rbd5` from an hour ago.
2. **Never unmap anything in `kube-rbd`.** That is platform Ceph-CSI.
3. **Never delete a Cinder attachment record whose `instance` is non-null.** Nova
   owns it.
4. **Never set `[RBD] exclusive = false`** to work around a lock conflict. The
   driver rejects it at startup and the chart refuses to render it, deliberately.
5. **Never copy a keyring, or the `userKey` value, into a ticket or log.**
6. **Never delete a Cinder volume to clear a stuck PVC** without checking whether
   the migration Blueprint still needs it. The driver retains volumes by design
   so the target VM can be built from them.
7. **Never edit a staging record to "fix" a device path.** Delete it and let the
   driver reconcile; a hand-edited record can authorize an unmap of the wrong
   device.

---

## Appendix: state locations

| Path | Contents | Safe to delete? |
|---|---|---|
| `/var/lib/cinder-rbd-csi/staged/<volume-id>.json` | node-scoped staging record | yes — the driver remaps on the next stage |
| `<kubelet>/plugins/kubernetes.io/csi/.../globalmount/rbd-staging.json` | per-volume staging record | yes, same |
| `/run/cinder-rbd-csi/ceph.conf` | cluster-scoped generated config, no secrets | yes — rewritten at startup |
| `/run/cinder-rbd-csi/<volume-id>/ceph.conf` | per-volume config, no secrets | yes, while unmapped |
| `/run/cinder-rbd-csi/<volume-id>/keyring` | **Ceph key**, mode 0400 | yes, while unmapped; shred rather than `rm` if the runtime dir is disk-backed |
| Cinder volume metadata `csi.rbd.attachment_id` | current attachment record ID | no — the driver's only source of truth |
| Cinder volume metadata `csi.rbd.cleanupVolume` | per-volume delete override | yes, changes deletion policy |
