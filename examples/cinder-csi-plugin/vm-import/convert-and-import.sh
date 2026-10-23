#!/usr/bin/env bash
#
# convert-and-import.sh
#
# Converts a source OVA/OVF/VMDK/QCOW2 disk image to RAW format and writes
# it block-by-block onto a target block device (a Cinder volume attached to
# the Pod via a Block-mode PersistentVolumeClaim, provisioned by the Cinder
# CSI driver in this project).
#
# Conversion strategy:
#   - CONVERT_MODE=direct (default): run `qemu-img convert -O raw`
#     directly onto the target block device, skipping any intermediate RAW
#     file. This uses `-n --target-is-zero`, which tells qemu-img to assume
#     the destination already reads back as all zeros and to skip writing
#     runs of zero bytes from sparse regions of the source -- the crucial
#     optimization for VMware's streamOptimized VMDK format, which is
#     typically mostly-empty (e.g. a 10GiB virtual disk backed by ~1GB of
#     actual data). WITHOUT -n --target-is-zero, qemu-img cannot "punch
#     holes" in a block device (unlike a file, a block device has no
#     concept of sparseness/truncation), so it falls back to writing real
#     zero bytes for every unallocated source cluster -- in testing this
#     made conversion of a mostly-empty 10GiB VMDK stall for minutes
#     (observed: 4+ minutes with zero additional bytes written, confirmed
#     via the kernel reporting the writer thread blocked in
#     submit_bio_wait) where this mode's actual run took ~18.5 seconds,
#     producing output with an identical sha256 checksum to a scratch-file
#     based conversion. This is a known qemu-img behavior, not a bug: see
#     https://github.com/oVirt/vdsm/commit/2bbe66274524cc009f39d3a5e5577b84144f1171
#     for upstream's own measurements (up to 250x on their workload).
#     SAFETY: --target-is-zero is only correct if the target block device
#     truly reads back as zero everywhere. This script verifies that by
#     sampling the device before using this mode (see Step 2 below) and
#     refuses to proceed if any sampled region is non-zero. A freshly
#     provisioned CSI volume (Cinder, Ceph RBD, etc.) should always read
#     as zero; a volume that has been written to before (e.g. reused
#     across test runs without the backing storage being wiped) will not
#     be, and this script will refuse to run rather than risk corruption
#     -- provision a fresh volume instead of reusing one in that case.
#     This mode only converts the disk's byte-for-byte format -- it does
#     not touch anything inside the guest filesystem. This is fine for
#     most Linux guests (modern distros already include virtio drivers and
#     don't hardcode hypervisor-specific boot paths) but is NOT sufficient
#     for Windows guests -- see CONVERT_MODE=direct-v2v below.
#   - CONVERT_MODE=direct-v2v: for Windows guests. Runs the exact same
#     direct-mode write described above (straight onto TARGET_DEVICE, no
#     intermediate RAW file, same --target-is-zero safety check), then
#     runs `virt-v2v-in-place -i disk TARGET_DEVICE` directly against the
#     target block device to perform driver injection/boot-config fixup
#     IN PLACE on the data that's already there. No intermediate RAW file
#     is ever created for virt-v2v either, so this mode needs no more
#     scratch space than CONVERT_MODE=direct (just the downloaded/
#     extracted source image, not the virtual disk size). The direct
#     write (`qemu-img convert`) is a pure format converter -- it copies
#     the disk's bytes from one container format to another (vmdk ->
#     raw) without touching anything inside the guest filesystem.
#     A Windows guest exported from VMware has VMware-specific drivers
#     (and often no virtio drivers at all), a boot configuration pointing
#     at VMware's virtual hardware, and (for older Windows versions) a HAL
#     tied to that hardware -- booting this unmodified under KVM/QEMU
#     (what Nova uses) typically fails with a boot-time bluescreen
#     (commonly INACCESSIBLE_BOOT_DEVICE or a similar STOP code) because
#     the guest can't find/drive its boot disk under the new virtual
#     hardware. virt-v2v-in-place fixes this: it mounts the guest
#     filesystem, injects VirtIO drivers, and rewrites the boot
#     configuration to work under KVM, in addition to converting the disk
#     format. Verified in testing against a 90GiB-virtual/~5.2GiB-actual
#     Windows Server 2019 guest with no KVM acceleration available: the
#     direct-mode write took ~3 minutes, virt-v2v-in-place took ~3.2
#     minutes, for a total of ~6.2 minutes, using only a small (8Gi)
#     emptyDir for scratch space -- no large scratch volume needed.
#     Produced a correct GPT layout and NTFS boot-sector signature on the
#     target device.
#     IMPORTANT SAFETY NOTE: virt-v2v-in-place modifies TARGET_DEVICE's
#     data IN PLACE, with NO backup of the pre-conversion state. Per
#     upstream's own documentation: "If virt-v2v-in-place fails it will
#     return a non-zero (error) exit code. In this case, the disk image
#     will be in an unknown, possibly corrupted state." This script does
#     not and cannot work around that risk -- if the virt-v2v-in-place
#     step fails, the just-written data on TARGET_DEVICE may be left
#     partially modified. Use a freshly provisioned, disposable target
#     volume; do not use this mode against a volume you cannot afford to
#     recreate from the source image again if something goes wrong.
#     Also requires the guest OS to be recognized by libguestfs's
#     inspection; OSTree/Ignition-based Linux distributions (e.g. Fedora
#     CoreOS) are NOT recognized and will fail -- use CONVERT_MODE=direct
#     for those instead. Performs noticeably better with real KVM
#     acceleration available (/dev/kvm on the node, e.g. via the KubeVirt
#     device plugin); still works without it, via software emulation,
#     just much slower (as in the ~6.2 minute figure above, which was
#     measured WITHOUT KVM acceleration).
#
# This script is intended to run inside the Pod/Job defined in job.yaml.
#
# Required environment variables:
#   TARGET_DEVICE       - Path to the target block device, e.g. /dev/xvda
#                         (must match volumeDevices.devicePath in job.yaml).
#   SOURCE_IMAGE_URL     - HTTP(S) URL to download the source OVA/OVF/VMDK
#                         image. Alternatively, set SOURCE_IMAGE_PATH to a
#                         path already mounted into the Pod (skips download).
#
# Optional environment variables:
#   SOURCE_IMAGE_PATH   - Local path to the source image if already present.
#                         Takes precedence over SOURCE_IMAGE_URL.
#   CONVERT_MODE        - "direct" (default, Linux guests) or "direct-v2v"
#                         (Windows guests -- see the comment above for why
#                         and for the in-place-modification risk it carries).
#   NO_FSTRIM           - If "true", passes --no-fstrim to virt-v2v-in-place
#                         under CONVERT_MODE=direct-v2v, skipping the
#                         filesystem trim virt-v2v normally does before
#                         conversion. Only relevant for that mode.
#                         Defaults to "false".
#   ZERO_CHECK_SAMPLE_BYTES - Bytes sampled from the start, middle, and end
#                         of TARGET_DEVICE to verify it reads as zero
#                         before writing (both modes perform this check).
#                         Defaults to 104857600 (100MiB) per sample.
#   WORK_DIR            - Scratch directory for intermediate files. Defaults
#                         to /work. Only needs to hold the downloaded/
#                         extracted source image -- neither mode ever
#                         writes a full intermediate RAW file here.
#   VERIFY_CHECKSUM      - If "true", compute and compare sha256 checksums
#                         of the source image and the data written to
#                         TARGET_DEVICE (the direct-mode write only; under
#                         CONVERT_MODE=direct-v2v this does not cover the
#                         subsequent virt-v2v-in-place modification).
#                         Defaults to "false" (reading back a large device
#                         roughly doubles total I/O time). Requires a
#                         temporary RAW file (qemu-img convert cannot
#                         stream RAW output to a pipe), removed immediately
#                         after the checksum comparison.
#   KEEP_WORK_DIR       - If "true", skip cleanup of WORK_DIR on success.
#   DISK_SELECTOR       - Filename (basename only) to pick a specific disk
#                         when the source has more than one .vmdk. Only
#                         affects the direct-mode write phase; see below.
#   PROGRESS_STEP_PERCENT   - Print a progress update whenever progress
#                         advances by at least this many percentage points.
#                         Defaults to 10.
#   PROGRESS_INTERVAL_SECONDS - Print a progress update at least this often
#                         (in seconds) even if PROGRESS_STEP_PERCENT hasn't
#                         been reached yet, so long-running steps with slow
#                         progress still show activity. Defaults to 15.
#   PROGRESS_MODE       - "clean" (default) throttles qemu-img's/
#                         virt-v2v-in-place's native \r-based progress
#                         output (hundreds of "(NN.NN/100%)" ticks that
#                         look like a garbled wall of text when captured
#                         non-interactively, e.g. by `kubectl logs`) into a
#                         handful of clean, timestamped single-line
#                         updates. Set to "raw" to disable this and see the
#                         tool's native output instead (e.g. if you're
#                         running the script directly in an interactive
#                         terminal where the \r-based live-updating bar
#                         renders fine).
#   ENABLE_DEBUG        - If "true", passes -v to virt-v2v-in-place for
#                         verbose troubleshooting output. Only relevant
#                         when CONVERT_MODE=direct-v2v. Defaults to "false".
#   UPLOAD_VMWARE_CLEANUP_SCRIPT - If "true" (default; only relevant under
#                         CONVERT_MODE=direct-v2v), uploads
#                         remove-vmware-tools.ps1 into the converted
#                         guest's filesystem (at 'C:\Program Files\
#                         Guestfs\remove-vmware-tools.ps1') after
#                         virt-v2v-in-place completes. This script is
#                         NOT executed automatically -- it is left for the
#                         guest's administrator to run manually, after the
#                         guest has actually booted on KVM, to force-remove
#                         VMware Tools. This exists because VMware Tools'
#                         own MSI uninstaller cannot complete once the
#                         guest is no longer running on a VMware
#                         hypervisor (see the uploaded script's own header
#                         comment, and OPERATIONS-GUIDE.md's VMware Tools
#                         uninstall-failure troubleshooting entry, for the
#                         full explanation and upstream references). Set
#                         to "false" to skip this (e.g. if the guest's
#                         filesystem can't be mounted read-write -- see
#                         below -- or you don't want the script present).
#                         If the upload itself fails (most commonly
#                         because the guest's NTFS partition has a
#                         "hibernated"/not-cleanly-unmounted flag set,
#                         which libguestfs refuses to mount read-write as
#                         a safety measure), this logs a warning and
#                         continues rather than failing the whole run,
#                         since virt-v2v-in-place has already completed
#                         successfully by this point.
#
set -euo pipefail

