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
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"

	cpoerrors "k8s.io/cloud-provider-openstack/pkg/util/errors"
	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-iscsi/openstack"
)

// ── Test Helpers ─────────────────────────────────────────────────────────────

func newTestControllerServer() (*controllerServer, *openstack.OpenStackISCSIMock) {
	mockCloud := &openstack.OpenStackISCSIMock{}
	driver := &Driver{
		name:      "cinder-iscsi.csi.windriver.com",
		fqVersion: "1.0.0@test",
		vcap: []*csi.VolumeCapability_AccessMode{
			{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		},
		cscap: []*csi.ControllerServiceCapability{
			NewControllerServiceCapability(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME),
			NewControllerServiceCapability(csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME),
		},
	}
	cs := &controllerServer{
		Driver: driver,
		Cloud:  mockCloud,
	}
	return cs, mockCloud
}

func blockVolumeCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{
			Block: &csi.VolumeCapability_BlockVolume{},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
}

func mountVolumeCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
}

// ── CreateVolume Tests ───────────────────────────────────────────────────────

func TestCreateVolume_Success(t *testing.T) {
	cs, mockCloud := newTestControllerServer()
	ctx := context.Background()

	// No existing volumes with this name
	mockCloud.On("GetVolumesByName", ctx, "test-vol").Return([]volumes.Volume{}, nil)

	// Create volume returns success
	mockCloud.On("CreateVolume", ctx, mock.AnythingOfType("*volumes.CreateOpts"), mock.Anything).
		Return(&volumes.Volume{
			ID:               "vol-123",
			Name:             "test-vol",
			Size:             10,
			AvailabilityZone: "nova",
		}, nil)

	// Wait for available
	mockCloud.On("WaitVolumeTargetStatus", ctx, "vol-123", []string{"available"}).Return(nil)

	// Create attachment
	mockCloud.On("CreateAttachment", ctx, "vol-123").Return("att-456", nil)

	// Set metadata
	mockCloud.On("SetVolumeMetadata", ctx, "vol-123", map[string]string{
		"csi.attachment_id": "att-456",
	}).Return(nil)

	req := &csi.CreateVolumeRequest{
		Name: "test-vol",
		VolumeCapabilities: []*csi.VolumeCapability{blockVolumeCapability()},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 10 * 1024 * 1024 * 1024,
		},
		Parameters: map[string]string{
			"type":         "pure-iscsi",
			"availability": "nova",
		},
	}

	resp, err := cs.CreateVolume(ctx, req)

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, "vol-123", resp.Volume.VolumeId)
	assert.Equal(t, int64(10*1024*1024*1024), resp.Volume.CapacityBytes)
	assert.Equal(t, "att-456", resp.Volume.VolumeContext["attachment_id"])
	mockCloud.AssertExpectations(t)
}

func TestCreateVolume_Idempotent(t *testing.T) {
	cs, mockCloud := newTestControllerServer()
	ctx := context.Background()

	// Volume already exists with same size
	mockCloud.On("GetVolumesByName", ctx, "test-vol").Return([]volumes.Volume{
		{
			ID:   "vol-123",
			Name: "test-vol",
			Size: 10,
			Metadata: map[string]string{
				"csi.attachment_id": "att-456",
			},
		},
	}, nil)

	req := &csi.CreateVolumeRequest{
		Name: "test-vol",
		VolumeCapabilities: []*csi.VolumeCapability{blockVolumeCapability()},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 10 * 1024 * 1024 * 1024,
		},
	}

	resp, err := cs.CreateVolume(ctx, req)

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, "vol-123", resp.Volume.VolumeId)
	assert.Equal(t, "att-456", resp.Volume.VolumeContext["attachment_id"])
	mockCloud.AssertExpectations(t)
}

func TestCreateVolume_RejectsMountCapability(t *testing.T) {
	cs, _ := newTestControllerServer()
	ctx := context.Background()

	req := &csi.CreateVolumeRequest{
		Name:               "test-vol",
		VolumeCapabilities: []*csi.VolumeCapability{mountVolumeCapability()},
	}

	resp, err := cs.CreateVolume(ctx, req)

	assert.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "raw block volumes")
}

