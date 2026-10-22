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
`virt-v2v`, `libguestfs`, and their dependencies; verified at ~50 seconds
on a reasonably fast connection. Final image size is around 2GB — most of
that is `libguestfs`'s dependency chain (it bundles a full QEMU/KVM stack
to run its internal "appliance" VM, used for guest filesystem inspection
and driver injection). This is a real tradeoff versus the ~600MB image
from before `virt-v2v` support was added back, but it's required: without
`libguestfs`/`virt-v2v`, Windows guest imports aren't possible at all (see
[Section 3](#3-choosing-a-conversion-mode)).

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
| `spec.volumeMode` | **Must be `Block`.** This is what causes the CSI driver to expose the volume as a raw block device (e.g. `/dev/xvda`) inside the Pod instead of a mounted filesystem — required for `dd` to write a full disk image. Omitting this (or leaving the default `Filesystem`) will cause `dd` to corrupt the filesystem rather than lay down a disk image. |
| `spec.storageClassName` | Defaults to `csi-sc-cinderplugin` (this project's standard example name for the Cinder CSI driver — see [Section 2.1](#21-choosing-a-storageclass)). Update it to your cluster's actual Cinder StorageClass name, or delete the field to use the cluster's default StorageClass instead. |
| `spec.resources.requests.storage` | **Must be ≥ the virtual size of the source disk once converted to RAW.** Check with `qemu-img info <source-image>` → `virtual size`. The conversion script also checks this at runtime and fails early (before writing) if the RAW image is larger than the target device, so an undersized PVC fails safely rather than silently truncating data. |

### 2.2.1 PersistentVolumeClaim (workdir scratch space)

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: vm-import-workdir-pvc
spec:
  accessModes:
    - ReadWriteOnce
  volumeMode: Filesystem
  storageClassName: csi-sc-cinderplugin
  resources:
    requests:
      storage: 110Gi
```

A second PVC, mounted as the Job's `workdir` (ordinary `Filesystem` mode,
not `Block` — this one is mounted as a regular directory, not exposed as a
raw device). Needed by `CONVERT_MODE=qemu-img` and `CONVERT_MODE=virt-v2v`,
both of which must materialize a full intermediate RAW file (roughly the
source disk's *virtual* size) before `dd`-ing it onto the target device —
`qemu-img-direct` never writes this intermediate file and doesn't actually
need the space, but the PVC is still provisioned by default for
simplicity and because switching `CONVERT_MODE` later doesn't require
editing the volumes section again.

This exists as a real PVC (backed by whatever storage class resolves,
Cinder/Ceph RBD/etc.) rather than the node's local disk via `emptyDir`
because the local-disk approach doesn't scale to large source disks: a
Job's `emptyDir` is backed by whatever local filesystem partition the node
happens to have free space on — commonly a constrained partition like
`/var/lib/kubelet`, not the node's full disk — and Windows system disks in
particular are routinely 60-100+ GiB of *virtual* size even when their
actual/compressed data is much smaller. Verified in testing: a
90GiB-virtual/~5.2GiB-actual Windows Server 2019 source could not fit this
test cluster's node-local `emptyDir` (only ~47GiB free on the relevant
partition) but converted successfully once `workdir` was switched to this
PVC, backed by a Ceph RBD pool with ~1TiB free.

| Field | Meaning |
|---|---|
| `spec.volumeMode` | `Filesystem` (the default, but shown explicitly here) — unlike the target PVC above, this one is mounted as an ordinary directory (`/work` in the container), not exposed as a raw block device. |
| `spec.resources.requests.storage` | Should be roughly **2x** the source disk's virtual size: one copy for the intermediate RAW file, plus headroom for the downloaded/extracted source image itself. Verified in testing with `110Gi` against a 90GiB-virtual-size Windows source disk. |

If you know you'll only ever use `CONVERT_MODE=qemu-img-direct` (no
intermediate RAW file) with source images small enough to comfortably fit
your scheduling node's local free disk space, you can skip provisioning
this PVC entirely and use a plain `emptyDir` for `workdir` instead — see
the comments in `job.yaml` for the exact substitution.

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
              value: "qemu-img-direct"
            - name: DD_BLOCK_SIZE
              value: "4M"
            - name: VERIFY_CHECKSUM
              value: "false"
          resources:
            requests:
              cpu: "500m"
              memory: "1Gi"
              # devices.kubevirt.io/kvm: "1"   # uncomment for CONVERT_MODE=virt-v2v
            limits:
              cpu: "2"
              memory: "2Gi"
              # devices.kubevirt.io/kvm: "1"   # uncomment for CONVERT_MODE=virt-v2v
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
          persistentVolumeClaim:
            claimName: vm-import-workdir-pvc
            readOnly: false
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

The `devices.kubevirt.io/kvm` resource (commented out above) only matters
for `CONVERT_MODE=virt-v2v` (required for Windows guests — see
[Section 3](#3-choosing-a-conversion-mode)), which uses it for KVM
acceleration of its internal appliance VM. It's commented out by default
because requesting it unconditionally would prevent the Pod from
scheduling on clusters/nodes that don't advertise this resource at all —
most users on the `qemu-img-direct`/`qemu-img` default don't need it and
shouldn't be blocked by an unused request. Uncomment both the `requests`
and `limits` lines when you switch to `CONVERT_MODE=virt-v2v` and your
node has a KubeVirt (or equivalent) device plugin advertising
`devices.kubevirt.io/kvm` with a non-zero allocatable count. If it's
advertised but allocatable is `0` (no free device, or the plugin reports
zero capacity), `virt-v2v` still works without this resource request —
just via software emulation instead of hardware acceleration, which is
significantly slower (verified in testing: see
[Section 3](#3-choosing-a-conversion-mode) for real numbers).

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
| `volumeMounts` (for `workdir`, `source-input`) | Regular filesystem mounts for scratch space (`workdir`: downloaded/extracted source + intermediate RAW file) and, by default, the locally-mounted source image (`source-input`, backing `SOURCE_IMAGE_PATH`). Neither is used for the target device — that's `volumeDevices`, above. If you switch to `SOURCE_IMAGE_URL` instead of a local file, remove the `source-input` mount and its corresponding volume. |

#### Environment variables (consumed by `convert-and-import.sh`)

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `TARGET_DEVICE` | Yes | — | Path to the target block device inside the container, e.g. `/dev/xvda`. Must exactly match `volumeDevices[].devicePath`. The script verifies this path exists and is a block device before doing any work. |
| `SOURCE_IMAGE_PATH` | One of `SOURCE_IMAGE_PATH` or `SOURCE_IMAGE_URL` is required | — | Local filesystem path to a source image already present inside the Pod (mounted via `hostPath` or another PVC — in the shipped `job.yaml`, the `source-input` volume). Takes precedence over `SOURCE_IMAGE_URL` if both are set. **This is the default/verified path in `job.yaml`.** Point it at a specific file (e.g. `/input/disk.ova`) or, for a bare OVF directory, at the directory itself (e.g. `/input`). |
| `SOURCE_IMAGE_URL` | One of `SOURCE_IMAGE_PATH` or `SOURCE_IMAGE_URL` is required | — | HTTP(S) URL to download the source `.ova`/`.ovf`/`.vmdk` image. Downloaded into the `workdir` scratch volume. Use this only when the source image is actually reachable over HTTP(S); it is **not** used in the shipped `job.yaml` default, which reads a locally-mounted file via `SOURCE_IMAGE_PATH` instead. |
| `CONVERT_MODE` | No | `qemu-img-direct` | Conversion strategy: `qemu-img-direct`, `qemu-img`, or `virt-v2v` (required for Windows guests). See [Section 3](#3-choosing-a-conversion-mode). |
| `DD_BLOCK_SIZE` | No | `4M` | Block size passed to `dd`'s `bs=` option. Larger values (e.g. `8M`, `16M`) can improve throughput on fast storage backends at the cost of memory; `4M` is a safe general-purpose default. Only used by `CONVERT_MODE=qemu-img`/`virt-v2v` (their `dd` step); `qemu-img-direct` has no `dd` step. |
| `VERIFY_CHECKSUM` | No | `false` | If `"true"`, computes a sha256 checksum and compares it against a checksum read back from `TARGET_DEVICE`. This roughly **doubles** the total I/O time (every byte is read back from the target volume), so use it for validation runs, not routine production imports. Under `CONVERT_MODE=qemu-img-direct`, enabling this creates a temporary RAW file (removed immediately after comparison) since `qemu-img convert` cannot stream RAW output to a pipe — this temporarily reintroduces the scratch-space cost that mode otherwise avoids. |
| `ZERO_CHECK_SAMPLE_BYTES` | No | `104857600` (100MiB) | Only used by `CONVERT_MODE=qemu-img-direct`. Number of bytes sampled from the start, middle, and end of `TARGET_DEVICE` to verify it reads back as all-zero before trusting `--target-is-zero`. Increase for extra assurance on very large or suspect volumes, at the cost of a slightly longer check (three reads of this size). |
| `WORK_DIR` | No | `/work` | Scratch directory for the downloaded/extracted source image (and, under `CONVERT_MODE=qemu-img`/`virt-v2v`, the intermediate RAW file). Should point at a volume with enough free space — only the source image's size under the default `qemu-img-direct`, or ~2x the virtual disk size under `qemu-img`/`virt-v2v`. |
| `KEEP_WORK_DIR` | No | `false` | If `"true"`, skips cleanup of `WORK_DIR` on success — useful for debugging a failed run without re-downloading/re-converting. |
| `DISK_SELECTOR` | Required if the source has more than one `.vmdk` **and** `CONVERT_MODE` is `qemu-img-direct`/`qemu-img`; optional otherwise | — | Filename (basename only, e.g. `disk1.vmdk`) to pick a specific disk when an `.ova` archive or OVF directory contains more than one `.vmdk`. With exactly one `.vmdk` found, it's used automatically and `DISK_SELECTOR` isn't needed. With more than one found and `DISK_SELECTOR` unset, the script **fails immediately** with an error listing every candidate's full path, rather than guessing (see [Section 4.2](#42-importing-a-multi-disk-source-more-than-one-vmdk)). **Has no effect under `CONVERT_MODE=virt-v2v`** — `virt-v2v` converts every disk referenced by the source VM's definition as a single unit rather than one `.vmdk` at a time; the script logs a warning if this variable is set in that mode. |
| `ENABLE_DEBUG` | No | `false` | If `"true"`, passes `--debug` to `virt-v2v` for verbose troubleshooting output. Only relevant under `CONVERT_MODE=virt-v2v`. |
| `PROGRESS_MODE` | No | `clean` | `clean` throttles `qemu-img convert -p`'s and `dd status=progress`'s native `\r`-delimited output (which renders as an unreadable wall of text in `kubectl logs` and similar non-interactive viewers) into periodic, clean, timestamped single-line updates. Set to `raw` to disable this and see the tool's native output instead — useful when running the script directly in an interactive terminal, where `\r` correctly overwrites a single line. See [Section 3.1](#31-progress-output) for examples of the difference. |
| `PROGRESS_STEP_PERCENT` | No | `10` | Only relevant under `PROGRESS_MODE=clean`, for `qemu-img convert -p` output (which reports a percentage). Print an update whenever progress advances by at least this many percentage points since the last one. |
| `PROGRESS_INTERVAL_SECONDS` | No | `15` | Only relevant under `PROGRESS_MODE=clean`. Print an update at least this often regardless of percentage progress — this is the *only* throttling signal for `dd`'s output, since `dd status=progress` reports bytes-copied-and-rate rather than a percentage. |

#### Resource requests/limits

| Field | Meaning |
|---|---|
| `cpu` / `memory` | Standard Kubernetes resource sizing. The defaults shown (500m-2 CPU, 1-2Gi memory) were verified sufficient for both `qemu-img-direct` and `qemu-img` conversion of 10-16GiB test disks. |

#### Volumes

| Volume | Purpose |
|---|---|
| `vm-import-target` | The Block-mode PVC from [Section 2.2](#22-persistentvolumeclaim). Referenced by the container's `volumeDevices`, not `volumeMounts`. |
| `workdir` | Scratch space, PVC-backed by default (`vm-import-workdir-pvc`, see [Section 2](#2-manifest-reference-jobyaml)). Under the default `CONVERT_MODE=qemu-img-direct`, no RAW file is written here, so it only needs to hold the downloaded/extracted source image — for a `streamOptimized` VMDK (the common case), that's its *actual* (compressed) size, which can be far smaller than its virtual size. Under `CONVERT_MODE=qemu-img`/`virt-v2v`, size it to roughly 2x the expected virtual disk size (source + intermediate RAW file). A PVC is recommended here (rather than `emptyDir`) because it isn't limited by a single node's local free disk space — this matters most for `virt-v2v` with large Windows source disks; verified in testing, a 90GiB-virtual Windows source's intermediate RAW file did not fit this test cluster's node-local scratch partition (~47GiB free) but converted successfully once backed by a PVC against a storage backend with ample free space. If you know you'll only ever use `qemu-img-direct` with small sources, you can swap this for a plain `emptyDir` instead and skip provisioning the extra PVC — see the comments in `job.yaml` for the exact substitution. |

---

## 3. Choosing a Conversion Mode

Set via `CONVERT_MODE`. Three modes are supported, covering two different
concerns: (1) whether a space-saving direct write to the target block
device is possible (`qemu-img-direct` vs. the other two), and (2) whether
the guest OS needs driver injection/boot-configuration fixups to actually
boot afterward (`virt-v2v` vs. the other two — this is the Windows case).

`virt-v2v` has no way to write directly to an arbitrary pre-existing block
device: none of its output modes (`-o local`, `-o qemu`, `-o rhv-upload`,
`-o openstack`, `-o glance`) support that — they target local directories,
a management-platform REST API, or (for OpenStack) require running inside
an OpenStack-hosted "conversion appliance" VM. So `virt-v2v` always
converts to a scratch RAW file first, then `dd`s it onto the target, like
`CONVERT_MODE=qemu-img` — it cannot use the same space-saving direct-write
approach that `qemu-img-direct` uses. (virt-v2v's own disk-copy step
internally uses `nbdcopy`, which does support an equivalent
`--target-is-zero` flag and can write to a block device directly — but
that isn't exposed through any virt-v2v command-line option.) This is a
real cost (more scratch space, generally slower), but it's not a reason to
avoid `virt-v2v` when it's actually needed: for Windows guests, it's not
optional at all. `qemu-img`/`qemu-img-direct` are pure format converters —
they copy the disk's bytes from one container format to another (vmdk ->
raw) without touching anything inside the guest filesystem. A Windows
guest exported from VMware has VMware-specific drivers (and often no
virtio drivers at all), a boot configuration pointing at VMware's virtual
hardware, and (for older Windows versions) a HAL tied to that hardware —
booting this unmodified under KVM/QEMU (what Nova uses) typically fails to
boot (commonly `INACCESSIBLE_BOOT_DEVICE` or a similar STOP code) because
the guest can't find/drive its boot disk under the new virtual hardware.
`virt-v2v` fixes this: it mounts the guest filesystem, injects VirtIO
drivers (if available — see the notes under the `virt-v2v` row below),
and rewrites the boot configuration to work under KVM, in addition to
converting the disk format. Most modern Linux guests don't need this (they
already include virtio drivers and don't hardcode hypervisor-specific boot
paths), which is why `qemu-img-direct`/`qemu-img` are suitable there and
remain the default — but for Windows sources, `virt-v2v` is required.

| Mode | When to use | Requirements | Notes |
|---|---|---|---|
| `qemu-img-direct` (default) | The common case for Linux guests: scratch space is limited (no room for a full intermediate RAW file) and `TARGET_DEVICE` is freshly provisioned (reads back as all-zero) — true for any newly-created CSI volume. Handles both `.ova` archives (auto-extracts the first `.vmdk`, or a specific one via `DISK_SELECTOR`) and bare `.vmdk`/`.qcow2`/etc. files, as well as bare OVF directories. **Not suitable for Windows guests** — see `virt-v2v` below. | Target device must read back as all-zero; the script verifies this automatically (sampling start/middle/end, size controlled by `ZERO_CHECK_SAMPLE_BYTES`) and refuses to proceed if it doesn't, rather than risk corrupt output. | Writes directly to `TARGET_DEVICE` via `qemu-img convert -n --target-is-zero -O raw`, skipping any intermediate RAW file. **Why `--target-is-zero` matters:** writing to a block device *without* it is a known qemu-img pitfall — unlike a file, a block device can't be "truncated"/sparse, so qemu-img falls back to writing real zero bytes for every unallocated (hole) region of the source. For a mostly-empty `streamOptimized` VMDK (e.g. 10GiB virtual / ~1GiB actual data), this stalled for 4+ minutes with zero additional bytes written in testing (confirmed via the kernel reporting the writer thread blocked in `submit_bio_wait`, i.e. waiting on a slow/congested block I/O request, not deadlocked). Upstream (oVirt/vdsm) documented the same behavior and measured up to 250x slowdowns: https://github.com/oVirt/vdsm/commit/2bbe66274524cc009f39d3a5e5577b84144f1171. With `--target-is-zero`, the identical conversion completed in ~18.5-20 seconds in testing, producing a byte-for-byte identical result (verified via sha256) to the `qemu-img` (file + dd) path. Also verified: the safety check correctly **refuses** to run against a previously-written (non-zero) volume rather than silently producing corrupt output. Verified end-to-end against both a self-contained `.ova` archive (Fedora CoreOS, 10GiB virtual/~1GiB actual, GPT/UEFI layout) and a full OVF directory (Fedora, `.ovf`+`.vmdk`+`.nvram`+installer `.iso`, 16GiB virtual/~1.7GiB actual, BIOS/MBR+LVM layout, `SOURCE_IMAGE_PATH` pointed at the directory) — both completed in well under a minute with `VERIFY_CHECKSUM=true` passing. |
| `qemu-img` | `TARGET_DEVICE` might not read back as zero (e.g. a reused volume), or you'd rather not depend on that assumption at all. Same source-format support as `qemu-img-direct`. **Not suitable for Windows guests** — see `virt-v2v` below. | None beyond what's in the base image. Needs scratch space ≈2x the source disk's virtual size (source + RAW file). | Converts to a scratch RAW file first (`qemu-img convert -O raw`), then `dd`s it onto `TARGET_DEVICE`. Slower than `qemu-img-direct` (needs the full `dd` pass in addition to the conversion) and needs more scratch space, but has no precondition on the target device's prior contents. Verified in testing against the same two source images as `qemu-img-direct` above — both produced a correctly partitioned block device after `dd`. |
| `virt-v2v` | **Required for Windows guests**, not optional — see the explanation above for why `qemu-img`/`qemu-img-direct` aren't sufficient there. Also an option (though not required) for Linux guests that specifically need driver injection for some other reason; otherwise prefer `qemu-img-direct`/`qemu-img`, which are faster and don't need KVM. Accepts the same `.ova`/`.ovf`-directory/bare-disk-file inputs as the other modes, but has **no `DISK_SELECTOR` support** — it converts the whole VM definition (every disk referenced by the source `.ovf`/`.vmx`) as a single unit; the script logs a warning if `DISK_SELECTOR` is set in this mode. | Needs the guest OS to be recognized by libguestfs's inspection. Works significantly better with real KVM acceleration available (`/dev/kvm` on the node, typically via the KubeVirt device plugin's `devices.kubevirt.io/kvm` resource — see [Section 2.3](#23-job)); still works without it via software emulation, just much slower. Needs scratch space ≈2x the source disk's virtual size, same as `qemu-img` (no direct-write option — see the explanation above). | Converts to a scratch RAW file via `virt-v2v -i ova\|disk ... -o local -of raw` (note: output filenames are `NAME-sda`, not `NAME-sda.raw` — the script's output-file discovery accounts for this), then falls through into the same `dd`/checksum/cleanup logic used by `CONVERT_MODE=qemu-img`. **Verified end-to-end against a real Windows Server 2019 guest** (90GiB-virtual/~5.2GiB-actual `streamOptimized` VMDK, no KVM acceleration available on the test node — the appliance VM fell back to software/tcg emulation): libguestfs correctly inspected and identified it as "Windows Server 2019 Standard Evaluation (win2k19)"; `virt-v2v` logged warnings that no VirtIO drivers were available (`/usr/share/virtio-win` wasn't bundled in the test image) and that it would configure the guest with slower emulated devices instead — this is a real, if suboptimal, fallback, not a failure (see the note below). Total time: ~29 minutes (virt-v2v conversion+inspection ~6.5 min, `dd` write ~22 min at a rate that ramped from ~32 MB/s to ~75 MB/s as it progressed). The resulting block device had a correct GPT layout (a 499MiB "Windows recovery environment" partition, a 99MiB EFI System partition, a 16MiB Microsoft Reserved partition, and an 89.4GiB "Microsoft basic data" partition with a confirmed NTFS boot-sector signature at its start) — i.e. a structurally valid, bootable-looking Windows disk layout, not corrupted or empty data. Separately verified on a Linux source (before Windows availability was confirmed in testing): a Fedora Server BIOS/MBR+LVM guest converted successfully with output identical (via sha256) to the `qemu-img` path, but took ~12 minutes vs. ~90 seconds for `qemu-img` on the same 16GiB disk — again because the test node had no real KVM acceleration (`devices.kubevirt.io/kvm` allocatable was `0`) and fell back to software emulation. Also verified to correctly **fail** against a Fedora CoreOS (OSTree/Ignition-based) guest with `No root device found in this operating system image` — a known libguestfs inspection limitation for non-traditional Linux distributions; use `qemu-img-direct`/`qemu-img` for those. **VirtIO drivers were not installed in testing** because the tool image doesn't bundle the `virtio-win` driver ISO; the converted Windows guest is bootable but uses emulated (not VirtIO) devices, which will be slower at runtime than a guest with VirtIO drivers installed. If you need VirtIO drivers installed, extend `Dockerfile` to bundle Fedora's `virtio-win` ISO and make it available where `virt-v2v` looks for it by default — this was not exercised in testing. |

### 3.1 Progress output

`qemu-img convert -p` and `dd status=progress` both report progress using
`\r` (carriage return) to repeatedly overwrite the same line — correct
behavior for a real interactive terminal, but `kubectl logs` and most
other non-interactive log viewers don't interpret `\r` as "go back to the
start of the line". The raw output looks like this when captured (this is
an actual excerpt from early testing, truncated):

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
`CONVERT_MODE=qemu-img-direct` against a 16GiB-virtual/~1.7GiB-actual
Fedora OVF-directory source (note the `elapsed` values climbing in step
with real wall-clock time — confirmed by polling `kubectl logs` mid-run
and seeing new lines appear progressively, not all at once at the end):

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

And from a verified `CONVERT_MODE=qemu-img` run's `dd` step (no percentage
available from `dd`, so updates are throttled purely by elapsed time
instead, showing bytes copied and current rate):

```
  progress: 29 MB, 28 MiB copied so far, 23.4 MB/s (elapsed 70s)
  progress: 5.7 GB, 5.3 GiB copied so far, 81.3 MB/s (elapsed 145s)
```

A new update is printed whenever progress advances by
`PROGRESS_STEP_PERCENT` (default `10`, only applicable to `qemu-img`'s
percentage-based output) **or** `PROGRESS_INTERVAL_SECONDS` (default `15`)
have elapsed since the last update, whichever comes first — so you always
see *some* activity at least every 15 seconds even during a slow stretch,
without being flooded by every single tick. Any non-progress output from
the wrapped command (e.g. `qemu-img info`'s normal output, or `dd`'s final
`N+0 records in/out` summary) passes through unchanged. The wrapped
command's actual exit status is preserved and still fails the script on
error — the filter never swallows a failure.

If you're running the script directly in an interactive terminal (not as a
Kubernetes Job) and want the native live-updating progress bar instead,
set `PROGRESS_MODE=raw`.

`virt-v2v`'s own early-stage output (before the disk-copy step that
produces the `(NN/100%)`-style progress shown above) uses a different,
already line-oriented format — timestamped stage descriptions like
`[  27.1] Opening the source`, one per line, not `\r`-delimited — so it
passes through the filter unchanged and reads fine in `kubectl logs`
as-is; no special-casing was needed for it. Verified in testing (actual
excerpt from a real run):

```
[   0.0] Setting up the source: -i ova /input
[  27.1] Opening the source
[  60.6] Checking filesystem integrity before conversion
[  67.6] Detecting if this guest uses BIOS or UEFI to boot
[  75.4] Inspecting the source
[ 100.5] Detecting the boot device
[ 100.8] Checking for sufficient free disk space in the guest
[ 100.8] Converting Windows Server 2019 Standard Evaluation (win2k19) to run on KVM
```

Once `virt-v2v` reaches its actual disk-copy step, its progress reverts to
the same `\r`-delimited `(NN/100%)`-style ticks as `qemu-img`, and is
cleaned up the same way.

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
   - `CONVERT_MODE` (Section 3)
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
      sizeLimit: 40Gi
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
              value: "qemu-img-direct"
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
both completed successfully (`qemu-img-direct` mode, `VERIFY_CHECKSUM=true`
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
CoreOS, or (for a Windows guest converted via `CONVERT_MODE=virt-v2v`) a
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
| `RAW image (...) is larger than target device (...)` | PVC too small for the converted disk | Increase `resources.requests.storage` on the PVC and re-run |
| `source disk (...) is larger than target device (...)` | Same as above, but raised by `CONVERT_MODE=qemu-img-direct` (checked before writing, using the source disk's virtual size rather than a converted RAW file's size) | Increase `resources.requests.storage` on the PVC and re-run |
| `TARGET_DEVICE is not all-zero at byte offset ...; refusing to use CONVERT_MODE=qemu-img-direct` | The target device has data from a previous write (e.g. a reused test PVC) — `--target-is-zero` would be unsafe and could silently corrupt the output | Use `CONVERT_MODE=qemu-img` instead, or provision a fresh volume. Do **not** work around this by skipping the check; the whole point of `qemu-img-direct`'s speed depends on the destination genuinely being zero |
| Pod evicted with event `Usage of EmptyDir volume "workdir" exceeds the limit` while using `CONVERT_MODE=qemu-img-direct` | `workdir`'s `emptyDir.sizeLimit` was sized assuming the old (RAW-file) scratch requirement is still needed, but set too small even for the downloaded/extracted source image itself | `qemu-img-direct` doesn't need room for a RAW file, but it still needs room for the downloaded/extracted compressed source image (e.g. the extracted `.vmdk` from an `.ova`) — don't shrink `workdir` all the way to near-zero; a few GiB of headroom is typically enough for a `streamOptimized` VMDK, but verify against your actual source image's *actual* (not virtual) size |
| `a bare .ovf file was provided but it has no embedded disk` | Source is a loose `.ovf` without its referenced `.vmdk` reachable, and/or missing other referenced files (e.g. an installer `.iso`) | Point `SOURCE_IMAGE_PATH` (preferred) or `SOURCE_IMAGE_URL` at the directory containing the `.ovf`/`.vmdk` pair, or use the full `.ova` archive instead |
| `multiple .vmdk files found in ... but DISK_SELECTOR is not set` | The source (an OVF directory or `.ova` archive) has more than one disk and `DISK_SELECTOR` wasn't set — the script deliberately refuses to guess which one to import | Set `DISK_SELECTOR` to one of the filenames listed in the error (basename only), and run the Job again once per disk — see [Section 4.2](#42-importing-a-multi-disk-source-more-than-one-vmdk) for the full procedure and how to list a source's `.vmdk` filenames beforehand without running anything |
| Job fails with no visible error message in `kubectl logs` beyond the last `INFO`/`DEBUG` line before it stopped | Fixed in this script: `log_error()` used to write to stdout, which meant that when a failure happened inside a function called via command substitution (e.g. `DISK_IMAGE="$(select_sole_vmdk ...)"`), the error text was captured into the variable instead of being displayed — only the (non-zero) exit code made it through, with no visible explanation. `log_error`/`log_warn` now write to stderr, so this can't happen going forward | If you see this on an image you built yourself, make sure it's using the current `convert-and-import.sh` from this directory (`log_error`/`log_warn` should redirect to `>&2`) |
| Job fails immediately with a registry/image pull error | Image not pushed, wrong tag, or missing registry credentials on the cluster | Re-verify with `docker pull <image>` from a node, check `imagePullSecrets` if the registry requires auth |
| `dd` throughput much lower than expected | Underlying storage backend (Ceph/Cinder backend) is the bottleneck, not the script | Expected; `qemu-img convert`/`dd` are both sequential I/O bound by the storage backend. Observed throughput in testing: ~65-94 MB/s against a Ceph RBD-backed Block PVC for 10-16GiB disks. |
| `qemu-img convert` directly to a block device (`CONVERT_MODE=qemu-img-direct` without `--target-is-zero`, or any custom invocation that skips it) stalls for minutes with no progress | qemu-img cannot "punch holes" in a block device the way it can in a file; without `--target-is-zero` it falls back to writing real zero bytes for every unallocated (sparse) region of the source, which for a mostly-empty `streamOptimized` VMDK is most of the disk | This script's `qemu-img-direct` mode already passes `-n --target-is-zero` after verifying the target reads as zero — don't disable that. If writing to a block device without this flag for some other reason, expect multi-minute stalls on sparse sources; this is a documented upstream qemu-img behavior (see Section 3), not something fixable via more CPU/memory — confirmed in testing via `cgroup cpu.stat` showing `nr_throttled: 0` (zero CPU throttling) while the write thread sat in the kernel's `submit_bio_wait` for 4+ minutes with no additional bytes written. |
| `line NNN: cmp: command not found` during the zero-read check, immediately before a (spurious) `TARGET_DEVICE is not all-zero` failure | The image is missing `cmp` (normally provided by `diffutils`) — this happened once during development when the Dockerfile's package list was trimmed down from an earlier, larger version without checking every script dependency against it | Already fixed in this script: the zero-check now uses `python3` (bundled in the image for other reasons) instead of shelling out to `cmp`, removing the dependency entirely. If you see this on an image you built yourself, make sure it's using the current `convert-and-import.sh` and `Dockerfile` from this directory, not a locally patched/cached copy |
| Progress lines (`progress: NN.N% complete ...`) all show the **same** `elapsed` value and only appear in `kubectl logs` right as (or after) the Job finishes, instead of appearing gradually while it runs | `qemu-img`'s progress output ends in `\r`, not `\n`; `stdbuf -oL` ("line buffered") only flushes on `\n`, so `\r`-terminated output stayed trapped in a buffer until the process exited and flushed everything at once — this happened once during development (confirmed via a real run where an 37-second conversion produced 11 progress lines all stamped "elapsed 37s", i.e. all flushed simultaneously at exit) | Already fixed in this script: `run_with_progress` now uses `stdbuf -o0 -e0` (fully unbuffered, not just line-buffered) so every write is flushed immediately regardless of whether it ends in `\r` or `\n`; the Python filter also reads via `os.read()` on the raw file descriptor rather than Python's buffered `sys.stdin.read()`, for the same reason on the consuming side. If you see this on an image you built yourself, make sure it's using the current `convert-and-import.sh` from this directory |
| `unknown CONVERT_MODE '...' (expected 'qemu-img-direct', 'qemu-img', or 'virt-v2v')` | Typo in `CONVERT_MODE`, or using an older image built before `virt-v2v` support was added | Check spelling; if using a self-built image, rebuild from the current `Dockerfile`/`convert-and-import.sh` in this directory |
| `CONVERT_MODE=virt-v2v` is extremely slow (many minutes to hours depending on disk size) | No real KVM acceleration available to the Pod — `virt-v2v`'s internal appliance VM fell back to software/tcg emulation | Check whether the node advertises `devices.kubevirt.io/kvm` with a non-zero allocatable count (`kubectl get node <name> -o jsonpath='{.status.allocatable}'`). If it's `0` or absent, either deploy/fix the KubeVirt device plugin so the node can advertise real KVM capacity, or accept the slower software-emulation fallback — it still produces a correct result, just much slower (verified in testing: ~12 minutes for a 16GiB Linux disk, ~29 minutes total for a 90GiB Windows disk, both without KVM acceleration) |
| `virt-v2v: warning: there are no virtio drivers available for this version of Windows ... The guest will be configured to use slower emulated devices` | Expected with the stock `Dockerfile` in this directory — it doesn't bundle the `virtio-win` driver ISO | Not an error; the conversion still succeeds and produces a bootable guest, just configured with emulated (slower) devices instead of VirtIO ones. If you want VirtIO drivers installed during conversion, extend `Dockerfile` to bundle Fedora's `virtio-win` ISO (not exercised in this project's own testing) |
| `No root device found in this operating system image` from `virt-v2v` | The guest OS is not recognized by libguestfs's inspection — known to affect OSTree/Ignition-based Linux distributions (e.g. Fedora CoreOS) | Use `CONVERT_MODE=qemu-img-direct` or `CONVERT_MODE=qemu-img` instead for these guests — they don't need libguestfs inspection to succeed, only `virt-v2v` does |
| `virt-v2v did not produce a disk output file in ...` | `virt-v2v -o local` names its output file `NAME-sda` (no `.raw` suffix, even with `-of raw`) — if this error appears despite `virt-v2v` itself reporting success, the output-file discovery logic may not be matching what was actually produced | Already handled in this script (it searches for `*-sda`/`*-sda.raw`, then falls back to any non-XML file in the output directory); if you still see this on a self-built image, confirm you're using the current `convert-and-import.sh`, and if the issue persists, run with `ENABLE_DEBUG=true` and inspect the actual filename(s) `virt-v2v` left in `$WORK_DIR/raw` |
| `DISK_SELECTOR` set but appears to have no effect under `CONVERT_MODE=virt-v2v` | Expected — `virt-v2v` converts every disk referenced by the source VM's `.ovf`/`.vmx` definition as a single unit; it has no per-`.vmdk` selection mechanism like `qemu-img`/`qemu-img-direct` do. The script logs a warning (not an error) when this is detected | If you need to import only one disk of a multi-disk Windows (or other `virt-v2v`-requiring) source, this isn't currently supported by this script. Consider `CONVERT_MODE=qemu-img` against that disk's `.vmdk` directly if driver injection/boot-fixup isn't needed for that specific disk (e.g. a non-boot data disk) |
| Pod evicted or Job fails with `no space left on device` while using `CONVERT_MODE=qemu-img`/`virt-v2v` against a large source disk | `workdir`'s scratch space (PVC or `emptyDir`) is too small for the intermediate RAW file, which is roughly the source disk's *virtual* size | Increase `vm-import-workdir-pvc`'s `resources.requests.storage` (if PVC-backed) to ≥2x the source disk's virtual size, or switch from `emptyDir` to a PVC-backed `workdir` if the node's local free space can't accommodate it — verified in testing, a 90GiB-virtual Windows source's scratch requirement exceeded this test cluster's node-local `emptyDir` capacity (~47GiB free) but succeeded once backed by a 110Gi PVC |
