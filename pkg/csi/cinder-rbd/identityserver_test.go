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
	"errors"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-rbd/openstack"
)

// fakeDriver builds a Driver without going through NewDriver, so tests do not
// depend on capability wiring they are not exercising.
func fakeDriver() *Driver {
	d := &Driver{name: driverName, fqVersion: "1.0.0@test"}
	d.AddVolumeCapabilityAccessModes([]csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
	})
	d.AddControllerServiceCapabilities([]csi.ControllerServiceCapability_RPC_Type{
		csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME,
		csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME,
	})
	d.AddNodeServiceCapabilities([]csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
	})
	return d
}

func TestGetPluginInfo(t *testing.T) {
	ids := &identityServer{Driver: fakeDriver()}
	resp, err := ids.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
	require.NoError(t, err)
	assert.Equal(t, "cinder-rbd.csi.windriver.com", resp.Name)
	assert.Equal(t, "1.0.0@test", resp.VendorVersion)
}

func TestGetPluginInfo_MissingNameOrVersion(t *testing.T) {
	t.Run("missing name", func(t *testing.T) {
		ids := &identityServer{Driver: &Driver{fqVersion: "1.0.0@test"}}
		_, err := ids.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
		assert.Error(t, err)
	})
	t.Run("missing version", func(t *testing.T) {
		ids := &identityServer{Driver: &Driver{name: driverName}}
		_, err := ids.GetPluginInfo(context.Background(), &csi.GetPluginInfoRequest{})
		assert.Error(t, err)
	})
}

// GetPluginCapabilities must not advertise topology or volume expansion:
// both are non-goals, and advertising them would make the external-provisioner
// attempt operations the driver rejects.
func TestGetPluginCapabilities_ControllerServiceOnly(t *testing.T) {
	ids := &identityServer{Driver: fakeDriver()}
	resp, err := ids.GetPluginCapabilities(context.Background(), &csi.GetPluginCapabilitiesRequest{})
	require.NoError(t, err)
	require.Len(t, resp.Capabilities, 1)

	svc := resp.Capabilities[0].GetService()
	require.NotNil(t, svc)
	assert.Equal(t, csi.PluginCapability_Service_CONTROLLER_SERVICE, svc.Type)

	for _, c := range resp.Capabilities {
		assert.Nil(t, c.GetVolumeExpansion(), "volume expansion must not be advertised")
	}
}

// ── Node-only mode ───────────────────────────────────────────────────────────

func TestProbe_NodeOnly_Ready(t *testing.T) {
	mapper := &RBDMapperMock{}
	creds := &CephCredentialProviderMock{}
	mapper.On("CheckClient", mock.Anything).Return("18.2.8", nil)
	creds.On("Available", mock.Anything).Return(nil)

	ids := &identityServer{Driver: fakeDriver(), mapper: mapper, creds: creds}
	resp, err := ids.Probe(context.Background(), &csi.ProbeRequest{})

	require.NoError(t, err)
	assert.True(t, resp.Ready.Value)
	mapper.AssertExpectations(t)
	creds.AssertExpectations(t)
}

// The wiring-bug guard: node-only mode with no mapper means SetupNodeService was
// never called. Reporting ready here would let kubelet send stage requests to a
// driver that cannot map anything.
func TestProbe_NodeOnly_NoMapperIsNotReady(t *testing.T) {
	ids := &identityServer{Driver: fakeDriver()}
	resp, err := ids.Probe(context.Background(), &csi.ProbeRequest{})
	require.NoError(t, err)
	assert.False(t, resp.Ready.Value)
}

func TestProbe_NodeOnly_RBDClientUnavailable(t *testing.T) {
	mapper := &RBDMapperMock{}
	mapper.On("CheckClient", mock.Anything).Return("", errors.New("exec: rbd not found"))

	ids := &identityServer{Driver: fakeDriver(), mapper: mapper, creds: &CephCredentialProviderMock{}}
	resp, err := ids.Probe(context.Background(), &csi.ProbeRequest{})

	require.NoError(t, err, "Probe reports readiness in the response, never as a gRPC error")
	assert.False(t, resp.Ready.Value)
}

// A missing credential Secret is an operator action, so it must surface as
// not-ready rather than as a per-volume staging failure later.
func TestProbe_NodeOnly_CredentialMissing(t *testing.T) {
	mapper := &RBDMapperMock{}
	creds := &CephCredentialProviderMock{}
	mapper.On("CheckClient", mock.Anything).Return("18.2.8", nil)
	creds.On("Available", mock.Anything).Return(ErrCredentialMissing)

	ids := &identityServer{Driver: fakeDriver(), mapper: mapper, creds: creds}
	resp, err := ids.Probe(context.Background(), &csi.ProbeRequest{})

	require.NoError(t, err)
	assert.False(t, resp.Ready.Value)
}

func TestProbe_NodeOnly_NoCredentialProvider(t *testing.T) {
	mapper := &RBDMapperMock{}
	mapper.On("CheckClient", mock.Anything).Return("18.2.8", nil)

	ids := &identityServer{Driver: fakeDriver(), mapper: mapper}
	resp, err := ids.Probe(context.Background(), &csi.ProbeRequest{})

	require.NoError(t, err)
	assert.False(t, resp.Ready.Value)
}

// ── Controller mode ──────────────────────────────────────────────────────────

func TestProbe_Controller_Ready(t *testing.T) {
	cloud := &openstack.OpenStackRBDMock{}
	cloud.On("GetCinderCapabilities").Return(&openstack.CinderCapabilities{
		SupportsV327: true, SupportsV344: true,
	})

	ids := &identityServer{Driver: fakeDriver(), cloud: cloud}
	resp, err := ids.Probe(context.Background(), &csi.ProbeRequest{})

	require.NoError(t, err)
	assert.True(t, resp.Ready.Value)
}

// 3.44 is optional: completion is skipped when absent, so the driver is still
// ready without it.
func TestProbe_Controller_ReadyWithout344(t *testing.T) {
	cloud := &openstack.OpenStackRBDMock{}
	cloud.On("GetCinderCapabilities").Return(&openstack.CinderCapabilities{
		SupportsV327: true, SupportsV344: false,
	})

	ids := &identityServer{Driver: fakeDriver(), cloud: cloud}
	resp, err := ids.Probe(context.Background(), &csi.ProbeRequest{})

	require.NoError(t, err)
	assert.True(t, resp.Ready.Value)
}

func TestProbe_Controller_CapabilitiesNotYetDiscovered(t *testing.T) {
	cloud := &openstack.OpenStackRBDMock{}
	cloud.On("GetCinderCapabilities").Return(nil)

	ids := &identityServer{Driver: fakeDriver(), cloud: cloud}
	resp, err := ids.Probe(context.Background(), &csi.ProbeRequest{})

	require.NoError(t, err)
	assert.False(t, resp.Ready.Value)
}

// 3.27 is mandatory. Startup should already have failed, so this is defensive.
func TestProbe_Controller_No327(t *testing.T) {
	cloud := &openstack.OpenStackRBDMock{}
	cloud.On("GetCinderCapabilities").Return(&openstack.CinderCapabilities{SupportsV327: false})

	ids := &identityServer{Driver: fakeDriver(), cloud: cloud}
	resp, err := ids.Probe(context.Background(), &csi.ProbeRequest{})

	require.NoError(t, err)
	assert.False(t, resp.Ready.Value)
}