func TestCreateVolume_MissingName(t *testing.T) {
	cs, _ := newTestControllerServer()
	ctx := context.Background()

	req := &csi.CreateVolumeRequest{
		VolumeCapabilities: []*csi.VolumeCapability{blockVolumeCapability()},
	}

	resp, err := cs.CreateVolume(ctx, req)

	assert.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "volume name must be provided")
}

func TestCreateVolume_MissingCapabilities(t *testing.T) {
	cs, _ := newTestControllerServer()
	ctx := context.Background()

	req := &csi.CreateVolumeRequest{
		Name: "test-vol",
	}

	resp, err := cs.CreateVolume(ctx, req)

	assert.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "volume capabilities must be provided")
}

func TestCreateVolume_ExistingVolumeDifferentSize(t *testing.T) {
	cs, mockCloud := newTestControllerServer()
	ctx := context.Background()

	mockCloud.On("GetVolumesByName", ctx, "test-vol").Return([]volumes.Volume{
		{ID: "vol-123", Name: "test-vol", Size: 20},
	}, nil)

	req := &csi.CreateVolumeRequest{
		Name: "test-vol",
		VolumeCapabilities: []*csi.VolumeCapability{blockVolumeCapability()},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 10 * 1024 * 1024 * 1024,
		},
	}

	resp, err := cs.CreateVolume(ctx, req)

	assert.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "AlreadyExists")
}

// ── DeleteVolume Tests ───────────────────────────────────────────────────────

func TestDeleteVolume_SuccessHandoff(t *testing.T) {
	cs, mockCloud := newTestControllerServer()
	ctx := context.Background()

	mockCloud.On("GetVolume", ctx, "vol-123").Return(&volumes.Volume{
		ID:     "vol-123",
		Status: "reserved",
		Metadata: map[string]string{
			"csi.attachment_id": "att-456",
		},
	}, nil)

	mockCloud.On("DeleteAttachment", ctx, "att-456").Return(nil)
	mockCloud.On("DeleteVolumeMetadata", ctx, "vol-123", []string{
		"csi.attachment_id", "csi.cleanupVolume",
	}).Return(nil)

	req := &csi.DeleteVolumeRequest{VolumeId: "vol-123"}
	resp, err := cs.DeleteVolume(ctx, req)

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	mockCloud.AssertExpectations(t)
}

func TestDeleteVolume_SuccessCleanup(t *testing.T) {
	cs, mockCloud := newTestControllerServer()
	ctx := context.Background()

	mockCloud.On("GetVolume", ctx, "vol-123").Return(&volumes.Volume{
		ID:     "vol-123",
		Status: "reserved",
		Metadata: map[string]string{
			"csi.attachment_id": "att-456",
			"csi.cleanupVolume": "true",
		},
	}, nil)

	mockCloud.On("DeleteAttachment", ctx, "att-456").Return(nil)
	mockCloud.On("DeleteVolume", ctx, "vol-123").Return(nil)

	req := &csi.DeleteVolumeRequest{VolumeId: "vol-123"}
	resp, err := cs.DeleteVolume(ctx, req)

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	mockCloud.AssertExpectations(t)
}

func TestDeleteVolume_VolumeNotFound(t *testing.T) {
	cs, mockCloud := newTestControllerServer()
	ctx := context.Background()

	mockCloud.On("GetVolume", ctx, "vol-999").Return(
		(*volumes.Volume)(nil),
		cpoerrors.ErrNotFound,
	)

	req := &csi.DeleteVolumeRequest{VolumeId: "vol-999"}
	resp, err := cs.DeleteVolume(ctx, req)

	assert.NoError(t, err)
	assert.NotNil(t, resp)
}

func TestDeleteVolume_MissingVolumeID(t *testing.T) {
	cs, _ := newTestControllerServer()
	ctx := context.Background()

	req := &csi.DeleteVolumeRequest{}
	resp, err := cs.DeleteVolume(ctx, req)

	assert.Error(t, err)
	assert.Nil(t, resp)
}

// ── ControllerPublishVolume Tests ────────────────────────────────────────────