GREEN="\033[0;32m"; RED="\033[0;31m"; YELLOW="\033[0;33m"; CYAN="\033[0;36m"; NC="\033[0m"
log_info()  { echo -e "${GREEN}INFO:${NC}  $1"; }
log_warn()  { echo -e "${YELLOW}WARN:${NC}  $1" >&2; }
log_error() { echo -e "${RED}ERROR:${NC} $1" >&2; }
log_debug() { echo -e "${CYAN}DEBUG:${NC} $1"; }

fail() {
  log_error "$1"
  exit 1
}

require_var() {
  local name="$1"
  if [ -z "${!name:-}" ]; then
    fail "required environment variable ${name} is not set"
  fi
}

WORK_DIR="${WORK_DIR:-/work}"
CONVERT_MODE="${CONVERT_MODE:-direct}"
VERIFY_CHECKSUM="${VERIFY_CHECKSUM:-false}"
ZERO_CHECK_SAMPLE_BYTES="${ZERO_CHECK_SAMPLE_BYTES:-104857600}"
PROGRESS_MODE="${PROGRESS_MODE:-clean}"
PROGRESS_STEP_PERCENT="${PROGRESS_STEP_PERCENT:-10}"
PROGRESS_INTERVAL_SECONDS="${PROGRESS_INTERVAL_SECONDS:-15}"

