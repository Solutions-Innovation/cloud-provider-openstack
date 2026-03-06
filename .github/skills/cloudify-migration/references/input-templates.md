# Input Templates & e2e Configuration

This reference contains the full YAML input templates for warm and cold V2O
migration blueprints, plus the defaults tables for different environments.

## Warm Blueprint Input Template

Use this template when the blueprint ID contains `warm` (e.g. `warm-migration`,
`warm-rhoso`). Replace `<SRC_VM_NAME>` and `<STORAGE_CLASS>` with user values.

```yaml
src_vm_name: "<SRC_VM_NAME>"

dst_vm_settings:
  vm_name:
    get_input: src_vm_name
  vm_zone_id: ""
  disk_bus: virtio
  vm_flavor_id: m1.large
  vm_network-id: vlan-2011
  vm_4_fixip: ""
  vm_keyname: fedora-key

warm_migration_parameters:
  precopy_period: "600"

vmware_vcenter_config:
  govc_url: "https://69.167.148.53"
  govc_username: "administrator@vsphere.local"
  govc_password_secret_ref: vcenter_password

openstack_settings:
  openstack_auth_url: "http://keystone.openstack.svc.cluster.local/v3"
  openstack_ep_ip: "69.167.148.57"
  openstack_auth_type: password
  openstack_app_cred_id: openstack_app_cred_id
  openstack_app_cred_secret: openstack_app_cred_secret
  openstack_password: openstack_password
  openstack_region_name: "672c9fb4061a4920b4b543583cfcf706"

kubernetes_master_endpoint: "https://192.168.206.1:6443"

kubernetes_client_config:
  configuration:
    api_options:
      host:
        get_input: kubernetes_master_endpoint
      api_key:
        get_secret: kubernetes_admin_token
      debug: false
      verify_ssl: false

cdi_settings:
  vddk_credential-secret-ref: vddk-credentials
  storage_size: "50Gi"
  storage_class: "<STORAGE_CLASS>"
  enable_resize: "false"
  via_service_account: vi-admin
  enable_preallocate: "false"
  enable_efi: "true"
  runtime_env: openstack
  enable_debug: "false"
  container: "registry.local:9001/public/fedora-libguestfs:v2.41.8"
  mode: vmware
  timeout_image_prepare: "1800"
  enable_boot_from_volume: "false"

validate_status: "True"
```

## Cold Blueprint Input Template

Use this template when the blueprint ID contains `cold` (e.g. `cold-migration`,
`cold-rhoso`). Replace `<SRC_VM_NAME>` and `<STORAGE_CLASS>` with user values.

```yaml
src_vm_name: "<SRC_VM_NAME>"
src_vm_rootdisk: "single"

dst_vm_settings:
  vm_name:
    get_input: src_vm_name
  vm_zone_id: ""
  disk_bus: virtio
  vm_flavor_id: m1.large
  vm_networks:
    - vlan-2011
  vm_network_default: vlan-2011
  vm_network_mapping:
    - "vlan-2011:network-12"
  vm_4_fixip: ""
  vm_keyname: fedora-key

vmware_vcenter_config:
  govc_url: "https://69.167.148.53"
  govc_username: "administrator@vsphere.local"
  govc_password_secret_ref: vcenter_password

openstack_settings:
  openstack_auth_url: "http://keystone.openstack.svc.cluster.local/v3"
  openstack_ep_ip: "69.167.148.57"
  openstack_auth_type: password
  openstack_app_cred_id: openstack_app_cred_id
  openstack_app_cred_secret: openstack_app_cred_secret
  openstack_password: openstack_password
  openstack_region_name: "672c9fb4061a4920b4b543583cfcf706"

kubernetes_master_endpoint: "https://192.168.206.1:6443"

kubernetes_client_config:
  configuration:
    api_options:
      host:
        get_input: kubernetes_master_endpoint
      api_key:
        get_secret: kubernetes_admin_token
      debug: false
      verify_ssl: false

migration_system_settings:
  storage_class: "<STORAGE_CLASS>"
  via_service_account: vi-admin
  runtime_env: openstack
  enable_debug: "false"
  enable_efi: "true"
  container: "registry.local:9001/public/fedora-libguestfs:v2.41.8"

validate_status: "True"
```

