# VM Image Import via Cinder CSI Block Volume

This example demonstrates how to migrate a source VM disk (OVA/OVF/VMDK) into
an OpenStack Cinder volume using a Kubernetes Job, the Cinder CSI driver's
**Block Volume** mode, and `qemu-img`/`virt-v2v`.

> For a detailed, step-by-step operations manual covering the image
> build/pull/tag/push workflow and a full reference of every YAML field and
> environment variable, see [OPERATIONS-GUIDE.md](./OPERATIONS-GUIDE.md).

## How it works

1. A `PersistentVolumeClaim` is created with `volumeMode: Block`. The Cinder
   CSI driver provisions a Cinder volume and, when attached to the Job's
   pod, exposes it as a **raw block device** (e.g. `/dev/xvda`) instead of a
   mounted filesystem. This is required because writing a disk image onto a
   filesystem-formatted PVC would corrupt it — we need the raw device.
2. A one-shot `Job` runs a container that:
   - downloads (or reads a locally mounted) source OVA/OVF/VMDK image
   - converts and writes it using one of three strategies (`CONVERT_MODE`):
     - `qemu-img-direct` (default) — `qemu-img convert -n --target-is-zero
       -O raw` straight onto the target device, skipping any intermediate
       RAW file. Needs scratch space only for the downloaded/extracted
       source image, not the (potentially much larger) virtual disk size —
       the key benefit when working with limited node ephemeral storage.
       Verified end-to-end: ~20 seconds for a 10GiB-virtual/~1GiB-actual
       `streamOptimized` VMDK, with a sha256 checksum identical to the
       `qemu-img` mode's output. The script automatically verifies the
       target device reads as all-zero before using this mode and refuses
       to proceed otherwise, since the underlying `--target-is-zero`
       assumption is unsafe on a previously-written volume. **This mode
       (and `qemu-img` below) only converts the disk's byte-for-byte
       format — it does not touch anything inside the guest filesystem,
       which is fine for most Linux guests but is NOT sufficient for
       Windows guests (see `virt-v2v` below).** See
       [OPERATIONS-GUIDE.md](./OPERATIONS-GUIDE.md#3-choosing-a-conversion-mode)
       for the full performance analysis (including why writing to a block
       device without `--target-is-zero` can stall for minutes on a sparse
       VMDK).
     - `qemu-img` — `qemu-img convert -O raw` to a scratch file, then `dd`
       that file onto the target device. Safe regardless of the target
       device's prior contents (no zero-read assumption), but needs
       scratch space roughly 2x the source disk's virtual size. Use this
       when `TARGET_DEVICE` might not read back as zero (e.g. a reused
       volume) or when you'd rather not depend on that assumption at all.
     - `virt-v2v` — **required for Windows guests.** `qemu-img`/
       `qemu-img-direct` are pure format converters: they never touch the
       guest filesystem. A Windows VM exported from VMware has
       VMware-specific drivers and a boot configuration tied to VMware's
       virtual hardware, and will typically fail to boot under KVM/QEMU
       (the hypervisor Nova uses) without driver injection and
       boot-configuration fixups — only `virt-v2v` performs these.
       Converts to a scratch RAW file first (like `qemu-img` above;
       `virt-v2v` has no way to write directly to a block device), then
       `dd`s it onto the target. Needs real KVM acceleration for
       reasonable speed (works without it, via software emulation, just
       much slower) and requires the guest OS to be recognized by
       libguestfs's inspection — Windows guests are well supported;
       OSTree/Ignition-based Linux distros (e.g. Fedora CoreOS) are
       **not** and will fail here, so use `qemu-img-direct`/`qemu-img` for
       those instead. Verified end-to-end against a real Windows Server
       2019 guest (90GiB-virtual/~5.2GiB-actual `streamOptimized` VMDK):
       libguestfs correctly identified it as "Windows Server 2019
       Standard Evaluation (win2k19)" and converted it to boot under KVM;
       the resulting Cinder/Ceph RBD volume showed a correct GPT partition
       layout (Windows RE + EFI System + Microsoft Reserved + an
       NTFS-signed main partition) after import. Also verified (on a
       Linux source, before Windows support was confirmed) against a
       Fedora Server BIOS/MBR+LVM guest, with output identical (via
       sha256) to the `qemu-img` path. See
       [OPERATIONS-GUIDE.md](./OPERATIONS-GUIDE.md#3-choosing-a-conversion-mode)
       for the full comparison and real numbers for all three modes.
   - optionally verifies a sha256 checksum of what was written
3. Once the Job completes successfully, the Cinder volume behind the PVC
   contains the imported disk and can be used to boot a Nova instance, or
   referenced directly in OpenStack (`openstack volume show <volume-id>`),
   or bound into a different Pod as a data/boot volume.

### Progress output

`qemu-img convert -p` and `dd status=progress` both report progress by
repeatedly overwriting the same terminal line with `\r` (carriage return).
That renders fine in a real, interactive terminal, but `kubectl logs` and
similar non-interactive log viewers don't interpret `\r` — the result is
hundreds of `(NN.NN/100%)` fragments smashed together into an unreadable
wall of text. By default, the script filters this down into a handful of
clean, timestamped lines printed as the operation actually progresses
(e.g. `progress:  29.0% complete (elapsed 3s)` ... `progress:  99.1%
complete (elapsed 37s)`), printed whenever progress advances by
`PROGRESS_STEP_PERCENT` (default 10) or every `PROGRESS_INTERVAL_SECONDS`
(default 15), whichever comes first.
Set `PROGRESS_MODE=raw` to disable this and see the tool's native `\r`
output instead (useful if you're running the script directly in an
interactive terminal, where the live-updating bar renders normally). See
[OPERATIONS-GUIDE.md](./OPERATIONS-GUIDE.md#31-progress-output) for actual
before/after log excerpts.

This relies entirely on functionality already provided by this project (the
Cinder CSI driver's dynamic provisioning + Block volume support, see
[Using Block Volume](../../../docs/cinder-csi-plugin/examples.md#using-block-volume)).
No changes to the driver itself are needed. `qemu-img` runs entirely inside
the Job's container, independent of the CSI driver.

## Files

- `convert-and-import.sh` — the conversion/import script, baked into the
  tool image as its `ENTRYPOINT`.
- `Dockerfile` — tool image (`qemu-img`, `virt-v2v`/`libguestfs`, `curl`,
  `tar`, `python3`) used to run the script. `virt-v2v`/`libguestfs` add
  roughly 1.5GB to the image but are required for Windows guest support
  (see `CONVERT_MODE=virt-v2v` above). Build and push it to a registry
  reachable by your cluster.
- `job.yaml` — Block-mode target PVC + PVC-backed scratch space + Job
  manifest (no `StorageClass` object; see
  [OPERATIONS-GUIDE.md](./OPERATIONS-GUIDE.md#21-choosing-a-storageclass)
  for why).

## Usage

### 1. (Optional) Build and push your own tool image

A pre-built image is already available at `jonzhang/vm-import:latest`
(referenced in `job.yaml`). To build and push your own instead:

```bash
cd examples/cinder-csi-plugin/vm-import
docker build -t jonzhang/vm-import:latest .
docker push jonzhang/vm-import:latest
```

If your build environment requires a proxy to reach package repositories,
pass it as a build arg (not baked into the final image):

```bash
docker build \
  --build-arg http_proxy=http://your-proxy:3128/ \
  --build-arg https_proxy=http://your-proxy:3128/ \
  -t jonzhang/vm-import:latest .
```

Update `image:` in `job.yaml` if you push to a different repository.

### 2. Configure the Job

Edit `job.yaml`:

- `storageClassName` on the PVC — defaults to `csi-sc-cinderplugin`, the
  name used throughout this project's examples/docs for the Cinder CSI
  driver. Update it to match whatever your cluster's Cinder StorageClass
  is actually named, and first confirm its `provisioner` has a matching
  entry in `kubectl get csidrivers` — otherwise the PVC will hang in
  `Pending` forever. Delete the field entirely to use the cluster's
  default StorageClass instead (this is what was used during this
  example's own testing, since the test cluster had no Cinder CSI driver
  and relied on a Ceph-RBD-backed default StorageClass instead). See
  [OPERATIONS-GUIDE.md](./OPERATIONS-GUIDE.md#21-choosing-a-storageclass)
  for details.
- `resources.requests.storage` on the PVC — must be **greater than or equal
  to** the virtual size of the source disk once converted to RAW.
- `env.SOURCE_IMAGE_PATH` (default) — path to the source `.ova`/`.ovf`
  directory/`.vmdk` as seen inside the container (e.g.
  `/input/my-disk.ova`), backed by the `source-input` `hostPath` volume.
  Update both the env var value and the `source-input` volume's `path` to
  point at your actual source file/directory on the node. Alternatively,
  if your source image is reachable over HTTP(S), replace this with
  `env.SOURCE_IMAGE_URL` and remove the `source-input` volume/mount — see
  [OPERATIONS-GUIDE.md](./OPERATIONS-GUIDE.md#41-using-sourceimageurl-instead-of-a-local-file).
- `env.CONVERT_MODE` — `qemu-img-direct` (default; writes straight to the
  block device, needs the target to read back as all-zero — verified
  automatically), `qemu-img` (converts to a scratch file first, then
  `dd`s it onto the device; safe on any target but needs ~2x the virtual
  disk size in scratch space), or `virt-v2v` (**required for Windows
  guests** — injects drivers and fixes up the boot configuration so the
  guest can actually boot under KVM; also converts to a scratch file
  first). See
  [OPERATIONS-GUIDE.md](./OPERATIONS-GUIDE.md#3-choosing-a-conversion-mode)
  for the full comparison.
- `env.TARGET_DEVICE` and `volumeDevices[0].devicePath` — must match (the
  device path as seen inside the container, e.g. `/dev/xvda`).
- `volumes[1]` (the `workdir` volume, PVC-backed by default) — scratch
  space. Under the default `qemu-img-direct` mode, this only needs to hold
  the downloaded/extracted source image (its *actual*, not virtual, size
  for a `streamOptimized` VMDK) — if you know you'll only ever use that
  mode and your source images are small, you can swap this for a plain
  `emptyDir` instead to avoid provisioning the extra PVC (see the comments
  in `job.yaml`). Under `qemu-img`/`virt-v2v`, it additionally needs the
  intermediate RAW file, so budget roughly 2x the virtual disk size —
  this matters most for `virt-v2v` with large Windows system disks, where
  a node-local `emptyDir` is likely to run out of space (verified in
  testing: a 90GiB-virtual Windows source did not fit this test cluster's
  node-local scratch partition, but converted successfully once `workdir`
  was backed by a 110Gi PVC instead).
- `volumes[2].hostPath.path` (the `source-input` volume) — the directory on
  the node containing your source image; must match what
  `SOURCE_IMAGE_PATH` points at under `/input`.

### 3. Run it

```bash
kubectl apply -f job.yaml
kubectl logs -f job/vm-import-job
```

### 4. Verify

```bash
kubectl get job vm-import-job
openstack volume show <volume-id-behind-vm-import-target-pvc>
```

To find the underlying Cinder volume ID:

```bash
kubectl get pv -o jsonpath='{range .items[*]}{.spec.claimRef.name}{"\t"}{.spec.csi.volumeHandle}{"\n"}{end}' | grep vm-import-target-pvc
```

### 5. Clean up

```bash
kubectl delete -f job.yaml
```

Deleting the PVC with the default StorageClass's reclaim policy will delete
the backing Cinder volume too. If you want to keep the imported volume (e.g.
to boot an instance from it), set `persistentVolumeReclaimPolicy: Retain` on
the resulting PV, or detach/orphan it from Kubernetes before deleting the PVC.

## Notes and limitations

- **Before applying `job.yaml`, check whether your source has more than
  one disk.** For an `.ova` archive, run `tar -tf source.ova` and count
  the `.vmdk` entries — you can't tell from the file alone otherwise,
  since it's a single opaque tar file. For an OVF directory, just
  `ls *.vmdk`. Do this check first; don't find out by running the Job and
  having it fail.
- **Multi-disk sources (more than one `.vmdk`):** this tool imports **one
  disk per Job run** — there is no built-in way to import all disks of a
  multi-disk VM in a single invocation. For a source with N disks, apply
  `job.yaml` N times with a different `TARGET_DEVICE`-backed PVC and a
  different `DISK_SELECTOR` each time (e.g. `DISK_SELECTOR=disk1.vmdk`,
  then `DISK_SELECTOR=disk2.vmdk`, ...). **If `DISK_SELECTOR` is not set
  and more than one `.vmdk` is found**, the script fails immediately with
  an error listing every candidate filename, rather than silently picking
  one — alphabetical filename order is not guaranteed to match the source
  VM's actual disk numbering or boot order, so guessing would risk
  importing the wrong disk without any indication something was off.
  (With exactly one `.vmdk`, no selector is needed — that one disk is
  used automatically.) Verified in testing against a two-disk OVF
  directory (a 16GiB-virtual/~1.7GiB-actual boot disk and a
  10GiB-virtual/~220KiB-actual secondary disk): omitting `DISK_SELECTOR`
  correctly failed with both filenames listed; setting it explicitly to
  each disk in turn, with two separate Job runs and their own PVCs, both
  completed successfully with `VERIFY_CHECKSUM=true` passing. See
  [OPERATIONS-GUIDE.md](./OPERATIONS-GUIDE.md#4-running-an-import) for the
  pre-flight check as step 0 of the overall procedure, and
  [OPERATIONS-GUIDE.md](./OPERATIONS-GUIDE.md#42-importing-a-multi-disk-source-more-than-one-vmdk)
  for a full worked example with the exact YAML to change per disk and how
  to match each `.vmdk` to its disk when filenames alone aren't enough.
- A bare `.ovf` file (with no accompanying `.vmdk` in the same directory)
  has no embedded disk data; point `SOURCE_IMAGE_PATH`/`SOURCE_IMAGE_URL` at
  the containing directory or the `.ova` archive instead, or supply the
  matching `.vmdk` directly.
- `qemu-img-direct` mode requires the target device to read back as
  all-zero (it uses `qemu-img convert --target-is-zero`, which is unsafe
  otherwise). The script checks this automatically by sampling the device
  and refuses to proceed if any sampled region is non-zero — a freshly
  provisioned CSI volume should always pass this check, but a volume
  reused from a previous write will not. If the check fails, use
  `CONVERT_MODE=qemu-img` instead.
- **`CONVERT_MODE=virt-v2v` is required for Windows guests**, not optional
  — `qemu-img-direct`/`qemu-img` never touch anything inside the guest
  filesystem, so a Windows VM exported from VMware will typically fail to
  boot under KVM/QEMU without the driver injection and boot-configuration
  fixups that only `virt-v2v` performs. A few things to know before using
  it:
  - `DISK_SELECTOR` has **no effect** under this mode — `virt-v2v`
    converts the whole VM definition (all disks referenced by the
    `.ovf`/`.vmx`) as a single unit, unlike `qemu-img`/`qemu-img-direct`,
    which select one `.vmdk` at a time. The script logs a warning if
    `DISK_SELECTOR` is set in this mode.
  - It performs noticeably better with real KVM acceleration available
    (the node needs `/dev/kvm`, typically exposed via the KubeVirt device
    plugin as the `devices.kubevirt.io/kvm` resource — see the commented-out
    block in `job.yaml`). It still works without it, falling back to
    software emulation, just much slower — verified in testing: a
    90GiB-virtual/~5.2GiB-actual Windows Server 2019 guest took about 29
    minutes end-to-end (virt-v2v conversion ~6.5 min + `dd` write ~22 min
    @ 70+ MB/s) with no KVM acceleration available on the test node.
  - The container image used for testing doesn't bundle VirtIO drivers
    (`/usr/share/virtio-win`), so `virt-v2v` falls back to configuring the
    guest with slower emulated devices rather than installing VirtIO
    drivers — it still produces a bootable disk, just without the
    VirtIO performance benefit. If you want VirtIO drivers installed,
    extend the `Dockerfile` to bundle the `virtio-win` ISO (available from
    Fedora's virtio-win project) and either place it where `virt-v2v`
    looks by default or point it there via its `-i` input options; this
    was not exercised in testing.
  - Relies on libguestfs's OS inspection to recognize the guest. Windows
    guests are well supported (verified: correctly identified a Windows
    Server 2019 guest and converted it to a bootable disk with a valid
    GPT/NTFS layout). OSTree/Ignition-based Linux distributions (e.g.
    Fedora CoreOS) are **not** recognized and will fail with `No root
    device found in this operating system image` — use
    `qemu-img-direct`/`qemu-img` for those instead.
- `VERIFY_CHECKSUM=true` reads the full image back from the Cinder volume
  over the network/storage backend to compute a checksum — this roughly
  doubles the I/O cost and time. Under `CONVERT_MODE=qemu-img-direct`, it
  also creates a temporary RAW file (removed immediately after comparison)
  since `qemu-img convert` cannot stream RAW output to a pipe, temporarily
  reintroducing the scratch-space cost that mode otherwise avoids. Use it
  for one-off validation, not routine runs.
- The Job's pod must be schedulable on a node where Cinder/Nova can attach a
  volume (i.e., a node that is itself an OpenStack instance), consistent
  with the general requirement described in
  [examples.md](../../../docs/cinder-csi-plugin/examples.md).
- No authentication is added around the Job itself — it runs with whatever
  RBAC/service account permissions your namespace grants. If you use
  `SOURCE_IMAGE_URL`, treat that endpoint as untrusted input: the script
  downloads and parses that data with `qemu-img`, and VM disk image parsers
  have historically had vulnerabilities with malformed images, so prefer
  image sources you control. The default `SOURCE_IMAGE_PATH` + `hostPath`
  setup reads a file already present on the node instead of fetching from
  the network, but the same parser-risk caveat applies to whatever image
  you point it at.
