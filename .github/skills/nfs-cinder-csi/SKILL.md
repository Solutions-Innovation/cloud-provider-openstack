---
name: nfs-cinder-csi
description: >
  Develop, modify, and debug the NFS-backed Cinder CSI plugin (cinder-nfs.csi.openstack.org)
  for the cloud-provider-openstack project. Use when working on pkg/csi/cinder-nfs/,
  cmd/cinder-nfs-csi-plugin/, Shadow VM lifecycle, NFS mount logic, CSI controller/node RPCs,
  OpenStack Cinder/Nova API integration, or migration-related storage provisioning.
  Covers architecture decisions, CSI RPC mapping, IOpenStackNFS interface, configuration,
  build system, and deployment manifests.
license: Apache-2.0
---

# NFS-Backed Cinder CSI Plugin Development

## When to use this skill

Use this skill when:
- Implementing or modifying CSI RPCs in `pkg/csi/cinder-nfs/`
- Working on Shadow VM lifecycle (create, stop, delete, wait)
- Implementing NFS mount/unmount logic in the Node service
- Working on the `IOpenStackNFS` interface or OpenStack API calls
- Modifying build/deploy artifacts (Makefile, Dockerfile, manifests, Helm chart)
- Debugging volume provisioning, NFS connection discovery, or bind mount issues
- Adding tests (unit, sanity, E2E) for the NFS-Cinder CSI driver

## Project context

This is a **new, separate CSI driver** in the `cloud-provider-openstack` monorepo — it
does NOT extend the existing `cinder.csi.openstack.org` driver. It follows the **Manila
CSI precedent** (`pkg/csi/manila/`) as a fully independent package.

**Driver name:** `cinder-nfs.csi.openstack.org`

**Purpose:** Enable VM migration (V2O and O2O) by mounting NFS-backed Cinder volumes
directly on WRCP/WRC worker hosts, bypassing the Nova block-attach path that requires
an addon K8S cluster on target OpenStack.

## Key design references

For detailed designs, see:
- [Architecture & design decisions](references/architecture.md)
- [CSI RPC implementation mapping](references/csi-rpcs.md)
- [IOpenStackNFS interface & config structs](references/interfaces.md)
- [Shadow VM lifecycle state machine](references/shadow-vm.md)
- [Build, deploy & configuration](references/build-deploy.md)

Full design documents in the repo:
- `docs/cinder-csi-plugin/migration/nfs-backed-cinder-volume-for-wrcp-migration.md`
- `docs/cinder-csi-plugin/migration/nfs-cinder-csi-implementation-design.md`

## Package structure

```
pkg/csi/cinder-nfs/
├── driver.go                  # Driver struct, capabilities, NewDriver(), Run()
├── controllerserver.go        # Controller RPCs (Shadow VM + NFS connection discovery)
├── nodeserver.go              # Node RPCs (NFS mount/unmount, bind mount)
├── identityserver.go          # Identity RPCs (plugin info, capabilities)I t hi
├── server.go                  # NonBlockingGRPCServer (copied from cinder/)
├── utils.go                   # Capability factories, gRPC logger
├── shadowvm.go                # Shadow VM lifecycle manager
├── connectioninfo.go          # Cinder attachment connection_info parser
└── openstack/
    ├── openstack.go           # IOpenStackNFS interface, config structs, provider init
    ├── openstack_volumes.go   # Volume CRUD (Cinder API)
    ├── openstack_servers.go   # Shadow VM server lifecycle (Nova API)
    ├── openstack_attachments.go # Volume attachment + connection_info (Cinder v3)
    ├── openstack_mock.go      # Mock for unit tests
    └── fixtures/              # Test fixtures

cmd/cinder-nfs-csi-plugin/
└── main.go                    # Binary entry point

manifests/cinder-nfs-csi-plugin/   # Kubernetes manifests
charts/cinder-nfs-csi-plugin/      # Helm chart
tests/sanity/cinder-nfs/           # CSI sanity tests
```

## Critical implementation rules

### 1. ControllerUnpublishVolume MUST be a no-op
CDI multi-phase precopy cycles pods between stages. Between stages, the CO fires
`ControllerUnpublishVolume`. If this detaches the Shadow VM, the next
`ControllerPublishVolume` fails — no attachment record to query NFS info from.
**Always return success without modifying any state.**

### 2. Shadow VM persists until DeleteVolume
The Shadow VM is created in `CreateVolume` and deleted only in `DeleteVolume`.
Its attachment record holds NFS `connection_info`. Store the Shadow VM ID in
Cinder volume metadata: `csi.shadow_vm_id`.

