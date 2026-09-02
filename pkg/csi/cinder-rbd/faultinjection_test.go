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

// This file covers the failure matrix in section 9.2 of the implementation
// design. Each test names the row it demonstrates, so a reviewer can map the
// document to executable evidence.
//
// The shape of every case is the same: perform an operation, fail immediately
// after one external side effect, then verify the retry converges without
// duplicating the side effect or destroying state.

package rbd

import (
	"os"
	"path/filepath"
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

// §9.2 row 1: volume created, reservation not created.
//
// The retry must find the volume by name rather than create a second one, then
// create the missing reservation.
func TestFault_VolumeCreatedReservationFailed_RetryReusesVolume(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := t.Context()

	// First attempt: volume created, reservation fails, cleanup deletes it.
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetVolumesByName", ctx, "pvc-1").Return([]volumes.Volume{}, nil).Once()
	cloud.On("CreateVolume", ctx, mock.Anything, nil).
		Return(&volumes.Volume{ID: testVolumeID, Size: 1}, nil).Once()
	cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, mock.Anything, mock.Anything).Return(nil)
	cloud.On("CreateAttachment", ctx, testVolumeID).Return("", assert.AnError).Once()
	cloud.On("DeleteVolume", ctx, testVolumeID).Return(nil).Once()

	_, err := cs.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
	})
	require.Error(t, err)

	// Second attempt: a fresh volume plus a working reservation.
	cloud.On("GetVolumesByName", ctx, "pvc-1").Return([]volumes.Volume{}, nil).Once()
	cloud.On("CreateVolume", ctx, mock.Anything, nil).
		Return(&volumes.Volume{ID: testVolumeID, Size: 1}, nil).Once()
	cloud.On("CreateAttachment", ctx, testVolumeID).Return(testAttachmentID, nil).Once()
	cloud.On("SetVolumeMetadata", ctx, testVolumeID, mock.Anything).Return(nil)

	resp, err := cs.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
	})
	require.NoError(t, err)
	assert.Equal(t, testVolumeID, resp.Volume.VolumeId)
}

// §9.2 row 1 variant: the volume already exists from a prior attempt whose
// response was lost. It must be adopted, not duplicated.
func TestFault_CreateVolumeResponseLost_RetryAdoptsExistingVolume(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := t.Context()

	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetVolumesByName", ctx, "pvc-1").Return([]volumes.Volume{{
		ID: testVolumeID, Name: "pvc-1", Size: 1,
		Metadata: map[string]string{attachmentMetaKey: testAttachmentID},
	}}, nil)

	resp, err := cs.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1024 * 1024 * 1024},
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
	})
	require.NoError(t, err)
	assert.Equal(t, testVolumeID, resp.Volume.VolumeId)

	cloud.AssertNotCalled(t, "CreateVolume")
	cloud.AssertNotCalled(t, "CreateAttachment")
}

// §9.2 row 1 variant: the volume exists from a prior attempt but carries no
// attachment ID — the reservation half never landed.
//
// Returning the volume as-is would report success for an unreserved volume, and
// because a listed record is never adopted it would then fail closed at publish.
// The retry must reconcile the missing reservation so it converges.
func TestFault_ExistingVolumeMissingReservation_RetryCreatesIt(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := t.Context()

	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetVolumesByName", ctx, "pvc-1").Return([]volumes.Volume{{
		ID: testVolumeID, Name: "pvc-1", Size: 1, Metadata: map[string]string{},
	}}, nil)
	cloud.On("ListAttachmentsByVolume", ctx, testVolumeID).
		Return([]openstack.Attachment{}, nil).Once()
	cloud.On("CreateAttachment", ctx, testVolumeID).Return(testAttachmentID, nil).Once()
	cloud.On("SetVolumeMetadata", ctx, testVolumeID,
		map[string]string{attachmentMetaKey: testAttachmentID}).Return(nil).Once()

	resp, err := cs.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1024 * 1024 * 1024},
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
	})
	require.NoError(t, err)
	assert.Equal(t, testAttachmentID, resp.Volume.VolumeContext[metaKeySuffixAttachmentID],
		"the reconciled reservation must be reported back")
	cloud.AssertNotCalled(t, "CreateVolume")
	cloud.AssertExpectations(t)
}

