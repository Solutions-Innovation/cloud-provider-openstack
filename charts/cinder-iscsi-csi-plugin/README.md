# Openstack Cinder iSCSI CSI Plugin

This chart deploys the iSCSI-backed Cinder CSI driver for OpenStack, designed for
WRC migration workflows (V2O/O2O) using Pure Storage FlashArray iSCSI backends.

## Key differences from cinder-csi-plugin

- **Block-only**: Only `volumeMode: Block` is supported (no filesystem volumes)
- **No snapshotter/resizer**: Only attacher + provisioner sidecars
- **iSCSI host mounts**: Node DaemonSet mounts `/etc/iscsi`, `/var/lib/iscsi`, `/run/lock/iscsi`, `/sys`
- **hostPID: true**: Required for iscsiadm ↔ host iscsid communication
- **ConfigMap for driver.conf**: iSCSI and volume lifecycle configuration separate from cloud credentials
- **Driver name**: `cinder-iscsi.csi.windriver.com`

## Installation

```bash
helm install cinder-iscsi-csi charts/cinder-iscsi-csi-plugin/ \
  --namespace kube-system \
  --set secret.enabled=true \
  --set secret.create=true \
  --set secret.name=cinder-iscsi-cloud-config \
  --set secret.data."cloud\.conf"="$(cat /etc/config/cloud.conf)"
```

## Configuration

See [values.yaml](values.yaml) for all configurable options.