func TestControllerPublishVolume_Success(t *testing.T) {
	cs, mockCloud := newTestControllerServer()
	ctx := context.Background()

	connInfo := &openstack.ISCSIConnectionInfo{
		DriverVolumeType: "iscsi",
		TargetPortal:     "69.167.149.97:3260",
		TargetIQN:        "iqn.2010-10.org.openstack:volume-vol-123",
		TargetLUN:        0,
		AuthMethod:       "CHAP",
		AuthUsername:     "user123",
		AuthPassword:     "pass456",
	}

	mockCloud.On("UpdateAttachmentConnector", ctx, "att-456",
		mock.AnythingOfType("*openstack.AttachmentConnector")).Return(connInfo, nil)
	mockCloud.On("CompleteAttachment", ctx, "att-456").Return(nil)

	req := &csi.ControllerPublishVolumeRequest{
		VolumeId:         "vol-123",
		NodeId:           "worker-3;iqn.1993-08.org.debian:01:abc;10.0.0.103",
		VolumeCapability: blockVolumeCapability(),
		VolumeContext: map[string]string{
			"attachment_id": "att-456",
		},
	}

	resp, err := cs.ControllerPublishVolume(ctx, req)

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, "69.167.149.97:3260", resp.PublishContext["target_portal"])
	assert.Equal(t, "iqn.2010-10.org.openstack:volume-vol-123", resp.PublishContext["target_iqn"])
	assert.Equal(t, "0", resp.PublishContext["target_lun"])
	assert.Equal(t, "CHAP", resp.PublishContext["auth_method"])
	assert.Equal(t, "user123", resp.PublishContext["auth_username"])
	assert.Equal(t, "pass456", resp.PublishContext["auth_password"])
	assert.Equal(t, "iscsi", resp.PublishContext["driver_volume_type"])
	mockCloud.AssertExpectations(t)
}

func TestControllerPublishVolume_FallbackToMetadata(t *testing.T) {
	cs, mockCloud := newTestControllerServer()
	ctx := context.Background()

	// No attachment_id in volume context — fall back to volume metadata
	mockCloud.On("GetVolume", ctx, "vol-123").Return(&volumes.Volume{
		ID: "vol-123",
		Metadata: map[string]string{
			"csi.attachment_id": "att-789",
		},
	}, nil)

	connInfo := &openstack.ISCSIConnectionInfo{
		DriverVolumeType: "iscsi",
		TargetPortal:     "10.0.0.1:3260",
		TargetIQN:        "iqn.2010-10.org.openstack:volume-vol-123",
		TargetLUN:        1,
	}

	mockCloud.On("UpdateAttachmentConnector", ctx, "att-789",
		mock.AnythingOfType("*openstack.AttachmentConnector")).Return(connInfo, nil)
	mockCloud.On("CompleteAttachment", ctx, "att-789").Return(nil)

	req := &csi.ControllerPublishVolumeRequest{
		VolumeId:         "vol-123",
		NodeId:           "worker-1;iqn.1993-08.org.debian:01:xyz;10.0.0.101",
		VolumeCapability: blockVolumeCapability(),
		// No VolumeContext — forces metadata fallback
	}

	resp, err := cs.ControllerPublishVolume(ctx, req)

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, "10.0.0.1:3260", resp.PublishContext["target_portal"])
	mockCloud.AssertExpectations(t)
}

func TestControllerPublishVolume_InvalidNodeID(t *testing.T) {
	cs, _ := newTestControllerServer()
	ctx := context.Background()

	req := &csi.ControllerPublishVolumeRequest{
		VolumeId:         "vol-123",
		NodeId:           "invalid-no-semicolons",
		VolumeCapability: blockVolumeCapability(),
		VolumeContext: map[string]string{
			"attachment_id": "att-456",
		},
	}

	resp, err := cs.ControllerPublishVolume(ctx, req)

	assert.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "invalid node ID")
}

func TestControllerPublishVolume_RejectsMountCapability(t *testing.T) {
	cs, _ := newTestControllerServer()
	ctx := context.Background()

	req := &csi.ControllerPublishVolumeRequest{
		VolumeId:         "vol-123",
		NodeId:           "worker-3;iqn.xxx;10.0.0.103",
		VolumeCapability: mountVolumeCapability(),
	}

	resp, err := cs.ControllerPublishVolume(ctx, req)

	assert.Error(t, err)
	assert.Nil(t, resp)
	assert.Contains(t, err.Error(), "raw block volumes")
}

