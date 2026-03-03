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

package iscsi

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-iscsi/openstack"
	"k8s.io/klog/v2"
)

type identityServer struct {
	Driver    *Driver
	cloud     openstack.IOpenStackISCSI // nil for node-only mode
	iscsiInit ISCSIInitiator            // nil for controller-only mode; set by SetupNodeService
	csi.UnimplementedIdentityServer
}

// GetPluginInfo returns metadata about the iSCSI-Cinder CSI plugin.
func (ids *identityServer) GetPluginInfo(ctx context.Context, req *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	klog.V(5).Infof("GetPluginInfo called")

	if ids.Driver.name == "" {
		return nil, status.Error(codes.Unavailable, "Driver name not configured")
	}
	if ids.Driver.fqVersion == "" {
		return nil, status.Error(codes.Unavailable, "Driver is missing version")
	}

	return &csi.GetPluginInfoResponse{
		Name:          ids.Driver.name,
		VendorVersion: ids.Driver.fqVersion,
	}, nil
}

// Probe returns whether the iSCSI-Cinder CSI plugin is healthy.
// The livenessprobe sidecar calls this every ~10s. We check the cached
// Cinder capabilities that were populated at startup by
// DiscoverCinderCapabilities. For node-only mode (cloud==nil), we verify
// that iscsiadm is reachable.
func (ids *identityServer) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	// Node-only mode — verify iscsiadm availability
	if ids.cloud == nil {
		if ids.iscsiInit == nil {
			// SetupNodeService was never called — wiring bug.
			klog.Warning("Probe: node-only mode but iSCSI initiator not configured (SetupNodeService not called)")
			return &csi.ProbeResponse{Ready: &wrapperspb.BoolValue{Value: false}}, nil
		}
		if err := ids.iscsiInit.CheckIscsiadm(ctx); err != nil {
			klog.V(3).Infof("Probe: iscsiadm check failed: %v", err)
			return &csi.ProbeResponse{Ready: &wrapperspb.BoolValue{Value: false}}, nil
		}
		return &csi.ProbeResponse{Ready: &wrapperspb.BoolValue{Value: true}}, nil
	}

	// Controller mode — check cached Cinder capabilities
	caps := ids.cloud.GetCinderCapabilities()
	if caps == nil {
		// Discovery hasn't completed yet (still initializing)
		klog.V(3).Info("Probe: Cinder capabilities not yet discovered")
		return &csi.ProbeResponse{Ready: &wrapperspb.BoolValue{Value: false}}, nil
	}
	if !caps.SupportsV327 {
		// Should not happen (startup would have failed), but defensive
		klog.Warning("Probe: Cinder does not support microversion 3.27")
		return &csi.ProbeResponse{Ready: &wrapperspb.BoolValue{Value: false}}, nil
	}

	return &csi.ProbeResponse{Ready: &wrapperspb.BoolValue{Value: true}}, nil
}

// GetPluginCapabilities returns the capabilities of this CSI plugin.
func (ids *identityServer) GetPluginCapabilities(ctx context.Context, req *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	klog.V(5).Infof("GetPluginCapabilities called with req %+v", req)
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
					},
				},
			},
			// TODO: Add VolumeExpansion (ONLINE/OFFLINE) when EXPAND_VOLUME
			// is implemented in the controller service.
		},
	}, nil
}
