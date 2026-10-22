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
   - writes it straight onto the target device with `qemu-img convert -n
     --target-is-zero -O raw` — no intermediate RAW file at any point, so
     scratch space only ever needs to hold the downloaded/extracted source
     image itself, not the (potentially much larger) virtual disk size.
     This relies on the target device genuinely reading back as all-zero;
     the script verifies this automatically and refuses to proceed
     otherwise. Verified end-to-end: ~20 seconds for a
     10GiB-virtual/~1GiB-actual `streamOptimized` VMDK. See
     [OPERATIONS-GUIDE.md](./OPERATIONS-GUIDE.md#3-choosing-a-conversion-mode)
     for the full performance analysis (including why writing to a block
     device without `--target-is-zero` can stall for minutes on a sparse
     VMDK).
   - optionally (`CONVERT_MODE=direct-v2v`), runs `virt-v2v-in-place`
     directly against the target device afterward, to inject drivers and
     fix up the boot configuration **in place** — required for Windows
     guests (see below)
   - optionally verifies a sha256 checksum of what was written
3. Once the Job completes successfully, the Cinder volume behind the PVC
   contains the imported disk and can be used to boot a Nova instance, or
   referenced directly in OpenStack (`openstack volume show <volume-id>`),
   or bound into a different Pod as a data/boot volume.

### Choosing `CONVERT_MODE`

Two modes are supported, both writing straight onto the target device with
no intermediate RAW file:

- **`direct` (default)** — just the direct write described above. Fine
  for most Linux guests (modern distros already include virtio drivers
  and don't hardcode hypervisor-specific boot paths), but **not**
  sufficient for Windows guests, which need driver injection/boot fixup —
  see `direct-v2v` below.
- **`direct-v2v`** — **required for Windows guests.** Runs the same
  direct write, then runs `virt-v2v-in-place` directly against the
  target device to inject VirtIO drivers and fix up the boot
  configuration **in place** on the data just written. A Windows VM
  exported from VMware has VMware-specific drivers and a boot
  configuration tied to VMware's virtual hardware, and will typically
  fail to boot under KVM/QEMU (the hypervisor Nova uses) without this
  step. Verified end-to-end against a real Windows Server 2019 guest
  (90GiB-virtual/~5.2GiB-actual `streamOptimized` VMDK, no KVM
  acceleration on the test node): **~6.2 minutes total** (direct write
  ~3 min + `virt-v2v-in-place` ~3.2 min), using only an 8Gi `emptyDir`
  for scratch space. libguestfs correctly identified the guest as
  "Windows Server 2019 Standard Evaluation (win2k19)"; the resulting
  Cinder/Ceph RBD volume showed a correct GPT partition layout (Windows
  RE + EFI System + Microsoft Reserved + an NTFS-signed main partition)
  after import.

  **IMPORTANT SAFETY NOTE**: `virt-v2v-in-place` modifies the target
  device's data **in place, with no backup**. Per upstream's own
  documentation, if it fails, "the disk image will be in an unknown,
  possibly corrupted state." Only use this mode against a freshly
  provisioned, disposable target volume — one you can recreate from the
  source image again if something goes wrong. Also requires the guest OS
  to be recognized by libguestfs's inspection; OSTree/Ignition-based
  Linux distributions (e.g. Fedora CoreOS) are **not** recognized and
  will fail here — use `CONVERT_MODE=direct` for those instead.

See
[OPERATIONS-GUIDE.md](./OPERATIONS-GUIDE.md#3-choosing-a-conversion-mode)
for the full comparison, real numbers, and additional caveats (KVM
acceleration, VirtIO driver availability, `DISK_SELECTOR` interaction).

### Progress output

`qemu-img convert -p` and `virt-v2v-in-place` both report progress by
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
No changes to the driver itself are needed. `qemu-img`/`virt-v2v` run
entirely inside the Job's container, independent of the CSI driver.

## Files

- `convert-and-import.sh` — the conversion/import script, baked into the
  tool image as its `ENTRYPOINT`.
- `Dockerfile` — tool image (`qemu-img`, `virt-v2v`/`libguestfs`, `curl`,
  `tar`, `python3`) used to run the script. `virt-v2v`/`libguestfs` add
  roughly 1.5GB to the image but are required for Windows guest support
  (`CONVERT_MODE=direct-v2v`). Build and push it to a registry reachable
  by your cluster.
- `job.yaml` — Block-mode target PVC + Job manifest (no `StorageClass`
  object; see
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
  to** the virtual size of the source disk.
- `env.SOURCE_IMAGE_PATH` (default) — path to the source `.ova`/`.ovf`
  directory/`.vmdk` as seen inside the container (e.g.
  `/input/my-disk.ova`), backed by the `source-input` `hostPath` volume.
  Update both the env var value and the `source-input` volume's `path` to
  point at your actual source file/directory on the node. Alternatively,
  if your source image is reachable over HTTP(S), replace this with
  `env.SOURCE_IMAGE_URL` and remove the `source-input` volume/mount — see
  [OPERATIONS-GUIDE.md](./OPERATIONS-GUIDE.md#41-using-sourceimageurl-instead-of-a-local-file).
- `env.CONVERT_MODE` — `direct` (default; for Linux guests) or
  `direct-v2v` (required for Windows guests — see
  [How it works](#choosing-convert_mode) above for the full tradeoff).
- `env.TARGET_DEVICE` and `volumeDevices[0].devicePath` — must match (the
  device path as seen inside the container, e.g. `/dev/xvda`).
- `volumes[1]` (the `workdir` volume, `emptyDir`) — scratch space. Neither
  `CONVERT_MODE` value ever writes a full intermediate RAW file here, so
  it only needs to hold the downloaded/extracted source image itself
  (its *actual*, not virtual, size for a `streamOptimized` VMDK) —
  verified in testing, an 8Gi `emptyDir` was sufficient even for a
  90GiB-virtual Windows guest under `CONVERT_MODE=direct-v2v`.
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
  used automatically.) **Under `CONVERT_MODE=direct-v2v`, `DISK_SELECTOR`
  only affects the direct-write phase** — the subsequent
  `virt-v2v-in-place` step always operates on the whole target device as
  a single disk, with no disk-selection concept of its own; the script
  logs a warning if `DISK_SELECTOR` is set in that mode. Verified in
  testing against a two-disk OVF directory (a 16GiB-virtual/~1.7GiB-actual
  boot disk and a 10GiB-virtual/~220KiB-actual secondary disk, both under
  `CONVERT_MODE=direct`): omitting `DISK_SELECTOR` correctly failed with
  both filenames listed; setting it explicitly to each disk in turn, with
  two separate Job runs and their own PVCs, both completed successfully
  with `VERIFY_CHECKSUM=true` passing. See
  [OPERATIONS-GUIDE.md](./OPERATIONS-GUIDE.md#4-running-an-import) for the
  pre-flight check as step 0 of the overall procedure, and
  [OPERATIONS-GUIDE.md](./OPERATIONS-GUIDE.md#42-importing-a-multi-disk-source-more-than-one-vmdk)
  for a full worked example with the exact YAML to change per disk and how
  to match each `.vmdk` to its disk when filenames alone aren't enough.
- A bare `.ovf` file (with no accompanying `.vmdk` in the same directory)
  has no embedded disk data; point `SOURCE_IMAGE_PATH`/`SOURCE_IMAGE_URL` at
  the containing directory or the `.ova` archive instead, or supply the
  matching `.vmdk` directly.
- **Both modes require the target device to read back as all-zero**
  (they use `qemu-img convert --target-is-zero` for the direct write,
  which is unsafe otherwise). The script checks this automatically by
  sampling the device and refuses to proceed if any sampled region is
  non-zero — a freshly provisioned CSI volume should always pass this
  check, but a volume reused from a previous write will not. If the
  check fails, provision a fresh volume instead of reusing one that has
  been written to before; there is no fallback mode that skips this
  requirement.
- **`CONVERT_MODE=direct-v2v` carries real risk** — see
  [Choosing CONVERT_MODE](#choosing-convert_mode) above for the full
  explanation of why `virt-v2v-in-place`'s in-place modification means a
  failure can leave the target device in an unknown state, with no
  automatic recovery. A few additional things to know before using it:
  - Benefits from real KVM acceleration available (the node needs
    `/dev/kvm`, typically exposed via the KubeVirt device plugin as the
    `devices.kubevirt.io/kvm` resource — see the commented-out block in
    `job.yaml`). It still works without it, falling back to software
    emulation, just much slower — the ~6.2 minute figure above was
    measured **without** KVM acceleration available on the test node.
  - The container image used for testing doesn't bundle VirtIO drivers
    (`/usr/share/virtio-win`), so `virt-v2v-in-place` falls back to
    configuring the guest with slower emulated devices rather than
    installing VirtIO drivers — it still produces a bootable disk, just
    without the VirtIO performance benefit. If you want VirtIO drivers
    installed, extend the `Dockerfile` to bundle the `virtio-win` ISO
    (available from Fedora's virtio-win project) and either place it
    where `virt-v2v` looks by default or point it there via its input
    options; this was not exercised in testing.
  - Relies on libguestfs's OS inspection to recognize the guest. Windows
    guests are well supported (verified: correctly identified a Windows
    Server 2019 guest and converted it to a disk with a valid GPT/NTFS
    layout). OSTree/Ignition-based Linux distributions (e.g. Fedora
    CoreOS) are **not** recognized and will fail with `No root device
    found in this operating system image` — use `CONVERT_MODE=direct`
    for those instead.
- `VERIFY_CHECKSUM=true` reads the full image back from the Cinder volume
  over the network/storage backend to compute a checksum — this roughly
  doubles the I/O cost and time, and creates a temporary RAW file
  (removed immediately after comparison) since `qemu-img convert` cannot
  stream RAW output to a pipe. Under `CONVERT_MODE=direct-v2v`, it only
  covers the direct-write phase, not the subsequent `virt-v2v-in-place`
  modification. Use it for one-off validation, not routine runs.
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
