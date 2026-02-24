# Build, Deploy & Configuration

## Build system

### Makefile entries

Add to `IMAGE_NAMES` and `BUILD_CMDS`:

```makefile
IMAGE_NAMES ?= ... cinder-nfs-csi-plugin ...
BUILD_CMDS  ?= ... cinder-nfs-csi-plugin ...
```

Sanity test target:
```makefile
test-cinder-nfs-csi-sanity: work
	go test $(GIT_HOST)/$(BASE_DIR)/tests/sanity/cinder-nfs
```

Cross-build entry:
```makefile
CGO_ENABLED=0 gox -parallel=$(GOX_PARALLEL) \
  -output="_dist/{{.OS}}-{{.Arch}}/{{.Dir}}" -osarch='$(TARGETS)' \
  $(GOFLAGS) $(if $(TAGS),-tags '$(TAGS)',) -ldflags '$(GOX_LDFLAGS)' \
  $(GIT_HOST)/$(BASE_DIR)/cmd/cinder-nfs-csi-plugin/
```

### Build commands

```bash
make cinder-nfs-csi-plugin                    # Build binary
make build-local-image-cinder-nfs-csi-plugin  # Build container image
make test-cinder-nfs-csi-sanity               # Sanity tests
make unit                                      # Unit tests (all packages)
make check                                     # Linting (golangci-lint)
```

### Dockerfile

**Development phase (current):** Debian-based with NFS tools via `apt`:

```dockerfile
FROM ${DEBIAN_IMAGE} AS cinder-nfs-csi-plugin
RUN clean-install nfs-common mount util-linux
COPY --from=builder /build/cinder-nfs-csi-plugin /bin/cinder-nfs-csi-plugin
COPY --from=certs /etc/ssl/certs /etc/ssl/certs
LABEL name="cinder-nfs-csi-plugin" \
      license="Apache Version 2.0"
CMD ["/bin/cinder-nfs-csi-plugin"]
```

**Production phase (TODO):** 3-step distroless build following `cinder-csi-plugin`:
1. Debian image → install `nfs-common` + run `tools/csi-nfs-deps.sh`
2. Distroless image → copy `/dest` + validate with `tools/csi-nfs-deps-check.sh`
3. Final distroless image with checked deps + binary

NFS tools bundled in container (not host-dependent), matching existing cinder-csi-plugin
convention.

## Deployment manifests

### Controller Deployment (`cinder-nfs-csi-controllerplugin.yaml`)

- Image: `registry.k8s.io/provider-os/cinder-nfs-csi-plugin:latest`
- Sidecars: `external-provisioner`, `livenessprobe`
- **NO `external-attacher` sidecar** (ControllerPublish only queries, no Nova attach)
- Volumes: Secret (`cloud.conf`) + ConfigMap (`driver.conf`)

### Node DaemonSet (`cinder-nfs-csi-nodeplugin.yaml`)

- Image: `registry.k8s.io/provider-os/cinder-nfs-csi-plugin:latest`
- Sidecars: `node-driver-registrar`, `livenessprobe`
- Socket: `/var/lib/kubelet/plugins/cinder-nfs.csi.windriver.com/csi.sock`
- **`privileged: true`** with `mountPropagation: Bidirectional`
- Volume: ConfigMap (`driver.conf`) — NFS mount options

### CSIDriver resource (`csi-cinder-nfs-driver.yaml`)

```yaml
apiVersion: storage.k8s.io/v1
kind: CSIDriver
metadata:
  name: cinder-nfs.csi.windriver.com
spec:
  attachRequired: true       # ControllerPublishVolume needed for connection_info
  podInfoOnMount: false
  volumeLifecycleModes:
    - Persistent
```

### StorageClass (`csi-cinder-nfs-storageclass.yaml`)

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: cinder-nfs-migration
provisioner: cinder-nfs.csi.openstack.org
parameters:
  type: netapp-nfs
  availability: nova
