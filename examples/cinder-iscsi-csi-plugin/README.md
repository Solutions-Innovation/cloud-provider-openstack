# Cinder iSCSI CSI Plugin — Dev Testing Examples

These examples demonstrate how to test the **cinder-iscsi CSI plugin** against
an OpenStack cloud with LVM iSCSI backing storage.

The driver supports **raw block volumes only**. Use `volumeMode: Block` and
`volumeDevices`; do not present these examples as a filesystem mount workflow.

## Prerequisites

1. A running Kubernetes cluster with the cinder-iscsi CSI plugin deployed
   (see `plugin/` folder below or `manifests/cinder-iscsi-csi-plugin/`).
2. An OpenStack cloud with Cinder configured to use the LVM iSCSI backend.
3. Network connectivity from Kubernetes nodes to the OpenStack endpoints
   and iSCSI targets.

## Automated Dev Workflow

Use the `/dev-deploy` agent skill to automate the full pipeline:
build image → push to registry → deploy to staging cluster → verify.

```bash
# In the Copilot chat:
/dev-deploy
```

The skill prompts for registry, kubeconfig, and token interactively.
See `.github/skills/dev-deploy/SKILL.md` for details.

## Deploy the Plugin

```bash
# Install the full CSI plugin stack (secret, driver, RBAC, controller, node)
kubectl apply -f plugin/
```

## Quick Start

For a presenter-oriented demo flow with validation steps, use
`demo-walkthrough.md` in this folder.

```bash
# 1. Deploy the secret and driver config (update cloud.conf with your credentials)
kubectl apply -f secret.yaml

# 2. Create the StorageClass
kubectl apply -f storageclass.yaml

# 3. Test with a raw block volume
kubectl apply -f block/block.yaml
```

## Cleanup

```bash
kubectl delete -f block/block.yaml
kubectl delete -f storageclass.yaml
kubectl delete -f secret.yaml
```

## Files

| File / Folder | Description |
|---------------|-------------|
| `plugin/` | All manifests to deploy the CSI plugin (secret, driver, RBAC, controller, node) |
| `secret.yaml` | Secret (cloud.conf) and ConfigMap (driver.conf) for dev target |
| `storageclass.yaml` | StorageClass using the cinder-iscsi CSI driver |
| `demo-walkthrough.md` | Presenter-focused runbook for demonstrating provisioning, raw block attachment, and iSCSI validation |
| `nginx.yaml` | Pod example that still uses a raw block PVC via `volumeDevices` |
| `block/block.yaml` | Pod with a raw block volume device |