// Same shape, but a record already exists that the driver cannot attribute.
// Adopting it is exactly what must not happen, so CreateVolume fails closed.
func TestFault_ExistingVolumeWithUnattributableRecord_FailsClosed(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := t.Context()

	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetVolumesByName", ctx, "pvc-1").Return([]volumes.Volume{{
		ID: testVolumeID, Name: "pvc-1", Size: 1, Metadata: map[string]string{},
	}}, nil)
	cloud.On("ListAttachmentsByVolume", ctx, testVolumeID).Return([]openstack.Attachment{
		{ID: "att-unknown", VolumeID: testVolumeID, Status: "reserved"},
	}, nil).Once()

	_, err := cs.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		CapacityRange:      &csi.CapacityRange{RequiredBytes: 1024 * 1024 * 1024},
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
	})
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
	cloud.AssertNotCalled(t, "CreateAttachment")
	cloud.AssertNotCalled(t, "SetVolumeMetadata")
}

// §9.2 row 2: reservation created, metadata write failed.
//
// The attachment ID in volume metadata is the only record of driver ownership,
// and a listed record is never adopted, so a volume whose ID was not persisted is
// unusable. CreateVolume must therefore roll both halves back and fail, leaving
// nothing for the retry to reconcile — and the retry must then succeed.
func TestFault_ReservationCreatedMetadataWriteFailed_RollsBackAndRetrySucceeds(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := t.Context()

	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetVolumesByName", ctx, "pvc-1").Return([]volumes.Volume{}, nil).Once()
	cloud.On("CreateVolume", ctx, mock.Anything, nil).
		Return(&volumes.Volume{ID: testVolumeID, Size: 1}, nil)
	cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, mock.Anything, mock.Anything).Return(nil)
	cloud.On("CreateAttachment", ctx, testVolumeID).Return(testAttachmentID, nil).Once()
	cloud.On("SetVolumeMetadata", ctx, testVolumeID,
		map[string]string{attachmentMetaKey: testAttachmentID}).Return(assert.AnError).Once()
	// Rollback: the record first, then the volume that would otherwise be
	// orphaned. Both are confirmed here, so no ownership claim survives.
	cloud.On("DeleteAttachment", ctx, testAttachmentID).Return(nil).Once()
	cloud.On("DeleteVolume", ctx, testVolumeID).Return(nil).Once()

	_, err := cs.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
	})
	assert.Equal(t, codes.Aborted, codeOf(t, err),
		"an unpersisted ownership record must fail the RPC retryably")

	// The retry finds nothing left over and completes normally.
	cloud.On("GetVolumesByName", ctx, "pvc-1").Return([]volumes.Volume{}, nil).Once()
	cloud.On("CreateAttachment", ctx, testVolumeID).Return("att-retry", nil).Once()
	cloud.On("SetVolumeMetadata", ctx, testVolumeID,
		map[string]string{attachmentMetaKey: "att-retry"}).Return(nil).Once()

	resp, err := cs.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
	})
	require.NoError(t, err)
	assert.Equal(t, "att-retry", resp.Volume.VolumeContext[metaKeySuffixAttachmentID])
	cloud.AssertExpectations(t)
}

// §9.2 row 2, unconfirmed-rollback variant: the record could not be deleted.
//
// This is an operator-resolution boundary, not a recoverable one. An
// unattributable record is left in Cinder, and the next publish must fail closed
// on it rather than adopt it — adoption is what would let the driver attach a
// record it cannot prove it owns.
func TestFault_UnconfirmedRollbackLeavesVolumeFailingClosed(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := t.Context()

	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetVolumesByName", ctx, "pvc-1").Return([]volumes.Volume{}, nil)
	cloud.On("CreateVolume", ctx, mock.Anything, nil).
		Return(&volumes.Volume{ID: testVolumeID, Size: 1}, nil)
	cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, mock.Anything, mock.Anything).Return(nil)
	cloud.On("CreateAttachment", ctx, testVolumeID).Return(testAttachmentID, nil).Once()
	cloud.On("SetVolumeMetadata", ctx, testVolumeID, mock.Anything).Return(assert.AnError).Once()
	cloud.On("DeleteAttachment", ctx, testAttachmentID).Return(assert.AnError).Once()
	cloud.On("DeleteVolume", ctx, testVolumeID).Return(nil).Once()

	_, err := cs.CreateVolume(ctx, &csi.CreateVolumeRequest{
		Name:               "pvc-1",
		VolumeCapabilities: []*csi.VolumeCapability{blockCapability()},
	})
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
	assert.Contains(t, status.Convert(err).Message(), "unattributable",
		"the operator must be told a record was left behind")

	// A publish against a volume carrying that leftover record refuses to run.
	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID: testVolumeID, Metadata: map[string]string{},
	}, nil)
	cloud.On("ListAttachmentsByVolume", ctx, testVolumeID).Return([]openstack.Attachment{
		{ID: testAttachmentID, VolumeID: testVolumeID, Status: "reserved"},
	}, nil)

	_, err = cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID, VolumeCapability: blockCapability(),
	})
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
	cloud.AssertNumberOfCalls(t, "CreateAttachment", 1)
	cloud.AssertNotCalled(t, "UpdateAttachmentConnector")
}