reclaimPolicy: Delete
volumeBindingMode: Immediate
```

## Configuration

### Dual config pattern

| Config Object | K8S Kind | Mount Path | Contents |
|---|---|---|---|
| `cinder-nfs-cloud-config` | Secret | `/etc/cloud-config/cloud.conf` | OpenStack credentials (Keystone auth) |
| `cinder-nfs-config` | ConfigMap | `/etc/cinder-nfs/driver.conf` | Shadow VM, NFS, and volume operational params |

### CLI flags

```
--endpoint        CSI unix socket endpoint
--cloud-config    Path to cloud.conf (Secret)
--driver-config   Path to driver.conf (ConfigMap)
--provide-controller-service  Enable/disable controller (default: true)
--provide-node-service        Enable/disable node (default: true)
```

### driver.conf format (INI via gcfg)

```ini
[ShadowVM]
flavor-ref = m1.small
network-id = 8e3f3c4a-1234-5678-abcd-9876543210ab
availability-zone = nova
name-prefix = shadow
create-timeout = 300
stop-timeout = 120

[NFS]
mount-options = rw,hard,intr,rsize=1048576,wsize=1048576
mount-base-path = /var/lib/cinder-nfs
nfs-version = 4.1

[Volume]
create-timeout = 300
detach-timeout = 120
metadata-prefix = csi
```

### cloud.conf format (same as existing Cinder CSI)

```ini
[Global]
auth-url = https://<keystone_ip>/identity/v3
username = cinder-nfs-service
password = <service_password>
domain-name = default
tenant-id = cc34b11ff95d42309081d0bd46c0ff89
region = RegionOne
```

## Helm chart

`charts/cinder-nfs-csi-plugin/` follows existing chart pattern:

```yaml
# values.yaml excerpt
csi:
  plugin:
    image:
      repository: registry.k8s.io/provider-os/cinder-nfs-csi-plugin
      tag: latest
    shadowVM:
      flavorID: ""          # Required
      imageID: ""           # Required
      subnetID: ""          # Required
      networkID: ""         # Required
      stopAfterAttach: true
    nfs:
      mountOptions: "nfsvers=4.1,rsize=1048576,wsize=1048576"
      defaultFsType: "nfs4"
```

## Metrics

Integrate with `pkg/metrics/` framework:

| Metric | Type | Labels |
|--------|------|--------|
| `cinder_nfs_csi_operation_duration_seconds` | Histogram | `operation` |
| `cinder_nfs_csi_operations_total` | Counter | `operation` |
| `cinder_nfs_csi_operation_errors_total` | Counter | `operation` |
| `cinder_nfs_csi_openstack_api_request_duration_seconds` | Histogram | `request` |

## Testing

- **Unit tests:** `//go:build unit` constraint
- **Sanity tests:** `tests/sanity/cinder-nfs/`
- **E2E tests:** Ansible playbooks (`tests/playbooks/test-csi-cinder-nfs-e2e.yaml`)
  orchestrated by `tests/ci-csi-cinder-nfs-e2e.sh`
- **Mock:** `IOpenStackNFS` mock in `openstack/openstack_mock.go`

## Release alignment

1. `hack/bump-release.sh` — version strings in manifests/charts
2. `hack/bump-charts.sh` — chart versions (`appVersion: 1.XX.Y`, `version: 2.XX.Y`)
3. Image promotion: `gcr.io/k8s-staging-provider-os/cinder-nfs-csi-plugin`
4. Image digest verification: `hack/release-image-digests.sh` + `hack/verify-image-digests.sh`

## Network requirements

| Path | Protocol | Port | Purpose |
|------|----------|------|---------|
| WRCP Worker → vCenter | HTTPS | 443 | VDDK data transfer |
| WRCP Worker → NFS Server | NFS | 2049 | Volume data writes |
| WRCP Worker → NFS Server | RPC | 111 | Portmapper/rpcbind |
| WRC → OpenStack API | HTTPS | 5000, 8776, 8774 | Keystone, Cinder, Nova |
