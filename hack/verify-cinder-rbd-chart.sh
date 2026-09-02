#!/usr/bin/env bash
# Copyright (c) 2024-2026 Wind River Systems, Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Render checks for the Cinder RBD CSI Helm chart.
#
# These assert properties that are silently dangerous when wrong rather than
# loudly broken: a missing FSID disables an identity check, a read-only /sys
# makes every map fail, and a leaked key in rendered output is unrecoverable.

set -o errexit
set -o nounset
set -o pipefail

chart_dir="charts/cinder-rbd-csi-plugin"
testdata="${chart_dir}/testdata"
valid="${testdata}/minimal-valid-values.yaml"

fail() { echo "FAIL: $*" >&2; exit 1; }
pass() { echo "  ok: $*"; }

render() { helm template rbd "${chart_dir}" "$@" 2>/dev/null; }

# expect_render_failure <message-fragment> <helm args...>
expect_render_failure() {
  local want="$1"; shift
  local out
  if out=$(helm template rbd "${chart_dir}" "$@" 2>&1); then
    fail "rendering should have failed (expected: ${want})"
  fi
  grep -q -- "${want}" <<<"${out}" || fail "expected failure mentioning '${want}', got: ${out}"
  pass "rejected: ${want}"
}

echo "==> 1. lint"
helm lint "${chart_dir}" -f "${valid}" >/dev/null || fail "helm lint failed"
pass "helm lint"

echo "==> 2. guards reject unsafe configurations"
# An empty FSID would disable identity check 1 of the pre-publish gate.
expect_render_failure "expectedFsid is required" -f /dev/null
# A writable non-exclusive mapping defeats the single-writer guarantee.
expect_render_failure "exclusive must be true" -f "${valid}" \
  --set driverConfig.rbd.exclusive=false
# Only kernel RBD is supported.
expect_render_failure 'mounter must be "krbd"' -f "${valid}" \
  --set driverConfig.rbd.mounter=nbd
# A typo such as "delet" must not be silently read as retain.
expect_render_failure "deleteVolumeMode must be" -f "${valid}" \
  --set driverConfig.volume.deleteVolumeMode=delet
# Retain-only: the controller cannot prove no node holds a kernel mapping, so
# asking it to destroy Cinder volumes must be refused at render time. Refusing
# here rather than at the DeleteVolume RPC is deliberate — failing the RPC would
# leave the PersistentVolume in a delete loop that never completes.
expect_render_failure "deleteVolumeMode must be" -f "${valid}" \
  --set driverConfig.volume.deleteVolumeMode=delete
# The node plugin cannot read a credential without a Secret name.
expect_render_failure "cephCredential.secretName is required" -f "${valid}" \
  --set cephCredential.secretName=""
# Creating the Secret without a key would render an empty credential.
expect_render_failure "cephCredential.userKey is empty" -f "${valid}" \
  --set cephCredential.create=true

echo "==> 3. node plugin host access"
ds=$(render -f "${valid}" | awk '/kind: DaemonSet/,0')

# /sys must be writable: rbd device map/unmap work by writing to
# /sys/bus/rbd/{add,remove}. A readOnly mount makes every map fail.
sys_block=$(grep -A1 'mountPath: /sys$' <<<"${ds}" || true)
[[ -n "${sys_block}" ]] || fail "/sys is not mounted in the node plugin"
grep -q 'readOnly: true' <<<"${sys_block}" && fail "/sys must NOT be readOnly (rbd map writes to /sys/bus/rbd/add)"
pass "/sys mounted read-write"

grep -q 'mountPath: /dev' <<<"${ds}" || fail "/dev is not mounted"
pass "/dev mounted"

grep -q 'privileged: true' <<<"${ds}" || fail "node plugin is not privileged"
pass "node plugin privileged"

# hostPID is an iSCSI-era requirement this driver does not have.
grep -q 'hostPID: false' <<<"${ds}" || fail "hostPID should be false for the RBD driver"
pass "hostPID disabled"

# The runtime directory holds generated keyrings and must be memory-backed.
grep -q 'medium: Memory' <<<"${ds}" || fail "the runtime directory must be memory-backed"
pass "runtime directory memory-backed"

