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
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-rbd/openstack"
	mountutil "k8s.io/cloud-provider-openstack/pkg/util/mount"
)

// nodeFixture bundles a node server with its mocks and temporary directories.
type nodeFixture struct {
	ns      *nodeServer
	mapper  *RBDMapperMock
	creds   *CephCredentialProviderMock
	mounter *mountutil.MountMock
	opts    openstack.RBDOpts
	staging string
}

// newNodeFixture builds a node server whose runtime, state and staging paths all
// live under t.TempDir(). No test may write to the real /run, /var/lib or /dev.
func newNodeFixture(t *testing.T) *nodeFixture {
	t.Helper()

	base := t.TempDir()
	var opts openstack.RBDOpts
	require.NoError(t, opts.ApplyDefaults())
	opts.RuntimeDir = filepath.Join(base, "run")
	opts.StateDir = filepath.Join(base, "state")
	opts.ExpectedFSID = testFSID

	var vopts openstack.VolumeOpts
	require.NoError(t, vopts.ApplyDefaults())

	f := &nodeFixture{
		mapper:  &RBDMapperMock{},
		creds:   &CephCredentialProviderMock{},
		mounter: &mountutil.MountMock{},
		opts:    opts,
		staging: filepath.Join(base, "staging"),
	}
	f.ns = &nodeServer{
		Driver:      fakeDriver(),
		Opts:        opts,
		VolumeOpts:  vopts,
		Mapper:      f.mapper,
		Credentials: f.creds,
		Mounter:     f.mounter,
		Staging:     newStagingStore(opts),
		Isolation:   newIsolationSet(),
	}
	return f
}

// stagePublishContext returns a publish context matching the lab payload.
func stagePublishContext() map[string]string {
	return BuildPublishContext(labConnectionInfo(), testAttachmentID)
}

func mappedDevice(id int) MappedDevice {
	return MappedDevice{
		ID:          id,
		Pool:        "cinder-volumes",
		Image:       testVolumeID,
		DevicePath:  devicePathFromID(id),
		ClusterFSID: testFSID,
	}
}

// expectSuccessfulStage wires the mocks for a clean map.
func (f *nodeFixture) expectSuccessfulStage(t *testing.T, dev MappedDevice) {
	t.Helper()
	f.creds.On("Load", mock.Anything, "cinder").Return(NewTestCredential("cinder", redactedKey), nil)
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil).Once()
	f.mapper.On("Map", mock.Anything, mock.AnythingOfType("MapRequest")).Return(dev, nil)
	f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).Return(nil)
	f.mapper.On("DeviceSize", mock.Anything, dev.DevicePath).Return(int64(1073741824), nil)
	f.mapper.On("LockHolders", mock.Anything, mock.Anything).Return([]string{"client.1@10.0.0.1"}, nil)
}

// createFakeDevice makes the device path stat-able, since the identity gate
// waits for udev to create it.
func createFakeDevice(t *testing.T, dev *MappedDevice) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "rbd-device")
	require.NoError(t, os.WriteFile(p, []byte("fake block device"), 0o600))
	dev.DevicePath = p
}

// writeOwnershipIntent persists a map-pending intent, simulating an attempt that
// mapped and then died before recording completion.
func writeOwnershipIntent(t *testing.T, f *nodeFixture, volumeID string) *StagingRecord {
	t.Helper()
	rec := newMapIntentRecord(volumeID, testAttachmentID, labConnectionInfo(), 1)
	require.NoError(t, f.ns.Staging.WriteIntent(rec))
	return rec
}

// ── NodeGetInfo ──────────────────────────────────────────────────────────────

func TestNodeGetInfo_ReturnsBareHostname(t *testing.T) {
	f := newNodeFixture(t)
	f.ns.hostnameFunc = func() (string, error) { return "worker-0", nil }

	resp, err := f.ns.NodeGetInfo(t.Context(), &csi.NodeGetInfoRequest{})
	require.NoError(t, err)

	// No initiator identity and no IP: the connector needs only host.
	assert.Equal(t, "worker-0", resp.NodeId)
	assert.NotContains(t, resp.NodeId, ";")
	// Topology is a non-goal, so none is advertised.
	assert.Nil(t, resp.AccessibleTopology)
}

