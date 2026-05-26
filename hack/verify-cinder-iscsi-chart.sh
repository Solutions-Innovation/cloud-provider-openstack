#!/usr/bin/env bash

set -euo pipefail

repo_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
chart_dir="$repo_root/charts/cinder-iscsi-csi-plugin"
legacy_values="$chart_dir/testdata/legacy-values-no-cacert.yaml"
cacert_values="$chart_dir/testdata/cacert-enabled-values.yaml"

tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

echo "Verifying legacy nil-cacert render remains upgrade-safe"
helm template cinder-iscsi-legacy "$chart_dir" -f "$legacy_values" >"$tmpdir/legacy-template.yaml"

if grep -q 'name: cacert' "$tmpdir/legacy-template.yaml"; then
	echo "legacy nil-cacert render unexpectedly emitted cacert resources" >&2
	exit 1
fi

echo "Verifying templates use the shared cacert helper instead of direct value dereferences"
if rg -n '\.Values\.cacert' "$chart_dir/templates" | grep -v '_helpers.tpl' >/dev/null; then
	echo "direct .Values.cacert dereference found outside _helpers.tpl" >&2
	rg -n '\.Values\.cacert' "$chart_dir/templates" | grep -v '_helpers.tpl' >&2
	exit 1
fi

echo "Verifying enabled cacert render still emits controller mount"
helm template cinder-iscsi-cacert "$chart_dir" -f "$cacert_values" >"$tmpdir/cacert-template.yaml"

grep -q 'name: cacert' "$tmpdir/cacert-template.yaml"
grep -q 'mountPath: /etc/ssl/certs' "$tmpdir/cacert-template.yaml"
grep -q 'path: /etc/ssl/certs' "$tmpdir/cacert-template.yaml"

echo "cinder-iscsi chart compatibility checks passed"
