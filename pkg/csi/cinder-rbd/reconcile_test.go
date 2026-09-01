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
	"errors"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-rbd/openstack"
)

// reconcileFixture wires a reconciler over a temporary staging store.
type reconcileFixture struct {
	rec       *reconciler
	mapper    *RBDMapperMock
	staging   *stagingStore
	isolation *isolationSet
	opts      openstack.RBDOpts
	base      string
}

func newReconcileFixture(t *testing.T) *reconcileFixture {
	t.Helper()

	base := t.TempDir()
	var opts openstack.RBDOpts
	require.NoError(t, opts.ApplyDefaults())
	opts.RuntimeDir = filepath.Join(base, "run")
	opts.StateDir = filepath.Join(base, "state")

	store := newStagingStore(opts)
	mapper := &RBDMapperMock{}
	isolation := newIsolationSet()

	return &reconcileFixture{
		rec:       newReconciler(mapper, store, isolation),
		mapper:    mapper,
		staging:   store,
		isolation: isolation,
		opts:      opts,
		base:      base,
	}
}

// indexRecord persists a record into the node index only, which is what startup
// reconciliation reads.
func (f *reconcileFixture) indexRecord(t *testing.T, volumeID, pool, image, devicePath string, deviceID int) {
	t.Helper()
	ci := labConnectionInfo()
	ci.Pool = pool
	ci.Image = image
	dev := MappedDevice{ID: deviceID, Pool: pool, Image: image, DevicePath: devicePath, ClusterFSID: testFSID}
	rec := newStagingRecord(volumeID, testAttachmentID, ci, dev, 1, 1073741824)
	require.NoError(t, f.staging.WriteIndexOnly(rec))
}

func liveDevice(id int, pool, image string) MappedDevice {
	return MappedDevice{
		ID: id, Pool: pool, Image: image,
		DevicePath: devicePathFromID(id), ClusterFSID: testFSID,
	}
}

// ── Adoption ─────────────────────────────────────────────────────────────────

// A plugin restart with a live mapping must reuse the same kernel mapping, not
// remap: the mapping is kernel-owned and survives the userspace process.
func TestReconcile_AdoptsMatchingLiveMapping(t *testing.T) {
	f := newReconcileFixture(t)
	f.indexRecord(t, "vol-a", "cinder-volumes", "img-a", "/dev/rbd5", 5)

	f.mapper.On("ListMapped", mock.Anything).
		Return([]MappedDevice{liveDevice(5, "cinder-volumes", "img-a")}, nil)
	f.mapper.On("VerifyIdentity", mock.Anything, "/dev/rbd5", mock.Anything).Return(nil)

	result, err := f.rec.Reconcile(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 1, result.Adopted)
	assert.Zero(t, result.Unstaged)
	assert.Zero(t, result.Isolated)
	f.mapper.AssertNotCalled(t, "Unmap")
	f.mapper.AssertNotCalled(t, "Map")
}

// Kernel device numbers are reused across a reboot, so an adopted record must be
// refreshed with the current path rather than left pointing at the old one.
func TestReconcile_RefreshesChangedDevicePath(t *testing.T) {
	f := newReconcileFixture(t)
	f.indexRecord(t, "vol-a", "cinder-volumes", "img-a", "/dev/rbd5", 5)

	// After a reboot the same image came back as /dev/rbd2.
	f.mapper.On("ListMapped", mock.Anything).
		Return([]MappedDevice{liveDevice(2, "cinder-volumes", "img-a")}, nil)
	f.mapper.On("VerifyIdentity", mock.Anything, "/dev/rbd2", mock.Anything).Return(nil)

	result, err := f.rec.Reconcile(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, result.Adopted)

	updated, err := f.staging.ReadIndex("vol-a")
	require.NoError(t, err)
	assert.Equal(t, "/dev/rbd2", updated.DevicePath, "the record must track the current device path")
	assert.Equal(t, 2, updated.DeviceID)

	// The generation is preserved: this is the same mapping, not a new one.
	assert.Equal(t, 1, updated.MapGeneration)
}

