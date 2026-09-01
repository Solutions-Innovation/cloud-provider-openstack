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

package rbd

import (
	"context"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/wrapperspb"
	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-rbd/openstack"
	"k8s.io/klog/v2"
)

type identityServer struct {
	Driver *Driver

	// cloud is nil in node-only mode.
	cloud openstack.IOpenStackRBD
	// mapper and creds are nil in controller-only mode; SetupNodeService
	// injects them.
	mapper RBDMapper
	creds  CephCredentialProvider

	csi.UnimplementedIdentityServer
}

// GetPluginInfo returns the driver name and version.
func (ids *identityServer) GetPluginInfo(_ context.Context, _ *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
	klog.V(5).Info("GetPluginInfo called")

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

// Probe reports driver health to the livenessprobe sidecar.
//
// Readiness is expressed through the response rather than a gRPC error, as in
// the sibling iSCSI driver: returning an error makes kubelet's handling noisier
// without conveying more information.
//
// Node-only mode checks the two things that make mapping possible at all: the
// bundled rbd CLI and the projected credential Secret. Controller mode checks
// the cached microversion probe.
func (ids *identityServer) Probe(ctx context.Context, _ *csi.ProbeRequest) (*csi.ProbeResponse, error) {
	if ids.cloud == nil {
		// Node-only mode.
		if ids.mapper == nil {
			// SetupNodeService was never called: a wiring bug, not a
			// transient condition.
			klog.Warning("Probe: node-only mode but RBD mapper not configured (SetupNodeService not called)")
			return notReady(), nil
		}
		if _, err := ids.mapper.CheckClient(ctx); err != nil {
			klog.V(3).Infof("Probe: rbd client check failed: %v", err)
			return notReady(), nil
		}
		if ids.creds == nil {
			klog.Warning("Probe: node-only mode but credential provider not configured")
			return notReady(), nil
		}
		if err := ids.creds.Available(ctx); err != nil {
			// A missing Secret is an operator action, so it is surfaced as
			// not-ready here rather than as a per-volume staging failure.
			klog.V(3).Infof("Probe: ceph credential unavailable: %v", err)
			return notReady(), nil
		}
		return ready(), nil
	}

	// Controller mode.
	caps := ids.cloud.GetCinderCapabilities()
	if caps == nil {
		klog.V(3).Info("Probe: Cinder capabilities not yet discovered")
		return notReady(), nil
	}
	if !caps.SupportsV327 {
		// Startup should already have failed; this is defensive.
		klog.Warningf("Probe: Cinder does not support microversion %s", openstack.MvSelfServiceAttach)
		return notReady(), nil
	}
	return ready(), nil
}

func ready() *csi.ProbeResponse {
	return &csi.ProbeResponse{Ready: &wrapperspb.BoolValue{Value: true}}
}

func notReady() *csi.ProbeResponse {
	return &csi.ProbeResponse{Ready: &wrapperspb.BoolValue{Value: false}}
}

// GetPluginCapabilities reports the plugin-level capabilities.
//
// Only CONTROLLER_SERVICE is advertised. VOLUME_ACCESSIBILITY_CONSTRAINTS is
// omitted because topology-aware provisioning is a non-goal, and VolumeExpansion
// is omitted because expansion is not implemented.
func (ids *identityServer) GetPluginCapabilities(_ context.Context, _ *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
	klog.V(5).Info("GetPluginCapabilities called")
	return &csi.GetPluginCapabilitiesResponse{
		Capabilities: []*csi.PluginCapability{
			{
				Type: &csi.PluginCapability_Service_{
					Service: &csi.PluginCapability_Service{
						Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
					},
				},
			},
		},
	}, nil
}