func TestNodeGetInfo_MaxVolumesFromConfig(t *testing.T) {
	f := newNodeFixture(t)
	f.ns.Opts.MaxVolumesPerNode = 32
	f.ns.hostnameFunc = func() (string, error) { return "worker-0", nil }

	resp, err := f.ns.NodeGetInfo(t.Context(), &csi.NodeGetInfoRequest{})
	require.NoError(t, err)
	assert.Equal(t, int64(32), resp.MaxVolumesPerNode)
}

func TestNodeGetInfo_HostnameFailures(t *testing.T) {
	tests := []struct {
		name string
		fn   func() (string, error)
	}{
		{name: "error from hostname", fn: func() (string, error) { return "", errors.New("boom") }},
		{name: "empty hostname", fn: func() (string, error) { return "", nil }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newNodeFixture(t)
			f.ns.hostnameFunc = tt.fn

			_, err := f.ns.NodeGetInfo(t.Context(), &csi.NodeGetInfoRequest{})
			assert.Equal(t, codes.Internal, codeOf(t, err))
		})
	}
}

// ── NodeStageVolume ──────────────────────────────────────────────────────────

func TestNodeStageVolume_MapsExclusivelyAndRecordsState(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	createFakeDevice(t, &dev)
	f.expectSuccessfulStage(t, dev)

	_, err := f.ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})
	require.NoError(t, err)

	// The map must have been requested exclusively.
	call := f.mapper.Calls[1]
	req := call.Arguments.Get(1).(MapRequest)
	assert.True(t, req.Exclusive, "writable maps must always be exclusive")
	assert.Equal(t, "cinder", req.UserID)

	// The record must be persisted in both places.
	rec, err := f.ns.Staging.Read(f.staging)
	require.NoError(t, err)
	assert.Equal(t, testVolumeID, rec.VolumeID)
	assert.Equal(t, testFSID, rec.ClusterFSID)
	assert.Equal(t, "cinder-volumes", rec.Pool)
	assert.Equal(t, 1, rec.MapGeneration)

	indexed, err := f.ns.Staging.ReadIndex(testVolumeID)
	require.NoError(t, err)
	assert.Equal(t, rec.DevicePath, indexed.DevicePath)
}

// Idempotency requires BOTH an ownership intent and kernel identity. With a
// valid intent from the interrupted attempt, a retried stage reuses the mapping
// rather than attempting a second map, which the exclusive lock would deny.
func TestNodeStageVolume_ReusesOwnedMappingWithoutRemapping(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	createFakeDevice(t, &dev)
	writeOwnershipIntent(t, f, testVolumeID)

	f.creds.On("Load", mock.Anything, "cinder").Return(NewTestCredential("cinder", redactedKey), nil)
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{dev}, nil)
	f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).Return(nil)
	f.mapper.On("DeviceSize", mock.Anything, dev.DevicePath).Return(int64(1073741824), nil)

	_, err := f.ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})
	require.NoError(t, err)

	f.mapper.AssertNotCalled(t, "Map")

	// The reuse advances the intent to staged rather than leaving it pending.
	rec, readErr := f.ns.Staging.ReadIndex(testVolumeID)
	require.NoError(t, readErr)
	assert.Equal(t, PhaseStaged, rec.Phase)
}

// A live mapping with matching identity but NO ownership intent must be isolated.
//
// This is the platform Ceph-CSI hazard: identity proves what an image is, never
// who mapped it. Adopting on identity alone would let the driver hand a foreign
// device to a migration pod.
func TestNodeStageVolume_IsolatesMatchingMappingWithoutOwnershipIntent(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	createFakeDevice(t, &dev)

	f.creds.On("Load", mock.Anything, "cinder").Return(NewTestCredential("cinder", redactedKey), nil)
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{dev}, nil)

	_, err := f.ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
	assert.Contains(t, status.Convert(err).Message(), "ownership intent")

	f.mapper.AssertNotCalled(t, "Map")
	f.mapper.AssertNotCalled(t, "Unmap")
	// Isolation is registered so later attempts fail fast with the same reason.
	_, isolated := f.ns.Isolation.Get(testVolumeID)
	assert.True(t, isolated)
}

// An intent belonging to a different volume must not authorize this one.
func TestNodeStageVolume_IntentForAnotherVolumeDoesNotAuthorize(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	createFakeDevice(t, &dev)
	writeOwnershipIntent(t, f, "some-other-volume")

	f.creds.On("Load", mock.Anything, "cinder").Return(NewTestCredential("cinder", redactedKey), nil)
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{dev}, nil)

	_, err := f.ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
	f.mapper.AssertNotCalled(t, "Map")
	f.mapper.AssertNotCalled(t, "Unmap")
}

