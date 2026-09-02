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
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-rbd/openstack"
	cpoerrors "k8s.io/cloud-provider-openstack/pkg/util/errors"
)

const (
	testVolumeID      = "3018df26-0ba3-45a3-adfd-4a84ed59fff1"
	testAttachmentID  = "8d6cd8d7-1e6f-4bfd-8753-42332b4bc42d"
	testNodeID        = "worker-0"
	testFSID          = "c5f7876d-258c-4152-b26a-a3ab532fda28"
	attachmentMetaKey = "csi.rbd.attachment_id"
	cleanupMetaKey    = "csi.rbd.cleanupVolume"
)

func newTestControllerServer(t *testing.T) (*controllerServer, *openstack.OpenStackRBDMock) {
	t.Helper()
	cloud := &openstack.OpenStackRBDMock{}
	cs := &controllerServer{Driver: fakeDriver(), Cloud: cloud}
	return cs, cloud
}

func defaultVolumeOpts(t *testing.T) openstack.VolumeOpts {
	t.Helper()
	var o openstack.VolumeOpts
	require.NoError(t, o.ApplyDefaults())
	return o
}

func defaultRBDOpts(t *testing.T) openstack.RBDOpts {
	t.Helper()
	var o openstack.RBDOpts
	require.NoError(t, o.ApplyDefaults())
	return o
}

func blockCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
}

func mountCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"}},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
}

func multiNodeCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{Block: &csi.VolumeCapability_BlockVolume{}},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
		},
	}
}

func labConnectionInfo() *openstack.RBDConnectionInfo {
	return &openstack.RBDConnectionInfo{
		DriverVolumeType: openstack.DriverVolumeTypeRBD,
		ClusterName:      "ceph",
		ClusterFSID:      testFSID,
		Pool:             "cinder-volumes",
		Image:            testVolumeID,
		AuthEnabled:      true,
		AuthUsername:     "cinder",
		Monitors: []openstack.MonAddr{
			{Host: "10.107.190.121", Port: "6789"},
			{Host: "10.106.210.60", Port: "6789"},
		},
		VolumeID:   testVolumeID,
		AccessMode: "rw",
	}
}

func codeOf(t *testing.T, err error) codes.Code {
	t.Helper()
	require.Error(t, err)
	return status.Convert(err).Code()
}

// expectNoStoredConnection makes the reuse probe find a reserved record that has
// no connection information yet, which is the normal first-publish state. Tests
// exercising the connector-update path use this so the probe does not short out.
func expectNoStoredConnection(cloud *openstack.OpenStackRBDMock) {
	cloud.On("GetAttachment", mock.Anything, mock.Anything).
		Return(&openstack.Attachment{Status: "reserved"}, nil).Maybe()
}

// ── Block-only enforcement ───────────────────────────────────────────────────

// Filesystem mode and multi-writer must be rejected at every capability-bearing
// entry point. Accepting them would bind a PVC that later fails to stage,
// surfacing as a stuck pod instead of a clear provisioning error.
func TestBlockOnly_RejectedEverywhere(t *testing.T) {
	tests := []struct {
		name string
		cap  *csi.VolumeCapability
	}{
		{name: "filesystem mode", cap: mountCapability()},
		{name: "multi node writer", cap: multiNodeCapability()},
	}

	for _, tt := range tests {
		t.Run("CreateVolume rejects "+tt.name, func(t *testing.T) {
			cs, cloud := newTestControllerServer(t)
			cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t)).Maybe()

			_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
				Name:               "pvc-1",
				VolumeCapabilities: []*csi.VolumeCapability{tt.cap},
			})
			assert.Equal(t, codes.InvalidArgument, codeOf(t, err))
			cloud.AssertNotCalled(t, "CreateVolume")
		})

		t.Run("ControllerPublishVolume rejects "+tt.name, func(t *testing.T) {
			cs, cloud := newTestControllerServer(t)

			_, err := cs.ControllerPublishVolume(context.Background(), &csi.ControllerPublishVolumeRequest{
				VolumeId:         testVolumeID,
				NodeId:           testNodeID,
				VolumeCapability: tt.cap,
			})
			assert.Equal(t, codes.InvalidArgument, codeOf(t, err))
			cloud.AssertNotCalled(t, "UpdateAttachmentConnector")
		})
	}
}

// ── CreateVolume ─────────────────────────────────────────────────────────────

func TestCreateVolume_Success(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetVolumesByName", ctx, "pvc-1").Return([]volumes.Volume{}, nil)
	cloud.On("CreateVolume", ctx, mock.AnythingOfType("*volumes.CreateOpts"), nil).
		Return(&volumes.Volume{ID: testVolumeID, Name: "pvc-1", Size: 20}, nil)
	cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, []string{"available"}, 300).Return(nil)
	cloud.On("CreateAttachment", ctx, testVolumeID).Return(testAttachmentID, nil)
	cloud.On("SetVolumeMetadata", ctx, testVolumeID,
		map[string]string{attachmentMetaKey: testAttachmentID}).Return(nil)

	resp, err := cs.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 20 * 1024 * 1024 * 1024},
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
		Parameters:         map[string]string{"type": "ceph-rook-store", "availability": "nova"},
	})

	require.NoError(t, err)
	assert.Equal(t, testVolumeID, resp.Volume.VolumeId)
	assert.Equal(t, int64(20*1024*1024*1024), resp.Volume.CapacityBytes)
	cloud.AssertExpectations(t)
}