// §9.2 row 4: map succeeded, staging record write failed.
//
// The mapping must be rolled back, not left for a later stage to adopt. Ownership
// now rests on the pre-map intent, and a mapping whose completed record never
// landed has had nothing verify its size or record its device for unstage.
// Leaving it live would mean a later stage reuses a mapping the driver never
// finished validating.
func TestFault_MapSucceededRecordWriteFailed_RollsBackTheMap(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	createFakeDevice(t, &dev)

	// Make the staging path a file so the record write fails.
	require.NoError(t, os.MkdirAll(filepath.Dir(f.staging), 0o755))
	require.NoError(t, os.WriteFile(f.staging, []byte("not a directory"), 0o600))

	f.creds.On("Load", mock.Anything, "cinder").Return(NewTestCredential("cinder", redactedKey), nil)
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil).Once()
	f.mapper.On("Map", mock.Anything, mock.Anything).Return(dev, nil).Once()
	f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).Return(nil)
	f.mapper.On("DeviceSize", mock.Anything, dev.DevicePath).Return(int64(1073741824), nil)
	f.mapper.On("LockHolders", mock.Anything, mock.Anything).Return([]string{"client.1@ip"}, nil)
	// Rollback: unmap, then confirm absence.
	f.mapper.On("Unmap", mock.Anything, dev.DevicePath, mock.Anything).Return(nil).Once()
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil).Once()

	_, err := f.ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})
	require.Error(t, err, "the RPC must fail so kubelet retries")

	// Identity was verified before unmapping: a device path alone never
	// authorizes an unmap.
	f.mapper.AssertCalled(t, "Unmap", mock.Anything, dev.DevicePath, mock.Anything)
	// Absence confirmed, so the ownership intent is gone and the retry maps afresh.
	_, intentErr := f.ns.Staging.ReadIndex(testVolumeID)
	require.Error(t, intentErr, "the intent must be removed once absence is confirmed")

	// Retry with a usable staging path: a clean map, not a reuse.
	require.NoError(t, os.Remove(f.staging))
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil)
	f.mapper.On("Map", mock.Anything, mock.Anything).Return(dev, nil).Once()

	_, err = f.ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})
	require.NoError(t, err)
	f.mapper.AssertNumberOfCalls(t, "Map", 2)
}

// Rollback that cannot confirm absence must retain the intent, so the mapping
// stays attributable and reconciliation can still finish the job.
func TestFault_RollbackUnmapFailureRetainsOwnershipIntent(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	createFakeDevice(t, &dev)

	require.NoError(t, os.MkdirAll(filepath.Dir(f.staging), 0o755))
	require.NoError(t, os.WriteFile(f.staging, []byte("not a directory"), 0o600))

	f.creds.On("Load", mock.Anything, "cinder").Return(NewTestCredential("cinder", redactedKey), nil)
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil).Once()
	f.mapper.On("Map", mock.Anything, mock.Anything).Return(dev, nil).Once()
	f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).Return(nil)
	f.mapper.On("DeviceSize", mock.Anything, dev.DevicePath).Return(int64(1073741824), nil)
	f.mapper.On("LockHolders", mock.Anything, mock.Anything).Return([]string{"client.1@ip"}, nil)
	f.mapper.On("Unmap", mock.Anything, dev.DevicePath, mock.Anything).Return(assert.AnError).Once()

	_, err := f.ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})
	require.Error(t, err)
	assert.Contains(t, status.Convert(err).Message(), "intent is retained")

	rec, intentErr := f.ns.Staging.ReadIndex(testVolumeID)
	require.NoError(t, intentErr, "the intent must survive a failed rollback")
	assert.Equal(t, PhaseMapPending, rec.Phase)
}