// The intent must be durable before the map call, not after it: a crash in
// between is the whole reason it exists.
func TestNodeStageVolume_WritesOwnershipIntentBeforeMapping(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	createFakeDevice(t, &dev)

	f.creds.On("Load", mock.Anything, "cinder").Return(NewTestCredential("cinder", redactedKey), nil)
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil).Once()
	f.mapper.On("Map", mock.Anything, mock.Anything).
		Run(func(mock.Arguments) {
			// Observed from inside Map: the intent is already on disk.
			rec, err := f.ns.Staging.ReadIndex(testVolumeID)
			require.NoError(t, err, "the ownership intent must exist before rbd device map")
			assert.Equal(t, PhaseMapPending, rec.Phase)
			assert.Equal(t, testVolumeID, rec.VolumeID)
			assert.Empty(t, rec.DevicePath, "the intent cannot know the device yet")
		}).
		Return(dev, nil).Once()
	f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).Return(nil)
	f.mapper.On("DeviceSize", mock.Anything, dev.DevicePath).Return(int64(1073741824), nil)
	f.mapper.On("LockHolders", mock.Anything, mock.Anything).Return([]string{"client.1@ip"}, nil)

	_, err := f.ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})
	require.NoError(t, err)

	rec, err := f.ns.Staging.ReadIndex(testVolumeID)
	require.NoError(t, err)
	assert.Equal(t, PhaseStaged, rec.Phase, "a completed stage advances the intent")
	assert.Equal(t, dev.DevicePath, rec.DevicePath)
}

// A map that fails without creating a mapping must not leave an intent behind.
func TestNodeStageVolume_FailedMapDiscardsOwnershipIntent(t *testing.T) {
	f := newNodeFixture(t)

	f.creds.On("Load", mock.Anything, "cinder").Return(NewTestCredential("cinder", redactedKey), nil)
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil)
	f.mapper.On("Map", mock.Anything, mock.Anything).
		Return(MappedDevice{}, assert.AnError).Once()

	_, err := f.ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})
	require.Error(t, err)

	_, readErr := f.ns.Staging.ReadIndex(testVolumeID)
	require.Error(t, readErr, "no mapping was created, so no ownership claim may remain")
}

// A device occupying the same pool/image that fails verification must be
// isolated: not adopted, and above all not unmapped, since it may belong to the
// platform Ceph-CSI which shares this kernel path.
func TestNodeStageVolume_IsolatesMismatchedMappingWithoutUnmapping(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)

	f.creds.On("Load", mock.Anything, "cinder").Return(NewTestCredential("cinder", redactedKey), nil)
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{dev}, nil)
	f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).
		Return(ErrIdentityMismatch)

	_, err := f.ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))

	f.mapper.AssertNotCalled(t, "Unmap")
	f.mapper.AssertNotCalled(t, "Map")
}

// The entity check must happen before any map, so a misconfigured Secret is
// reported as such instead of as an opaque Ceph auth failure.
func TestNodeStageVolume_CredentialEntityMismatchFailsBeforeMapping(t *testing.T) {
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

	f.mapper.AssertNotCalled(t, "ListMapped")
	f.mapper.AssertNotCalled(t, "Map")
}

// An exclusive-lock denial must never fall back to a non-exclusive map, and the
// error should name the holder so an operator can act.
func TestNodeStageVolume_ExclusiveLockDeniedNamesHolder(t *testing.T) {
	f := newNodeFixture(t)

	f.creds.On("Load", mock.Anything, "cinder").Return(NewTestCredential("cinder", redactedKey), nil)
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil)
	f.mapper.On("Map", mock.Anything, mock.Anything).
		Return(MappedDevice{}, ErrExclusiveLockDenied)
	f.mapper.On("LockHolders", mock.Anything, mock.Anything).
		Return([]string{"client.999@10.9.9.9"}, nil)

	_, err := f.ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
	assert.Contains(t, err.Error(), "client.999")
}