mkdir -p "${WORK_DIR}/input" "${WORK_DIR}/raw"

require_var TARGET_DEVICE

[ -e "${TARGET_DEVICE}" ] || fail "target device ${TARGET_DEVICE} does not exist. Check volumeDevices.devicePath in the Pod spec."
[ -b "${TARGET_DEVICE}" ] || fail "target device ${TARGET_DEVICE} is not a block device."

# Resolves SRC_PATH (a .ova archive, a bare .vmdk/.qcow2/etc. file, or a
# directory containing a loose .ovf + its referenced disk) to a single
# qemu-img-readable disk image path. Sets DISK_IMAGE as a side effect.
resolve_disk_image() {
  DISK_IMAGE="${SRC_PATH}"

  case "${SRC_PATH}" in
    *.ova)
      log_info "Detected OVA archive, extracting disk image(s)"
      local extract_dir="${WORK_DIR}/input/extracted"
      mkdir -p "${extract_dir}"
      tar -xf "${SRC_PATH}" -C "${extract_dir}"

      if [ -n "${DISK_SELECTOR:-}" ]; then
        DISK_IMAGE="${extract_dir}/${DISK_SELECTOR}"
        [ -f "${DISK_IMAGE}" ] || fail "DISK_SELECTOR='${DISK_SELECTOR}' not found inside OVA archive ${SRC_PATH}"
      else
        DISK_IMAGE="$(select_sole_vmdk "${extract_dir}" "OVA archive ${SRC_PATH}")"
      fi
      log_debug "Using extracted disk image: ${DISK_IMAGE}"
      ;;
    *.ovf)
      fail "a bare .ovf file was provided but it has no embedded disk; provide the matching .vmdk directly instead, or point SOURCE_IMAGE_PATH at the containing directory"
      ;;
    *)
      if [ -d "${SRC_PATH}" ]; then
        # A directory containing a loose OVF descriptor + its referenced
        # .vmdk (e.g. unpacked from an OVA, or exported directly as an
        # OVF directory).
        if [ -n "${DISK_SELECTOR:-}" ]; then
          DISK_IMAGE="${SRC_PATH}/${DISK_SELECTOR}"
          [ -f "${DISK_IMAGE}" ] || fail "DISK_SELECTOR='${DISK_SELECTOR}' not found in directory ${SRC_PATH}"
        else
          DISK_IMAGE="$(select_sole_vmdk "${SRC_PATH}" "directory ${SRC_PATH}")"
        fi
        log_debug "Using disk image from directory: ${DISK_IMAGE}"
      fi
      ;;
  esac
}