// Location is by pool/image, never by the recorded device number: /dev/rbd5 may
// now be a different image entirely.
func TestReconcile_LocatesByPoolImageNotDeviceNumber(t *testing.T) {
	f := newReconcileFixture(t)
	f.indexRecord(t, "vol-a", "cinder-volumes", "img-a", "/dev/rbd5", 5)

	// Device 5 is now someone else's image; ours is on device 9.
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{
		liveDevice(5, "kube-rbd", "csi-vol-other"),
		liveDevice(9, "cinder-volumes", "img-a"),
	}, nil)
	f.mapper.On("VerifyIdentity", mock.Anything, "/dev/rbd9", mock.Anything).Return(nil)

	result, err := f.rec.Reconcile(t.Context())
	require.NoError(t, err)
	assert.Equal(t, 1, result.Adopted)

	// Device 5 must never have been verified against our identity, let alone
	// unmapped.
	f.mapper.AssertNotCalled(t, "VerifyIdentity", mock.Anything, "/dev/rbd5", mock.Anything)
	f.mapper.AssertNotCalled(t, "Unmap")
}

// ── Unstaging ────────────────────────────────────────────────────────────────

// A record with no live mapping is stale: discard it so a later stage maps
// afresh, rather than leaving a record that would authorize an unmap.
func TestReconcile_DiscardsRecordWithNoLiveMapping(t *testing.T) {
	f := newReconcileFixture(t)
	f.indexRecord(t, "vol-a", "cinder-volumes", "img-a", "/dev/rbd5", 5)

	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil)

	result, err := f.rec.Reconcile(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 1, result.Unstaged)
	assert.Zero(t, result.Adopted)

	_, readErr := f.staging.ReadIndex("vol-a")
	assert.Error(t, readErr, "a stale record must be removed")
}

// A record that cannot describe an identity cannot authorize anything, so it is
// discarded rather than used.
func TestReconcile_DiscardsIncompleteRecord(t *testing.T) {
	f := newReconcileFixture(t)

	// Hand-build a record missing its FSID.
	rec := &StagingRecord{
		Schema: stagingRecordSchema, VolumeID: "vol-broken",
		Pool: "cinder-volumes", Image: "img-a", DevicePath: "/dev/rbd5",
	}
	require.NoError(t, f.staging.WriteIndexOnly(rec))

	f.mapper.On("ListMapped", mock.Anything).
		Return([]MappedDevice{liveDevice(5, "cinder-volumes", "img-a")}, nil)

	result, err := f.rec.Reconcile(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 1, result.Unstaged)
	f.mapper.AssertNotCalled(t, "Unmap")
	_, readErr := f.staging.ReadIndex("vol-broken")
	assert.Error(t, readErr)
}

// ── Isolation ────────────────────────────────────────────────────────────────

// The central safety property: a mapping that occupies the recorded pool/image
// but fails verification is neither adopted nor unmapped. Unmapping it could
// fault an unrelated workload.
func TestReconcile_IsolatesMismatchWithoutUnmapping(t *testing.T) {
	f := newReconcileFixture(t)
	f.indexRecord(t, "vol-a", "cinder-volumes", "img-a", "/dev/rbd5", 5)

	f.mapper.On("ListMapped", mock.Anything).
		Return([]MappedDevice{liveDevice(5, "cinder-volumes", "img-a")}, nil)
	f.mapper.On("VerifyIdentity", mock.Anything, "/dev/rbd5", mock.Anything).
		Return(ErrIdentityMismatch)

	result, err := f.rec.Reconcile(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 1, result.Isolated)
	assert.Zero(t, result.Adopted)
	f.mapper.AssertNotCalled(t, "Unmap")

	// The record is retained so an operator can inspect it.
	_, readErr := f.staging.ReadIndex("vol-a")
	assert.NoError(t, readErr)

	// And the volume is recorded as isolated so staging refuses it fast.
	entry, isolated := f.isolation.Get("vol-a")
	require.True(t, isolated)
	assert.Equal(t, ActionIsolated, entry.Action)
	assert.Contains(t, entry.Detail, "not adopted and not unmapped")

	assert.Equal(t, 1, f.isolation.Len())
}

// An isolated volume must be refused up front on the next stage, rather than
// re-discovering the same conflict and producing an identical failure.
func TestNodeStageVolume_RefusesIsolatedVolume(t *testing.T) {
	f := newNodeFixture(t)
	f.ns.Isolation.Add(ReconcileEntry{
		Action:   ActionIsolated,
		VolumeID: testVolumeID,
		Identity: testIdentity(),
		Detail:   "device /dev/rbd5 failed identity verification",
	})

	f.creds.On("Load", mock.Anything, "cinder").Return(NewTestCredential("cinder", redactedKey), nil)

	_, err := f.ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
	assert.Contains(t, err.Error(), "isolated")

	f.mapper.AssertNotCalled(t, "Map")
	f.mapper.AssertNotCalled(t, "Unmap")
}