// ── ControllerUnpublishVolume Tests ──────────────────────────────────────────

func TestControllerUnpublishVolume_Success(t *testing.T) {
	cs, mockCloud := newTestControllerServer()
	ctx := context.Background()

	mockCloud.On("GetVolume", ctx, "vol-123").Return(&volumes.Volume{
		ID:     "vol-123",
		Status: "in-use",
		Metadata: map[string]string{
			"csi.attachment_id": "att-old",
		},
	}, nil)

	mockCloud.On("DeleteAttachment", ctx, "att-old").Return(nil)
	mockCloud.On("CreateAttachment", ctx, "vol-123").Return("att-new", nil)
	mockCloud.On("SetVolumeMetadata", ctx, "vol-123", map[string]string{
		"csi.attachment_id": "att-new",
	}).Return(nil)

	req := &csi.ControllerUnpublishVolumeRequest{
		VolumeId: "vol-123",
		NodeId:   "worker-3",
	}

	resp, err := cs.ControllerUnpublishVolume(ctx, req)

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	mockCloud.AssertExpectations(t)
}

func TestControllerUnpublishVolume_VolumeNotFound(t *testing.T) {
	cs, mockCloud := newTestControllerServer()
	ctx := context.Background()

	mockCloud.On("GetVolume", ctx, "vol-999").Return(
		(*volumes.Volume)(nil),
		cpoerrors.ErrNotFound,
	)

	req := &csi.ControllerUnpublishVolumeRequest{
		VolumeId: "vol-999",
		NodeId:   "worker-3",
	}

	resp, err := cs.ControllerUnpublishVolume(ctx, req)

	assert.NoError(t, err)
	assert.NotNil(t, resp)
}

func TestControllerUnpublishVolume_NoExistingAttachment(t *testing.T) {
	cs, mockCloud := newTestControllerServer()
	ctx := context.Background()

	mockCloud.On("GetVolume", ctx, "vol-123").Return(&volumes.Volume{
		ID:       "vol-123",
		Status:   "available",
		Metadata: map[string]string{},
	}, nil)

	// No DeleteAttachment call expected - attachment is empty
	mockCloud.On("CreateAttachment", ctx, "vol-123").Return("att-new", nil)
	mockCloud.On("SetVolumeMetadata", ctx, "vol-123", map[string]string{
		"csi.attachment_id": "att-new",
	}).Return(nil)

	req := &csi.ControllerUnpublishVolumeRequest{
		VolumeId: "vol-123",
		NodeId:   "worker-3",
	}

	resp, err := cs.ControllerUnpublishVolume(ctx, req)

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	mockCloud.AssertExpectations(t)
	mockCloud.AssertNotCalled(t, "DeleteAttachment", mock.Anything, mock.Anything)
}

// ── ValidateVolumeCapabilities Tests ─────────────────────────────────────────

func TestValidateVolumeCapabilities_BlockSupported(t *testing.T) {
	cs, mockCloud := newTestControllerServer()
	ctx := context.Background()

	mockCloud.On("GetVolume", ctx, "vol-123").Return(&volumes.Volume{ID: "vol-123"}, nil)

	req := &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           "vol-123",
		VolumeCapabilities: []*csi.VolumeCapability{blockVolumeCapability()},
	}

	resp, err := cs.ValidateVolumeCapabilities(ctx, req)

	assert.NoError(t, err)
	assert.NotNil(t, resp.Confirmed)
}

func TestValidateVolumeCapabilities_MountRejected(t *testing.T) {
	cs, mockCloud := newTestControllerServer()
	ctx := context.Background()

	mockCloud.On("GetVolume", ctx, "vol-123").Return(&volumes.Volume{ID: "vol-123"}, nil)

	req := &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           "vol-123",
		VolumeCapabilities: []*csi.VolumeCapability{mountVolumeCapability()},
	}

	resp, err := cs.ValidateVolumeCapabilities(ctx, req)

	assert.NoError(t, err)
	assert.Nil(t, resp.Confirmed)
	assert.Contains(t, resp.Message, "raw block volumes")
}

// ── ControllerGetCapabilities Test ───────────────────────────────────────────