# Finds all *.vmdk files directly inside $1 (a directory -- either an
# OVF directory or an extracted OVA). If there is exactly one, returns its
# path. If there are zero, fails. If there are MORE THAN ONE, fails with
# an explicit list of candidates rather than silently picking the
# alphabetically-first one -- a multi-disk source requires DISK_SELECTOR
# to be set explicitly (see README.md/OPERATIONS-GUIDE.md), since
# alphabetical filename order is not guaranteed to match the source VM's
# actual disk numbering or boot order (e.g. a real two-disk OVF directory
# encountered in testing had disk1.vmdk as the 16GiB boot disk and
# disk2.vmdk as a 10GiB secondary disk -- alphabetical order happened to
# match there, but nothing in the OVF/OVA format guarantees that in
# general, and this script has no way to verify it without DISK_SELECTOR
# telling it which one you mean).
select_sole_vmdk() {
  local dir="$1"
  local context="$2"
  local vmdks
  vmdks="$(find "${dir}" -maxdepth 1 -iname '*.vmdk' | sort)"

  local count
  count="$(printf '%s\n' "${vmdks}" | grep -c . || true)"

  if [ "${count}" -eq 0 ]; then
    fail "no .vmdk disk found in ${context}"
  elif [ "${count}" -gt 1 ]; then
    fail "multiple .vmdk files found in ${context} but DISK_SELECTOR is not set -- refusing to guess which one to import. Set DISK_SELECTOR to one of the filenames below (basename only, e.g. DISK_SELECTOR=disk1.vmdk), and run this Job again once per disk for a multi-disk source:
$(printf '%s\n' "${vmdks}" | sed 's/^/    /')"
  fi

  printf '%s' "${vmdks}"
}