## iSCSI-Cinder CSI e2e Testing Context

When using the cloudify-migration skill for Phase 5 (CDI Multi-Phase Precopy)
e2e testing of the iSCSI-Cinder CSI plugin:

1. **StorageClass must be `csi-sc-cinder-iscsi`** — this routes PVCs to the
   `cinder-iscsi.csi.windriver.com` CSI driver instead of the default NFS-backed
   StorageClass.

2. **The CSI plugin must be deployed first** — use the `dev-deploy` skill to build,
   push, and deploy the iSCSI-Cinder CSI plugin to the staging cluster before
   triggering a migration.

3. **Warm migration exercises the full CSI lifecycle:**
   - `install` → CDI creates PVC → `CreateVolume` + `ControllerPublishVolume` +
     `NodeStageVolume` (iSCSI login) + `NodePublishVolume` (bind mount)
   - Precopy runs with CDI importer reading from VMware VDDK, writing to iSCSI block device
   - `cutover` → Final sync → `NodeUnpublishVolume` + `NodeUnstageVolume` (iSCSI logout) +
     `ControllerUnpublishVolume` (attachment rotation) →
     New CDI stage → full re-attach cycle
   - `uninstall` → PVC delete → `DeleteVolume` (attachment cleanup)

4. **Attachment rotation validation:** Between CDI stages (during cutover),
   `ControllerUnpublishVolume` deletes + recreates the Cinder v3 attachment.
   The next `ControllerPublishVolume` updates the new attachment connector.
   This is the critical iSCSI-specific behavior being tested.

## Input Template Defaults

### Generic defaults (conductor57 internal OpenStack)

Used when explicitly selecting non-iSCSI StorageClasses (e.g. `general`).

| Input | Default | User-provided? |
|-------|---------|---------------|
| `src_vm_name` | `fedora-bios` | **Yes — always ask** |
| `storage_class` | `general` | **Yes — ask** |
| `vm_flavor_id` | `m1.large` | Rarely changed |
| `vm_network-id` | `vlan-2011` | Rarely changed |
| `openstack_auth_url` | `http://keystone.openstack.svc.cluster.local/v3` | Fixed |
| `openstack_ep_ip` | `69.167.148.57` | Fixed |
| `openstack_region_name` | `672c9fb4061a4920b4b543583cfcf706` | Fixed |

### iSCSI CSI e2e defaults (fast-path)

Used when the user says "default" / "go ahead" or selects all recommended options.
Stored in `assets/e2e-iscsi-defaults.yaml`.

| Input | e2e Default | Notes |
|-------|-------------|-------|
| `src_vm_name` | `fedora-bios` | Known test VM |
| `storage_class` | `csi-sc-cinder-iscsi` | iSCSI-Cinder CSI driver |
| `vm_flavor_id` | `ds4G` | e2e target flavor |
| `vm_network-id` | `shared` | e2e target network |
| `openstack_auth_url` | `http://69.167.149.97/identity` | e2e OpenStack |
| `openstack_ep_ip` | `69.167.149.97` | e2e OpenStack |
| `openstack_password` | `openstack_keys_97` | Secret ref for e2e OpenStack |
| `openstack_region_name` | `RegionOne` | e2e OpenStack |
| `precopy_period` | `600` (10 min) | Occasionally changed |
| `storage_size` | `50Gi` | Occasionally changed |
| `govc_url` | `https://69.167.148.53` | Fixed for conductor57 |
| `kubernetes_master_endpoint` | `https://192.168.206.1:6443` | Fixed for conductor57 |
