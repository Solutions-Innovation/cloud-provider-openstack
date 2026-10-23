# VM Import Operations Guide

This guide explains how to operate the OVA/OVF/VMDK → RAW → Cinder (or any
Block-mode CSI volume) import tool end to end: building/distributing the
container image, and configuring every parameter in the Kubernetes
manifests. It assumes the files in this directory (`Dockerfile`,
`convert-and-import.sh`, `job.yaml`) as the reference implementation.

For the conceptual overview (why Block Volume mode is required, etc.), see
[README.md](./README.md). This document focuses on day-to-day operation.

---

## 1. Container Image Lifecycle

The tool image bundles `qemu-img`, `virt-v2v`/`libguestfs`, `curl`, `tar`,
and `python3` on a Fedora minimal base, with `convert-and-import.sh` baked
in as the `ENTRYPOINT`.

### 1.1 Build

```bash
cd examples/cinder-csi-plugin/vm-import
docker build -t <your-registry>/vm-import:latest .
```

If your build host requires a proxy to reach package repositories
(Fedora/DNF mirrors), pass it as a **build argument** so it is only used
during the build and never baked into the runtime image:

```bash
docker build \
  --build-arg http_proxy=http://your-proxy:3128/ \
  --build-arg https_proxy=http://your-proxy:3128/ \
  -t <your-registry>/vm-import:latest .
```