# Verifies that TARGET_DEVICE reads back as all zeros at the start, middle,
# and end, each over ZERO_CHECK_SAMPLE_BYTES. This is the safety check
# required before using `qemu-img convert --target-is-zero`: that flag
# tells qemu-img to assume the destination is already zeroed and to skip
# writing zero runs from the source, which produces corrupt output if the
# assumption is false. A freshly provisioned CSI volume should always read
# as zero; this is a defense against reusing a volume that was previously
# written to.
verify_target_is_zero() {
  local dev_size="$1"
  local sample="${ZERO_CHECK_SAMPLE_BYTES}"
  if [ "${sample}" -gt "${dev_size}" ]; then
    sample="${dev_size}"
  fi
  local mid_skip=$(( (dev_size / 2 / sample) * sample ))
  local end_skip=$(( ((dev_size - sample) / sample) * sample ))

  log_debug "Verifying ${TARGET_DEVICE} reads as zero (sampling ${sample} bytes at offset 0, ${mid_skip}, ${end_skip})"

  local offset
  for offset in 0 "${mid_skip}" "${end_skip}"; do
    if ! dd if="${TARGET_DEVICE}" bs="${sample}" count=1 skip="$(( offset / sample ))" iflag=skip_bytes 2>/dev/null \
         | python3 -c 'import sys; data = sys.stdin.buffer.read(); sys.exit(0 if data == b"\x00" * len(data) else 1)'; then
      fail "TARGET_DEVICE is not all-zero at byte offset ${offset}; refusing to proceed (--target-is-zero would be unsafe -- it assumes the destination already reads as zero). Provision a fresh volume instead of reusing one that has been written to before."
    fi
  done
  log_debug "Zero check passed"
}

