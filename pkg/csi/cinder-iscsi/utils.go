/*
Copyright 2017 The Kubernetes Authors.
(original upstream copyright preserved)

Copyright (c) 2024-2026 Wind River Systems, Inc.
Wind River Migration Framework Team
Modifications: Copied from pkg/csi/cinder/utils.go — removed cinder-specific constructors,
replaced with iSCSI-specific constructors for the iSCSI-Cinder CSI driver.

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
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	"google.golang.org/grpc"
	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-iscsi/openstack"
	"k8s.io/cloud-provider-openstack/pkg/util/mount"
	"k8s.io/klog/v2"
)

var (
	serverGRPCEndpointCallCounter uint64
)

// NewControllerServiceCapability creates a new ControllerServiceCapability.
func NewControllerServiceCapability(cap csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
	return &csi.ControllerServiceCapability{
		Type: &csi.ControllerServiceCapability_Rpc{
			Rpc: &csi.ControllerServiceCapability_RPC{
				Type: cap,
			},
		},
	}
}

// NewNodeServiceCapability creates a new NodeServiceCapability.
func NewNodeServiceCapability(cap csi.NodeServiceCapability_RPC_Type) *csi.NodeServiceCapability {
	return &csi.NodeServiceCapability{
		Type: &csi.NodeServiceCapability_Rpc{
			Rpc: &csi.NodeServiceCapability_RPC{
				Type: cap,
			},
		},
	}
}

// NewVolumeCapabilityAccessMode creates a new VolumeCapability_AccessMode.
func NewVolumeCapabilityAccessMode(mode csi.VolumeCapability_AccessMode_Mode) *csi.VolumeCapability_AccessMode {
	return &csi.VolumeCapability_AccessMode{Mode: mode}
}

//revive:disable:unexported-return

// NewControllerServer creates a new iSCSI controller server.
func NewControllerServer(d *Driver, cloud openstack.IOpenStackISCSI) *controllerServer {
	return &controllerServer{
		Driver: d,
		Cloud:  cloud,
	}
}

// NewIdentityServer creates a new iSCSI identity server.
// cloud may be nil for node-only mode; SetupControllerService injects it later.
func NewIdentityServer(d *Driver, cloud openstack.IOpenStackISCSI) *identityServer {
	return &identityServer{
		Driver: d,
		cloud:  cloud,
	}
}

// NewNodeServer creates a new iSCSI node server.
func NewNodeServer(d *Driver, opts openstack.ISCSIOpts, iscsiInit ISCSIInitiator, mounter mount.IMount) *nodeServer {
	return &nodeServer{
		Driver:  d,
		Opts:    opts,
		ISCSI:   iscsiInit,
		Mounter: mounter,
	}
}

//revive:enable:unexported-return

// RunServicesInitialized creates and starts the gRPC server.
func RunServicesInitialized(endpoint string, ids csi.IdentityServer, cs csi.ControllerServer, ns csi.NodeServer) {
	s := NewNonBlockingGRPCServer()
	s.Start(endpoint, ids, cs, ns)
	s.Wait()
}

// ParseEndpoint parses a CSI endpoint string into protocol and address.
func ParseEndpoint(ep string) (string, string, error) {
	if strings.HasPrefix(strings.ToLower(ep), "unix://") || strings.HasPrefix(strings.ToLower(ep), "tcp://") {
		s := strings.SplitN(ep, "://", 2)
		if s[1] != "" {
			return s[0], s[1], nil
		}
	}
	return "", "", fmt.Errorf("invalid endpoint: %v", ep)
}

func logGRPC(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	callID := atomic.AddUint64(&serverGRPCEndpointCallCounter, 1)

	klog.V(3).Infof("[ID:%d] GRPC call: %s", callID, info.FullMethod)
	klog.V(5).Infof("[ID:%d] GRPC request: %s", callID, protosanitizer.StripSecrets(req))
	resp, err := handler(ctx, req)
	if err != nil {
		klog.Errorf("[ID:%d] GRPC error: %v", callID, err)
	} else {
		klog.V(5).Infof("[ID:%d] GRPC response: %s", callID, protosanitizer.StripSecrets(resp))
	}

	return resp, err
}