// The volume type must come from the StorageClass and never be hard-coded.
func TestCreateVolume_VolumeTypeFromStorageClass(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	vopts := defaultVolumeOpts(t)
	vopts.DefaultVolumeType = "fallback-type"
	cloud.On("GetVolumeOpts").Return(vopts)
	cloud.On("GetVolumesByName", ctx, "pvc-1").Return([]volumes.Volume{}, nil)
	cloud.On("CreateVolume", ctx, mock.MatchedBy(func(o *volumes.CreateOpts) bool {
		return o.VolumeType == "ceph-rook-store"
	}), nil).Return(&volumes.Volume{ID: testVolumeID, Size: 1}, nil)
	cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, mock.Anything, mock.Anything).Return(nil)
	cloud.On("CreateAttachment", ctx, testVolumeID).Return(testAttachmentID, nil)
	cloud.On("SetVolumeMetadata", ctx, testVolumeID, mock.Anything).Return(nil)

	_, err := cs.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
		Parameters:         map[string]string{"type": "ceph-rook-store"},
	})
	require.NoError(t, err)
	cloud.AssertExpectations(t)
}

func TestCreateVolume_FallsBackToDefaultVolumeType(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	vopts := defaultVolumeOpts(t)
	vopts.DefaultVolumeType = "ceph-rook-store"
	cloud.On("GetVolumeOpts").Return(vopts)
	cloud.On("GetVolumesByName", ctx, "pvc-1").Return([]volumes.Volume{}, nil)
	cloud.On("CreateVolume", ctx, mock.MatchedBy(func(o *volumes.CreateOpts) bool {
		return o.VolumeType == "ceph-rook-store"
	}), nil).Return(&volumes.Volume{ID: testVolumeID, Size: 1}, nil)
	cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, mock.Anything, mock.Anything).Return(nil)
	cloud.On("CreateAttachment", ctx, testVolumeID).Return(testAttachmentID, nil)
	cloud.On("SetVolumeMetadata", ctx, testVolumeID, mock.Anything).Return(nil)

	_, err := cs.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
	})
	require.NoError(t, err)
	cloud.AssertExpectations(t)
}

func TestCreateVolume_Idempotent(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetVolumesByName", ctx, "pvc-1").Return([]volumes.Volume{{
		ID:       testVolumeID,
		Name:     "pvc-1",
		Size:     20,
		Metadata: map[string]string{attachmentMetaKey: testAttachmentID},
	}}, nil)

	resp, err := cs.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 20 * 1024 * 1024 * 1024},
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
	})

	require.NoError(t, err)
	assert.Equal(t, testVolumeID, resp.Volume.VolumeId)
	// A second volume must not be created for a retried request.
	cloud.AssertNotCalled(t, "CreateVolume")
	cloud.AssertNotCalled(t, "CreateAttachment")
}

func TestCreateVolume_SizeMismatchIsAlreadyExists(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetVolumesByName", ctx, "pvc-1").Return([]volumes.Volume{{
		ID: testVolumeID, Name: "pvc-1", Size: 10,
	}}, nil)

	_, err := cs.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 20 * 1024 * 1024 * 1024},
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
	})
	assert.Equal(t, codes.AlreadyExists, codeOf(t, err))
}

func TestCreateVolume_DuplicateNamesIsInternal(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetVolumesByName", ctx, "pvc-1").Return([]volumes.Volume{
		{ID: "vol-a", Name: "pvc-1", Size: 1},
		{ID: "vol-b", Name: "pvc-1", Size: 1},
	}, nil)

	_, err := cs.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
	})
	assert.Equal(t, codes.Internal, codeOf(t, err))
}

// Rounding up to a whole GiB can exceed an explicit limit; the CSI spec
// requires OUT_OF_RANGE rather than silent over-allocation.
func TestCreateVolume_LimitBytesRespected(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))

	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-1",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 1024*1024*1024 + 1, // rounds up to 2 GiB
			LimitBytes:    1024 * 1024 * 1024 * 3 / 2,
		},
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
	})
	assert.Equal(t, codes.OutOfRange, codeOf(t, err))
	cloud.AssertNotCalled(t, "CreateVolume")
}

func TestCreateVolume_RoundsUpToWholeGiB(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetVolumesByName", ctx, "pvc-1").Return([]volumes.Volume{}, nil)
	cloud.On("CreateVolume", ctx, mock.MatchedBy(func(o *volumes.CreateOpts) bool {
		return o.Size == 2
	}), nil).Return(&volumes.Volume{ID: testVolumeID, Size: 2}, nil)
	cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, mock.Anything, mock.Anything).Return(nil)
	cloud.On("CreateAttachment", ctx, testVolumeID).Return(testAttachmentID, nil)
	cloud.On("SetVolumeMetadata", ctx, testVolumeID, mock.Anything).Return(nil)

	_, err := cs.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1024*1024*1024 + 1},
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
	})
	require.NoError(t, err)
	cloud.AssertExpectations(t)
}

