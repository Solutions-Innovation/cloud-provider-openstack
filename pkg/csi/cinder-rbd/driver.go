/*
Copyright (c) 2024-2026 Wind River Systems, Inc.
Wind River Migration Framework Team

SPDX-License-Identifier: Apache-2.0

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package rbd implements the Cinder RBD CSI driver
// (cinder-rbd.csi.windriver.com): a block-only driver that reserves Ceph
// RBD-backed Cinder volumes with self-service attachment records and maps them
// on worker nodes with exclusive kernel RBD.
//
// Design reference:
//
//	docs/cinder-csi-plugin/migration/rbd-cinder-csi-implementation-design.md
package rbd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-rbd/openstack"
	"k8s.io/cloud-provider-openstack/pkg/util/mount"
	"k8s.io/cloud-provider-openstack/pkg/version"
	"k8s.io/klog/v2"
)

// driverName is a constant, not configuration. A mutable driver name would
// break CSIDriver and CSINode registration.
const driverName = "cinder-rbd.csi.windriver.com"

// nodeReconcileTimeout bounds startup reconciliation. It must not block node
// readiness indefinitely if the rbd CLI hangs.
const nodeReconcileTimeout = 2 * time.Minute

var (
	// specVersion is the CSI spec version this driver targets.
	specVersion = "1.8.0"

	// Version is the driver version.
	Version = "1.0.0"
)

// Driver implements the Cinder RBD CSI driver.
type Driver struct {
	name      string
	fqVersion string // {Version}@{CPO version}
	endpoint  string
	clusterID string

	ids *identityServer
	cs  *controllerServer
	ns  *nodeServer

	vcap  []*csi.VolumeCapability_AccessMode
	cscap []*csi.ControllerServiceCapability
	nscap []*csi.NodeServiceCapability
}

// DriverOpts contains options for creating a Driver.
type DriverOpts struct {
	ClusterID string
	Endpoint  string
}

// NewDriver creates a new Cinder RBD CSI driver.
func NewDriver(o *DriverOpts) *Driver {
	d := &Driver{
		name:      driverName,
		fqVersion: fmt.Sprintf("%s@%s", Version, version.Version),
		endpoint:  o.Endpoint,
		clusterID: o.ClusterID,
	}

	klog.Info("Driver: ", d.name)
	klog.Info("Driver version: ", d.fqVersion)
	klog.Info("CSI Spec version: ", specVersion)

	// Raw block only, single writer. The Ceph exclusive lock enforces the
	// same constraint at the storage layer.
	d.AddVolumeCapabilityAccessModes(
		[]csi.VolumeCapability_AccessMode_Mode{
			csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		})

	// Only capabilities that are actually implemented are advertised.
	// Expansion, snapshots, clones and topology are non-goals and are
	// deliberately absent rather than stubbed.
	d.AddControllerServiceCapabilities(
		[]csi.ControllerServiceCapability_RPC_Type{
			csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
			csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
		})

	d.AddNodeServiceCapabilities(
		[]csi.NodeServiceCapability_RPC_Type{
			csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
			csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
		})

	// The identity server starts without a cloud reference; whichever
	// Setup*Service call runs will inject what Probe needs.
	d.ids = NewIdentityServer(d, nil)

	return d
}

// AddControllerServiceCapabilities enables controller service capabilities.
func (d *Driver) AddControllerServiceCapabilities(cl []csi.ControllerServiceCapability_RPC_Type) {
	csc := make([]*csi.ControllerServiceCapability, 0, len(cl))
	for _, c := range cl {
		klog.Infof("Enabling controller service capability: %v", c.String())
		csc = append(csc, NewControllerServiceCapability(c))
	}
	d.cscap = csc
}

// AddVolumeCapabilityAccessModes enables volume access modes.
func (d *Driver) AddVolumeCapabilityAccessModes(vc []csi.VolumeCapability_AccessMode_Mode) []*csi.VolumeCapability_AccessMode {
	vca := make([]*csi.VolumeCapability_AccessMode, 0, len(vc))
	for _, c := range vc {
		klog.Infof("Enabling volume access mode: %v", c.String())
		vca = append(vca, NewVolumeCapabilityAccessMode(c))
	}
	d.vcap = vca
	return vca
}

// AddNodeServiceCapabilities enables node service capabilities.
func (d *Driver) AddNodeServiceCapabilities(nl []csi.NodeServiceCapability_RPC_Type) {
	nsc := make([]*csi.NodeServiceCapability, 0, len(nl))
	for _, n := range nl {
		klog.Infof("Enabling node service capability: %v", n.String())
		nsc = append(nsc, NewNodeServiceCapability(n))
	}
	d.nscap = nsc
}

// ValidateControllerServiceRequest validates that a capability is supported.
func (d *Driver) ValidateControllerServiceRequest(c csi.ControllerServiceCapability_RPC_Type) error {
	if c == csi.ControllerServiceCapability_RPC_UNKNOWN {
		return nil
	}
	for _, capability := range d.cscap {
		if c == capability.GetRpc().GetType() {
			return nil
		}
	}
	return status.Error(codes.InvalidArgument, c.String())
}

// GetVolumeCapabilityAccessModes returns the supported access modes.
func (d *Driver) GetVolumeCapabilityAccessModes() []*csi.VolumeCapability_AccessMode {
	return d.vcap
}

// SetupControllerService wires the controller service and lets Probe report
// Cinder readiness.
func (d *Driver) SetupControllerService(cloud openstack.IOpenStackRBD) {
	klog.Info("Providing controller service")
	d.cs = NewControllerServer(d, cloud)
	d.ids.cloud = cloud
}

// SetupNodeService wires the node service with production dependencies.
//
// opts carries the [RBD] section of driver.conf and must already have had
// ApplyDefaults and Validate applied by the config loader.
func (d *Driver) SetupNodeService(opts openstack.RBDOpts, vopts openstack.VolumeOpts) {
	d.SetupNodeServiceWithDeps(opts, vopts,
		NewRBDCLIMapper(opts),
		NewFileCredentialProvider(opts.CredentialPath),
		mount.GetMountProvider())
}

// SetupNodeServiceWithDeps wires the node service with explicit dependencies.
//
// This exists so the CSI sanity suite can substitute an in-memory kernel and
// credential source. Production code goes through SetupNodeService.
func (d *Driver) SetupNodeServiceWithDeps(opts openstack.RBDOpts, vopts openstack.VolumeOpts,
	mapper RBDMapper, creds CephCredentialProvider, mounter mount.IMount) {
	klog.Info("Providing node service")

	d.ns = NewNodeServer(d, opts, vopts, mapper, creds, mounter)

	// Probe needs both in node-only mode: a missing rbd CLI and a missing
	// credential Secret are both reasons to report not-ready.
	d.ids.mapper = mapper
	d.ids.creds = creds

	if err := prepareNodeRuntime(opts); err != nil {
		// Not fatal: Probe reports not-ready and kubelet retries, which is
		// preferable to crash-looping a DaemonSet on a transient filesystem
		// problem. The staging RPCs fail loudly until it is fixed.
		klog.Errorf("SetupNodeService: node runtime preparation failed: %v", err)
		return
	}

	// Reconcile before the gRPC server starts accepting calls. Running this
	// after would let a NodeStageVolume race the pass that decides whether an
	// existing mapping is adoptable or isolated.
	d.reconcileNodeState()
}

// reconcileNodeState compares durable staging records against live kernel state.
//
// A failure leaves the node serving: the per-RPC paths perform the same identity
// checks, so correctness does not depend on this pass succeeding. What is lost is
// the up-front report, so the failure is logged prominently.
func (d *Driver) reconcileNodeState() {
	ctx, cancel := context.WithTimeout(context.Background(), nodeReconcileTimeout)
	defer cancel()

	rec := newReconciler(d.ns.Mapper, d.ns.Staging, d.ns.Isolation)
	result, err := rec.Reconcile(ctx)
	if err != nil {
		klog.Errorf("SetupNodeService: startup reconciliation failed: %v; "+
			"per-RPC identity checks still apply", err)
		return
	}

	klog.Infof("SetupNodeService: startup reconciliation complete "+
		"(adopted=%d unstaged=%d isolated=%d reported=%d)",
		result.Adopted, result.Unstaged, result.Isolated, result.Reported)

	if result.Isolated > 0 {
		// Loud on purpose: an isolated mapping means this node cannot serve
		// those volumes until an operator resolves the conflict.
		klog.Errorf("SetupNodeService: %d mapping(s) are ISOLATED and will not be served: %v. "+
			"See the operator runbook for resolution.", result.Isolated, result.IsolatedIdentities())
	}
}

// prepareNodeRuntime creates the private directories and writes the
// cluster-scoped ceph.conf.
//
// The cluster conf exists so credential-free commands (`rbd device list`,
// `rbd device unmap`) never fall back to the host's /etc/ceph/ceph.conf, which
// on the validated platform is an unsubstituted template whose fsid is the
// literal %CLUSTER_UUID%. Reconciliation runs before any volume is staged, so a
// per-volume conf cannot be relied on.
func prepareNodeRuntime(opts openstack.RBDOpts) error {
	for _, dir := range []string{opts.RuntimeDir, opts.StateDir} {
		if err := os.MkdirAll(dir, runtimeDirMode); err != nil {
			return fmt.Errorf("create %s: %w", dir, err)
		}
		if err := os.Chmod(dir, runtimeDirMode); err != nil {
			return fmt.Errorf("chmod %s: %w", dir, err)
		}
	}

	// Monitors are not known until the first publish, so the cluster conf is
	// written with the configured FSID only and refreshed per volume.
	if err := writeClusterConf(opts, nil); err != nil {
		return fmt.Errorf("write cluster ceph.conf: %w", err)
	}
	return nil
}

// csiServices returns the initialized services as interface values.
//
// The explicit nil checks are load-bearing. d.cs and d.ns are concrete pointer
// types, and a nil *controllerServer assigned to a csi.ControllerServer
// interface is NOT equal to nil. Passing them directly would make server.go's
// `if cs != nil` check pass, register the controller service with a nil
// receiver, and segfault the process on the first controller RPC — killing a
// privileged node DaemonSet on an unexpected but harmless request.
func (d *Driver) csiServices() (csi.IdentityServer, csi.ControllerServer, csi.NodeServer) {
	var cs csi.ControllerServer
	if d.cs != nil {
		cs = d.cs
	}
	var ns csi.NodeServer
	if d.ns != nil {
		ns = d.ns
	}
	return d.ids, cs, ns
}

// Run starts the gRPC server and blocks.
func (d *Driver) Run() {
	if d.cs == nil && d.ns == nil {
		klog.Fatal("No CSI services initialized")
	}
	ids, cs, ns := d.csiServices()
	RunServicesInitialized(d.endpoint, ids, cs, ns)
}