# The credential is projected read-only with a restrictive mode.
grep -q 'defaultMode: 0400' <<<"${ds}" || fail "the Ceph credential must be projected with mode 0400"
pass "credential projected 0400"

echo "==> 4. no iSCSI residue"
full=$(render -f "${valid}")
for path in '/etc/iscsi' '/var/lib/iscsi' '/run/lock/iscsi'; do
  grep -q -- "${path}" <<<"${full}" && fail "iSCSI path ${path} leaked into the RBD chart"
done
grep -qi 'iscsiadm\|initiatorname\|chap' <<<"${full}" && fail "iSCSI-specific configuration leaked into the RBD chart"
pass "no iSCSI paths or options"

echo "==> 5. rendered output carries no credential material"
# The chart must never emit a key. cephCredential.create defaults to false, so
# the Secret is only referenced, never populated.
grep -qE '^\s+(userKey|key):\s*\S' <<<"${full}" && fail "rendered output appears to contain key material"
grep -q 'AQ' <<<"${full}" && fail "rendered output contains something resembling a Ceph key"
pass "no key material in rendered output"

echo "==> 6. cacert indirection"
# Only the helper may dereference .Values.cacert, so the legacy no-cacert path
# cannot regress.
if command -v rg >/dev/null 2>&1; then
  offenders=$(rg -l '\.Values\.cacert' "${chart_dir}/templates" | grep -v '_helpers.tpl' || true)
else
  offenders=$(grep -rl '\.Values\.cacert' "${chart_dir}/templates" | grep -v '_helpers.tpl' || true)
fi
[[ -z "${offenders}" ]] || fail "templates dereference .Values.cacert directly: ${offenders}"
pass "cacert accessed only through the helper"

legacy=$(render -f "${testdata}/legacy-values-no-cacert.yaml")
grep -q 'name: cacert' <<<"${legacy}" && fail "a cacert volume was emitted for a legacy values file"
pass "no cacert volume without the option"

enabled=$(render -f "${testdata}/cacert-enabled-values.yaml")
grep -q 'name: cacert' <<<"${enabled}" || fail "no cacert volume when the option is enabled"
grep -q 'mountPath: /etc/ssl/certs' <<<"${enabled}" || fail "cacert not mounted at /etc/ssl/certs"
pass "cacert volume emitted when enabled"

echo "==> 7. CSI driver object and CDI profile"
grep -q 'name: cinder-rbd.csi.windriver.com' <<<"${full}" || fail "CSIDriver object missing"
grep -q 'attachRequired: true' <<<"${full}" || fail "attachRequired must be true (external-attacher drives publish)"
pass "CSIDriver object correct"

# CDI defaults unknown provisioners to Filesystem, which this driver rejects,
# leaving DataVolume PVCs Pending. The profile must exist and be Block/RWO.
profile=$(awk '/kind: StorageProfile/,0' <<<"${full}")
[[ -n "${profile}" ]] || fail "CDI StorageProfile missing"
grep -q 'volumeMode: Block' <<<"${profile}" || fail "StorageProfile must declare volumeMode: Block"
grep -q 'ReadWriteOnce' <<<"${profile}" || fail "StorageProfile must declare ReadWriteOnce"
pass "CDI StorageProfile declares Block + ReadWriteOnce"

# The profile name must equal the StorageClass name or CDI ignores it.
sc_name=$(grep -A2 'kind: StorageClass' <<<"${full}" | grep 'name:' | head -1 | awk '{print $2}')
prof_name=$(grep -A3 'kind: StorageProfile' <<<"${full}" | grep 'name:' | head -1 | awk '{print $2}')
[[ "${sc_name}" == "${prof_name}" ]] || \
  fail "StorageProfile name (${prof_name}) must equal the StorageClass name (${sc_name})"
pass "StorageProfile name matches the StorageClass"

echo "==> 8. block-only StorageClass"
grep -q 'allowVolumeExpansion: false' <<<"${full}" || fail "expansion must not be advertised"
pass "volume expansion not advertised"

echo
echo "All Cinder RBD chart checks passed."