// Persisting the attachment ID is the only record of driver ownership, and a
// listed record is never adopted — so a failed write leaves an unusable volume.
// It must be fatal, with both the record and the volume rolled back.
func TestCreateVolume_MetadataWriteFailureIsFatalAndRollsBack(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetVolumesByName", ctx, "pvc-1").Return([]volumes.Volume{}, nil)
	cloud.On("CreateVolume", ctx, mock.Anything, nil).
		Return(&volumes.Volume{ID: testVolumeID, Size: 1}, nil)
	cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, mock.Anything, mock.Anything).Return(nil)
	cloud.On("CreateAttachment", ctx, testVolumeID).Return(testAttachmentID, nil)
	cloud.On("SetVolumeMetadata", ctx, testVolumeID, mock.Anything).
		Return(assert.AnError)
	cloud.On("DeleteAttachment", ctx, testAttachmentID).Return(nil)
	cloud.On("DeleteVolume", ctx, testVolumeID).Return(nil)

	_, err := cs.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
	})
	assert.Equal(t, codes.Aborted, codeOf(t, err))
	// Both halves of the ownership claim are withdrawn, in that order.
	cloud.AssertCalled(t, "DeleteAttachment", ctx, testAttachmentID)
	cloud.AssertCalled(t, "DeleteVolume", ctx, testVolumeID)
}

// When rollback itself cannot be confirmed, the error must say so: an
// unattributable record is left behind and the operator has to resolve it.
func TestCreateVolume_UnconfirmedRollbackIsSurfaced(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetVolumesByName", ctx, "pvc-1").Return([]volumes.Volume{}, nil)
	cloud.On("CreateVolume", ctx, mock.Anything, nil).
		Return(&volumes.Volume{ID: testVolumeID, Size: 1}, nil)
	cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, mock.Anything, mock.Anything).Return(nil)
	cloud.On("CreateAttachment", ctx, testVolumeID).Return(testAttachmentID, nil)
	cloud.On("SetVolumeMetadata", ctx, testVolumeID, mock.Anything).Return(assert.AnError)
	cloud.On("DeleteAttachment", ctx, testAttachmentID).Return(assert.AnError)
	cloud.On("DeleteVolume", ctx, testVolumeID).Return(nil)

	_, err := cs.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
	})
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
	assert.Contains(t, status.Convert(err).Message(), "unattributable")
}

// A failure after the volume exists must attempt cleanup so retries do not
// accumulate orphaned volumes.
func TestCreateVolume_CleansUpAfterLaterFailure(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetVolumesByName", ctx, "pvc-1").Return([]volumes.Volume{}, nil)
	cloud.On("CreateVolume", ctx, mock.Anything, nil).
		Return(&volumes.Volume{ID: testVolumeID, Size: 1}, nil)
	cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, mock.Anything, mock.Anything).
		Return(assert.AnError)
	cloud.On("DeleteVolume", ctx, testVolumeID).Return(nil)

	_, err := cs.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
	})
	assert.Equal(t, codes.Internal, codeOf(t, err))
	cloud.AssertCalled(t, "DeleteVolume", ctx, testVolumeID)
}

func TestCreateVolume_MissingName(t *testing.T) {
	cs, _ := newTestControllerServer(t)
	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
	})
	assert.Equal(t, codes.InvalidArgument, codeOf(t, err))
}

func TestCreateVolume_MissingCapabilities(t *testing.T) {
	cs, _ := newTestControllerServer(t)
	_, err := cs.CreateVolume(context.Background(), &csi.CreateVolumeRequest{Name: "pvc-1"})
	assert.Equal(t, codes.InvalidArgument, codeOf(t, err))
}

func TestCreateVolume_CustomMetadataPrefix(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	vopts := defaultVolumeOpts(t)
	vopts.MetadataPrefix = "mycorp"
	cloud.On("GetVolumeOpts").Return(vopts)
	cloud.On("GetVolumesByName", ctx, "pvc-1").Return([]volumes.Volume{}, nil)
	cloud.On("CreateVolume", ctx, mock.Anything, nil).
		Return(&volumes.Volume{ID: testVolumeID, Size: 1}, nil)
	cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, mock.Anything, mock.Anything).Return(nil)
	cloud.On("CreateAttachment", ctx, testVolumeID).Return(testAttachmentID, nil)
	cloud.On("SetVolumeMetadata", ctx, testVolumeID,
		map[string]string{"mycorp.attachment_id": testAttachmentID}).Return(nil)

	_, err := cs.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
	})
	require.NoError(t, err)
	cloud.AssertExpectations(t)
}

// ── DeleteVolume ─────────────────────────────────────────────────────────────

// Retain is the default: the migration Blueprint needs the volume after the PVC
// is gone.
func TestDeleteVolume_RetainsByDefault(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID:       testVolumeID,
		Status:   "in-use",
		Metadata: map[string]string{attachmentMetaKey: testAttachmentID},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("DeleteAttachment", ctx, testAttachmentID).Return(nil)
	cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, []string{"available"}, 120).Return(nil)
	cloud.On("DeleteVolumeMetadata", ctx, testVolumeID,
		[]string{attachmentMetaKey, cleanupMetaKey}).Return(nil)

	_, err := cs.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: testVolumeID})
	require.NoError(t, err)
	cloud.AssertExpectations(t)
	cloud.AssertNotCalled(t, "DeleteVolume")
}