// A gate failure that is not itself an identity problem must roll back the
// mapping this call created, and must remove the generated key material.
func TestNodeStageVolume_IdentityGateFailureRollsBackMapping(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	createFakeDevice(t, &dev)

	f.creds.On("Load", mock.Anything, "cinder").Return(NewTestCredential("cinder", redactedKey), nil)
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil).Once()
	f.mapper.On("Map", mock.Anything, mock.Anything).Return(dev, nil)
	f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).Return(nil)
	// Gate check: a non-positive size fails the gate while identity still holds.
	f.mapper.On("DeviceSize", mock.Anything, dev.DevicePath).Return(int64(0), nil)
	f.mapper.On("Unmap", mock.Anything, dev.DevicePath, mock.Anything).Return(nil)
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil)

	_, err := f.ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})
	require.Error(t, err)

	f.mapper.AssertCalled(t, "Unmap", mock.Anything, dev.DevicePath, mock.Anything)
	// Key material must not outlive the failed attempt.
	assert.NoDirExists(t, volumeRuntimeDir(f.opts, testVolumeID))
	// Absence confirmed, so no ownership claim remains.
	_, readErr := f.ns.Staging.ReadIndex(testVolumeID)
	require.Error(t, readErr)
}

// When the gate fails *because* identity does not match, the device must not be
// unmapped: either the kernel handed back something unexpected or the device
// number was recycled, and in both cases the device is not ours to remove.
//
// The intent is kept instead, because reconciliation locates a mapping by
// pool/image — the only lookup that survives device renumbering.
func TestNodeStageVolume_IdentityMismatchLeavesDeviceAndRetainsIntent(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	createFakeDevice(t, &dev)

	f.creds.On("Load", mock.Anything, "cinder").Return(NewTestCredential("cinder", redactedKey), nil)
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil).Once()
	f.mapper.On("Map", mock.Anything, mock.Anything).Return(dev, nil)
	f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).
		Return(ErrIdentityMismatch)

	_, err := f.ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))

	f.mapper.AssertNotCalled(t, "Unmap")
	assert.NoDirExists(t, volumeRuntimeDir(f.opts, testVolumeID))

	rec, readErr := f.ns.Staging.ReadIndex(testVolumeID)
	require.NoError(t, readErr, "the intent must survive so the mapping stays attributable")
	assert.Equal(t, PhaseMapPending, rec.Phase)
}

func TestNodeStageVolume_RejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name string
		req  *csi.NodeStageVolumeRequest
	}{
		{name: "missing volume id", req: &csi.NodeStageVolumeRequest{
			StagingTargetPath: "/staging", VolumeCapability: blockCapability()}},
		{name: "missing staging path", req: &csi.NodeStageVolumeRequest{
			VolumeId: testVolumeID, VolumeCapability: blockCapability()}},
		{name: "missing capability", req: &csi.NodeStageVolumeRequest{
			VolumeId: testVolumeID, StagingTargetPath: "/staging"}},
		{name: "filesystem capability", req: &csi.NodeStageVolumeRequest{
			VolumeId: testVolumeID, StagingTargetPath: "/staging",
			VolumeCapability: mountCapability()}},
		{name: "multi node capability", req: &csi.NodeStageVolumeRequest{
			VolumeId: testVolumeID, StagingTargetPath: "/staging",
			VolumeCapability: multiNodeCapability()}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newNodeFixture(t)
			_, err := f.ns.NodeStageVolume(t.Context(), tt.req)
			assert.Equal(t, codes.InvalidArgument, codeOf(t, err))
		})
	}
}

func TestNodeStageVolume_RejectsBadPublishContext(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{name: "empty context", mutate: func(pc map[string]string) {
			for k := range pc {
				delete(pc, k)
			}
		}},
		{name: "missing monitors", mutate: func(pc map[string]string) {
			delete(pc, PublishContextMonitors)
		}},
		{name: "missing pool", mutate: func(pc map[string]string) {
			delete(pc, PublishContextPool)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newNodeFixture(t)
			pc := stagePublishContext()
			tt.mutate(pc)

			_, err := f.ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
				VolumeId:          testVolumeID,
				StagingTargetPath: f.staging,
				VolumeCapability:  blockCapability(),
				PublishContext:    pc,
			})
			require.Error(t, err)
			code := codeOf(t, err)
			assert.True(t, code == codes.InvalidArgument || code == codes.FailedPrecondition,
				"want InvalidArgument or FailedPrecondition, got %s", code)
		})
	}
}