# Runs "$@" (expected to be `qemu-img convert -p ...` or
# `virt-v2v-in-place ...`) and, unless PROGRESS_MODE=raw, pipes its
# combined stdout+stderr through a Python filter that throttles the
# command's native \r-delimited progress updates (hundreds of
# "(NN.NN/100%)" ticks) down to a handful of clean, timestamped,
# newline-terminated lines -- readable in `kubectl logs` and similar
# non-interactive log viewers, which don't interpret \r the way a real
# terminal does.
#
# `stdbuf -o0 -e0` forces the wrapped command's stdout/stderr to be
# COMPLETELY unbuffered (not just line-buffered) even though it's not a
# tty. This matters because qemu-img's progress updates end in \r, not
# \n -- glibc's "line buffered" mode only flushes on \n, so `stdbuf -oL`
# alone does NOT flush \r-terminated progress output promptly; without
# -o0, all progress lines sit in a buffer and only appear once the
# process exits (observed in testing: a 37-second conversion produced 11
# progress lines that all showed "elapsed 37s" -- i.e. they were all
# flushed at once at the very end, not as they happened). The Python
# filter itself also reads via os.read() on the raw file descriptor
# (bypassing Python's own buffered io, which could introduce the same
# problem on the reading side) and flushes every line it prints.
# Preserves and returns the wrapped command's exit status (not the
# filter's).
run_with_progress() {
  if [ "${PROGRESS_MODE}" = "raw" ]; then
    "$@"
    return $?
  fi

  stdbuf -o0 -e0 "$@" 2>&1 | PROGRESS_STEP_PERCENT="${PROGRESS_STEP_PERCENT}" \
    PROGRESS_INTERVAL_SECONDS="${PROGRESS_INTERVAL_SECONDS}" python3 -u -c '
import os, re, sys, time

step = float(os.environ.get("PROGRESS_STEP_PERCENT", "10"))
interval = float(os.environ.get("PROGRESS_INTERVAL_SECONDS", "15"))
pct_re = re.compile(r"\(([0-9.]+)/100%\)")
dd_re = re.compile(r"^([0-9]+) bytes \(([^)]+)\) copied, [0-9.]+ s, (.+)$")

start = time.time()
last_pct = -1.0
last_time = start
buf = ""
fd = sys.stdin.fileno()

while True:
    chunk = os.read(fd, 4096)
    if not chunk:
        break
    buf += chunk.decode("utf-8", errors="replace")
    while True:
        cr, nl = buf.find("\r"), buf.find("\n")
        candidates = [i for i in (cr, nl) if i != -1]
        if not candidates:
            break
        idx = min(candidates)
        line, buf = buf[:idx], buf[idx + 1:]
        line = line.strip()
        if not line:
            continue

        m = pct_re.search(line)
        if m:
            pct = float(m.group(1))
            now = time.time()
            if pct - last_pct >= step or now - last_time >= interval or pct >= 99.99:
                print("  progress: {:5.1f}% complete (elapsed {:.0f}s)".format(pct, now - start), flush=True)
                last_pct = pct
                last_time = now
            continue

        m = dd_re.search(line)
        if m:
            size_human, rate = m.group(2), m.group(3)
            now = time.time()
            if now - last_time >= interval:
                print("  progress: {} copied so far, {} (elapsed {:.0f}s)".format(size_human, rate, now - start), flush=True)
                last_time = now
            continue

        # Anything else (non-progress output, e.g. a final summary line or
        # an error message) is passed through unchanged and immediately.
        print(line, flush=True)
'
  return "${PIPESTATUS[0]}"
}

# ---------------------------------------------------------------------------
# Step 1: obtain the source image
# ---------------------------------------------------------------------------
SRC_PATH="${SOURCE_IMAGE_PATH:-}"

if [ -z "${SRC_PATH}" ]; then
  require_var SOURCE_IMAGE_URL
  SRC_FILENAME="$(basename "${SOURCE_IMAGE_URL}")"
  SRC_PATH="${WORK_DIR}/input/${SRC_FILENAME}"
  log_info "Downloading source image from ${SOURCE_IMAGE_URL} to ${SRC_PATH}"
  curl -fL --retry 3 --retry-delay 5 -o "${SRC_PATH}" "${SOURCE_IMAGE_URL}"
else
  log_info "Using local source image at ${SRC_PATH}"
fi

[ -e "${SRC_PATH}" ] || fail "source image ${SRC_PATH} not found"

RAW_PATH="${WORK_DIR}/raw/disk.raw"
DEV_SIZE_BYTES=$(blockdev --getsize64 "${TARGET_DEVICE}")
log_debug "Target device size: ${DEV_SIZE_BYTES} bytes"

# ---------------------------------------------------------------------------
# Step 2: write the source image directly onto TARGET_DEVICE
# ---------------------------------------------------------------------------