// A rollback must never unmap on a device path alone. If the device no longer
// verifies as the expected image, something else owns it now.
func TestFault_RollbackRefusesUnmapOnIdentityMismatch(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	createFakeDevice(t, &dev)

	require.NoError(t, os.MkdirAll(filepath.Dir(f.staging), 0o755))
	require.NoError(t, os.WriteFile(f.staging, []byte("not a directory"), 0o600))

	f.creds.On("Load", mock.Anything, "cinder").Return(NewTestCredential("cinder", redactedKey), nil)
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil).Once()
	f.mapper.On("Map", mock.Anything, mock.Anything).Return(dev, nil).Once()
	f.mapper.On("DeviceSize", mock.Anything, dev.DevicePath).Return(int64(1073741824), nil)
	f.mapper.On("LockHolders", mock.Anything, mock.Anything).Return([]string{"client.1@ip"}, nil)
	// Verification passes for the identity gate, then fails at rollback time.
	f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).Return(nil).Once()
	f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).
		Return(assert.AnError).Once()

	_, err := f.ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
	f.mapper.AssertNotCalled(t, "Unmap")

	rec, intentErr := f.ns.Staging.ReadIndex(testVolumeID)
	require.NoError(t, intentErr, "the intent must survive so the mapping stays attributable")
	assert.Equal(t, PhaseMapPending, rec.Phase)
}

// §9.2 row 5: plugin restart with a live map.
//
// The kernel owns the mapping, so a restart must reuse it. This drives the
// reconciler and then a stage, as the real sequence does.
func TestFault_PluginRestartWithLiveMap_ReusesMapping(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	createFakeDevice(t, &dev)

	// Pre-restart state: a persisted record and a live mapping.
	rec := newStagingRecord(testVolumeID, testAttachmentID, labConnectionInfo(), dev, 1, 1073741824)
	require.NoError(t, f.ns.Staging.Write(f.staging, rec))

	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{dev}, nil)
	f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).Return(nil)

	// Startup reconciliation adopts it.
	r := newReconciler(f.mapper, f.ns.Staging, f.ns.Isolation)
	result, err := r.Reconcile(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, result.Adopted)

	// A subsequent stage is a no-op.
	f.creds.On("Load", mock.Anything, "cinder").Return(NewTestCredential("cinder", redactedKey), nil)
	f.mapper.On("DeviceSize", mock.Anything, dev.DevicePath).Return(int64(1073741824), nil)

	_, err = f.ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})
	require.NoError(t, err)

	f.mapper.AssertNotCalled(t, "Map")
	f.mapper.AssertNotCalled(t, "Unmap")
}

// §9.2 row 7: exclusive map denied.
//
// No fallback path exists, so no second invocation may occur.
func TestFault_ExclusiveMapDenied_NoFallback(t *testing.T) {
	f := newNodeFixture(t)

	f.creds.On("Load", mock.Anything, "cinder").Return(NewTestCredential("cinder", redactedKey), nil)
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil)
	f.mapper.On("Map", mock.Anything, mock.Anything).
		Return(MappedDevice{}, ErrExclusiveLockDenied).Once()
	f.mapper.On("LockHolders", mock.Anything, mock.Anything).
		Return([]string{"client.999@10.9.9.9"}, nil)

	_, err := f.ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))

	f.mapper.AssertNumberOfCalls(t, "Map", 1)
	// Key material must not be left behind after a failed attempt.
	assert.NoDirExists(t, volumeRuntimeDir(f.opts, testVolumeID))
}

// §9.2 row 8: conflicting Cinder attachment records.
//
// Nothing may be mutated; the operator must resolve it.
func TestFault_ConflictingAttachmentRecords_NothingMutated(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := t.Context()

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
	cloud.AssertNotCalled(t, "SetVolumeMetadata")
}

