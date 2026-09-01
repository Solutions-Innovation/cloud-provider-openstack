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
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewDriver_Capabilities(t *testing.T) {
	d := NewDriver(&DriverOpts{Endpoint: "unix:///tmp/csi.sock", ClusterID: "test"})

	assert.Equal(t, "cinder-rbd.csi.windriver.com", d.name)

	// Raw block, single writer only. Any additional access mode would let the
	// external-provisioner bind a PVC this driver cannot safely serve, because
	// the Ceph exclusive lock permits exactly one writer.
	require.Len(t, d.vcap, 1)
	assert.Equal(t, csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER, d.vcap[0].Mode)

	// Only implemented capabilities are advertised. Expansion, snapshots,
	// clones and LIST_VOLUMES are non-goals and must stay absent.
	var controller []csi.ControllerServiceCapability_RPC_Type
	for _, c := range d.cscap {
		controller = append(controller, c.GetRpc().GetType())
	}
	assert.ElementsMatch(t, []csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
	}, controller)
	assert.NotContains(t, controller, csi.ControllerServiceCapability_RPC_EXPAND_VOLUME)
	assert.NotContains(t, controller, csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT)
	assert.NotContains(t, controller, csi.ControllerServiceCapability_RPC_CLONE_VOLUME)

	var node []csi.NodeServiceCapability_RPC_Type
	for _, n := range d.nscap {
		node = append(node, n.GetRpc().GetType())
	}
	assert.ElementsMatch(t, []csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
	}, node)
	assert.NotContains(t, node, csi.NodeServiceCapability_RPC_EXPAND_VOLUME)

	// The identity server exists from construction so Probe can answer before
	// either service is wired.
	require.NotNil(t, d.ids)
	assert.Nil(t, d.cs)
	assert.Nil(t, d.ns)
}

func TestValidateControllerServiceRequest(t *testing.T) {
	d := NewDriver(&DriverOpts{Endpoint: "unix:///tmp/csi.sock"})

	assert.NoError(t, d.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_UNKNOWN))
	assert.NoError(t, d.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME))
	assert.NoError(t, d.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME))
	assert.Error(t, d.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_EXPAND_VOLUME))
	assert.Error(t, d.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT))
}

// TestCSIServices_TypedNilInterface is a regression test for a crash found
// during Phase 1 verification.
//
// d.cs and d.ns are concrete pointer types. Assigning a nil *controllerServer
// to a csi.ControllerServer interface produces a NON-nil interface, so the
// gRPC registration guard in server.go ("if cs != nil") passed, the controller
// service was registered with a nil receiver, and the first controller RPC
// segfaulted the process. In node-only mode that means any stray controller
// call kills a privileged DaemonSet pod.
//
// csiServices must hand back untyped nil interfaces for services that were
// never set up.
func TestCSIServices_TypedNilInterface(t *testing.T) {
	t.Run("node-only mode yields a nil controller interface", func(t *testing.T) {
		d := NewDriver(&DriverOpts{Endpoint: "unix:///tmp/csi.sock"})
		d.ns = &nodeServer{Driver: d}

		ids, cs, ns := d.csiServices()

		require.NotNil(t, ids)
		assert.Nil(t, cs, "a nil *controllerServer must not be registered as a service")
		assert.NotNil(t, ns)
	})

	t.Run("controller-only mode yields a nil node interface", func(t *testing.T) {
		d := NewDriver(&DriverOpts{Endpoint: "unix:///tmp/csi.sock"})
		d.cs = &controllerServer{Driver: d}

		ids, cs, ns := d.csiServices()

		require.NotNil(t, ids)
		assert.NotNil(t, cs)
		assert.Nil(t, ns, "a nil *nodeServer must not be registered as a service")
	})

	t.Run("all-in-one mode yields both", func(t *testing.T) {
		d := NewDriver(&DriverOpts{Endpoint: "unix:///tmp/csi.sock"})
		d.cs = &controllerServer{Driver: d}
		d.ns = &nodeServer{Driver: d}

		_, cs, ns := d.csiServices()

		assert.NotNil(t, cs)
		assert.NotNil(t, ns)
	})
}

func TestParseEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name      string
		ep        string
		wantProto string
		wantAddr  string
		wantErr   bool
	}{
		{"unix", "unix:///csi/csi.sock", "unix", "/csi/csi.sock", false},
		{"unix uppercase scheme", "UNIX:///csi/csi.sock", "UNIX", "/csi/csi.sock", false},
		{"tcp", "tcp://127.0.0.1:10000", "tcp", "127.0.0.1:10000", false},
		{"empty", "", "", "", true},
		{"no address", "unix://", "", "", true},
		{"unsupported scheme", "http://localhost", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proto, addr, err := ParseEndpoint(tc.ep)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantProto, proto)
			assert.Equal(t, tc.wantAddr, addr)
		})
	}
}
