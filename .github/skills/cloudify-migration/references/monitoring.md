# Monitoring, Debugging & Edge Cases

Operational reference for monitoring V2O migration progress via Cloudify CLI
and Kubernetes, debugging common failures, and handling edge cases.

## Cloudify Monitoring Commands

```bash
# Activate environment (MUST do first in every terminal session)
source ~/cloudifyenv/bin/activate

# List all deployments
cfy deployments list

# List executions for a deployment
cfy executions list -d <DEPLOYMENT_ID>

# Get details of a specific execution
cfy executions get <EXECUTION_ID>

# Get deployment inputs (useful for debugging)
cfy deployments inputs <DEPLOYMENT_ID>

# List node instances for a deployment (shows node IDs, states, host IDs)
cfy node-instances list -d <DEPLOYMENT_ID>

# List available blueprints
cfy blueprints list

# Check current profile
cfy profiles list
```

## Kubernetes CDI Monitoring Commands

Use these commands with `--kubeconfig=$HOME/.kube/config-staging` to monitor
CDI resources during migration:

```bash
# List all DataVolumes
kubectl get datavolume

# Get DataVolume detail (phase, progress, conditions)
kubectl get datavolume <DV_NAME> -o yaml

# List PVCs — check for target PVC (Pending) and prime PVC (Bound)
kubectl get pvc

# Describe PVC to see events (ImportPaused, Provisioning, etc.)
kubectl describe pvc <PVC_NAME>

# Check StorageProfile for correct volumeMode
kubectl get storageprofile csi-sc-cinder-iscsi -o yaml

# List pods with migration annotations
kubectl get pods -o wide | grep -E 'fedora|snapshot|ation'

# Check CSI driver is running
kubectl get pods -n kube-system | grep cinder-iscsi

# Check PV binding (after cutover/rebind)
kubectl get pv | grep csi-sc-cinder-iscsi

# VolumeImportSource (CDI internal)
kubectl get volumeimportsource
```

## Debugging Common Failures

### PVC stuck in Pending (not expected Paused state)

1. Check StorageProfile: `kubectl get storageprofile csi-sc-cinder-iscsi -o yaml`
   - If `claimPropertySets` is empty → apply the StorageProfile patch
   - If `volumeMode: Filesystem` → patch to `volumeMode: Block`
2. Check CSI driver pods: `kubectl get pods -n kube-system | grep cinder-iscsi`
3. Check PVC events: `kubectl describe pvc <PVC_NAME>` — look for provisioning errors
4. Check CSI driver logs: `kubectl logs -n kube-system <controller-pod> -c cinder-csi-plugin`

### DataVolume stuck in ImportScheduled (importer pod not starting)

1. Check VDDK ConfigMap exists: `kubectl get cm v2v-vmware -n cdi`
2. Check importer pod events: `kubectl describe pod importer-<name>`
3. Check VDDK credentials secret: `kubectl get secret vddk-credentials`

### DataVolume shows Paused but progress < 100%

- Importer pod may have failed and restarted. Check:
  `kubectl get dv <name> -o jsonpath='{.status.conditions}'`
- Look for `Running` condition with `reason: Error`

### Target PVC never becomes Bound after cutover

- CDI Rebind may have failed. Check CDI controller logs:
  `kubectl logs -n cdi deploy/cdi-deployment -c cdi-controller | grep -i rebind`
- Verify VolumeImportSource has `FinalCheckpoint: true`
- Check if prime PVC still exists and is still Bound (rebind hasn't happened)

## Edge Cases

### Deployment already exists

If `cfy deployments create` fails with "already exists", ask the user to either:
- Choose a different deployment ID
- Delete the existing deployment first: `cfy deployments delete <ID>`

### Execution stuck or failed

If an execution shows `failed` or `started` for too long:
```bash
cfy executions list -d <DEPLOYMENT_ID>
cfy executions get <EXECUTION_ID>
```
Report the status and error. The user may need to:
- Cancel the execution: `cfy executions cancel <EXECUTION_ID>`
- Force-cancel: `cfy executions cancel <EXECUTION_ID> -f`
- Investigate via Cloudify Manager UI at `https://conductor57.windriver.liquidweb`

### Profile connectivity failure

If `cfy deployments list` fails:
1. Check the profile is active: `cfy profiles list`
2. Re-authenticate: `cfy profiles use conductor57.windriver.liquidweb -u admin -p 'admin' --ssl --rest-certificate ~/manager57.crt`
3. Check certificate exists: `ls -la ~/manager57.crt`
4. Check network connectivity: `ping -c1 conductor57.windriver.liquidweb`

### Installation status shows "requires_attention"

This is normal for completed/cleaned-up deployments. The `deployment_status`
and `installation_status` columns in `cfy deployments list` reflect the
current state — `inactive` means the deployment exists but no workflows are running.