// A per-volume cleanup request is reported and ignored: the volume is retained
// regardless. Failing the RPC instead would put the PV in a delete loop that can
// never complete, so retain-only always succeeds.
func TestDeleteVolume_PerVolumeCleanupRequestIsIgnored(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID:     testVolumeID,
		Status: "available",
		Metadata: map[string]string{
			attachmentMetaKey: testAttachmentID,
			cleanupMetaKey:    "true",
		},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("DeleteAttachment", ctx, testAttachmentID).Return(nil)
	cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, mock.Anything, mock.Anything).Return(nil)
	cloud.On("DeleteVolumeMetadata", ctx, testVolumeID,
		[]string{attachmentMetaKey, cleanupMetaKey}).Return(nil)

	_, err := cs.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: testVolumeID})
	require.NoError(t, err)
	cloud.AssertExpectations(t)
	cloud.AssertNotCalled(t, "DeleteVolume")
}

// Even with a truthy cleanup flag in any casing, the Cinder volume survives.
func TestDeleteVolume_NeverDeletesPhysicalVolume(t *testing.T) {
	for _, cleanup := range []string{"true", "TRUE", " True ", "false", ""} {
		t.Run("cleanup="+cleanup, func(t *testing.T) {
			cs, cloud := newTestControllerServer(t)
			ctx := context.Background()

			cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
				ID: testVolumeID, Status: "available",
				Metadata: map[string]string{cleanupMetaKey: cleanup},
			}, nil)
			cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
			cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, mock.Anything, mock.Anything).Return(nil)
			cloud.On("DeleteVolumeMetadata", ctx, testVolumeID, mock.Anything).Return(nil)

			_, err := cs.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: testVolumeID})
			require.NoError(t, err)
			cloud.AssertNotCalled(t, "DeleteVolume")
		})
	}
}

// Deleting a Cinder volume whose image may still be mapped risks corruption,
// so a volume that will not return to available yields a retryable Aborted.
func TestDeleteVolume_AbortsIfVolumeDoesNotBecomeAvailable(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID:       testVolumeID,
		Status:   "in-use",
		Metadata: map[string]string{attachmentMetaKey: testAttachmentID},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("DeleteAttachment", ctx, testAttachmentID).Return(nil)
	cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, mock.Anything, mock.Anything).
		Return(assert.AnError)

	_, err := cs.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: testVolumeID})
	assert.Equal(t, codes.Aborted, codeOf(t, err))
	cloud.AssertNotCalled(t, "DeleteVolume")
	cloud.AssertNotCalled(t, "DeleteVolumeMetadata")
}

func TestDeleteVolume_NotFoundIsSuccess(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, "gone").Return((*volumes.Volume)(nil), cpoerrors.ErrNotFound)

	_, err := cs.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: "gone"})
	require.NoError(t, err)
}

func TestDeleteVolume_MissingVolumeID(t *testing.T) {
	cs, _ := newTestControllerServer(t)
	_, err := cs.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{})
	assert.Equal(t, codes.InvalidArgument, codeOf(t, err))
}

// ── ControllerPublishVolume ──────────────────────────────────────────────────

func TestControllerPublishVolume_Success(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID:       testVolumeID,
		Metadata: map[string]string{attachmentMetaKey: testAttachmentID},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetRBDOpts").Return(defaultRBDOpts(t))
	expectNoStoredConnection(cloud)
	cloud.On("UpdateAttachmentConnector", ctx, testAttachmentID,
		mock.MatchedBy(func(c *openstack.AttachmentConnector) bool {
			// The connector carries only host: measured as the sole requirement.
			return c.Host == testNodeID
		})).Return(labConnectionInfo(), nil)
	cloud.On("CompleteAttachment", ctx, testAttachmentID).Return(nil)

	resp, err := cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId:         testVolumeID,
		NodeId:           testNodeID,
		VolumeCapability: blockCapability(),
	})

	require.NoError(t, err)
	pc := resp.PublishContext
	assert.Equal(t, "rbd", pc[PublishContextDriverVolumeType])
	assert.Equal(t, testFSID, pc[PublishContextClusterFSID])
	assert.Equal(t, "cinder-volumes", pc[PublishContextPool])
	assert.Equal(t, testVolumeID, pc[PublishContextImage])
	assert.Equal(t, "10.107.190.121:6789,10.106.210.60:6789", pc[PublishContextMonitors])
	assert.Equal(t, "cinder", pc[PublishContextAuthUsername])
	assert.Equal(t, testAttachmentID, pc[PublishContextAttachmentID])
	cloud.AssertExpectations(t)
}

