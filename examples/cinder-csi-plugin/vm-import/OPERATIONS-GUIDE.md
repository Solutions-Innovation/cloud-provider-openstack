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

The tool image bundles `qemu-img`, `curl`, `tar`, and `python3` on a Fedora
minimal base, with `convert-and-import.sh` baked in as the `ENTRYPOINT`.

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

Build time is dominated by `microdnf install` pulling `qemu-img` and a
handful of small dependencies; expect well under a minute on a reasonably
fast connection. Final image size is a few hundred MB.

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
            sizeLimit: 40Gi
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

The `devices.kubevirt.io/kvm` resource that older versions of this guide
mentioned is no longer relevant: `virt-v2v` support was evaluated and
dropped (see [Section 3](#3-choosing-a-conversion-mode) for why), so no
mode in the current script needs KVM access.

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
| `CONVERT_MODE` | No | `qemu-img-direct` | Conversion strategy: `qemu-img-direct` or `qemu-img`. See [Section 3](#3-choosing-a-conversion-mode). |
| `DD_BLOCK_SIZE` | No | `4M` | Block size passed to `dd`'s `bs=` option. Larger values (e.g. `8M`, `16M`) can improve throughput on fast storage backends at the cost of memory; `4M` is a safe general-purpose default. Only used by `CONVERT_MODE=qemu-img` (its `dd` step); `qemu-img-direct` has no `dd` step. |
| `VERIFY_CHECKSUM` | No | `false` | If `"true"`, computes a sha256 checksum and compares it against a checksum read back from `TARGET_DEVICE`. This roughly **doubles** the total I/O time (every byte is read back from the target volume), so use it for validation runs, not routine production imports. Under `CONVERT_MODE=qemu-img-direct`, enabling this creates a temporary RAW file (removed immediately after comparison) since `qemu-img convert` cannot stream RAW output to a pipe — this temporarily reintroduces the scratch-space cost that mode otherwise avoids. |
| `ZERO_CHECK_SAMPLE_BYTES` | No | `104857600` (100MiB) | Only used by `CONVERT_MODE=qemu-img-direct`. Number of bytes sampled from the start, middle, and end of `TARGET_DEVICE` to verify it reads back as all-zero before trusting `--target-is-zero`. Increase for extra assurance on very large or suspect volumes, at the cost of a slightly longer check (three reads of this size). |
| `WORK_DIR` | No | `/work` | Scratch directory for the downloaded/extracted source image (and, under `CONVERT_MODE=qemu-img`, the intermediate RAW file). Should point at a volume with enough free space — only the source image's size under the default `qemu-img-direct`, or ~2x the virtual disk size under `qemu-img`. |
| `KEEP_WORK_DIR` | No | `false` | If `"true"`, skips cleanup of `WORK_DIR` on success — useful for debugging a failed run without re-downloading/re-converting. |
| `DISK_SELECTOR` | Required if the source has more than one `.vmdk`; optional otherwise | — | Filename (basename only, e.g. `disk1.vmdk`) to pick a specific disk when an `.ova` archive or OVF directory contains more than one `.vmdk`. With exactly one `.vmdk` found, it's used automatically and `DISK_SELECTOR` isn't needed. With more than one found and `DISK_SELECTOR` unset, the script **fails immediately** with an error listing every candidate's full path, rather than guessing (see [Section 4.2](#42-importing-a-multi-disk-source-more-than-one-vmdk)). |
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
| `workdir` | `emptyDir` scratch space. Under the default `CONVERT_MODE=qemu-img-direct`, no RAW file is written here, so it only needs to hold the downloaded/extracted source image — for a `streamOptimized` VMDK (the common case), that's its *actual* (compressed) size, which can be far smaller than its virtual size. Under `CONVERT_MODE=qemu-img`, size it to roughly 2x the expected virtual disk size (source + intermediate RAW file). For very large disks, replace this with a dedicated scratch PVC instead of `emptyDir` if node ephemeral storage is limited. |

---

## 3. Choosing a Conversion Mode

Set via `CONVERT_MODE`. Only `qemu-img`-based modes are supported; `virt-v2v`
was evaluated and dropped because none of its output modes (`-o local`,
`-o qemu`, `-o rhv-upload`, `-o openstack`, `-o glance`) can write directly
to an arbitrary pre-existing block device — they target local directories,
a management-platform REST API, or (for OpenStack) require running inside
an OpenStack-hosted "conversion appliance" VM. That ruled out `virt-v2v`
supporting the same space-saving direct-write approach that
`qemu-img-direct` uses below. (virt-v2v's own disk-copy step internally
uses `nbdcopy`, which does support an equivalent `--target-is-zero` flag
and can write to a block device directly — but that isn't exposed through
any virt-v2v command-line option.)

| Mode | When to use | Requirements | Notes |
|---|---|---|---|
| `qemu-img-direct` (default) | The common case: scratch space is limited (no room for a full intermediate RAW file) and `TARGET_DEVICE` is freshly provisioned (reads back as all-zero) — true for any newly-created CSI volume. Handles both `.ova` archives (auto-extracts the first `.vmdk`, or a specific one via `DISK_SELECTOR`) and bare `.vmdk`/`.qcow2`/etc. files, as well as bare OVF directories. | Target device must read back as all-zero; the script verifies this automatically (sampling start/middle/end, size controlled by `ZERO_CHECK_SAMPLE_BYTES`) and refuses to proceed if it doesn't, rather than risk corrupt output. | Writes directly to `TARGET_DEVICE` via `qemu-img convert -n --target-is-zero -O raw`, skipping any intermediate RAW file. **Why `--target-is-zero` matters:** writing to a block device *without* it is a known qemu-img pitfall — unlike a file, a block device can't be "truncated"/sparse, so qemu-img falls back to writing real zero bytes for every unallocated (hole) region of the source. For a mostly-empty `streamOptimized` VMDK (e.g. 10GiB virtual / ~1GiB actual data), this stalled for 4+ minutes with zero additional bytes written in testing (confirmed via the kernel reporting the writer thread blocked in `submit_bio_wait`, i.e. waiting on a slow/congested block I/O request, not deadlocked). Upstream (oVirt/vdsm) documented the same behavior and measured up to 250x slowdowns: https://github.com/oVirt/vdsm/commit/2bbe66274524cc009f39d3a5e5577b84144f1171. With `--target-is-zero`, the identical conversion completed in ~18.5-20 seconds in testing, producing a byte-for-byte identical result (verified via sha256) to the `qemu-img` (file + dd) path. Also verified: the safety check correctly **refuses** to run against a previously-written (non-zero) volume rather than silently producing corrupt output. Verified end-to-end against both a self-contained `.ova` archive (Fedora CoreOS, 10GiB virtual/~1GiB actual, GPT/UEFI layout) and a full OVF directory (Fedora, `.ovf`+`.vmdk`+`.nvram`+installer `.iso`, 16GiB virtual/~1.7GiB actual, BIOS/MBR+LVM layout, `SOURCE_IMAGE_PATH` pointed at the directory) — both completed in well under a minute with `VERIFY_CHECKSUM=true` passing. |
| `qemu-img` | `TARGET_DEVICE` might not read back as zero (e.g. a reused volume), or you'd rather not depend on that assumption at all. Same source-format support as `qemu-img-direct`. | None beyond what's in the base image. Needs scratch space ≈2x the source disk's virtual size (source + RAW file). | Converts to a scratch RAW file first (`qemu-img convert -O raw`), then `dd`s it onto `TARGET_DEVICE`. Slower than `qemu-img-direct` (needs the full `dd` pass in addition to the conversion) and needs more scratch space, but has no precondition on the target device's prior contents. Verified in testing against the same two source images as `qemu-img-direct` above — both produced a correctly partitioned block device after `dd`. |

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
CoreOS. A `GPT PMBR size mismatch` warning from `fdisk` is expected and
harmless when the target device is larger than the source disk (the backup
GPT header was written for the original, smaller device); it does not
indicate corrupted data.

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