### 3. DeleteVolume behavior depends on cleanup flag
- **Default (success path):** Detach volume from Shadow VM → delete Shadow VM →
  volume becomes `available`. Volume is NOT deleted. Blueprint creates target VM.
- **Failure path:** Blueprint sets `csi.cleanupVolume=true` in Cinder metadata
  before PVC deletion → `DeleteVolume` also deletes the Cinder volume.

### 4. Volume stays `in-use` throughout migration
Shadow VM attachment acts as a lock. NFS mounts from WRCP hosts are invisible
to Cinder. Two decoupled planes: Cinder control plane (Shadow VM) and NFS data
plane (WRCP host).

### 5. NFS mount in NodeStageVolume, bind mount in NodePublishVolume
- `NodeStageVolume`: `mount -t nfs -o <opts> <nfs_export> <staging_path>`
- `NodePublishVolume`: `mount --bind <staging_path>/<volume_file> <target_path>`
- For Block access type: create target file with `touch` before bind mount

### 6. No external-attacher sidecar needed
`ControllerPublishVolume` only queries `connection_info` — it does NOT perform
Nova attachment. The controller deployment does not need the `external-attacher`
sidecar container.

### 7. Dual config pattern
- **Secret** (`cloud.conf`): OpenStack credentials (Keystone auth)
- **ConfigMap** (`driver.conf`): Shadow VM opts, NFS opts, volume opts
- CLI flags: `--cloud-config` and `--driver-config`

## CSI RPC quick reference

| RPC | What it does |
|-----|-------------|
| `CreateVolume` | Cinder create volume + create Shadow VM + attach + stop Shadow VM |
| `DeleteVolume` | Detach from Shadow VM + delete Shadow VM + optionally delete volume |
| `ControllerPublishVolume` | Query Cinder attachment → extract NFS export/path from `connection_info` |
| `ControllerUnpublishVolume` | **No-op** (Shadow VM persists) |
| `NodeStageVolume` | `mount -t nfs` NFS export to staging path |
| `NodeUnstageVolume` | `umount` NFS mount |
| `NodePublishVolume` | `mount --bind` volume file to pod target path |
| `NodeUnpublishVolume` | `umount` bind mount |

## Reusable components (import directly)

| Package | Use |
|---------|-----|
| `pkg/csi/csi.go` | Shared CSI constants and helpers |
| `pkg/client/` | OpenStack authentication |
| `pkg/metrics/` | Prometheus metrics framework |
| `pkg/util/metadata/` | Node metadata service |
| `pkg/util/mount/` | Mount utilities (NFS mount via `Mounter()`) |
| `pkg/util/errors/` | Error utilities |

Components to **copy** from `pkg/csi/cinder/`: `server.go`, `utils.go` (unexported types).

## Testing conventions

- **Unit tests:** `//go:build unit` constraint, run via `make unit`
- **Sanity tests:** `tests/sanity/cinder-nfs/`, run via `make test-cinder-nfs-csi-sanity`
- **E2E tests:** Ansible playbooks in `tests/playbooks/`, CI script `tests/ci-csi-cinder-nfs-e2e.sh`
- **Linting:** `make check` (golangci-lint on all packages)
- **Mock:** `IOpenStackNFS` mock in `openstack/openstack_mock.go`

## Common tasks

### Adding a new CSI RPC or modifying existing one
1. Update the interface in `openstack/openstack.go` if new OpenStack calls needed
2. Update the mock in `openstack/openstack_mock.go`
3. Implement the RPC in `controllerserver.go` or `nodeserver.go`
4. Add/update unit tests
5. See [CSI RPCs reference](references/csi-rpcs.md) for detailed mapping

### Modifying Shadow VM lifecycle
1. See [Shadow VM reference](references/shadow-vm.md) for state machine
2. Core logic lives in `shadowvm.go`
3. Nova API calls in `openstack/openstack_servers.go`
4. Shadow VM ID stored in Cinder volume metadata (`csi.shadow_vm_id`)

### Adding configuration options
1. Add field to appropriate struct in `openstack/openstack.go` (`ShadowVMOpts`, `NFSOpts`, `VolumeOpts`)
2. Add `gcfg` tag for INI parsing
3. Update validation in driver startup (`cmd/cinder-nfs-csi-plugin/main.go`)
4. Update ConfigMap manifest + Helm chart values
5. See [Build & deploy reference](references/build-deploy.md) for config details

### Building the driver
```bash
make cinder-nfs-csi-plugin                    # Build binary
make build-local-image-cinder-nfs-csi-plugin  # Build container image
make test-cinder-nfs-csi-sanity               # Run sanity tests
make unit                                      # Run unit tests
make check                                     # Run linter
```
