#!/usr/bin/env bash
# Copyright (c) 2024-2026 Wind River Systems, Inc.
# SPDX-License-Identifier: Apache-2.0
#
# Package the Cinder RBD CSI Helm chart locally.
#
# Runs the same lint and render checks as the release workflow, then writes
# _dist/openstack-cinder-rbd-csi-<version>.tgz. Nothing is pushed anywhere.
#
# Usage: hack/package-cinder-rbd-chart.sh [output-dir]

set -o errexit
set -o nounset
set -o pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${repo_root}"

chart_dir="charts/cinder-rbd-csi-plugin"
out_dir="${1:-_dist}"

command -v helm >/dev/null 2>&1 || { echo "helm is required but was not found in PATH" >&2; exit 1; }

# The default values deliberately fail rendering (expectedFsid has no safe
# default), so lint against the minimal valid configuration.
echo "==> helm lint"
helm lint "${chart_dir}" -f "${chart_dir}/testdata/minimal-valid-values.yaml"

echo "==> render safety checks"
bash hack/verify-cinder-rbd-chart.sh

echo "==> helm package"
mkdir -p "${out_dir}"
rm -f "${out_dir}"/openstack-cinder-rbd-csi-*.tgz
helm package "${chart_dir}" --destination "${out_dir}"