// Once an operator resolves the conflict the volume can be served again.
func TestIsolationSet_ClearAllowsStagingAgain(t *testing.T) {
	f := newNodeFixture(t)
	f.ns.Isolation.Add(ReconcileEntry{VolumeID: testVolumeID, Action: ActionIsolated})
	require.Equal(t, 1, f.ns.Isolation.Len())

	f.ns.Isolation.Clear(testVolumeID)
	assert.Zero(t, f.ns.Isolation.Len())

	_, isolated := f.ns.Isolation.Get(testVolumeID)
	assert.False(t, isolated)
}

// ── Foreign and unrecognized mappings ────────────────────────────────────────

// Platform Ceph-CSI shares this kernel path. Its mappings live in a different
// pool and must be ignored entirely, not reported as anomalies on every restart.
func TestReconcile_IgnoresForeignPoolMappings(t *testing.T) {
	f := newReconcileFixture(t)
	f.indexRecord(t, "vol-a", "cinder-volumes", "img-a", "/dev/rbd5", 5)

	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{
		liveDevice(0, "kube-rbd", "csi-vol-746d4c96"),
		liveDevice(1, "kube-rbd", "csi-vol-5344958c"),
		liveDevice(5, "cinder-volumes", "img-a"),
	}, nil)
	f.mapper.On("VerifyIdentity", mock.Anything, "/dev/rbd5", mock.Anything).Return(nil)

	result, err := f.rec.Reconcile(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 1, result.Adopted)
	assert.Zero(t, result.Reported, "platform Ceph-CSI mappings must not be reported")
	f.mapper.AssertNotCalled(t, "Unmap")
}

// An unrecorded mapping in a driver-owned pool is reported but never adopted:
// a node-only process cannot prove ownership without a Cinder client.
func TestReconcile_ReportsUnrecordedMappingInOwnedPoolWithoutAdopting(t *testing.T) {
	f := newReconcileFixture(t)
	f.indexRecord(t, "vol-a", "cinder-volumes", "img-a", "/dev/rbd5", 5)

	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{
		liveDevice(5, "cinder-volumes", "img-a"),
		liveDevice(7, "cinder-volumes", "img-orphan"),
	}, nil)
	f.mapper.On("VerifyIdentity", mock.Anything, "/dev/rbd5", mock.Anything).Return(nil)

	result, err := f.rec.Reconcile(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 1, result.Adopted)
	assert.Equal(t, 1, result.Reported)
	f.mapper.AssertNotCalled(t, "Unmap")

	var reported *ReconcileEntry
	for i := range result.Entries {
		if result.Entries[i].Action == ActionReported {
			reported = &result.Entries[i]
		}
	}
	require.NotNil(t, reported)
	assert.Equal(t, "/dev/rbd7", reported.DevicePath)
	assert.Contains(t, reported.Detail, "left untouched")
}

// ── Failure handling ─────────────────────────────────────────────────────────

// Without the kernel inventory nothing can be decided safely, so the pass fails
// rather than acting on partial information.
func TestReconcile_ListFailureIsAnError(t *testing.T) {
	f := newReconcileFixture(t)
	f.indexRecord(t, "vol-a", "cinder-volumes", "img-a", "/dev/rbd5", 5)

	f.mapper.On("ListMapped", mock.Anything).
		Return([]MappedDevice(nil), errors.New("rbd not available"))

	_, err := f.rec.Reconcile(t.Context())
	require.Error(t, err)
	f.mapper.AssertNotCalled(t, "Unmap")

	// The record survives: a failed pass must not destroy state.
	_, readErr := f.staging.ReadIndex("vol-a")
	assert.NoError(t, readErr)
}

func TestReconcile_EmptyStateIsClean(t *testing.T) {
	f := newReconcileFixture(t)
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil)

	result, err := f.rec.Reconcile(t.Context())
	require.NoError(t, err)

	assert.Zero(t, result.Adopted)
	assert.Zero(t, result.Unstaged)
	assert.Zero(t, result.Isolated)
	assert.Zero(t, result.Reported)
}