// publish_context is stored in a world-readable VolumeAttachment, so it must
// never contain credential material.
func TestControllerPublishVolume_PublishContextHasNoSecrets(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID: testVolumeID, Metadata: map[string]string{attachmentMetaKey: testAttachmentID},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetRBDOpts").Return(defaultRBDOpts(t))
	expectNoStoredConnection(cloud)
	cloud.On("UpdateAttachmentConnector", ctx, testAttachmentID, mock.Anything).
		Return(labConnectionInfo(), nil)
	cloud.On("CompleteAttachment", ctx, testAttachmentID).Return(nil)

	resp, err := cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID, VolumeCapability: blockCapability(),
	})
	require.NoError(t, err)

	for k, v := range resp.PublishContext {
		for _, forbidden := range []string{"keyring", "userKey", "secret_key", "password"} {
			assert.NotContains(t, k, forbidden)
			assert.NotContains(t, v, forbidden)
		}
	}
	assert.NotContains(t, resp.PublishContext, "keyring")
}

// Metadata lost but a record exists. A connector-less Cinder attachment carries
// no ownership marker — nothing in it can be stamped or read back to prove this
// driver created it — so "exactly one record" is evidence of a record, not of
// ownership. Adopting it could attach a record another actor owns, so the driver
// refuses and mutates nothing.
func TestControllerPublishVolume_RefusesToAdoptListedRecord(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID: testVolumeID, Metadata: map[string]string{},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("ListAttachmentsByVolume", ctx, testVolumeID).Return([]openstack.Attachment{
		{ID: testAttachmentID, VolumeID: testVolumeID, Status: "reserved"},
	}, nil)

	_, err := cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID, VolumeCapability: blockCapability(),
	})
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
	assert.Contains(t, status.Convert(err).Message(), "ownership marker")
	cloud.AssertNotCalled(t, "CreateAttachment")
	cloud.AssertNotCalled(t, "SetVolumeMetadata")
	cloud.AssertNotCalled(t, "UpdateAttachmentConnector")
	cloud.AssertNotCalled(t, "DeleteAttachment")
}

func TestControllerPublishVolume_CreatesRecordWhenNoneExists(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID: testVolumeID, Metadata: map[string]string{},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetRBDOpts").Return(defaultRBDOpts(t))
	expectNoStoredConnection(cloud)
	cloud.On("ListAttachmentsByVolume", ctx, testVolumeID).Return([]openstack.Attachment{}, nil)
	cloud.On("CreateAttachment", ctx, testVolumeID).Return(testAttachmentID, nil)
	cloud.On("SetVolumeMetadata", ctx, testVolumeID, mock.Anything).Return(nil)
	cloud.On("UpdateAttachmentConnector", ctx, testAttachmentID, mock.Anything).
		Return(labConnectionInfo(), nil)
	cloud.On("CompleteAttachment", ctx, testAttachmentID).Return(nil)

	_, err := cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID, VolumeCapability: blockCapability(),
	})
	require.NoError(t, err)
	cloud.AssertExpectations(t)
}

// Two candidate records: choosing one could attach a record another actor owns,
// so the driver refuses and mutates nothing.
func TestControllerPublishVolume_AmbiguousRecordsFailSafely(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID: testVolumeID, Metadata: map[string]string{},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("ListAttachmentsByVolume", ctx, testVolumeID).Return([]openstack.Attachment{
		{ID: "att-1", VolumeID: testVolumeID},
		{ID: "att-2", VolumeID: testVolumeID},
	}, nil)

	_, err := cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID, VolumeCapability: blockCapability(),
	})
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
	cloud.AssertNotCalled(t, "CreateAttachment")
	cloud.AssertNotCalled(t, "DeleteAttachment")
	cloud.AssertNotCalled(t, "UpdateAttachmentConnector")
}

// A stale record yields 404; the driver replaces it and retries exactly once.
func TestControllerPublishVolume_StaleRecordReplacedAndRetriedOnce(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID: testVolumeID, Metadata: map[string]string{attachmentMetaKey: "att-stale"},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetRBDOpts").Return(defaultRBDOpts(t))
	expectNoStoredConnection(cloud)
	cloud.On("UpdateAttachmentConnector", ctx, "att-stale", mock.Anything).
		Return((*openstack.RBDConnectionInfo)(nil), cpoerrors.ErrNotFound).Once()
	cloud.On("CreateAttachment", ctx, testVolumeID).Return(testAttachmentID, nil).Once()
	cloud.On("SetVolumeMetadata", ctx, testVolumeID,
		map[string]string{attachmentMetaKey: testAttachmentID}).Return(nil)
	cloud.On("UpdateAttachmentConnector", ctx, testAttachmentID, mock.Anything).
		Return(labConnectionInfo(), nil).Once()
	cloud.On("CompleteAttachment", ctx, testAttachmentID).Return(nil)

	resp, err := cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID, VolumeCapability: blockCapability(),
	})
	require.NoError(t, err)
	assert.Equal(t, testAttachmentID, resp.PublishContext[PublishContextAttachmentID])
	cloud.AssertExpectations(t)
}