# Resolves the source disk image, verifies TARGET_DEVICE reads as
# all-zero, checks the source fits, and writes straight onto TARGET_DEVICE
# with `qemu-img convert -n --target-is-zero`, optionally verifying a
# checksum afterward. Used by both CONVERT_MODE=direct and
# CONVERT_MODE=direct-v2v as their (identical) first phase.
do_direct_write() {
  resolve_disk_image

  verify_target_is_zero "${DEV_SIZE_BYTES}"

  qemu-img info "${DISK_IMAGE}"

  SRC_VIRTUAL_SIZE_BYTES=$(qemu-img info --output=json "${DISK_IMAGE}" | python3 -c 'import json,sys; print(json.load(sys.stdin)["virtual-size"])')
  if [ -n "${SRC_VIRTUAL_SIZE_BYTES}" ] && [ "${SRC_VIRTUAL_SIZE_BYTES}" -gt "${DEV_SIZE_BYTES}" ]; then
    fail "source disk (${SRC_VIRTUAL_SIZE_BYTES} bytes) is larger than target device ${TARGET_DEVICE} (${DEV_SIZE_BYTES} bytes). Resize the PVC/Cinder volume before retrying."
  fi

  log_info "Writing directly to ${TARGET_DEVICE} using qemu-img convert -n --target-is-zero"
  run_with_progress qemu-img convert -p -n --target-is-zero -O raw "${DISK_IMAGE}" "${TARGET_DEVICE}"
  sync
  log_info "qemu-img convert completed"

  if [ "${VERIFY_CHECKSUM}" = "true" ]; then
    log_info "Verifying checksum (reads ${SRC_VIRTUAL_SIZE_BYTES:-${DEV_SIZE_BYTES}} bytes back from ${TARGET_DEVICE})"
    # qemu-img convert cannot write RAW output to a pipe/stdout (its
    # output driver needs a seekable target), so a temporary file is
    # required here. This only affects VERIFY_CHECKSUM=true runs, not
    # the default path, which never creates this file.
    qemu-img convert -O raw "${DISK_IMAGE}" "${RAW_PATH}"
    SRC_SUM=$(sha256sum "${RAW_PATH}" | awk '{print $1}')
    DST_SUM=$(head -c "${SRC_VIRTUAL_SIZE_BYTES:-${DEV_SIZE_BYTES}}" "${TARGET_DEVICE}" | sha256sum | awk '{print $1}')
    rm -f "${RAW_PATH}"
    if [ "${SRC_SUM}" != "${DST_SUM}" ]; then
      fail "checksum mismatch: source=${SRC_SUM} target=${DST_SUM}"
    fi
    log_info "Checksum verified: ${SRC_SUM}"
  fi
}

# Uploads remove-vmware-tools.ps1 into the just-converted Windows guest's
# filesystem (does NOT execute it). Standard VMware Tools uninstall
# (whether via its own firstboot-scheduled uninstaller, Apps & Features,
# or msiexec directly) cannot complete on a guest that's no longer running
# on a VMware hypervisor -- see the script's own header comment and
# OPERATIONS-GUIDE.md's troubleshooting entry for VMware Tools uninstall
# failures for the full explanation (MSI error 1603 from a VM_LogStart/
# VM_CheckRequirements custom action that validates the hypervisor).
# Placing this script in the guest lets its administrator run it manually
# once the guest has actually booted on KVM and can be verified -- this is
# deliberately NOT run automatically as a firstboot script, since removing
# registry/service/file state is an irreversible guest-side change that
# should happen under administrator control with the guest reachable to
# confirm the result, not unattended during conversion.
upload_vmware_cleanup_script() {
  local script_src="/scripts/remove-vmware-tools.ps1"
  local script_dest='/Program Files/Guestfs/remove-vmware-tools.ps1'

  if [ ! -f "${script_src}" ]; then
    log_warn "remove-vmware-tools.ps1 not found in the image at ${script_src} -- skipping upload. This should not happen with the stock Dockerfile; check for a locally patched image."
    return 0
  fi

  log_info "Uploading VMware Tools cleanup script into the guest for manual use after boot (see OPERATIONS-GUIDE.md)"
  if ! guestfish --rw -a "${TARGET_DEVICE}" -i -- \
       mkdir-p '/Program Files/Guestfs' : \
       upload "${script_src}" "${script_dest}" 2>&1; then
    log_warn "Failed to upload remove-vmware-tools.ps1 into the guest -- this does not affect the conversion result, but you will need to copy the script in manually. See OPERATIONS-GUIDE.md for its content and usage. Common cause: the guest's NTFS filesystem has a 'hibernated' or 'not cleanly unmounted' flag set, which libguestfs refuses to mount read-write as a safety measure (confirmed in this project's own testing via ntfs-3g.probe exit code 14 after the guest had been booted and stopped without a clean Windows shutdown) -- running `ntfsfix` against the OS partition clears this, but is not done automatically here since it modifies guest filesystem metadata outside the scope of a normal conversion run."
    return 0
  fi
  log_info "Uploaded cleanup script to '${script_dest}' inside the guest (run it from an elevated PowerShell prompt after boot)"
}

