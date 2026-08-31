<!-- START doctoc generated TOC please keep comment here to allow auto update -->
<!-- DON'T EDIT THIS SECTION, INSTEAD RE-RUN doctoc TO UPDATE -->
**Table of Contents**  *generated with [DocToc](https://github.com/thlorenz/doctoc)*

- [NFS-backed Cinder Volume for WRCP Migration](#nfs-backed-cinder-volume-for-wrcp-migration)
  - [Overview](#overview)
  - [Prerequisites](#prerequisites)
  - [Configure NFS Backend for OpenStack Cinder](#configure-nfs-backend-for-openstack-cinder)
    - [Configure the NFS shares file](#configure-the-nfs-shares-file)
    - [Configure cinder.conf](#configure-cinderconf)
  - [Deploy the Cinder CSI Driver](#deploy-the-cinder-csi-driver)
  - [Create a StorageClass for NFS-backed Cinder Volumes](#create-a-storageclass-for-nfs-backed-cinder-volumes)
  - [Migrate Existing PersistentVolumes](#migrate-existing-persistentvolumes)
    - [Identify volumes to migrate](#identify-volumes-to-migrate)
    - [Migrate in-tree PersistentVolumes to Cinder CSI](#migrate-in-tree-persistentvolumes-to-cinder-csi)
    - [Migrate using Backup and Restore](#migrate-using-backup-and-restore)
  - [Verify the Migration](#verify-the-migration)
  - [Troubleshooting](#troubleshooting)

<!-- END doctoc generated TOC please keep comment here to allow auto update -->

# NFS-backed Cinder Volume for WRCP Migration

## Overview

This guide explains how to use NFS-backed OpenStack Cinder volumes with the [Cinder CSI Plugin](../using-cinder-csi-plugin.md) in a Workload Runtime Cloud Provider (WRCP) environment, and how to migrate existing persistent volumes to use the CSI driver.

OpenStack Cinder supports multiple storage backends, including NFS. When Cinder is configured with an NFS backend, block volumes are stored as files on an NFS share rather than on a traditional block storage device. This approach can be useful in environments where NFS is the available shared storage infrastructure.

In WRCP environments (such as those running on vSphere with Tanzu supervisor clusters), migrating to the Cinder CSI driver ensures that persistent storage is managed through the standardized Container Storage Interface, enabling features like volume snapshots, cloning, and online resizing.

## Prerequisites

Before beginning, ensure the following requirements are met:

- OpenStack environment with Cinder configured (Queens release or later recommended)
- NFS server accessible from all OpenStack compute nodes
- Kubernetes cluster (v1.21 or later) running on OpenStack
- `kubectl` configured to communicate with the cluster
- Cinder CSI Plugin deployed on the cluster (see [Cinder CSI Plugin deployment](../using-cinder-csi-plugin.md#driver-deployment))
- OpenStack credentials with permission to manage volumes and volume types

## Configure NFS Backend for OpenStack Cinder

To use NFS-backed Cinder volumes, the OpenStack Cinder service must be configured to use the NFS volume driver.

### Configure the NFS shares file

On the Cinder volume node, create or update the NFS shares file (default: `/etc/cinder/nfs_shares`) with the NFS server and export path:

```
<nfs-server-ip>:/path/to/nfs/export
```

For example:

```
192.168.1.100:/exports/cinder
```

Ensure the NFS export is accessible and that the `cinder` user has read/write permissions on the export.

### Configure cinder.conf

Update `/etc/cinder/cinder.conf` to use the NFS driver:

```ini
[DEFAULT]
enabled_backends = nfs-backend

[nfs-backend]
volume_driver = cinder.volume.drivers.nfs.NfsDriver
nfs_shares_config = /etc/cinder/nfs_shares
nfs_mount_point_base = /var/lib/cinder/mnt
volume_backend_name = nfs-backend
```

After updating the configuration, restart the Cinder volume service:

```bash
systemctl restart openstack-cinder-volume
# or, for devstack-based environments:
# sudo systemctl restart devstack@c-vol
```

Create a corresponding Cinder volume type that maps to the NFS backend:

```bash
openstack volume type create nfs-type
openstack volume type set nfs-type --property volume_backend_name=nfs-backend
```

## Deploy the Cinder CSI Driver

If the Cinder CSI Plugin is not yet deployed, follow the [deployment instructions](../using-cinder-csi-plugin.md#driver-deployment).

Ensure the `cloud-config` secret contains valid OpenStack credentials and the correct Cinder endpoint:

```bash
kubectl -n kube-system get secret cloud-config
```

Verify that the CSI driver pods are running:

```bash
kubectl get pods -n kube-system -l app=csi-cinder-controllerplugin
kubectl get pods -n kube-system -l app=csi-cinder-nodeplugin
```

Expected output:

```
NAME                                        READY   STATUS    RESTARTS   AGE
csi-cinder-controllerplugin-xxxxx-yyyyy     6/6     Running   0          5m
csi-cinder-nodeplugin-xxxxx                 3/3     Running   0          5m
```

## Create a StorageClass for NFS-backed Cinder Volumes

Create a `StorageClass` that references the NFS-backed Cinder volume type:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: csi-cinder-nfs
provisioner: cinder.csi.openstack.org
parameters:
  type: nfs-type
reclaimPolicy: Delete
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
```

Apply the StorageClass:

```bash
kubectl apply -f storageclass-csi-cinder-nfs.yaml
```

Verify the StorageClass was created:

```bash
kubectl get storageclass csi-cinder-nfs
```

## Migrate Existing PersistentVolumes

### Identify volumes to migrate

List existing PersistentVolumes (PVs) backed by in-tree Cinder or WRCP storage:

```bash
kubectl get pv -o=custom-columns='NAME:.metadata.name,DRIVER:.spec.csi.driver,CAPACITY:.spec.capacity.storage,STATUS:.status.phase'
```

For in-tree Cinder volumes (provisioned by `kubernetes.io/cinder`), identify them with:

```bash
kubectl get pv -o jsonpath='{range .items[?(@.spec.cinder)]}{.metadata.name}{"\t"}{.spec.cinder.volumeID}{"\n"}{end}'
```

### Migrate in-tree PersistentVolumes to Cinder CSI

Starting from Kubernetes v1.21, CSI migration for OpenStack Cinder is enabled by default. This means existing in-tree Cinder volumes are automatically migrated to use the Cinder CSI driver without moving data.

To ensure CSI migration is active, verify that the `CSIMigrationOpenStack` feature gate is enabled on all nodes:

```bash
kubectl get csinode -o yaml | grep -A5 "migrated-plugins"
```

The annotation `storage.alpha.kubernetes.io/migrated-plugins: kubernetes.io/cinder` confirms that migration is active for a node.

For volumes that were provisioned by the in-tree provider and are not automatically migrated, update the `pv.kubernetes.io/provisioned-by` annotation:

```bash
kubectl annotate --overwrite pv <pv-name> pv.kubernetes.io/provisioned-by=cinder.csi.openstack.org
```

### Migrate using Backup and Restore

For workloads that require moving data to a new NFS-backed Cinder volume (for example, when switching storage backends), follow these steps:

1. **Scale down the workload** to detach the volume:

   ```bash
   kubectl scale deployment <deployment-name> --replicas=0
   ```

2. **Create a new PVC** targeting the NFS-backed StorageClass:

   ```yaml
   apiVersion: v1
   kind: PersistentVolumeClaim
   metadata:
     name: <new-pvc-name>
   spec:
     accessModes:
       - ReadWriteOnce
     storageClassName: csi-cinder-nfs
     resources:
       requests:
         storage: <size>Gi
   ```

   ```bash
   kubectl apply -f new-pvc.yaml
   ```

3. **Copy data** from the old PVC to the new PVC using a migration pod:

   ```yaml
   apiVersion: v1
   kind: Pod
   metadata:
     name: volume-migration
   spec:
     containers:
     - name: migration
       image: busybox
       command: ["sh", "-c", "cp -av /source/. /destination/ && echo 'Migration complete'"]
       volumeMounts:
       - name: source-volume
         mountPath: /source
       - name: destination-volume
         mountPath: /destination
     restartPolicy: Never
     volumes:
     - name: source-volume
       persistentVolumeClaim:
         claimName: <old-pvc-name>
     - name: destination-volume
       persistentVolumeClaim:
         claimName: <new-pvc-name>
   ```

   ```bash
   kubectl apply -f migration-pod.yaml
   kubectl wait --for=jsonpath='{.status.phase}'=Succeeded pod/volume-migration --timeout=300s
   ```

4. **Update the workload** to use the new PVC and scale it back up:

   ```bash
   kubectl patch deployment <deployment-name> -p \
     '{"spec":{"template":{"spec":{"volumes":[{"name":"<volume-name>","persistentVolumeClaim":{"claimName":"<new-pvc-name>"}}]}}}}'
   kubectl scale deployment <deployment-name> --replicas=<original-replicas>
   ```

5. **Clean up** the migration pod and old PVC once the workload is running correctly:

   ```bash
   kubectl delete pod volume-migration
   kubectl delete pvc <old-pvc-name>
   ```

## Verify the Migration

After migration, confirm that workloads are running and using the NFS-backed Cinder volumes:

```bash
# Check that PVCs are bound
kubectl get pvc

# Check that PVs are using the Cinder CSI driver
kubectl get pv -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.csi.driver}{"\n"}{end}'

# Verify volume attachments
kubectl get volumeattachment
```

To confirm that an NFS-backed Cinder volume is mounted correctly on a node:

```bash
# On the node where the pod is scheduled
mount | grep cinder
```

You should see an NFS mount corresponding to the Cinder volume.

## Troubleshooting

**Volume fails to attach with NFS backend:**

Ensure that the NFS export is reachable from all compute nodes where workloads may be scheduled. Check that firewall rules allow NFS traffic (TCP/UDP port 2049 and portmapper port 111).

```bash
# Test NFS connectivity from a compute node
showmount -e <nfs-server-ip>
```

**CSI driver cannot find the volume type:**

Verify that the Cinder volume type name in the `StorageClass` parameters matches an existing type in OpenStack:

```bash
openstack volume type list
```

**PVC remains in Pending state:**

Check the CSI controller plugin logs for provisioning errors:

```bash
kubectl logs -n kube-system -l app=csi-cinder-controllerplugin -c csi-provisioner
```

**In-tree volumes not migrating automatically:**

Ensure the `CSIMigrationOpenStack` feature gate is enabled. For Kubernetes v1.24 and later, CSI migration for OpenStack Cinder is enabled by default and the in-tree driver is removed. For earlier versions, explicitly enable the feature gate:

```yaml
# In kubelet config
featureGates:
  CSIMigration: true
  CSIMigrationOpenStack: true
```

For more information on Cinder CSI migration, refer to [Migrate from in-tree cloud provider to openstack-cloud-controller-manager and enable CSIMigration](../../openstack-cloud-controller-manager/migrate-to-ccm-with-csimigration.md).