// A publish context whose FSID disagrees with configuration means the backend is
// pointing at a different Ceph cluster.
func TestNodeStageVolume_FSIDMismatchIsRejected(t *testing.T) {
	f := newNodeFixture(t)
	f.ns.Opts.ExpectedFSID = "a-completely-different-fsid"

	_, err := f.ns.NodeStageVolume(t.Context(), &csi.NodeStageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
	f.mapper.AssertNotCalled(t, "Map")
}

// ── NodeUnstageVolume ────────────────────────────────────────────────────────

// stageForUnstage produces a persisted staging record without going through the
// full stage path.
func (f *nodeFixture) stageForUnstage(t *testing.T, dev MappedDevice) {
	t.Helper()
	rec := newStagingRecord(testVolumeID, testAttachmentID, labConnectionInfo(), dev, 1, 1073741824)
	require.NoError(t, f.ns.Staging.Write(f.staging, rec))
}

func TestNodeUnstageVolume_FlushesThenUnmaps(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	f.stageForUnstage(t, dev)

	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{dev}, nil).Once()
	f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).Return(nil).Once()
	f.mapper.On("Flush", mock.Anything, dev.DevicePath).Return(nil)
	f.mapper.On("Unmap", mock.Anything, dev.DevicePath, mock.Anything).Return(nil)
	// Confirmation pass: the device is gone.
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil).Once()

	_, err := f.ns.NodeUnstageVolume(t.Context(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
	})
	require.NoError(t, err)

	// Flush must precede unmap or the tail of a migration copy is lost.
	var flushIdx, unmapIdx = -1, -1
	for i, c := range f.mapper.Calls {
		switch c.Method {
		case "Flush":
			flushIdx = i
		case "Unmap":
			unmapIdx = i
		}
	}
	require.NotEqual(t, -1, flushIdx)
	require.NotEqual(t, -1, unmapIdx)
	assert.Less(t, flushIdx, unmapIdx, "flush must happen before unmap")

	// State and key material are gone.
	_, err = f.ns.Staging.Read(f.staging)
	assert.Error(t, err)
	assert.NoDirExists(t, volumeRuntimeDir(f.opts, testVolumeID))
}

// A missing staging record is NOT proof that no mapping exists: the node index
// is consulted, then the kernel.
func TestNodeUnstageVolume_RecoversFromNodeIndexWhenStagingRecordMissing(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	f.stageForUnstage(t, dev)

	// Simulate the staging copy being lost while the index survives.
	require.NoError(t, os.Remove(filepath.Join(f.staging, stagingRecordFile)))

	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{dev}, nil).Once()
	f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).Return(nil).Once()
	f.mapper.On("Flush", mock.Anything, dev.DevicePath).Return(nil)
	f.mapper.On("Unmap", mock.Anything, dev.DevicePath, mock.Anything).Return(nil)
	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{}, nil).Once()

	_, err := f.ns.NodeUnstageVolume(t.Context(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
	})
	require.NoError(t, err)
	f.mapper.AssertCalled(t, "Unmap", mock.Anything, dev.DevicePath, mock.Anything)
}

// With no state at all there is no expected identity, so an unmap cannot be
// authorized. Reporting success is correct; guessing would be dangerous.
func TestNodeUnstageVolume_NoStateIsSuccessAndUnmapsNothing(t *testing.T) {
	f := newNodeFixture(t)

	_, err := f.ns.NodeUnstageVolume(t.Context(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
	})
	require.NoError(t, err)
	f.mapper.AssertNotCalled(t, "Unmap")
}

// A recycled /dev/rbdN must never be unmapped on the strength of a recorded
// device number.
func TestNodeUnstageVolume_IsolatesMismatchedDeviceWithoutUnmapping(t *testing.T) {
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
	// State is retained so an operator can inspect it.
	_, readErr := f.ns.Staging.Read(f.staging)
	assert.NoError(t, readErr)
}

// An unmap that cannot proceed must keep state and return a retryable code.
func TestNodeUnstageVolume_BusyDeviceKeepsStateAndAborts(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	f.stageForUnstage(t, dev)

	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{dev}, nil)
	f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).Return(nil)
	f.mapper.On("Flush", mock.Anything, dev.DevicePath).Return(nil)
	f.mapper.On("Unmap", mock.Anything, dev.DevicePath, mock.Anything).Return(ErrDeviceBusy)

	_, err := f.ns.NodeUnstageVolume(t.Context(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
	})
	assert.Equal(t, codes.Aborted, codeOf(t, err))

	rec, readErr := f.ns.Staging.Read(f.staging)
	require.NoError(t, readErr, "staging state must survive a failed unmap")
	assert.Equal(t, testVolumeID, rec.VolumeID)
}

