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
# Same failure mode for the cloud config: an empty stringData renders cleanly and
# fails much later, as an empty OpenStack configuration in the controller.
expect_render_failure "secret.data is empty" -f "${valid}" \
  --set secret.create=true

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

# /lib/modules is an opt-in. The default must not mount it: the validated
# platform preloads the rbd module, and an undeclared value that silently
# defaults to off is indistinguishable from a typo, so both directions of the
# switch are asserted.
grep -q 'mountPath: /lib/modules' <<<"${ds}" && fail "/lib/modules must not be mounted by default"
pass "/lib/modules not mounted by default"

libmod=$(render -f "${valid}" --set csi.plugin.nodePlugin.mountLibModules=true \
  | awk '/kind: DaemonSet/,0')
grep -q 'mountPath: /lib/modules' <<<"${libmod}" \
  || fail "mountLibModules=true did not mount /lib/modules — the value is not wired up"
grep -A1 'mountPath: /lib/modules' <<<"${libmod}" | grep -q 'readOnly: true' \
  || fail "/lib/modules must be mounted readOnly"
pass "mountLibModules opt-in works and is read-only"

echo "==> 4. per-node scheduling limit"
full=$(render -f "${valid}")

# Keep the chart's scheduling limit explicit. A zero value means unlimited in
# CSI and would let Kubernetes schedule an unbounded number of this driver's
# volumes onto one node.
grep -q 'max-volumes-per-node = 32' <<<"${full}" \
  || fail "default max-volumes-per-node must be 32"
pass "default max-volumes-per-node is 32"

echo "==> 5. no iSCSI residue"
for path in '/etc/iscsi' '/var/lib/iscsi' '/run/lock/iscsi'; do
  grep -q -- "${path}" <<<"${full}" && fail "iSCSI path ${path} leaked into the RBD chart"
done
grep -qi 'iscsiadm\|initiatorname\|chap' <<<"${full}" && fail "iSCSI-specific configuration leaked into the RBD chart"
pass "no iSCSI paths or options"

echo "==> 6. rendered output carries no credential material"
# The chart must never emit a key. cephCredential.create defaults to false, so
# the Secret is only referenced, never populated.
grep -qE '^\s+(userKey|key):\s*\S' <<<"${full}" && fail "rendered output appears to contain key material"
grep -q 'AQ' <<<"${full}" && fail "rendered output contains something resembling a Ceph key"
pass "no key material in rendered output"

echo "==> 7. cacert indirection"
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

echo "==> 8. CSI driver object and CDI profile"
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

echo "==> 9. block-only StorageClass"
grep -q 'allowVolumeExpansion: false' <<<"${full}" || fail "expansion must not be advertised"
pass "volume expansion not advertised"

echo "==> 10. every value a template reads is declared"
# A template that reads .Values.some.undeclared.knob does not fail: Helm yields
# nil, so `if` and `with` blocks silently take the empty branch. The chart then
# has an invisible switch that no operator can discover from values.yaml and no
# reviewer can tell apart from a typo.
#
# The values tree comes from Helm rather than a YAML parser so this needs no
# Python packages beyond the standard library.
dump="${chart_dir}/templates/zz-values-dump.yaml"
cleanup_dump() { rm -f "${dump}"; }
trap cleanup_dump EXIT
printf '{{ .Values | toJson }}\n' > "${dump}"
values_json=$(helm template rbd "${chart_dir}" -f "${valid}" \
  --show-only templates/zz-values-dump.yaml 2>/dev/null | grep -v '^\(---\|#\)')
cleanup_dump
trap - EXIT
[[ -n "${values_json}" ]] || fail "could not obtain the resolved values tree from helm"

undeclared=$(VALUES_JSON="${values_json}" CHART_DIR="${chart_dir}" python3 - <<'PY'
import json, os, re, glob
values = json.loads(os.environ["VALUES_JSON"])
chart = os.environ["CHART_DIR"]

referenced = set()
for pattern in ("templates/**/*.yaml", "templates/**/*.tpl"):
    for path in glob.glob(os.path.join(chart, pattern), recursive=True):
        if path.endswith("zz-values-dump.yaml"):
            continue
        with open(path) as fh:
            for match in re.finditer(r"\.Values((?:\.[A-Za-z_][A-Za-z0-9_]*)+)", fh.read()):
                referenced.add(match.group(1).lstrip("."))

def declared(dotted):
    node = values
    for part in dotted.split("."):
        if not isinstance(node, dict) or part not in node:
            return False
        node = node[part]
    return True

for path in sorted(p for p in referenced if not declared(p)):
    print(path)
PY
) || fail "the undeclared-values check could not run"