// Retrying forever would mask a backend deleting records as fast as we create
// them, so a second 404 is surfaced.
func TestControllerPublishVolume_SecondFailureIsNotRetried(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID: testVolumeID, Metadata: map[string]string{attachmentMetaKey: "att-stale"},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	expectNoStoredConnection(cloud)
	cloud.On("UpdateAttachmentConnector", ctx, "att-stale", mock.Anything).
		Return((*openstack.RBDConnectionInfo)(nil), cpoerrors.ErrNotFound).Once()
	cloud.On("CreateAttachment", ctx, testVolumeID).Return("att-new", nil).Once()
	cloud.On("SetVolumeMetadata", ctx, testVolumeID, mock.Anything).Return(nil)
	cloud.On("UpdateAttachmentConnector", ctx, "att-new", mock.Anything).
		Return((*openstack.RBDConnectionInfo)(nil), cpoerrors.ErrNotFound).Once()

	_, err := cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID, VolumeCapability: blockCapability(),
	})
	assert.Equal(t, codes.Internal, codeOf(t, err))
	cloud.AssertNumberOfCalls(t, "CreateAttachment", 1)
}

// Completion needs microversion 3.44 and is optional, so its failure must not
// fail publish.
func TestControllerPublishVolume_CompletionFailureIsNotFatal(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID: testVolumeID, Metadata: map[string]string{attachmentMetaKey: testAttachmentID},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetRBDOpts").Return(defaultRBDOpts(t))
	expectNoStoredConnection(cloud)
	cloud.On("UpdateAttachmentConnector", ctx, testAttachmentID, mock.Anything).
		Return(labConnectionInfo(), nil)
	cloud.On("CompleteAttachment", ctx, testAttachmentID).Return(assert.AnError)

	_, err := cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID, VolumeCapability: blockCapability(),
	})
	require.NoError(t, err)
}

// An FSID that disagrees with configuration means the backend is pointing at a
// different Ceph cluster: publishing would let the node map an unrelated image.
func TestControllerPublishVolume_FSIDMismatchFailsPrecondition(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	ropts := defaultRBDOpts(t)
	ropts.ExpectedFSID = "a-totally-different-fsid"

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID: testVolumeID, Metadata: map[string]string{attachmentMetaKey: testAttachmentID},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetRBDOpts").Return(ropts)
	expectNoStoredConnection(cloud)
	cloud.On("UpdateAttachmentConnector", ctx, testAttachmentID, mock.Anything).
		Return(labConnectionInfo(), nil)
	cloud.On("CompleteAttachment", ctx, testAttachmentID).Return(nil)

	_, err := cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID, VolumeCapability: blockCapability(),
	})
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
}

// Validation must precede completion.
//
// Completion advances the record to in-use, and Cinder can refuse a later update
// on a completed record — so completing first would leave a volume with unusable
// connection information that retries cannot update out of. Ordering is the whole
// guarantee here, so it is asserted directly rather than inferred from the error.
func TestControllerPublishVolume_ValidatesBeforeCompleting(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	ropts := defaultRBDOpts(t)
	ropts.ExpectedFSID = "a-totally-different-fsid"

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID: testVolumeID, Metadata: map[string]string{attachmentMetaKey: testAttachmentID},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetRBDOpts").Return(ropts)
	expectNoStoredConnection(cloud)
	cloud.On("UpdateAttachmentConnector", ctx, testAttachmentID, mock.Anything).
		Return(labConnectionInfo(), nil)

	_, err := cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID, VolumeCapability: blockCapability(),
	})
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
	cloud.AssertNotCalled(t, "CompleteAttachment")
}

// Stored connection information that fails validation must not be trusted; the
// connector update runs instead, since it may return corrected information.
func TestControllerPublishVolume_UnusableStoredConnectionFallsBackToUpdate(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	// An empty pool is invalid regardless of configuration, unlike an FSID
	// mismatch which is only checked when expected-fsid is set.
	corrupt := labConnectionInfo()
	corrupt.Pool = ""

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID: testVolumeID, Metadata: map[string]string{attachmentMetaKey: testAttachmentID},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetRBDOpts").Return(defaultRBDOpts(t))
	cloud.On("GetAttachment", ctx, testAttachmentID).Return(&openstack.Attachment{
		ID: testAttachmentID, Status: "attached", ConnectionInfo: corrupt,
	}, nil).Once()
	cloud.On("UpdateAttachmentConnector", ctx, testAttachmentID, mock.Anything).
		Return(labConnectionInfo(), nil).Once()
	cloud.On("CompleteAttachment", ctx, testAttachmentID).Return(nil)

	_, err := cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID, VolumeCapability: blockCapability(),
	})
	require.NoError(t, err)
	cloud.AssertNumberOfCalls(t, "UpdateAttachmentConnector", 1)
}

// Rolling back a record whose ID could not be persisted is what keeps the next
// publish from finding an unattributable record.
func TestControllerPublishVolume_RollsBackWhenMetadataWriteFails(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID: testVolumeID, Metadata: map[string]string{},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("ListAttachmentsByVolume", ctx, testVolumeID).Return([]openstack.Attachment{}, nil)
	cloud.On("CreateAttachment", ctx, testVolumeID).Return(testAttachmentID, nil).Once()
	cloud.On("SetVolumeMetadata", ctx, testVolumeID, mock.Anything).Return(assert.AnError).Once()
	cloud.On("DeleteAttachment", ctx, testAttachmentID).Return(nil).Once()

	_, err := cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID, VolumeCapability: blockCapability(),
	})
	assert.Equal(t, codes.Aborted, codeOf(t, err))
	cloud.AssertCalled(t, "DeleteAttachment", ctx, testAttachmentID)
	cloud.AssertNotCalled(t, "UpdateAttachmentConnector")
}