// If the device is still present after a "successful" unmap, state must be kept
// rather than discarded.
func TestNodeUnstageVolume_StillMappedAfterUnmapAborts(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	f.stageForUnstage(t, dev)

	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{dev}, nil)
	f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).Return(nil)
	f.mapper.On("Flush", mock.Anything, dev.DevicePath).Return(nil)
	f.mapper.On("Unmap", mock.Anything, dev.DevicePath, mock.Anything).Return(nil)

	_, err := f.ns.NodeUnstageVolume(t.Context(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
	})
	assert.Equal(t, codes.Aborted, codeOf(t, err))
	_, readErr := f.ns.Staging.Read(f.staging)
	assert.NoError(t, readErr)
}

// A flush failure must abort: losing buffered writes would silently corrupt a
// migration copy.
func TestNodeUnstageVolume_FlushFailureAborts(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	f.stageForUnstage(t, dev)

	f.mapper.On("ListMapped", mock.Anything).Return([]MappedDevice{dev}, nil)
	f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).Return(nil)
	f.mapper.On("Flush", mock.Anything, dev.DevicePath).Return(errors.New("flush failed"))

	_, err := f.ns.NodeUnstageVolume(t.Context(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
	})
	require.Error(t, err)
	f.mapper.AssertNotCalled(t, "Unmap")
}

// The node must never call Cinder: deleting the attachment record is the
// controller's job, which is what makes a Cinder outage during unstage tractable.
func TestNodeUnstageVolume_NeverTouchesCinder(t *testing.T) {
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
	require.NoError(t, err)

	// The node server has no Cinder client at all: this is structural, and the
	// assertion documents the intent.
	assert.Nil(t, f.ns.Driver.cs, "the node service must not hold a controller/Cinder reference")
}

func TestNodeUnstageVolume_RejectsInvalidRequests(t *testing.T) {
	f := newNodeFixture(t)

	_, err := f.ns.NodeUnstageVolume(t.Context(), &csi.NodeUnstageVolumeRequest{
		StagingTargetPath: f.staging,
	})
	assert.Equal(t, codes.InvalidArgument, codeOf(t, err))

	_, err = f.ns.NodeUnstageVolume(t.Context(), &csi.NodeUnstageVolumeRequest{
		VolumeId: testVolumeID,
	})
	assert.Equal(t, codes.InvalidArgument, codeOf(t, err))
}

// ── NodePublishVolume ────────────────────────────────────────────────────────

func TestNodePublishVolume_VerifiesIdentityBeforeBindMount(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	createFakeDevice(t, &dev)
	f.stageForUnstage(t, dev)

	target := filepath.Join(t.TempDir(), "target")

	f.mounter.On("IsLikelyNotMountPointAttach", target).Return(true, nil)
	f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).Return(nil)
	f.mounter.On("MakeFile", target).Return(nil)

	_, err := f.ns.NodePublishVolume(t.Context(), &csi.NodePublishVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		TargetPath:        target,
		VolumeCapability:  blockCapability(),
	})
	// The mount itself goes through the real SafeFormatAndMount, which is not
	// available in a unit test; identity verification is what this asserts.
	f.mapper.AssertCalled(t, "VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything)
	_ = err
}

// A recycled device number must never be bind-mounted into a pod.
func TestNodePublishVolume_RefusesWhenStagedDeviceNoLongerMatches(t *testing.T) {
	f := newNodeFixture(t)
	dev := mappedDevice(5)
	f.stageForUnstage(t, dev)

	target := filepath.Join(t.TempDir(), "target")
	f.mounter.On("IsLikelyNotMountPointAttach", target).Return(true, nil)
	f.mapper.On("VerifyIdentity", mock.Anything, dev.DevicePath, mock.Anything).
		Return(ErrIdentityMismatch)

	_, err := f.ns.NodePublishVolume(t.Context(), &csi.NodePublishVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		TargetPath:        target,
		VolumeCapability:  blockCapability(),
	})
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
	f.mounter.AssertNotCalled(t, "MakeFile")
}