func TestControllerGetCapabilities(t *testing.T) {
	cs, _ := newTestControllerServer()
	ctx := context.Background()

	resp, err := cs.ControllerGetCapabilities(ctx, &csi.ControllerGetCapabilitiesRequest{})

	assert.NoError(t, err)
	assert.Len(t, resp.Capabilities, 2)

	capTypes := make([]csi.ControllerServiceCapability_RPC_Type, len(resp.Capabilities))
	for i, cap := range resp.Capabilities {
		capTypes[i] = cap.GetRpc().GetType()
	}
	assert.Contains(t, capTypes, csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME)
	assert.Contains(t, capTypes, csi.ControllerServiceCapability_RPC_PUBLISH_UNPUBLISH_VOLUME)
}

// ── connectioninfo Tests ─────────────────────────────────────────────────────

func TestParseNodeID_Valid(t *testing.T) {
	host, iqn, ip, err := ParseNodeID("worker-3;iqn.1993-08.org.debian:01:abc;10.0.0.103")
	assert.NoError(t, err)
	assert.Equal(t, "worker-3", host)
	assert.Equal(t, "iqn.1993-08.org.debian:01:abc", iqn)
	assert.Equal(t, "10.0.0.103", ip)
}

func TestParseNodeID_Invalid(t *testing.T) {
	_, _, _, err := ParseNodeID("no-semicolons")
	assert.Error(t, err)
}

func TestParseNodeID_EmptyPart(t *testing.T) {
	_, _, _, err := ParseNodeID("host;;10.0.0.1")
	assert.Error(t, err)
}

func TestBuildPublishContext(t *testing.T) {
	connInfo := &openstack.ISCSIConnectionInfo{
		TargetPortal:     "10.0.0.1:3260",
		TargetIQN:        "iqn.2010-10.org.openstack:volume-123",
		TargetLUN:        2,
		DriverVolumeType: "iscsi",
		AuthMethod:       "CHAP",
		AuthUsername:     "user",
		AuthPassword:     "pass",
	}

	ctx := BuildPublishContext(connInfo)

	assert.Equal(t, "10.0.0.1:3260", ctx["target_portal"])
	assert.Equal(t, "iqn.2010-10.org.openstack:volume-123", ctx["target_iqn"])
	assert.Equal(t, "2", ctx["target_lun"])
	assert.Equal(t, "iscsi", ctx["driver_volume_type"])
	assert.Equal(t, "CHAP", ctx["auth_method"])
	assert.Equal(t, "user", ctx["auth_username"])
	assert.Equal(t, "pass", ctx["auth_password"])
}

func TestBuildPublishContext_NoCHAP(t *testing.T) {
	connInfo := &openstack.ISCSIConnectionInfo{
		TargetPortal:     "10.0.0.1:3260",
		TargetIQN:        "iqn.2010-10.org.openstack:volume-123",
		TargetLUN:        0,
		DriverVolumeType: "iscsi",
	}

	ctx := BuildPublishContext(connInfo)

	assert.Equal(t, "10.0.0.1:3260", ctx["target_portal"])
	_, hasAuth := ctx["auth_method"]
	assert.False(t, hasAuth)
}

func TestValidateISCSIConnectionInfo_Valid(t *testing.T) {
	connInfo := &openstack.ISCSIConnectionInfo{
		DriverVolumeType: "iscsi",
		TargetPortal:     "10.0.0.1:3260",
		TargetIQN:        "iqn.2010-10.org.openstack:volume-123",
	}
	assert.NoError(t, ValidateISCSIConnectionInfo(connInfo))
}

func TestValidateISCSIConnectionInfo_WrongType(t *testing.T) {
	connInfo := &openstack.ISCSIConnectionInfo{
		DriverVolumeType: "nfs",
		TargetPortal:     "10.0.0.1:3260",
		TargetIQN:        "iqn.2010-10.org.openstack:volume-123",
	}
	err := ValidateISCSIConnectionInfo(connInfo)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unexpected driver_volume_type")
}

func TestValidateISCSIConnectionInfo_MissingPortal(t *testing.T) {
	connInfo := &openstack.ISCSIConnectionInfo{
		DriverVolumeType: "iscsi",
		TargetIQN:        "iqn.2010-10.org.openstack:volume-123",
	}
	err := ValidateISCSIConnectionInfo(connInfo)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "target_portal")
}