func TestControllerPublishVolume_VolumeNotFound(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, "gone").Return((*volumes.Volume)(nil), cpoerrors.ErrNotFound)

	_, err := cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: "gone", NodeId: testNodeID, VolumeCapability: blockCapability(),
	})
	assert.Equal(t, codes.NotFound, codeOf(t, err))
}

func TestControllerPublishVolume_InvalidArguments(t *testing.T) {
	tests := []struct {
		name string
		req  *csi.ControllerPublishVolumeRequest
	}{
		{name: "missing volume id", req: &csi.ControllerPublishVolumeRequest{
			NodeId: testNodeID, VolumeCapability: blockCapability()}},
		{name: "missing node id", req: &csi.ControllerPublishVolumeRequest{
			VolumeId: testVolumeID, VolumeCapability: blockCapability()}},
		{name: "missing capability", req: &csi.ControllerPublishVolumeRequest{
			VolumeId: testVolumeID, NodeId: testNodeID}},
		{name: "malformed node id", req: &csi.ControllerPublishVolumeRequest{
			VolumeId: testVolumeID, NodeId: "a;b;c", VolumeCapability: blockCapability()}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs, _ := newTestControllerServer(t)
			_, err := cs.ControllerPublishVolume(context.Background(), tt.req)
			assert.Equal(t, codes.InvalidArgument, codeOf(t, err))
		})
	}
}

// ── ControllerUnpublishVolume ────────────────────────────────────────────────

// Unpublish deletes the record and creates no replacement: the available window
// between migration pods is what lets a multi-phase CDI workflow advance.
func TestControllerUnpublishVolume_DeletesRecordWithoutRotating(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID:       testVolumeID,
		Status:   "in-use",
		Metadata: map[string]string{attachmentMetaKey: testAttachmentID},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("DeleteAttachment", ctx, testAttachmentID).Return(nil)
	cloud.On("DeleteVolumeMetadata", ctx, testVolumeID, []string{attachmentMetaKey}).Return(nil)
	cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, []string{"available"}, 120).Return(nil)

	_, err := cs.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID,
	})
	require.NoError(t, err)
	cloud.AssertExpectations(t)
	cloud.AssertNotCalled(t, "CreateAttachment")
}

func TestControllerUnpublishVolume_NoAttachmentInMetadata(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID: testVolumeID, Status: "available", Metadata: map[string]string{},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("DeleteVolumeMetadata", ctx, testVolumeID, []string{attachmentMetaKey}).Return(nil)
	cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, mock.Anything, mock.Anything).Return(nil)

	_, err := cs.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID,
	})
	require.NoError(t, err)
	cloud.AssertNotCalled(t, "DeleteAttachment")
}

// The next pod must not be published while the volume is still reserved, so a
// volume that will not return to available yields a retryable Aborted.
func TestControllerUnpublishVolume_AbortsIfNotAvailable(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID: testVolumeID, Status: "in-use",
		Metadata: map[string]string{attachmentMetaKey: testAttachmentID},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("DeleteAttachment", ctx, testAttachmentID).Return(nil)
	cloud.On("DeleteVolumeMetadata", ctx, testVolumeID, mock.Anything).Return(nil)
	cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, mock.Anything, mock.Anything).
		Return(assert.AnError)

	_, err := cs.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID,
	})
	assert.Equal(t, codes.Aborted, codeOf(t, err))
}

func TestControllerUnpublishVolume_VolumeNotFoundIsSuccess(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, "gone").Return((*volumes.Volume)(nil), cpoerrors.ErrNotFound)

	_, err := cs.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{VolumeId: "gone"})
	require.NoError(t, err)
}

// Clearing metadata is best effort: the record is already gone, which is what
// released the volume, and a stale key is recovered from on the next publish.
// Clearing the attachment ID is fatal: the key is the driver's authoritative
// ownership record, and the migration handoff reads it. The retry converges
// because deleting an already-deleted record now succeeds.
func TestControllerUnpublishVolume_MetadataClearFailureIsRetryable(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID: testVolumeID, Metadata: map[string]string{attachmentMetaKey: testAttachmentID},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("DeleteAttachment", ctx, testAttachmentID).Return(nil).Once()
	cloud.On("DeleteVolumeMetadata", ctx, testVolumeID, mock.Anything).Return(assert.AnError).Once()

	_, err := cs.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID,
	})
	assert.Equal(t, codes.Aborted, codeOf(t, err))
	// The wait is never reached: the volume is not released until its metadata is.
	cloud.AssertNotCalled(t, "WaitVolumeTargetStatus")
}