func TestNodePublishVolume_AlreadyPublishedIsIdempotent(t *testing.T) {
	f := newNodeFixture(t)
	target := filepath.Join(t.TempDir(), "target")

	f.mounter.On("IsLikelyNotMountPointAttach", target).Return(false, nil)

	_, err := f.ns.NodePublishVolume(t.Context(), &csi.NodePublishVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		TargetPath:        target,
		VolumeCapability:  blockCapability(),
	})
	require.NoError(t, err)
	f.mounter.AssertNotCalled(t, "MakeFile")
}

func TestNodePublishVolume_NotStagedIsFailedPrecondition(t *testing.T) {
	f := newNodeFixture(t)
	target := filepath.Join(t.TempDir(), "target")

	f.mounter.On("IsLikelyNotMountPointAttach", target).Return(true, nil)

	_, err := f.ns.NodePublishVolume(t.Context(), &csi.NodePublishVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		TargetPath:        target,
		VolumeCapability:  blockCapability(),
	})
	assert.Equal(t, codes.FailedPrecondition, codeOf(t, err))
}

func TestNodePublishVolume_RejectsFilesystemCapability(t *testing.T) {
	f := newNodeFixture(t)

	_, err := f.ns.NodePublishVolume(t.Context(), &csi.NodePublishVolumeRequest{
		VolumeId:          testVolumeID,
		StagingTargetPath: f.staging,
		TargetPath:        "/target",
		VolumeCapability:  mountCapability(),
	})
	assert.Equal(t, codes.InvalidArgument, codeOf(t, err))
}

// ── NodeUnpublishVolume ──────────────────────────────────────────────────────

func TestNodeUnpublishVolume_UnmountsTarget(t *testing.T) {
	f := newNodeFixture(t)
	target := "/var/lib/kubelet/pods/x/volumeDevices/y"

	f.mounter.On("UnmountPath", target).Return(nil)

	_, err := f.ns.NodeUnpublishVolume(t.Context(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   testVolumeID,
		TargetPath: target,
	})
	require.NoError(t, err)
	f.mounter.AssertExpectations(t)
}

func TestNodeUnpublishVolume_RejectsInvalidRequests(t *testing.T) {
	f := newNodeFixture(t)

	_, err := f.ns.NodeUnpublishVolume(t.Context(), &csi.NodeUnpublishVolumeRequest{
		TargetPath: "/target",
	})
	assert.Equal(t, codes.InvalidArgument, codeOf(t, err))

	_, err = f.ns.NodeUnpublishVolume(t.Context(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: testVolumeID,
	})
	assert.Equal(t, codes.InvalidArgument, codeOf(t, err))
}

// ── NodeGetVolumeStats ───────────────────────────────────────────────────────

// A raw block volume has no inode statistics, so only BYTES is reported.
func TestNodeGetVolumeStats_BlockReportsBytesOnly(t *testing.T) {
	f := newNodeFixture(t)
	volumePath := filepath.Join(t.TempDir(), "device")
	require.NoError(t, os.WriteFile(volumePath, []byte("x"), 0o600))

	f.mounter.On("GetDeviceStats", volumePath).Return(&mountutil.DeviceStats{
		Block:      true,
		TotalBytes: 1073741824,
	}, nil)

	resp, err := f.ns.NodeGetVolumeStats(t.Context(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   testVolumeID,
		VolumePath: volumePath,
	})
	require.NoError(t, err)
	require.Len(t, resp.Usage, 1)
	assert.Equal(t, csi.VolumeUsage_BYTES, resp.Usage[0].Unit)
	assert.Equal(t, int64(1073741824), resp.Usage[0].Total)
}

func TestNodeGetVolumeStats_MissingPathIsNotFound(t *testing.T) {
	f := newNodeFixture(t)

	_, err := f.ns.NodeGetVolumeStats(t.Context(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   testVolumeID,
		VolumePath: filepath.Join(t.TempDir(), "absent"),
	})
	assert.Equal(t, codes.NotFound, codeOf(t, err))
}

// ── NodeExpandVolume ─────────────────────────────────────────────────────────

func TestNodeExpandVolume_Unimplemented(t *testing.T) {
	f := newNodeFixture(t)

	_, err := f.ns.NodeExpandVolume(t.Context(), &csi.NodeExpandVolumeRequest{
		VolumeId: testVolumeID,
	})
	assert.Equal(t, codes.Unimplemented, codeOf(t, err))
}