if [[ -n "${undeclared}" ]]; then
  while read -r path; do
    [[ -n "${path}" ]] && echo "  undeclared: .Values.${path}" >&2
  done <<<"${undeclared}"
  fail "the templates above read values that values.yaml does not declare; declare them (with a default and a comment) so they are discoverable"
fi
pass "all $(grep -roh '\.Values\(\.[A-Za-z_][A-Za-z0-9_]*\)\+' "${chart_dir}/templates" | sort -u | wc -l | tr -d ' ') referenced value paths are declared"

echo "==> 11. example values stay valid"
example="examples/cinder-rbd-csi-plugin/values.yaml"
[[ -f "${example}" ]] || fail "${example} is missing"

# The example leaves the FSID for the operator, so it must not render as-is.
expect_render_failure "expectedFsid is required" -f "${example}"

ex_out=$(render -f "${example}" --set driverConfig.rbd.expectedFsid=c5f7876d-258c-4152-b26a-a3ab532fda28) \
  || fail "the example values do not render once the FSID is set"
pass "example renders once the FSID is set"

grep -q 'delete-volume-mode = retain' <<<"${ex_out}" || fail "example must render retain-only deletion"
grep -q 'exclusive = true' <<<"${ex_out}" || fail "example must render exclusive mapping"
grep -q 'mounter = krbd' <<<"${ex_out}" || fail "example must render the krbd mounter"
pass "example keeps retain, exclusive, and krbd"

# The example is committed and copied around; it must carry no credential.
grep -qE '^\s*(userKey|key|password|application-credential-secret):\s*\S' "${example}" \
  && fail "example values appear to contain a credential"
grep -q 'AQ[A-Za-z0-9+/]\{20,\}' "${example}" && fail "example values contain something resembling a Ceph key"
grep -q 'secretName: cinder-rbd-ceph-client' <<<"${ex_out}" || fail "example must reference the Ceph credential Secret by name"
pass "example carries no credential"

echo "==> 12. node health port"
# The node plugin is host-networked, so its health port is bound on the node. It
# must be movable when another host-network driver already holds 9808, and the
# sidecar argument, the declared port, and the probe target must move together.
node_ds() { awk '/^kind: DaemonSet/,/^---/' <<<"$1"; }
controller() { awk '/^kind: Deployment/,/^---/' <<<"$1"; }

default_ds=$(node_ds "${full}")
grep -q -- '--health-port=9808' <<<"${default_ds}" || fail "default sidecar must use --health-port=9808"
grep -q 'containerPort: 9808' <<<"${default_ds}" || fail "default node healthz port must be 9808"
pass "default node health port is 9808"

moved=$(render -f "${valid}" --set csi.plugin.nodePlugin.healthPort=9809)
moved_ds=$(node_ds "${moved}")
grep -q -- '--health-port=9809' <<<"${moved_ds}" || fail "override did not reach the liveness sidecar"
grep -q 'containerPort: 9809' <<<"${moved_ds}" || fail "override did not reach the healthz containerPort"
grep -q '9808' <<<"${moved_ds}" && fail "9808 remains in the node DaemonSet after overriding the health port"
grep -q 'port: healthz' <<<"${moved_ds}" || fail "the node liveness probe must target the named healthz port"
pass "override moves sidecar, containerPort, and probe together"

[[ "$(controller "${full}")" == "$(controller "${moved}")" ]] \
  || fail "the controller Deployment changed when only the node health port was overridden"
pass "controller unaffected by the node health port"

for bad in 0 70000 abc 98.5; do
  expect_render_failure "healthPort must be an integer between 1 and 65535" -f "${valid}" \
    --set-json "csi.plugin.nodePlugin.healthPort=$([[ ${bad} == abc ]] && echo '"abc"' || echo "${bad}")"
done

echo
echo "All Cinder RBD chart checks passed."