case "${CONVERT_MODE}" in
  direct)
    log_info "Writing directly onto ${TARGET_DEVICE} (mode=direct, no intermediate RAW file)"
    do_direct_write

    if [ "${KEEP_WORK_DIR:-false}" != "true" ]; then
      log_debug "Cleaning up ${WORK_DIR}"
      rm -rf "${WORK_DIR:?}"/input "${WORK_DIR:?}"/raw
    fi

    log_info "Import complete: ${SRC_PATH} -> ${TARGET_DEVICE}"
    exit 0
    ;;

  direct-v2v)
    log_info "Writing directly onto ${TARGET_DEVICE}, then running virt-v2v-in-place (mode=direct-v2v)"
    log_warn "virt-v2v-in-place modifies TARGET_DEVICE's data IN PLACE with no backup. If it fails, TARGET_DEVICE may be left in an unknown, possibly corrupted state. Only use this mode against a freshly provisioned, disposable target volume."
    do_direct_write

    if [ "${KEEP_WORK_DIR:-false}" != "true" ]; then
      log_debug "Cleaning up ${WORK_DIR}"
      rm -rf "${WORK_DIR:?}"/input "${WORK_DIR:?}"/raw
    fi

    if [ -n "${DISK_SELECTOR:-}" ]; then
      log_warn "DISK_SELECTOR is set but only affects the initial direct-mode write under CONVERT_MODE=direct-v2v -- the subsequent virt-v2v-in-place step operates on the whole TARGET_DEVICE as a single disk, with no disk-selection concept of its own."
    fi

    VIRT_V2V_IN_PLACE_FSTRIM_FLAG=""
    if [ "${NO_FSTRIM:-false}" = "true" ]; then
      VIRT_V2V_IN_PLACE_FSTRIM_FLAG="--no-fstrim"
    fi

    VIRT_V2V_DEBUG_FLAG=""
    if [ "${ENABLE_DEBUG:-false}" = "true" ]; then
      VIRT_V2V_DEBUG_FLAG="-v"
    fi

    log_info "Running virt-v2v-in-place against ${TARGET_DEVICE} for driver injection/boot-config fixup"
    # shellcheck disable=SC2086
    run_with_progress virt-v2v-in-place -i disk "${TARGET_DEVICE}" ${VIRT_V2V_IN_PLACE_FSTRIM_FLAG} ${VIRT_V2V_DEBUG_FLAG}
    sync
    log_info "virt-v2v-in-place completed"

    if [ "${UPLOAD_VMWARE_CLEANUP_SCRIPT:-true}" = "true" ]; then
      upload_vmware_cleanup_script
    fi

    log_info "Import complete: ${SRC_PATH} -> ${TARGET_DEVICE} (direct write + virt-v2v-in-place)"
    exit 0
    ;;

  *)
    fail "unknown CONVERT_MODE '${CONVERT_MODE}' (expected 'direct' or 'direct-v2v')"
    ;;
esac