// The retry after a failed metadata clear must converge. On the second attempt
// the record is already gone — that 404 must not become a permanent failure.
func TestControllerUnpublishVolume_RetryAfterMetadataFailureConverges(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID: testVolumeID, Metadata: map[string]string{attachmentMetaKey: testAttachmentID},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("DeleteAttachment", ctx, testAttachmentID).Return(nil).Once()
	cloud.On("DeleteVolumeMetadata", ctx, testVolumeID, mock.Anything).Return(assert.AnError).Once()

	_, err := cs.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID,
	})
	require.Error(t, err)

	// Second attempt: metadata still names the deleted record.
	cloud.On("DeleteAttachment", ctx, testAttachmentID).Return(cpoerrors.ErrNotFound).Once()
	cloud.On("DeleteVolumeMetadata", ctx, testVolumeID, mock.Anything).Return(nil).Once()
	cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, mock.Anything, mock.Anything).Return(nil)

	_, err = cs.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID,
	})
	require.NoError(t, err, "a record that is already gone must not block the retry")
	cloud.AssertExpectations(t)
}

// A missing record already satisfies the delete goal, in both RPCs that remove
// one. Returning an error would make the sidecar retry forever.
func TestDeleteAttachment_NotFoundIsSuccess(t *testing.T) {
	t.Run("DeleteVolume", func(t *testing.T) {
		cs, cloud := newTestControllerServer(t)
		ctx := context.Background()

		cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
			ID: testVolumeID, Status: "available",
			Metadata: map[string]string{attachmentMetaKey: testAttachmentID},
		}, nil)
		cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
		cloud.On("DeleteAttachment", ctx, testAttachmentID).Return(cpoerrors.ErrNotFound)
		cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, mock.Anything, mock.Anything).Return(nil)
		cloud.On("DeleteVolumeMetadata", ctx, testVolumeID, mock.Anything).Return(nil)

		_, err := cs.DeleteVolume(ctx, &csi.DeleteVolumeRequest{VolumeId: testVolumeID})
		require.NoError(t, err)
	})

	t.Run("ControllerUnpublishVolume", func(t *testing.T) {
		cs, cloud := newTestControllerServer(t)
		ctx := context.Background()

		cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
			ID: testVolumeID, Metadata: map[string]string{attachmentMetaKey: testAttachmentID},
		}, nil)
		cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
		cloud.On("DeleteAttachment", ctx, testAttachmentID).Return(cpoerrors.ErrNotFound)
		cloud.On("DeleteVolumeMetadata", ctx, testVolumeID, mock.Anything).Return(nil)
		cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, mock.Anything, mock.Anything).Return(nil)

		_, err := cs.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
			VolumeId: testVolumeID, NodeId: testNodeID,
		})
		require.NoError(t, err)
	})

	// Rollback: a record already gone means the rollback goal is met, so the
	// caller must not be told an unattributable record was left behind.
	t.Run("publish rollback", func(t *testing.T) {
		cs, cloud := newTestControllerServer(t)
		ctx := context.Background()

		cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
			ID: testVolumeID, Metadata: map[string]string{},
		}, nil)
		cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
		cloud.On("ListAttachmentsByVolume", ctx, testVolumeID).Return([]openstack.Attachment{}, nil)
		cloud.On("CreateAttachment", ctx, testVolumeID).Return(testAttachmentID, nil)
		cloud.On("SetVolumeMetadata", ctx, testVolumeID, mock.Anything).Return(assert.AnError)
		cloud.On("DeleteAttachment", ctx, testAttachmentID).Return(cpoerrors.ErrNotFound)

		_, err := cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
			VolumeId: testVolumeID, NodeId: testNodeID, VolumeCapability: blockCapability(),
		})
		assert.Equal(t, codes.Aborted, codeOf(t, err))
		assert.NotContains(t, status.Convert(err).Message(), "unattributable")
	})
}

// ── Capability RPCs ──────────────────────────────────────────────────────────

func TestValidateVolumeCapabilities_BlockConfirmed(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{ID: testVolumeID}, nil)

	resp, err := cs.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           testVolumeID,
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Confirmed)
	assert.Empty(t, resp.Message)
}

// The CSI spec wants a message, not an error, for unsupported capabilities.
func TestValidateVolumeCapabilities_MountReturnsMessageNotError(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{ID: testVolumeID}, nil)

	resp, err := cs.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           testVolumeID,
		VolumeCapabilities: []*csi.VolumeCapability{mountCapability()},
	})
	require.NoError(t, err)
	assert.Nil(t, resp.Confirmed)
	assert.NotEmpty(t, resp.Message)
}

func TestValidateVolumeCapabilities_VolumeNotFound(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := context.Background()

	cloud.On("GetVolume", ctx, "gone").Return((*volumes.Volume)(nil), cpoerrors.ErrNotFound)

	_, err := cs.ValidateVolumeCapabilities(ctx, &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId:           "gone",
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
	})
	assert.Equal(t, codes.NotFound, codeOf(t, err))
}

func TestControllerGetCapabilities(t *testing.T) {
	cs, _ := newTestControllerServer(t)
	resp, err := cs.ControllerGetCapabilities(context.Background(),
		&csi.ControllerGetCapabilitiesRequest{})
	require.NoError(t, err)
	assert.Len(t, resp.Capabilities, 2)
}

// Expansion is a non-goal and must stay Unimplemented rather than silently
// appearing to work.
func TestControllerExpandVolume_Unimplemented(t *testing.T) {
	cs, _ := newTestControllerServer(t)
	_, err := cs.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: testVolumeID,
	})
	assert.Equal(t, codes.Unimplemented, codeOf(t, err))
}