Build time is dominated by `microdnf install` pulling `qemu-img`,
`virt-v2v`, `libguestfs`, and their dependencies (~50 seconds on a
reasonably fast connection), plus downloading the ~840MiB
`virtio-win.iso` (bundled so `virt-v2v-in-place` can actually inject
VirtIO drivers into Windows guests under `CONVERT_MODE=direct-v2v` —
without it, conversion still succeeds but falls back to slower emulated
devices; see [Section 3](#3-choosing-a-conversion-mode) and the
Troubleshooting table in [Section 7](#7-troubleshooting)). Final image
size is around 3.2GB — most of that is `libguestfs`'s dependency chain
(it bundles a full QEMU/KVM stack to run its internal "appliance" VM,
used for guest filesystem inspection and driver injection) plus the
VirtIO ISO itself. Windows guest support (see
[Section 3](#3-choosing-a-conversion-mode)) requires both.

### 1.2 Pull (reuse an already-published image)

If you don't need to modify the tooling, pull the pre-built image instead
of building it yourself:

```bash
docker pull jonzhang/vm-import:latest
```

### 1.3 Tag

Tag the image for your target registry/repository before pushing. Replace
`registry.local:9001/public/vm-import` with your actual registry host,
project/namespace, and repository name:

```bash
docker tag jonzhang/vm-import:latest registry.local:9001/public/vm-import:latest
```

Use a specific version tag (e.g. `:v1`, `:2026-10-19`) instead of `:latest`
for anything beyond ad-hoc testing, so that `job.yaml` runs are
reproducible and you can roll back.

### 1.4 Push

Log in to the target registry first if required:

```bash
docker login registry.local:9001 -u <username>
```

Then push:

```bash
docker push registry.local:9001/public/vm-import:latest
```

### 1.5 Verify the push

Confirm the image is retrievable from the registry (not just present in
the local Docker cache) by removing the local copy and pulling it back:

```bash
docker rmi registry.local:9001/public/vm-import:latest
docker pull registry.local:9001/public/vm-import:latest
```

Compare the `Digest:` reported by `docker push` against the one reported by
`docker pull` — they must match.

### 1.6 Update `job.yaml`

Point the Job at your pushed image:

```yaml
containers:
  - name: vm-import
    image: registry.local:9001/public/vm-import:latest
```

---

## 2. Manifest Reference (`job.yaml`)

`job.yaml` contains two objects, applied together with
`kubectl apply -f job.yaml`:

1. `PersistentVolumeClaim` (the target volume, in **Block** mode)
2. `Job` (runs the conversion + copy once, to completion)

It does **not** define a `StorageClass`. A StorageClass is cluster-level
infrastructure tied to an actual CSI driver deployment (e.g. this project's
Cinder CSI driver, or Ceph RBD/CephFS CSI) — declaring one in a user
manifest doesn't make a matching provisioner exist, and if the name happens
to collide with an unrelated StorageClass that has no real provisioner
behind it, the PVC will hang in `Pending` forever (`ExternalProvisioning:
Waiting for a volume to be created by the external provisioner '...'`).

### 2.1 Choosing a StorageClass

The PVC in `job.yaml` sets `storageClassName: csi-sc-cinderplugin` by
default — this is the name used consistently across this project's own
examples and docs for the Cinder CSI driver (see e.g.
[../block/block.yaml](../block/block.yaml) and
[using-cinder-csi-plugin.md](../../../docs/cinder-csi-plugin/using-cinder-csi-plugin.md)).
Update the name to whatever your cluster's actual Cinder StorageClass is
called if it differs.

If you'd rather use the cluster's **default** StorageClass instead of
naming one explicitly (`kubectl get storageclass` shows `(default)` next
to its name), delete the `storageClassName` field entirely. This is what
was used during this example's own end-to-end testing, since the test
cluster had no Cinder CSI driver deployed and relied on a
Ceph-RBD-backed default StorageClass instead — `storageClassName:
csi-sc-cinderplugin` would have left the PVC permanently `Pending` there,
since no provisioner by that name existed.

```yaml
spec:
  # Either keep this pointed at your actual Cinder StorageClass name:
  storageClassName: your-existing-cinder-storageclass-name
  # ...or delete the field to use the cluster default instead.
```

Whatever StorageClass you reference (default or explicit), its
`provisioner` must support `volumeMode: Block`. Verified during testing:
`cinder.csi.openstack.org` (this project) and `rook-ceph.rbd.csi.ceph.com`.
Before applying, confirm the StorageClass you intend to use actually has a
matching, running CSI driver:

```bash
kubectl get storageclass                 # note the PROVISIONER column
kubectl get csidrivers                   # confirm that provisioner is listed here
```

If the provisioner isn't listed in `kubectl get csidrivers`, the PVC will
never bind no matter what you set `storageClassName` to.

### 2.2 PersistentVolumeClaim

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: vm-import-target-pvc
spec:
  accessModes:
    - ReadWriteOnce
  volumeMode: Block
  storageClassName: csi-sc-cinderplugin
  resources:
    requests:
      storage: 20Gi
```

| Field | Meaning |
|---|---|
| `metadata.name` | Referenced by the Job's `volumes[].persistentVolumeClaim.claimName`. |
| `spec.accessModes` | `ReadWriteOnce` is sufficient; the Job mounts it from a single Pod. |
| `spec.volumeMode` | **Must be `Block`.** This is what causes the CSI driver to expose the volume as a raw block device (e.g. `/dev/xvda`) inside the Pod instead of a mounted filesystem — required for `qemu-img`/`virt-v2v-in-place` to write/modify a full disk image. Omitting this (or leaving the default `Filesystem`) will cause the write to corrupt the filesystem rather than lay down a disk image. |
| `spec.storageClassName` | Defaults to `csi-sc-cinderplugin` (this project's standard example name for the Cinder CSI driver — see [Section 2.1](#21-choosing-a-storageclass)). Update it to your cluster's actual Cinder StorageClass name, or delete the field to use the cluster's default StorageClass instead. |
| `spec.resources.requests.storage` | **Must be ≥ the virtual size of the source disk.** Check with `qemu-img info <source-image>` → `virtual size`. The conversion script also checks this at runtime and fails early (before writing) if the source is larger than the target device, so an undersized PVC fails safely rather than silently truncating data. |

### 2.3 Job

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: vm-import-job
spec:
  backoffLimit: 1
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: vm-import
          image: jonzhang/vm-import:latest
          imagePullPolicy: IfNotPresent
          env:
            - name: SOURCE_IMAGE_PATH
              value: "/input/CHANGE-ME-source.ova"
            - name: TARGET_DEVICE
              value: "/dev/xvda"
            - name: CONVERT_MODE
              value: "direct"
            - name: VERIFY_CHECKSUM
              value: "false"
            - name: LIBGUESTFS_BACKEND_SETTINGS
              value: "qemu_options=-enable-kvm"
          securityContext:
            privileged: true
          resources:
            requests:
              cpu: "500m"
              memory: "1Gi"
            limits:
              cpu: "2"
              memory: "2Gi"
          volumeDevices:
            - name: vm-import-target
              devicePath: /dev/xvda
          volumeMounts:
            - name: workdir
              mountPath: /work
            - name: source-input
              mountPath: /input
              readOnly: true
      volumes:
        - name: vm-import-target
          persistentVolumeClaim:
            claimName: vm-import-target-pvc
            readOnly: false
        - name: workdir
          emptyDir:
            sizeLimit: 8Gi
        - name: source-input
          hostPath:
            path: /path/on/the/node/to/your/source
            type: Directory
```

`SOURCE_IMAGE_PATH` + the `source-input` `hostPath` volume is the default
here because it's what was actually verified end-to-end (see
[Section 3](#3-choosing-a-conversion-mode) and
[Section 4](#4-running-an-import)) — the exact value (file vs. directory)
and the `hostPath.path` need to be changed to match wherever your actual
source lives, as shown above with the `CHANGE-ME`/placeholder values. If
your source image is reachable over HTTP(S) instead of being present on a
node, replace `SOURCE_IMAGE_PATH` with `SOURCE_IMAGE_URL` and drop the
`source-input` volume/mount — see [Section 4](#4-running-an-import) for
that variant.

### 2.3.1 Enabling KVM acceleration for `virt-v2v-in-place` (`CONVERT_MODE=direct-v2v`)

Two settings must **both** be present together for `virt-v2v-in-place`'s
internal libguestfs appliance VM to actually get hardware-accelerated KVM
instead of silently falling back to software emulation (TCG) — this is an
AND relationship, not independent options:

| Setting | Why it's needed |
|---|---|
| `securityContext.privileged: true` | Grants the container permission to open and use the host's `/dev/kvm` directly (and other low-level operations libguestfs's appliance may need, e.g. loop devices). `privileged` bypasses Kubernetes' device-plugin gating entirely, so it alone is sufficient for `/dev/kvm` access — no `devices.kubevirt.io/kvm` resource request is needed (see the note below on why this project deliberately does **not** request that resource). |
| `env.LIBGUESTFS_BACKEND_SETTINGS=qemu_options=-enable-kvm` | Forces libguestfs's internal QEMU appliance to pass `-enable-kvm` explicitly, rather than relying solely on libguestfs's own automatic `/dev/kvm` probing. |

If either setting is missing, libguestfs's automatic detection simply
falls back to software emulation (TCG) **without erroring out** —
conversion still completes, just much slower. There is no error message
indicating this fallback happened; the only symptom is conversion taking
noticeably longer than expected. Both settings are unused (harmless)
under `CONVERT_MODE=direct`, which never launches the libguestfs
appliance at all.

**Why this manifest does not request `devices.kubevirt.io/kvm`:**
earlier versions of this manifest requested
`devices.kubevirt.io/kvm: "1"` alongside `privileged: true`, on the
assumption that a StarlingX (WRCP) cluster would always have a KubeVirt
(or equivalent) device plugin advertising non-zero allocatable capacity
for it. **Verified end-to-end on a real StarlingX (WRCP) cluster that
this assumption does not always hold**: `devices.kubevirt.io/kvm` was
registered on the node (present in `kubectl describe node`'s
`Allocatable` section) but with allocatable `0` (no KubeVirt device
plugin actually running) — requesting it unconditionally made the Pod
stay `Pending` forever (`Insufficient devices.kubevirt.io/kvm`), since
Kubernetes enforces resource requests strictly regardless of
`privileged`. Removing the resource request and relying solely on
`privileged: true` fixed scheduling immediately, and KVM acceleration
was still confirmed working afterward — `privileged` grants direct access
to the host's `/dev/kvm` node without going through Kubernetes' device-
plugin accounting at all, so the resource request was never actually
necessary once `privileged` is set. Confirmed via `virt-v2v-in-place -v`
(`ENABLE_DEBUG=true`) output showing `libguestfs: qemu KVM: enabled` and
the appliance VM's own kernel log reporting `Hypervisor detected: KVM`
and switching to the `kvm-clock` clocksource — i.e. real hardware
acceleration, not a TCG fallback that merely didn't error. The resulting
GPT partition layout (`Windows recovery environment` + `EFI System` +
`Microsoft reserved` + `Microsoft basic data`) matched the reference
test run described in [Section 3](#3-choosing-a-conversion-mode). If you
know your own cluster *does* advertise `devices.kubevirt.io/kvm` with
non-zero allocatable and would rather request it explicitly for
clearer scheduling intent/accounting, you can add it back — just be
aware it is not required for KVM acceleration to work once `privileged`
is set, and it introduces a scheduling risk (permanent `Pending`) on any
cluster where the resource is advertised with zero allocatable.

**Security note on `privileged: true`:** this grants the container
near-host-root capabilities — access to all host devices, the ability to
alter kernel/network settings, and a materially larger container-escape
attack surface compared to an unprivileged container. `job.yaml` enables
it by default on the assumption that this is a short-lived, one-shot Job
(not a long-running service) dedicated to this single low-level
block-device/appliance-VM operation, on a StarlingX (WRCP) cluster. If
your threat model does not accept a privileged container, remove
`securityContext` and the `LIBGUESTFS_BACKEND_SETTINGS` env var —
`CONVERT_MODE=direct-v2v` will still work, just via the slower
software-emulation path.

#### Job-level fields

| Field | Meaning |
|---|---|
| `spec.backoffLimit` | Number of retries on failure. Set to `1` so a transient failure gets one retry, but a persistent one (e.g. bad URL, undersized PVC) fails fast instead of looping. |
| `spec.template.spec.restartPolicy` | Must be `Never` or `OnFailure` for a Job's Pod template. `Never` is used here so each failed attempt is a fresh Pod, making `kubectl logs <pod>` per-attempt easier to read. |

#### Container fields

| Field | Meaning |
|---|---|
| `image` | The tool image built/pushed in [Section 1](#1-container-image-lifecycle). |
| `imagePullPolicy` | `IfNotPresent` avoids re-pulling if the image is already cached on the node (useful for repeated test runs). Use `Always` if you push updates to the same tag and need the latest version every run. |
| `volumeDevices` | **This (not `volumeMounts`) is what maps the Block-mode PVC into the container as `/dev/xvda`.** `name` must match a `volumes[].name` entry; `devicePath` is the path as seen inside the container and must match `env.TARGET_DEVICE`. |
| `volumeMounts` (for `workdir`, `source-input`) | Regular filesystem mounts for scratch space (`workdir`: downloaded/extracted source image only — neither `CONVERT_MODE` value ever writes a full intermediate RAW file here) and, by default, the locally-mounted source image (`source-input`, backing `SOURCE_IMAGE_PATH`). Neither is used for the target device — that's `volumeDevices`, above. If you switch to `SOURCE_IMAGE_URL` instead of a local file, remove the `source-input` mount and its corresponding volume. |

#### Environment variables (consumed by `convert-and-import.sh`)

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `TARGET_DEVICE` | Yes | — | Path to the target block device inside the container, e.g. `/dev/xvda`. Must exactly match `volumeDevices[].devicePath`. The script verifies this path exists and is a block device before doing any work. |
| `SOURCE_IMAGE_PATH` | One of `SOURCE_IMAGE_PATH` or `SOURCE_IMAGE_URL` is required | — | Local filesystem path to a source image already present inside the Pod (mounted via `hostPath` or another PVC — in the shipped `job.yaml`, the `source-input` volume). Takes precedence over `SOURCE_IMAGE_URL` if both are set. **This is the default/verified path in `job.yaml`.** Point it at a specific file (e.g. `/input/disk.ova`) or, for a bare OVF directory, at the directory itself (e.g. `/input`). |
| `SOURCE_IMAGE_URL` | One of `SOURCE_IMAGE_PATH` or `SOURCE_IMAGE_URL` is required | — | HTTP(S) URL to download the source `.ova`/`.ovf`/`.vmdk` image. Downloaded into the `workdir` scratch volume. Use this only when the source image is actually reachable over HTTP(S); it is **not** used in the shipped `job.yaml` default, which reads a locally-mounted file via `SOURCE_IMAGE_PATH` instead. |
| `CONVERT_MODE` | No | `direct` | Conversion strategy: `direct` (Linux guests) or `direct-v2v` (Windows guests). See [Section 3](#3-choosing-a-conversion-mode). |
| `VERIFY_CHECKSUM` | No | `false` | If `"true"`, computes a sha256 checksum and compares it against a checksum read back from `TARGET_DEVICE`. This roughly **doubles** the total I/O time (every byte is read back from the target volume), so use it for validation runs, not routine production imports. Creates a temporary RAW file (removed immediately after comparison) since `qemu-img convert` cannot stream RAW output to a pipe — this temporarily reintroduces scratch-space usage the direct write otherwise avoids. Under `CONVERT_MODE=direct-v2v`, this only covers the direct-write phase, not the subsequent `virt-v2v-in-place` modification. |
| `ZERO_CHECK_SAMPLE_BYTES` | No | `104857600` (100MiB) | Number of bytes sampled from the start, middle, and end of `TARGET_DEVICE` to verify it reads back as all-zero before trusting `--target-is-zero`. Used by both `CONVERT_MODE` values (the direct-write phase). Increase for extra assurance on very large or suspect volumes, at the cost of a slightly longer check (three reads of this size). |
| `WORK_DIR` | No | `/work` | Scratch directory for the downloaded/extracted source image. Should point at a volume with enough free space for that — neither `CONVERT_MODE` value ever writes a full intermediate RAW file here. |
| `KEEP_WORK_DIR` | No | `false` | If `"true"`, skips cleanup of `WORK_DIR` on success — useful for debugging a failed run without re-downloading/re-converting. |
| `DISK_SELECTOR` | Required if the source has more than one `.vmdk`; optional otherwise | — | Filename (basename only, e.g. `disk1.vmdk`) to pick a specific disk when an `.ova` archive or OVF directory contains more than one `.vmdk`. With exactly one `.vmdk` found, it's used automatically and `DISK_SELECTOR` isn't needed. With more than one found and `DISK_SELECTOR` unset, the script **fails immediately** with an error listing every candidate's full path, rather than guessing (see [Section 4.2](#42-importing-a-multi-disk-source-more-than-one-vmdk)). **Under `CONVERT_MODE=direct-v2v`, this only affects the direct-write phase** — the subsequent `virt-v2v-in-place` step always operates on the whole `TARGET_DEVICE` as a single disk, with no disk-selection concept of its own; the script logs a warning if `DISK_SELECTOR` is set in that mode. |
| `ENABLE_DEBUG` | No | `false` | If `"true"`, passes `-v` to `virt-v2v-in-place` for verbose troubleshooting output. Only relevant under `CONVERT_MODE=direct-v2v`. |
| `NO_FSTRIM` | No | `false` | If `"true"`, passes `--no-fstrim` to `virt-v2v-in-place`, skipping the filesystem trim it normally performs before conversion. Only relevant under `CONVERT_MODE=direct-v2v`. Not exercised in this project's own testing (the default trim behavior was used); see `virt-v2v-in-place`'s own documentation for when skipping it might be desirable. |
| `PROGRESS_MODE` | No | `clean` | `clean` throttles `qemu-img convert -p`'s and `virt-v2v-in-place`'s native `\r`-delimited output (which renders as an unreadable wall of text in `kubectl logs` and similar non-interactive viewers) into periodic, clean, timestamped single-line updates. Set to `raw` to disable this and see the tool's native output instead — useful when running the script directly in an interactive terminal, where `\r` correctly overwrites a single line. See [Section 3.1](#31-progress-output) for examples of the difference. |
| `PROGRESS_STEP_PERCENT` | No | `10` | Only relevant under `PROGRESS_MODE=clean`, for percentage-based progress output. Print an update whenever progress advances by at least this many percentage points since the last one. |
| `PROGRESS_INTERVAL_SECONDS` | No | `15` | Only relevant under `PROGRESS_MODE=clean`. Print an update at least this often regardless of percentage progress. |

Note: `LIBGUESTFS_BACKEND_SETTINGS` is not one of this script's own
variables (it is not read by `convert-and-import.sh`) — it is a
standard libguestfs environment variable, recognized directly by the
`virt-v2v-in-place` subprocess itself under `CONVERT_MODE=direct-v2v`.
See [Section 2.3.1](#231-enabling-kvm-acceleration-for-virt-v2v-in-place-convert_modedirect-v2v)
for what it does and why it must be combined with the
`devices.kubevirt.io/kvm` resource and a privileged `securityContext` to
actually take effect.

#### Resource requests/limits

| Field | Meaning |
|---|---|
| `cpu` / `memory` | Standard Kubernetes resource sizing. The defaults shown (500m-2 CPU, 1-2Gi memory) were verified sufficient for `CONVERT_MODE=direct` against 10-16GiB test disks, and for `CONVERT_MODE=direct-v2v` against a 90GiB-virtual Windows guest without KVM acceleration (with the higher end of the range, 2-4 CPU/4-8Gi memory, used in that specific test — see [Section 3](#3-choosing-a-conversion-mode)). |

#### Volumes

| Volume | Purpose |
|---|---|
| `vm-import-target` | The Block-mode PVC from [Section 2.2](#22-persistentvolumeclaim). Referenced by the container's `volumeDevices`, not `volumeMounts`. |
| `workdir` | `emptyDir` scratch space. Neither `CONVERT_MODE` value ever writes a full intermediate RAW file here, so it only needs to hold the downloaded/extracted source image — for a `streamOptimized` VMDK (the common case), that's its *actual* (compressed) size, which can be far smaller than its virtual size. Verified in testing: this exact 8Gi `sizeLimit` was sufficient even for a 90GiB-virtual/~5.2GiB-actual Windows Server 2019 guest under `CONVERT_MODE=direct-v2v`. |

---

## 3. Choosing a Conversion Mode

Set via `CONVERT_MODE`. Two modes are supported, both writing straight
onto `TARGET_DEVICE` via `qemu-img convert -n --target-is-zero -O raw`
with **no intermediate RAW file at any point** — scratch space (the
`workdir` volume) only ever needs to hold the downloaded/extracted source
image itself, never the virtual disk size, under either mode.

This direct write is a pure format converter — it copies the disk's bytes
from one container format to another (vmdk -> raw) without touching
anything inside the guest filesystem. A Windows guest exported from
VMware has VMware-specific drivers (and often no virtio drivers at all),
a boot configuration pointing at VMware's virtual hardware, and (for
older Windows versions) a HAL tied to that hardware — booting this
unmodified under KVM/QEMU (what Nova uses) typically fails to boot
(commonly `INACCESSIBLE_BOOT_DEVICE` or a similar STOP code) because the
guest can't find/drive its boot disk under the new virtual hardware. Most
modern Linux guests don't have this problem (they already include virtio
drivers and don't hardcode hypervisor-specific boot paths), which is why
the direct write alone is sufficient for them.

For Windows guests, this project uses `virt-v2v-in-place`, a sibling tool
to `virt-v2v` shipped by libguestfs that performs the same driver-
injection/boot-fixup conversion **directly against an existing disk**
(file or block device) with no separate output step at all — unlike
`virt-v2v` itself, whose output modes (`-o local`, `-o qemu`, `-o
rhv-upload`, `-o openstack`, `-o glance`) can't write to an arbitrary
pre-existing block device and would require a scratch RAW file +
subsequent `dd`. `CONVERT_MODE=direct-v2v` combines the direct write with
`virt-v2v-in-place -i disk TARGET_DEVICE` run immediately afterward
against the now-populated target device, fixing it up **in place**. The
cost of this approach: `virt-v2v-in-place` modifies its target with no
backup, so a failure partway through leaves `TARGET_DEVICE` itself (not a
disposable scratch file) in an undefined state — see the `direct-v2v` row
below for the full risk discussion.

| Mode | When to use | Requirements | Notes |
|---|---|---|---|
| `direct` (default) | The common case for Linux guests. Handles both `.ova` archives (auto-extracts the first `.vmdk`, or a specific one via `DISK_SELECTOR`) and bare `.vmdk`/`.qcow2`/etc. files, as well as bare OVF directories. **Not suitable for Windows guests** — see `direct-v2v` below. | Target device must read back as all-zero; the script verifies this automatically (sampling start/middle/end, size controlled by `ZERO_CHECK_SAMPLE_BYTES`) and refuses to proceed if it doesn't, rather than risk corrupt output. A freshly provisioned CSI volume should always pass this check. | Writes directly to `TARGET_DEVICE` via `qemu-img convert -n --target-is-zero -O raw`, skipping any intermediate RAW file. **Why `--target-is-zero` matters:** writing to a block device *without* it is a known qemu-img pitfall — unlike a file, a block device can't be "truncated"/sparse, so qemu-img falls back to writing real zero bytes for every unallocated (hole) region of the source. For a mostly-empty `streamOptimized` VMDK (e.g. 10GiB virtual / ~1GiB actual data), this stalled for 4+ minutes with zero additional bytes written in testing (confirmed via the kernel reporting the writer thread blocked in `submit_bio_wait`, i.e. waiting on a slow/congested block I/O request, not deadlocked). Upstream (oVirt/vdsm) documented the same behavior and measured up to 250x slowdowns: https://github.com/oVirt/vdsm/commit/2bbe66274524cc009f39d3a5e5577b84144f1171. With `--target-is-zero`, the identical conversion completed in ~18.5-20 seconds in testing. Also verified: the safety check correctly **refuses** to run against a previously-written (non-zero) volume rather than silently producing corrupt output. Verified end-to-end against both a self-contained `.ova` archive (Fedora CoreOS, 10GiB virtual/~1GiB actual, GPT/UEFI layout) and a full OVF directory (Fedora, `.ovf`+`.vmdk`+`.nvram`+installer `.iso`, 16GiB virtual/~1.7GiB actual, BIOS/MBR+LVM layout, `SOURCE_IMAGE_PATH` pointed at the directory) — both completed in well under a minute with `VERIFY_CHECKSUM=true` passing. |
| `direct-v2v` | **Required for Windows guests.** Also an option (though not required) for Linux guests that specifically need driver injection for some other reason; otherwise prefer `direct`, which is faster and carries no in-place-modification risk. | Target device must read back as all-zero, same as `direct` (checked before the direct-write phase). Needs the guest OS to be recognized by libguestfs's inspection for the `virt-v2v-in-place` phase — OSTree/Ignition-based Linux distributions (e.g. Fedora CoreOS) are **not** recognized and will fail here; use `CONVERT_MODE=direct` for those instead. Benefits from real KVM acceleration for that phase (`/dev/kvm` on the node, typically via the KubeVirt device plugin's `devices.kubevirt.io/kvm` resource — see [Section 2.3](#23-job)); still works without it via software emulation, just much slower. Needs scratch space only for the downloaded/extracted source image — NOT the virtual disk size, since no intermediate RAW file is ever written for either phase. | Two phases: (1) the exact same `qemu-img convert -n --target-is-zero -O raw` write as `CONVERT_MODE=direct`; (2) `virt-v2v-in-place -i disk TARGET_DEVICE` to inject drivers/fix up the boot configuration **in place** on the data just written, with `--no-fstrim` passed if `NO_FSTRIM=true` and `-v` passed if `ENABLE_DEBUG=true`. **Verified end-to-end against a real Windows Server 2019 guest** (90GiB-virtual/~5.2GiB-actual `streamOptimized` VMDK, no KVM acceleration available on the test node — the appliance VM fell back to software/tcg emulation), via the standard `kubectl apply -f job.yaml` flow (not manual steps): phase 1 took ~177-180 seconds; phase 2 took ~180-185 seconds, during which libguestfs correctly identified the guest as "Windows Server 2019 Standard Evaluation (win2k19)" and logged a VirtIO-drivers-unavailable fallback warning (see below). **Total: ~6.2-6.3 minutes**, using only an 8Gi `emptyDir` for `workdir` — no scratch PVC needed at all. The resulting block device showed a correct GPT layout (a 499MiB "Windows recovery environment" partition, a 99MiB EFI System partition, a 16MiB Microsoft Reserved partition, and an 89.4GiB "Microsoft basic data" partition with a confirmed NTFS boot-sector signature at its start) — verified identically across two separate end-to-end test runs. An additional message was observed in `virt-v2v-in-place`'s output: `This guest requires UEFI on the target to boot` -- i.e. whatever eventually boots this guest (e.g. a Nova instance) needs to use UEFI firmware, not legacy BIOS. **RISK, read before using**: `virt-v2v-in-place` modifies `TARGET_DEVICE` in place with **no backup of the pre-conversion state**. Per upstream's own manual page: "If virt-v2v-in-place fails it will return a non-zero (error) exit code. In this case, the disk image will be in an unknown, possibly corrupted state." This script's `log_warn` surfaces this risk at the start of this mode's run, but cannot mitigate it -- there is no way to "undo" an in-place conversion. Only use this mode against a freshly provisioned, disposable target volume that you can recreate from the source image again if something goes wrong. |

### 3.1 Progress output

`qemu-img convert -p` and `virt-v2v-in-place`'s disk-copy step both report
progress using `\r` (carriage return) to repeatedly overwrite the same
line — correct behavior for a real interactive terminal, but `kubectl
logs` and most other non-interactive log viewers don't interpret `\r` as
"go back to the start of the line". The raw output looks like this when
captured (this is an actual excerpt from early testing, truncated):

```
    (0.00/100%)\r    (1.00/100%)\r    (2.00/100%)\r    (3.00/100%)\r    ...
    (97.03/100%)\r    (98.03/100%)\r    (99.03/100%)\r    (100.00/100%)
```

— hundreds of these fragments concatenated into one unreadable block, with
no clear indication of how long each step took or whether the job is
actually progressing or stuck.

By default (`PROGRESS_MODE=clean`), the script pipes this output through a
filter (implemented in Python, no extra image dependencies) that converts
it into a handful of clean, timestamped lines instead, flushed as they
happen rather than batched. Actual output from a verified test run of
`CONVERT_MODE=direct` against a 16GiB-virtual/~1.7GiB-actual Fedora
OVF-directory source (note the `elapsed` values climbing in step with real
wall-clock time — confirmed by polling `kubectl logs` mid-run and seeing
new lines appear progressively, not all at once at the end):

```
  progress:   9.0% complete (elapsed 1s)
  progress:  19.0% complete (elapsed 1s)
  progress:  29.0% complete (elapsed 3s)
  progress:  39.0% complete (elapsed 8s)
  progress:  49.0% complete (elapsed 13s)
  progress:  59.1% complete (elapsed 18s)
  progress:  69.1% complete (elapsed 23s)
  progress:  79.1% complete (elapsed 29s)
  progress:  89.1% complete (elapsed 32s)
  progress:  99.1% complete (elapsed 37s)
  progress: 100.0% complete (elapsed 37s)
```

Getting this right required fixing a real buffering bug during
development: `qemu-img`'s progress updates end in `\r`, not `\n`, and
`stdbuf -oL` ("line buffered") only flushes output on `\n` — so the
`\r`-terminated progress text sat in a buffer and all got flushed at once
when the process exited, making every line show the *same* `elapsed`
value (e.g. eleven lines all stamped "elapsed 37s") and appear in
`kubectl logs` only once the Job was already finished. The fix was to use
`stdbuf -o0 -e0` (fully unbuffered, not just line-buffered) on the wrapped
command, and to have the Python filter read via `os.read()` on the raw
file descriptor instead of Python's own buffered `sys.stdin.read()` on the
consuming side.

A new update is printed whenever progress advances by
`PROGRESS_STEP_PERCENT` (default `10`) **or** `PROGRESS_INTERVAL_SECONDS`
(default `15`) have elapsed since the last update, whichever comes first
— so you always see *some* activity at least every 15 seconds even during
a slow stretch, without being flooded by every single tick. Any
non-progress output from the wrapped command (e.g. `qemu-img info`'s
normal output) passes through unchanged. The wrapped command's actual
exit status is preserved and still fails the script on error — the filter
never swallows a failure.

If you're running the script directly in an interactive terminal (not as a
Kubernetes Job) and want the native live-updating progress bar instead,
set `PROGRESS_MODE=raw`.

`virt-v2v-in-place`'s own early-stage output (before its disk-copy step
that produces the `(NN/100%)`-style progress shown above) uses a
different, already line-oriented format — timestamped stage descriptions
like `[  27.1] Opening the source`, one per line, not `\r`-delimited — so
it passes through the filter unchanged and reads fine in `kubectl logs`
as-is; no special-casing was needed for it. Verified in testing (actual
excerpt from a real run):

```
[   0.0] Setting up the source: -i disk /dev/xvda
[   1.1] Opening the source
[  33.6] Checking filesystem integrity before conversion
[  40.7] Detecting if this guest uses BIOS or UEFI to boot
[  48.5] Inspecting the source
[  74.6] Detecting the boot device
[  74.8] Checking for sufficient free disk space in the guest
[  74.8] Converting Windows Server 2019 Standard Evaluation (win2k19) to run on KVM
```

Once `virt-v2v-in-place` reaches its actual disk-remapping/copy step, its
progress reverts to the same `\r`-delimited `(NN/100%)`-style ticks as
`qemu-img`, and is cleaned up the same way.

---

## 4. Running an Import

0. **Inspect the source first and confirm whether it has more than one disk.**
   This applies to `.ova` archives in particular, since you cannot see
   their contents by just looking at the file (it's a single opaque tar
   file) — unlike an OVF directory, where `ls *.vmdk` shows you everything
   immediately. Run this *before* writing or applying `job.yaml`:
   ```bash
   tar -tf source.ova
   ```
   - **If this lists exactly one `.vmdk`:** single-disk source, proceed
     normally — `DISK_SELECTOR` is not required.
   - **If this lists more than one `.vmdk`:** multi-disk source. Do **not**
     apply `job.yaml` without `DISK_SELECTOR` set — the script will fail
     fast and refuse to guess, but it's better to know this up front and
     plan for one Job run per disk rather than discover it after setup.
     See [Section 4.2](#42-importing-a-multi-disk-source-more-than-one-vmdk)
     for the full multi-disk procedure, including how to match each
     `.vmdk` to its disk via the `.ovf` descriptor when filenames alone
     aren't conclusive.
   - For an OVF directory instead of an `.ova`, the equivalent check is
     simply `ls /path/to/the/ovf-directory/*.vmdk`.
1. Build/pull/tag/push the image (Section 1) and update `image:` in
   `job.yaml`.
2. Edit the PVC's `resources.requests.storage` and `storageClassName`
   (Section 2.2).
3. Edit the Job's environment variables and volumes as needed:
   - `SOURCE_IMAGE_PATH` (default) — update the `source-input` `hostPath`
     volume's `path` to point at your source file/directory on the node,
     and update `SOURCE_IMAGE_PATH`'s `value` to match (see
     [Section 4.1](#41-using-sourceimageurl-instead-of-a-local-file) to
     switch to a URL instead).
   - `TARGET_DEVICE` (and matching `volumeDevices[].devicePath`)
   - `CONVERT_MODE` (Section 3) — `direct` for Linux, `direct-v2v` for
     Windows
   - `DISK_SELECTOR`, if step 0 found more than one `.vmdk`
4. Apply:
   ```bash
   kubectl apply -f job.yaml
   ```
5. Watch progress:
   ```bash
   kubectl get pvc vm-import-target-pvc
   kubectl get pods -l job-name=vm-import-job
   kubectl logs -f -l job-name=vm-import-job
   ```
6. Confirm completion:
   ```bash
   kubectl get job vm-import-job
   # STATUS should show Complete, COMPLETIONS 1/1
   ```

### 4.1 Using `SOURCE_IMAGE_URL` instead of a local file

The shipped `job.yaml` defaults to reading a source image already present
on a node via `SOURCE_IMAGE_PATH` + a `hostPath` volume (`source-input`),
because that's what was verified end-to-end in testing. If your source
image is instead reachable over HTTP(S) and you'd rather not copy it onto a
node first, replace the `SOURCE_IMAGE_PATH` env var with `SOURCE_IMAGE_URL`
and remove the `source-input` volume and volume mount:

```yaml
env:
  - name: SOURCE_IMAGE_URL
    value: "https://example.com/path/to/source.ova"
  # ... (remove the SOURCE_IMAGE_PATH entry)
# ...
volumeMounts:
  - name: workdir
    mountPath: /work
  # (remove the source-input volumeMount)
# ...
volumes:
  - name: vm-import-target
    persistentVolumeClaim:
      claimName: vm-import-target-pvc
      readOnly: false
  - name: workdir
    emptyDir:
      sizeLimit: 8Gi
  # (remove the source-input volume)
```

This path was not exercised in testing (testing always used a
locally-mounted file), but the script's download step
(`curl -fL --retry 3 ...`) is straightforward; treat the URL as untrusted
input per the security note in [README.md](./README.md#notes-and-limitations).

Note: a `hostPath` volume (as used by the default `SOURCE_IMAGE_PATH`
setup) ties the Pod to whichever node has that path, implicitly via
scheduling — add `nodeName`/`nodeSelector` if you need to pin it
explicitly in a multi-node cluster. `SOURCE_IMAGE_URL` has no such
constraint since the Job downloads the file itself.

### 4.2 Importing a multi-disk source (more than one `.vmdk`)

This tool imports **one disk per Job run**. A multi-disk VM (an OVF
directory or `.ova` archive referencing more than one `.vmdk`) needs one
`job.yaml` apply per disk, each with:

- its own target PVC (a disk needs its own Cinder volume; you can't write
  two disks onto one volume)
- `DISK_SELECTOR` set explicitly to that disk's `.vmdk` filename. **This
  is required, not optional, when there's more than one `.vmdk`**: with
  exactly one found, the script uses it automatically; with more than one
  found and `DISK_SELECTOR` unset, the script fails immediately with an
  error listing every candidate, rather than guessing — alphabetical sort
  order is not guaranteed to match the VM's actual disk numbering or boot
  order, so there's no safe default to fall back to.

You should already know whether your source is multi-disk from
[Section 4, step 0](#4-running-an-import) (`tar -tf source.ova`, or
`ls *.vmdk` for an OVF directory) — do that check first, before setting
up anything below.

Note: under `CONVERT_MODE=direct-v2v`, `DISK_SELECTOR` only affects the
direct-write phase; the subsequent `virt-v2v-in-place` step always
operates on whatever ends up on `TARGET_DEVICE` as a single disk.

#### Matching each `.vmdk` to its disk, when filenames alone aren't enough

If a source's `.vmdk` filenames don't make it obvious which is the
boot/primary disk and which are secondary/data disks, read the `.ovf`
descriptor's `<DiskSection>` and `<References>` blocks, which map each
disk's capacity to its backing file. For a loose `.ovf` file, just read it
directly; for one packed inside an `.ova`, extract only the small XML
descriptor (not the large `.vmdk` payloads) using tar's `-O` flag to print
it to stdout:
  ```bash
  tar -xOf source.ova name-of-the.ovf | grep -A3 'DiskSection\|References'
  ```
  (Find the `.ovf`'s exact filename inside the archive first with
  `tar -tf source.ova` — it's usually the same base name as the `.ova`
  itself, e.g. `myvm.ova` → `myvm.ovf`.)

Example for a real two-disk OVF directory encountered in testing
(`fedora-uefi-1.vmdk` as the 16GiB boot disk, `fedora-uefi-2.vmdk` as a
10GiB secondary disk) — apply this once per disk, changing the PVC name,
Job name, and `DISK_SELECTOR` each time:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: vm-import-disk1-pvc        # unique per disk
spec:
  accessModes:
    - ReadWriteOnce
  volumeMode: Block
  resources:
    requests:
      storage: 20Gi                # >= this disk's virtual size

---
apiVersion: batch/v1
kind: Job
metadata:
  name: vm-import-disk1-job        # unique per disk
spec:
  backoffLimit: 1
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: vm-import
          image: jonzhang/vm-import:latest
          imagePullPolicy: IfNotPresent
          env:
            - name: SOURCE_IMAGE_PATH
              value: "/input"                  # the OVF directory
            - name: DISK_SELECTOR
              value: "fedora-uefi-1.vmdk"       # this disk, explicitly
            - name: TARGET_DEVICE
              value: "/dev/xvda"
            - name: CONVERT_MODE
              value: "direct"
          volumeDevices:
            - name: vm-import-target
              devicePath: /dev/xvda
          volumeMounts:
            - name: workdir
              mountPath: /work
            - name: source-input
              mountPath: /input
              readOnly: true
      volumes:
        - name: vm-import-target
          persistentVolumeClaim:
            claimName: vm-import-disk1-pvc     # matches the PVC above
            readOnly: false
        - name: workdir
          emptyDir:
            sizeLimit: 4Gi
        - name: source-input
          hostPath:
            path: /path/on/the/node/ovf-directory
            type: Directory
```

Repeat with `vm-import-disk2-pvc`/`vm-import-disk2-job`,
`DISK_SELECTOR=fedora-uefi-2.vmdk`, and a `storage` size matching disk 2's
virtual size, and so on for additional disks.

Verified end-to-end: this exact pattern was run against a real two-disk
OVF directory — `fedora-uefi-1.vmdk` (16GiB virtual, ~1.7GiB actual) and
`fedora-uefi-2.vmdk` (10GiB virtual, ~220KiB actual, i.e. almost entirely
sparse). Two separate Job runs, each with its own PVC and `DISK_SELECTOR`,
both completed successfully (`CONVERT_MODE=direct`, `VERIFY_CHECKSUM=true`
passing on both) in well under 5 minutes combined. Also verified: running
the same Job against this directory with `DISK_SELECTOR` deliberately
*omitted* correctly failed fast with:
```
ERROR: multiple .vmdk files found in directory /input but DISK_SELECTOR
is not set -- refusing to guess which one to import. Set DISK_SELECTOR to
one of the filenames below (basename only, e.g. DISK_SELECTOR=disk1.vmdk),
and run this Job again once per disk for a multi-disk source:
    /input/fedora-uefi-1.vmdk
    /input/fedora-uefi-2.vmdk
```

After both Jobs complete, you'll have two independent Cinder volumes, one
per disk. If you intend to boot a Nova instance from this multi-disk VM,
attach both volumes to it (typically the one converted from the first/boot
disk as the root/boot volume, and the other(s) as additional data
volumes) — this tool only handles getting each disk's data into its own
Cinder volume; attaching multiple volumes to a single instance and getting
the guest OS to recognize them in the right order is outside its scope.

---

## 5. Verifying the Result

After the Job completes, the source Pod/container is gone, so to inspect the
written block device, run a short-lived verification Pod against the same
PVC (read-only is sufficient):

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: vm-import-verify
spec:
  restartPolicy: Never
  containers:
    - name: verify
      image: <same tool image>
      command: ["/bin/bash", "-c", "sleep 300"]
      volumeDevices:
        - name: vm-import-target
          devicePath: /dev/xvda
  volumes:
    - name: vm-import-target
      persistentVolumeClaim:
        claimName: vm-import-target-pvc
        readOnly: true
```

Then:

```bash
kubectl apply -f verify-pod.yaml
kubectl exec vm-import-verify -- fdisk -l /dev/xvda
kubectl exec vm-import-verify -- lsblk /dev/xvda
kubectl delete pod vm-import-verify
```

A successful import shows a valid partition table (GPT or MBR) matching the
source OS's expected layout — e.g. `BIOS boot` + `Linux filesystem` + `Linux
LVM` for a traditional BIOS/LVM install, or `BIOS boot` + `EFI System` +
`Linux extended boot` + `Linux root` for a GPT/UEFI install such as Fedora
CoreOS, or (for a Windows guest converted via `CONVERT_MODE=direct-v2v`) a
"Windows recovery environment" partition + `EFI System` + `Microsoft
reserved` + `Microsoft basic data` (the latter being the main NTFS system
partition). A `GPT PMBR size mismatch` warning, or a separate `The backup
GPT table is not on the end of the device` warning, from `fdisk`/`sgdisk`
is expected and harmless when the target device (the PVC) is larger than
the source disk (the GPT backup header was written for the original,
smaller device, and ends up short of the actual end of the larger target
device); it does not indicate corrupted data. Verified in testing: for a
90GiB-virtual Windows source imported onto a 95Gi target PVC,
`fdisk -l`/`parted` reported this exact warning while still showing a
fully correct, readable partition table — and reading the main NTFS
partition's first bytes directly confirmed a valid `NTFS` boot-sector
signature. If you want the backup GPT table relocated to match the
larger device, boot/mount the guest once and let its own disk-management
tooling do it (e.g. Windows' Disk Management, or `growpart`/`sgdisk -e` on
Linux) — this tool does not do this automatically, since it would mean
modifying guest-internal metadata that's otherwise outside its scope.

---

## 6. Cleanup

```bash
kubectl delete -f job.yaml
```

Whether this deletes the backing storage volume (Cinder volume / Ceph RBD
image) depends on the StorageClass's `reclaimPolicy`:

- `Delete` (default for most CSI drivers): the volume is deleted with the
  PVC.
- `Retain`: the volume survives PVC deletion; use this if you intend to boot
  an instance from the imported disk or otherwise reuse it outside
  Kubernetes.

To keep an imported volume, either set `persistentVolumeReclaimPolicy:
Retain` on the resulting PV before deleting the PVC, or detach it from
Kubernetes management first.

---

## 7. Troubleshooting

| Symptom | Likely cause | Fix |
|---|---|---|
| `PVC` stuck `Pending` with event `ExternalProvisioning: Waiting for a volume to be created by the external provisioner '...'` | `storageClassName` (explicit or the cluster default) references a `provisioner` with no matching CSI driver actually running in the cluster | Run `kubectl get storageclass` to see the `PROVISIONER` for the StorageClass in use, then `kubectl get csidrivers` to confirm that provisioner is registered. If it isn't, either deploy that CSI driver, or point `storageClassName` at a StorageClass backed by a driver that *is* registered (see [Section 2.1](#21-choosing-a-storageclass)) |
| Pod stuck `Pending` with event `pod has unbound immediate PersistentVolumeClaims` | The PVC it depends on isn't `Bound` yet (see the `PVC` row above for the usual cause) | Fix the underlying PVC issue first; the Pod will schedule once the PVC binds |
| `required environment variable TARGET_DEVICE is not set` | Missing/misspelled env var | Check `env:` list in `job.yaml` |
| `target device ... does not exist` | `TARGET_DEVICE` doesn't match `volumeDevices[].devicePath`, or PVC isn't in `Block` mode | Align the two paths; confirm `volumeMode: Block` on the PVC |
| `source disk (...) is larger than target device (...)` | PVC too small for the source disk's virtual size | Increase `resources.requests.storage` on the PVC and re-run |
| `TARGET_DEVICE is not all-zero at byte offset ...; refusing to proceed` | The target device has data from a previous write (e.g. a reused test PVC) — `--target-is-zero` would be unsafe and could silently corrupt the output | Provision a fresh volume instead of reusing one that has been written to before. There is no mode that skips this check — do **not** work around it by disabling the check; the whole point of this approach's speed depends on the destination genuinely being zero |
| Pod evicted with event `Usage of EmptyDir volume "workdir" exceeds the limit` | `workdir`'s `emptyDir.sizeLimit` was sized too small for the downloaded/extracted source image itself | Neither `CONVERT_MODE` value needs room for a full intermediate RAW file, but `workdir` still needs room for the downloaded/extracted compressed source image (e.g. the extracted `.vmdk` from an `.ova`) — don't shrink `workdir` all the way to near-zero; a few GiB of headroom is typically enough for a `streamOptimized` VMDK, but verify against your actual source image's *actual* (not virtual) size |
| `a bare .ovf file was provided but it has no embedded disk` | Source is a loose `.ovf` without its referenced `.vmdk` reachable, and/or missing other referenced files (e.g. an installer `.iso`) | Point `SOURCE_IMAGE_PATH` (preferred) or `SOURCE_IMAGE_URL` at the directory containing the `.ovf`/`.vmdk` pair, or use the full `.ova` archive instead |
| `multiple .vmdk files found in ... but DISK_SELECTOR is not set` | The source (an OVF directory or `.ova` archive) has more than one disk and `DISK_SELECTOR` wasn't set — the script deliberately refuses to guess which one to import | Set `DISK_SELECTOR` to one of the filenames listed in the error (basename only), and run the Job again once per disk — see [Section 4.2](#42-importing-a-multi-disk-source-more-than-one-vmdk) for the full procedure and how to list a source's `.vmdk` filenames beforehand without running anything |
| Job fails with no visible error message in `kubectl logs` beyond the last `INFO`/`DEBUG` line before it stopped | Fixed in this script: `log_error()` used to write to stdout, which meant that when a failure happened inside a function called via command substitution (e.g. `DISK_IMAGE="$(select_sole_vmdk ...)"`), the error text was captured into the variable instead of being displayed — only the (non-zero) exit code made it through, with no visible explanation. `log_error`/`log_warn` now write to stderr, so this can't happen going forward | If you see this on an image you built yourself, make sure it's using the current `convert-and-import.sh` from this directory (`log_error`/`log_warn` should redirect to `>&2`) |
| Job fails immediately with a registry/image pull error | Image not pushed, wrong tag, or missing registry credentials on the cluster | Re-verify with `docker pull <image>` from a node, check `imagePullSecrets` if the registry requires auth |
| Throughput much lower than expected | Underlying storage backend (Ceph/Cinder backend) is the bottleneck, not the script | Expected; `qemu-img convert` is sequential I/O bound by the storage backend. Observed throughput in testing: ~32-94 MB/s depending on backend and disk size. |
| `qemu-img convert` directly to a block device stalls for minutes with no progress | qemu-img cannot "punch holes" in a block device the way it can in a file; without `--target-is-zero` it falls back to writing real zero bytes for every unallocated (sparse) region of the source, which for a mostly-empty `streamOptimized` VMDK is most of the disk | This script's write already passes `-n --target-is-zero` after verifying the target reads as zero — don't disable that. If writing to a block device without this flag for some other reason, expect multi-minute stalls on sparse sources; this is a documented upstream qemu-img behavior (see Section 3), not something fixable via more CPU/memory — confirmed in testing via `cgroup cpu.stat` showing `nr_throttled: 0` (zero CPU throttling) while the write thread sat in the kernel's `submit_bio_wait` for 4+ minutes with no additional bytes written. |
| `line NNN: cmp: command not found` during the zero-read check, immediately before a (spurious) `TARGET_DEVICE is not all-zero` failure | The image is missing `cmp` (normally provided by `diffutils`) — this happened once during development when the Dockerfile's package list was trimmed down from an earlier, larger version without checking every script dependency against it | Already fixed in this script: the zero-check now uses `python3` (bundled in the image for other reasons) instead of shelling out to `cmp`, removing the dependency entirely. If you see this on an image you built yourself, make sure it's using the current `convert-and-import.sh` and `Dockerfile` from this directory, not a locally patched/cached copy |
| Progress lines (`progress: NN.N% complete ...`) all show the **same** `elapsed` value and only appear in `kubectl logs` right as (or after) the Job finishes, instead of appearing gradually while it runs | `qemu-img`'s progress output ends in `\r`, not `\n`; `stdbuf -oL` ("line buffered") only flushes on `\n`, so `\r`-terminated output stayed trapped in a buffer until the process exited and flushed everything at once — this happened once during development (confirmed via a real run where a 37-second conversion produced 11 progress lines all stamped "elapsed 37s", i.e. all flushed simultaneously at exit) | Already fixed in this script: `run_with_progress` now uses `stdbuf -o0 -e0` (fully unbuffered, not just line-buffered) so every write is flushed immediately regardless of whether it ends in `\r` or `\n`; the Python filter also reads via `os.read()` on the raw file descriptor rather than Python's buffered `sys.stdin.read()`, for the same reason on the consuming side. If you see this on an image you built yourself, make sure it's using the current `convert-and-import.sh` from this directory |
| `unknown CONVERT_MODE '...' (expected 'direct' or 'direct-v2v')` | Typo in `CONVERT_MODE`, or using an older image built before `direct-v2v` support was added | Check spelling; if using a self-built image, rebuild from the current `Dockerfile`/`convert-and-import.sh` in this directory |
| `CONVERT_MODE=direct-v2v` is extremely slow (many minutes for a large disk) | No real KVM acceleration available to the Pod — `virt-v2v-in-place`'s internal appliance VM fell back to software/tcg emulation | Confirm the container has `securityContext.privileged: true` and `env.LIBGUESTFS_BACKEND_SETTINGS=qemu_options=-enable-kvm` set (see [Section 2.3.1](#231-enabling-kvm-acceleration-for-virt-v2v-in-place-convert_modedirect-v2v)) — both are required together for KVM acceleration, and a missing one silently falls back to TCG with no error. Re-run with `ENABLE_DEBUG=true` and check the Pod logs for `libguestfs: qemu KVM: enabled` and the appliance kernel log reporting `Hypervisor detected: KVM` to confirm acceleration is actually active (verified in testing: ~4 minutes total for a 90GiB Windows disk *with* KVM acceleration vs. ~6.2-6.3 minutes *without* it). A `devices.kubevirt.io/kvm` resource request is not needed and not checked here — it has no effect on whether KVM acceleration works once `privileged` is set (see Section 2.3.1) |
| `virt-v2v-in-place: warning: there are no virtio drivers available for this version of Windows ... The guest will be configured to use slower emulated devices` | The `Dockerfile` in this directory now bundles `/usr/share/virtio-win/virtio-win.iso`, so this warning should no longer appear in normal operation. If you still see it, the image was built before this ISO was added, or a custom `Dockerfile` variant dropped it — rebuild from the current `Dockerfile` in this directory | Not an error either way; the conversion still succeeds and produces a bootable guest, just configured with slower emulated devices instead of VirtIO ones if the ISO is genuinely missing. Confirm the ISO is present with `docker run --rm --entrypoint /bin/bash <image> -c 'ls -la /usr/share/virtio-win/'` |
| `No root device found in this operating system image` from `virt-v2v-in-place` | The guest OS is not recognized by libguestfs's inspection — known to affect OSTree/Ignition-based Linux distributions (e.g. Fedora CoreOS) | Use `CONVERT_MODE=direct` instead for these guests — it doesn't need libguestfs inspection to succeed |
| Job fails partway through `CONVERT_MODE=direct-v2v`'s `virt-v2v-in-place` phase, leaving `TARGET_DEVICE` in an uncertain state | This is the documented risk of in-place conversion — see [Section 3](#3-choosing-a-conversion-mode). `virt-v2v-in-place` has no backup/rollback mechanism | Delete the PVC and target volume, provision a fresh one, and re-run the import from the original source image. Do not attempt to boot or otherwise rely on a target device left in this state; there is no reliable way to determine how far the in-place conversion got before failing |
| `DISK_SELECTOR` set but appears to have no effect under `CONVERT_MODE=direct-v2v` | Expected — the `virt-v2v-in-place` phase operates on the whole `TARGET_DEVICE` as a single disk; it has no per-`.vmdk` selection mechanism like the direct-write phase does. The script logs a warning (not an error) when this is detected | `DISK_SELECTOR` still affects the direct-write phase under this mode. If you need to import only one disk of a multi-disk Windows source and apply driver injection to it specifically, this isn't currently supported by this script in a single Job run |