// §9.2 row 9: unmap timeout.
//
// Staging state must survive so the retry can find the mapping again.
func TestFault_UnmapTimeout_KeepsStateForRetry(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	f.stageForUnstage(t, dev)

	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{dev}, nil)
	f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).Return(nil)
	f.mapper.On("Flush", mock.Anything, dev.DevicePath).Return(nil)
	f.mapper.On("Unmap", mock.Anything, dev.DevicePath, mock.Anything).Return(ErrDeviceBusy).Once()

	_, err := f.ns.NodeUnstageVolume(t.Context(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
	})
	assert.Equal(t, codes.Aborted, codeOf(t, err))

	// State survived, and the key material is still present because the volume
	// is still mapped.
	rec, readErr := f.ns.Staging.Read(f.staging)
	require.NoError(t, readErr)
	assert.Equal(t, testVolumeID, rec.VolumeID)

	// Retry succeeds and cleans up.
	f.mapper.On("Unmap", mock.Anything, dev.DevicePath, mock.Anything).Return(nil).Once()
	f.mapper.ExpectedCalls = filterCalls(f.mapper.ExpectedCalls, "ListMapped")
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{dev}, nil).Once()
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil).Once()

	_, err = f.ns.NodeUnstageVolume(t.Context(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
	})
	require.NoError(t, err)
	_, readErr = f.ns.Staging.Read(f.staging)
	assert.Error(t, readErr, "state must be cleaned up after a successful unmap")
}

// filterCalls removes expectations for one method so a test can re-script it.
func filterCalls(calls []*mock.Call, method string) []*mock.Call {
	out := make([]*mock.Call, 0, len(calls))
	for _, c := range calls {
		if c.Method != method {
			out = append(out, c)
		}
	}
	return out
}

// §9.2 row 10: Cinder API unavailable during unstage.
//
// The node unmaps regardless; the controller retries its own step. This asserts
// the split is real by unstaging with no Cinder interaction at all.
func TestFault_CinderUnavailableDuringUnstage_NodeStillUnmaps(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	f.stageForUnstage(t, dev)

	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{dev}, nil).Once()
	f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).Return(nil).Once()
	f.mapper.On("Flush", mock.Anything, dev.DevicePath).Return(nil)
	f.mapper.On("Unmap", mock.Anything, dev.DevicePath, mock.Anything).Return(nil)
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil).Once()

	_, err := f.ns.NodeUnstageVolume(t.Context(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
	})
	require.NoError(t, err, "the node path must not depend on Cinder")
}

// §9.2 row 10 controller half: unpublish is retried until Cinder recovers, and
// the volume is not left half-detached.
func TestFault_CinderUnavailableDuringUnpublish_Retryable(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := t.Context()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID: testVolumeID, Metadata: map[string]string{attachmentMetaKey: testAttachmentID},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("DeleteAttachment", ctx, testAttachmentID).Return(assert.AnError).Once()

	_, err := cs.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID,
	})
	assert.Equal(t, codes.Internal, codeOf(t, err), "a retryable code so the attacher retries")

	// Metadata must not have been cleared while the record still exists,
	// otherwise the next publish would create a duplicate.
	cloud.AssertNotCalled(t, "DeleteVolumeMetadata")

	// Retry succeeds.
	cloud.On("DeleteAttachment", ctx, testAttachmentID).Return(nil).Once()
	cloud.On("DeleteVolumeMetadata", ctx, testVolumeID, []string{attachmentMetaKey}).Return(nil)
	cloud.On("WaitVolumeTargetStatus", ctx, testVolumeID, mock.Anything, mock.Anything).Return(nil)

	_, err = cs.ControllerUnpublishVolume(ctx, &csi.ControllerUnpublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID,
	})
	require.NoError(t, err)
}

// §9.2 row 11: credential rotated mid-flight.
//
// The projected Secret is re-read on every stage, so a rotated key is picked up
// without restarting the pod.
func TestFault_CredentialRotatedBetweenStages_PickedUpWithoutRestart(t *testing.T) {
	dir := writeCredentialDir(t, "cinder", "AQOLDKEY==")
	p := NewFileCredentialProvider(dir)

	first, err := p.Load(t.Context(), "cinder")
	require.NoError(t, err)
	assert.Equal(t, "cinder", first.UserID)

	// kubelet refreshes a projected Secret by replacing the file atomically,
	// not by writing through it — the file is mode 0400.
	keyPath := filepath.Join(dir, credentialKeyFile)
	require.NoError(t, os.Remove(keyPath))
	require.NoError(t, os.WriteFile(keyPath, []byte("AQNEWKEY=="), 0o400))

	second, err := p.Load(t.Context(), "cinder")
	require.NoError(t, err)
	assert.Equal(t, "cinder", second.UserID)

	// The keyring rendered from each credential must differ, proving the new key
	// is actually used rather than a cached one.
	assert.NotEqual(t, string(buildKeyring(first)), string(buildKeyring(second)))
}