// A node reboot: records survive on disk, mappings do not. Everything must be
// discarded so a later stage maps afresh, leaving no orphans behind.
func TestReconcile_NodeRebootDiscardsAllRecords(t *testing.T) {
	f := newReconcileFixture(t)
	f.indexRecord(t, "vol-a", "cinder-volumes", "img-a", "/dev/rbd5", 5)
	f.indexRecord(t, "vol-b", "cinder-volumes", "img-b", "/dev/rbd6", 6)

	// Post-reboot the kernel has no mappings.
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil)

	result, err := f.rec.Reconcile(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 2, result.Unstaged)
	assert.Zero(t, result.Isolated, "a reboot must not leave isolated mappings")

	remaining, err := f.staging.ListIndexed()
	require.NoError(t, err)
	assert.Empty(t, remaining, "no orphaned records may remain after a reboot")
}

// Mixed state in one pass, verifying the counters and that no unmap ever occurs.
func TestReconcile_MixedState(t *testing.T) {
	f := newReconcileFixture(t)
	f.indexRecord(t, "vol-adopt", "cinder-volumes", "img-adopt", "/dev/rbd1", 1)
	f.indexRecord(t, "vol-gone", "cinder-volumes", "img-gone", "/dev/rbd2", 2)
	f.indexRecord(t, "vol-bad", "cinder-volumes", "img-bad", "/dev/rbd3", 3)

	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{
		liveDevice(1, "cinder-volumes", "img-adopt"),
		liveDevice(3, "cinder-volumes", "img-bad"),
		liveDevice(8, "cinder-volumes", "img-unknown"),
		liveDevice(0, "kube-rbd", "csi-vol-foreign"),
	}, nil)
	f.mapper.On("VerifyIdentity", mock.Anything, "/dev/rbd1", mock.Anything).Return(nil)
	f.mapper.On("VerifyIdentity", mock.Anything, "/dev/rbd3", mock.Anything).
		Return(ErrIdentityMismatch)

	result, err := f.rec.Reconcile(t.Context())
	require.NoError(t, err)

	assert.Equal(t, 1, result.Adopted, "img-adopt")
	assert.Equal(t, 1, result.Unstaged, "img-gone")
	assert.Equal(t, 1, result.Isolated, "img-bad")
	assert.Equal(t, 1, result.Reported, "img-unknown; the kube-rbd mapping is ignored")

	// The invariant that matters most.
	f.mapper.AssertNotCalled(t, "Unmap")
	f.mapper.AssertNotCalled(t, "Map")
}

// Reconciliation must be safe to run repeatedly: the node plugin may restart
// several times in a row.
func TestReconcile_IsIdempotent(t *testing.T) {
	f := newReconcileFixture(t)
	f.indexRecord(t, "vol-a", "cinder-volumes", "img-a", "/dev/rbd5", 5)

	f.mapper.On("ListMapped", mock.Anything).
		Return([]MappedDevice{liveDevice(5, "cinder-volumes", "img-a")}, nil)
	f.mapper.On("VerifyIdentity", mock.Anything, "/dev/rbd5", mock.Anything).Return(nil)

	for i := 0; i < 3; i++ {
		result, err := f.rec.Reconcile(t.Context())
		require.NoError(t, err)
		assert.Equal(t, 1, result.Adopted, "pass %d", i+1)
		assert.Zero(t, result.Isolated, "pass %d", i+1)
	}

	_, err := f.staging.ReadIndex("vol-a")
	assert.NoError(t, err)
}

func TestReconcileResult_IsolatedIdentities(t *testing.T) {
	r := &ReconcileResult{
		Isolated: 2,
		Entries: []ReconcileEntry{
			{Action: ActionAdopted, Identity: ImageIdentity{Pool: "p", Image: "ok"}},
			{Action: ActionIsolated, Identity: ImageIdentity{Pool: "p", Image: "bad1"}},
			{Action: ActionIsolated, Identity: ImageIdentity{Pool: "p", Image: "bad2"}},
		},
	}

	ids := r.IsolatedIdentities()
	require.Len(t, ids, 2)
	assert.Equal(t, "bad1", ids[0].Image)
	assert.Equal(t, "bad2", ids[1].Image)
}
