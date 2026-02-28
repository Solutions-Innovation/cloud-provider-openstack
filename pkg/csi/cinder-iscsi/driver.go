/*
Copyright 2024 The Kubernetes Authors.

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

package iscsi

import (
	"fmt"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-iscsi/openstack"
	"k8s.io/cloud-provider-openstack/pkg/version"
	"k8s.io/klog/v2"
)

const (
	driverName  = "cinder-iscsi.csi.windriver.com"
	topologyKey = "topology." + driverName + "/zone"
)

var (
	// CSI spec version
	specVersion = "1.8.0"

	// Driver version
	Version = "1.0.0"
)

// Driver implements the iSCSI-Cinder CSI driver.
type Driver struct {
	name      string
	fqVersion string // Fully qualified version in format {Version}@{CPO version}
	endpoint  string
	clusterID string

	ids *identityServer
	cs  *controllerServer
	ns  *nodeServer

	vcap  []*csi.VolumeCapability_AccessMode
	cscap []*csi.ControllerServiceCapability
	nscap []*csi.NodeServiceCapability
}

// DriverOpts contains options for creating a new Driver.
type DriverOpts struct {
	ClusterID string
	Endpoint  string
}

// NewDriver creates a new iSCSI-Cinder CSI driver.
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

	// iSCSI volumes are block devices — SINGLE_NODE_WRITER only
	d.AddVolumeCapabilityAccessModes(
		[]csi.VolumeCapability_AccessMode_Mode{
			csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		})

	// Controller capabilities for Cinder v3 attachment lifecycle
	d.AddControllerServiceCapabilities(
		[]csi.ControllerServiceCapability_RPC_Type{
			csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
			csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
			csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT,
			csi.ControllerServiceCapability_RPC_LIST_VOLUMES,
			csi.ControllerServiceCapability_RPC_EXPAND_VOLUME,
		})

	// Node capabilities for iSCSI staging (login/logout)
	_ = d.AddNodeServiceCapabilities(
		[]csi.NodeServiceCapability_RPC_Type{
			csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
			csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
		})

	d.ids = NewIdentityServer(d)

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
func (d *Driver) AddNodeServiceCapabilities(nl []csi.NodeServiceCapability_RPC_Type) error {
	nsc := make([]*csi.NodeServiceCapability, 0, len(nl))
	for _, n := range nl {
		klog.Infof("Enabling node service capability: %v", n.String())
		nsc = append(nsc, NewNodeServiceCapability(n))
	}
	d.nscap = nsc
	return nil
}

// ValidateControllerServiceRequest validates that the given capability is supported.
func (d *Driver) ValidateControllerServiceRequest(c csi.ControllerServiceCapability_RPC_Type) error {
	if c == csi.ControllerServiceCapability_RPC_UNKNOWN {
		return nil
	}
	for _, cap := range d.cscap {
		if c == cap.GetRpc().GetType() {
			return nil
		}
	}
	return status.Error(codes.InvalidArgument, c.String())
}

// GetVolumeCapabilityAccessModes returns the supported volume access modes.
func (d *Driver) GetVolumeCapabilityAccessModes() []*csi.VolumeCapability_AccessMode {
	return d.vcap
}

// SetupControllerService configures the controller service with an OpenStack cloud.
func (d *Driver) SetupControllerService(cloud openstack.IOpenStackISCSI) {
	klog.Info("Providing controller service")
	d.cs = NewControllerServer(d, cloud)
}

// SetupNodeService configures the node service.
func (d *Driver) SetupNodeService() {
	klog.Info("Providing node service")
	d.ns = NewNodeServer(d)
}

// Run starts the gRPC server and blocks until stopped.
func (d *Driver) Run() {
	if nil == d.cs && nil == d.ns {
		klog.Fatal("No CSI services initialized")
	}
	RunServicesInitialized(d.endpoint, d.ids, d.cs, d.ns)
}