// §9.2 row 11 variant: a rotated Secret carrying the wrong entity must fail with
// a clear message rather than an opaque Ceph auth error.
func TestFault_RotatedCredentialWrongEntity_FailsClearly(t *testing.T) {
	f := newNodeFixture(t)
	f.creds.On("Load", mock.Anything, "cinder").
		Return((*CephCredential)(nil), ErrCredentialEntityMismatch)

	_, err := f.ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
	f.mapper.AssertNotCalled(t, "Map")
}

// §9.2 row 12: conflicting live map at unstage.
//
// Never unmap on the strength of a recorded device number.
func TestFault_ConflictingLiveMapAtUnstage_NeverUnmaps(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	f.stageForUnstage(t, dev)

	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{dev}, nil)
	f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).
		Return(ErrIdentityMismatch)

	_, err := f.ns.NodeUnstageVolume(t.Context(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
	})
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
	f.mapper.AssertNotCalled(t, "Unmap")
}

// §9.2 row 3: attachment update succeeded, RPC response lost.
//
// Recovery variant: the record is still present and already carries usable
// connection information. It must be reused as-is. Updating again would be at
// best wasteful and at worst rejected, because Cinder can refuse an update on a
// record it has already advanced to attached/in-use — which would strand a volume
// that is in fact correctly attached.
func TestFault_AttachmentUpdateResponseLost_StoredConnectionIsReused(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := t.Context()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID: testVolumeID, Metadata: map[string]string{attachmentMetaKey: testAttachmentID},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetRBDOpts").Return(defaultRBDOpts(t))
	cloud.On("GetAttachment", ctx, testAttachmentID).Return(&openstack.Attachment{
		ID:             testAttachmentID,
		VolumeID:       testVolumeID,
		Status:         "attached",
		ConnectionInfo: labConnectionInfo(),
	}, nil).Once()
	// Completion stays best-effort and idempotent.
	cloud.On("CompleteAttachment", ctx, testAttachmentID).Return(nil).Maybe()

	resp, err := cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID, VolumeCapability: blockCapability(),
	})
	require.NoError(t, err)
	assert.Equal(t, testAttachmentID, resp.PublishContext[PublishContextAttachmentID])

	cloud.AssertNotCalled(t, "UpdateAttachmentConnector")
	cloud.AssertNotCalled(t, "CreateAttachment")
}

// §9.2 row 3, stale variant: the record is gone rather than merely un-echoed.
//
// The 404 on update is what distinguishes the two. Exactly one replacement is
// created and the update retried once; retrying indefinitely would mask a backend
// deleting records as fast as they are created.
func TestFault_AttachmentUpdateResponseLost_OneReplacementOnly(t *testing.T) {
	cs, cloud := newTestControllerServer(t)
	ctx := t.Context()

	cloud.On("GetVolume", ctx, testVolumeID).Return(&volumes.Volume{
		ID: testVolumeID, Metadata: map[string]string{attachmentMetaKey: "att-stale"},
	}, nil)
	cloud.On("GetVolumeOpts").Return(defaultVolumeOpts(t))
	cloud.On("GetRBDOpts").Return(defaultRBDOpts(t))
	// The reuse probe finds nothing usable, so the update path runs.
	cloud.On("GetAttachment", ctx, "att-stale").
		Return((*openstack.Attachment)(nil), cpoerrors.ErrNotFound).Maybe()
	cloud.On("GetAttachment", ctx, testAttachmentID).
		Return(&openstack.Attachment{Status: "reserved"}, nil).Maybe()
	cloud.On("UpdateAttachmentConnector", ctx, "att-stale", mock.Anything).
		Return((*openstack.RBDConnectionInfo)(nil), cpoerrors.ErrNotFound).Once()
	cloud.On("CreateAttachment", ctx, testVolumeID).Return(testAttachmentID, nil).Once()
	cloud.On("SetVolumeMetadata", ctx, testVolumeID, mock.Anything).Return(nil)
	cloud.On("UpdateAttachmentConnector", ctx, testAttachmentID, mock.Anything).
		Return(labConnectionInfo(), nil).Once()
	cloud.On("CompleteAttachment", ctx, testAttachmentID).Return(nil)

	_, err := cs.ControllerPublishVolume(ctx, &csi.ControllerPublishVolumeRequest{
		VolumeId: testVolumeID, NodeId: testNodeID, VolumeCapability: blockCapability(),
	})
	require.NoError(t, err)

	cloud.AssertNumberOfCalls(t, "CreateAttachment", 1)
	cloud.AssertNumberOfCalls(t, "UpdateAttachmentConnector", 2)
}

