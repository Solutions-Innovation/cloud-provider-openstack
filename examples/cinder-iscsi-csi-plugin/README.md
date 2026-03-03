# Cinder iSCSI CSI Plugin — Dev Testing Examples

These examples demonstrate how to test the **cinder-iscsi CSI plugin** against
an OpenStack cloud with LVM iSCSI backing storage.

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

```bash
# 1. Deploy the secret and driver config (update cloud.conf with your credentials)
kubectl apply -f secret.yaml

# 2. Create the StorageClass
kubectl apply -f storageclass.yaml

# 3. Test with nginx (filesystem volume)
kubectl apply -f nginx.yaml

# 4. Test with a raw block volume
kubectl apply -f block/block.yaml
```

## Cleanup

```bash
kubectl delete -f nginx.yaml
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
| `nginx.yaml` | Nginx pod with a filesystem PVC |
| `block/block.yaml` | Pod with a raw block volume device |