// §9.2 row 6: node crash. Records survive, mappings do not.
//
// Reconciliation must discard every record and leave nothing isolated, so the
// node comes back clean.
func TestFault_NodeCrash_ReconciliationLeavesCleanState(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	f.stageForUnstage(t, dev)

	// Post-crash: the kernel has nothing.
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil)

	r := newReconciler(f.mapper, f.ns.Staging, f.ns.Isolation)
	result, err := r.Reconcile(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 1, result.Unstaged)
	assert.Zero(t, result.Isolated)
	assert.Zero(t, f.ns.Isolation.Len())
	f.mapper.AssertNotCalled(t, "Unmap")

	remaining, err := f.ns.Staging.ListIndexed()
	require.NoError(t, err)
	assert.Empty(t, remaining, "no orphaned records may survive a crash")
}

// Repeated publish/unpublish cycles, as a multi-phase CDI migration performs.
// Each cycle must map and unmap exactly once and leave no residue.
func TestFault_RepeatedStageUnstageCycles_NoResidue(t *testing.T) {
	const cycles = 3

	for i := 0; i < cycles; i++ {
		f := newNodeFixture(t)
		dev := mappedDevice(5)
		createFakeDevice(t, &dev)

		f.creds.On("Load", mock.Anything, "cinder").
			Return(NewTestCredential("cinder", redactedKey), nil)
		f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil).Once()
		f.mapper.On("Map", mock.Anything, mock.Anything).Return(dev, nil).Once()
		f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).Return(nil)
		f.mapper.On("DeviceSize", mock.Anything, dev.DevicePath).Return(int64(1073741824), nil)
		f.mapper.On("LockHolders", mock.Anything, mock.Anything).Return([]string{"c@1"}, nil)

		_, err := f.ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
			VolumeId:          testVolumeID,
			StagingTargetPath: f.staging,
			VolumeCapability:  blockCapability(),
			PublishContext:    stagePublishContext(),
		})
		require.NoError(t, err, "cycle %d stage", i)

		f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{dev}, nil).Once()
		f.mapper.On("Flush", mock.Anything, dev.DevicePath).Return(nil)
		f.mapper.On("Unmap", mock.Anything, dev.DevicePath, mock.Anything).Return(nil).Once()
		f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil).Once()

		_, err = f.ns.NodeUnstageVolume(t.Context(), &csi.NodeUnstageVolumeRequest{
			VolumeId:          testVolumeID,
			StagingTargetPath: f.staging,
		})
		require.NoError(t, err, "cycle %d unstage", i)

		f.mapper.AssertNumberOfCalls(t, "Map", 1)
		f.mapper.AssertNumberOfCalls(t, "Unmap", 1)
		assert.NoDirExists(t, volumeRuntimeDir(f.opts, testVolumeID), "cycle %d left key material", i)

		remaining, err := f.ns.Staging.ListIndexed()
		require.NoError(t, err)
		assert.Empty(t, remaining, "cycle %d left a staging record", i)
	}
}

// The map generation increments across cycles on the same node, which is what
// makes "the mapping I recorded" distinguishable from a later one in logs.
func TestFault_MapGenerationIncrementsAcrossCycles(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	createFakeDevice(t, &dev)
	ci := labConnectionInfo()

	for want := 1; want <= 3; want++ {
		rec := newStagingRecord(testVolumeID, testAttachmentID, ci, dev,
			f.ns.Staging.NextGeneration(testVolumeID), 0)
		require.NoError(t, f.ns.Staging.Write(f.staging, rec))
		assert.Equal(t, want, rec.MapGeneration)
	}
}
