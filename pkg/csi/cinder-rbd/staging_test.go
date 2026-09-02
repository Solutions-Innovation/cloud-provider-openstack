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
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-rbd/openstack"
)

func newTestStagingStore(t *testing.T) (*stagingStore, openstack.RBDOpts, string) {
	t.Helper()
	base := t.TempDir()

	var opts openstack.RBDOpts
	require.NoError(t, opts.ApplyDefaults())
	opts.RuntimeDir = filepath.Join(base, "run")
	opts.StateDir = filepath.Join(base, "state")

	return newStagingStore(opts), opts, filepath.Join(base, "staging")
}

func TestStagingStore_WriteReadRoundTrip(t *testing.T) {
	store, _, stagingPath := newTestStagingStore(t)
	rec := newStagingRecord(testVolumeID, testAttachmentID, labConnectionInfo(), mappedDevice(5), 3, 1073741824)

	require.NoError(t, store.Write(stagingPath, rec))

	got, err := store.Read(stagingPath)
	require.NoError(t, err)
	assert.Equal(t, testVolumeID, got.VolumeID)
	assert.Equal(t, testAttachmentID, got.AttachmentID)
	assert.Equal(t, testFSID, got.ClusterFSID)
	assert.Equal(t, "cinder-volumes", got.Pool)
	assert.Equal(t, "/dev/rbd5", got.DevicePath)
	assert.Equal(t, 3, got.MapGeneration)
	assert.True(t, got.Exclusive)
	assert.Equal(t, driverName, got.Driver)
}

// Both copies must be written: the staging copy drives unstage, the node index
// drives startup reconciliation.
func TestStagingStore_WritesBothCopies(t *testing.T) {
	store, opts, stagingPath := newTestStagingStore(t)
	rec := newStagingRecord(testVolumeID, testAttachmentID, labConnectionInfo(), mappedDevice(5), 1, 0)

	require.NoError(t, store.Write(stagingPath, rec))

	assert.FileExists(t, filepath.Join(stagingPath, stagingRecordFile))
	assert.FileExists(t, filepath.Join(opts.StateDir, stagingIndexDir, testVolumeID+".json"))

	indexed, err := store.ReadIndex(testVolumeID)
	require.NoError(t, err)
	assert.Equal(t, rec.DevicePath, indexed.DevicePath)
}

// The record is an identifier-only hint. A key leaking into it would end up on
// disk outside the protected runtime directory.
func TestStagingRecord_ContainsNoCredentialMaterial(t *testing.T) {
	store, _, stagingPath := newTestStagingStore(t)
	rec := newStagingRecord(testVolumeID, testAttachmentID, labConnectionInfo(), mappedDevice(5), 1, 0)
	require.NoError(t, store.Write(stagingPath, rec))

	raw, err := os.ReadFile(filepath.Join(stagingPath, stagingRecordFile))
	require.NoError(t, err)

	content := string(raw)
	for _, forbidden := range []string{redactedKey, "keyring", "userKey", "secret", "password"} {
		assert.NotContains(t, content, forbidden,
			"staging record must not contain %q", forbidden)
	}
	// auth_username is an identity, not a credential, and is needed to rebuild
	// the keyring entity.
	assert.Contains(t, content, "cinder")
}

func TestStagingStore_MissingRecordIsNotExist(t *testing.T) {
	store, _, stagingPath := newTestStagingStore(t)

	_, err := store.Read(stagingPath)
	require.Error(t, err)
	assert.True(t, errors.Is(err, os.ErrNotExist),
		"a missing record must be detectable with errors.Is(os.ErrNotExist)")
}

// An unknown schema means "reconcile from the kernel", so it must be reported as
// unusable rather than silently accepted with zero values.
func TestStagingStore_UnknownSchemaIsRejected(t *testing.T) {
	store, _, stagingPath := newTestStagingStore(t)
	require.NoError(t, os.MkdirAll(stagingPath, 0o750))

	bad := map[string]any{
		"schema": 99, "volume_id": testVolumeID, "pool": "p", "image": "i",
	}
	data, err := json.Marshal(bad)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(stagingPath, stagingRecordFile), data, 0o600))

	_, err = store.Read(stagingPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "schema")
}

func TestStagingStore_IncompleteRecordIsRejected(t *testing.T) {
	store, _, stagingPath := newTestStagingStore(t)
	require.NoError(t, os.MkdirAll(stagingPath, 0o750))

	incomplete := StagingRecord{Schema: stagingRecordSchema, VolumeID: testVolumeID}
	data, err := json.Marshal(incomplete)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(stagingPath, stagingRecordFile), data, 0o600))

	_, err = store.Read(stagingPath)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing required fields")
}

func TestStagingStore_CorruptJSONIsRejected(t *testing.T) {
	store, _, stagingPath := newTestStagingStore(t)
	require.NoError(t, os.MkdirAll(stagingPath, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(stagingPath, stagingRecordFile),
		[]byte("{not json"), 0o600))

	_, err := store.Read(stagingPath)
	require.Error(t, err)
}

func TestStagingStore_Remove(t *testing.T) {
	store, opts, stagingPath := newTestStagingStore(t)
	rec := newStagingRecord(testVolumeID, testAttachmentID, labConnectionInfo(), mappedDevice(5), 1, 0)
	require.NoError(t, store.Write(stagingPath, rec))

	require.NoError(t, store.Remove(stagingPath, testVolumeID))
	assert.NoFileExists(t, filepath.Join(stagingPath, stagingRecordFile))
	assert.NoFileExists(t, filepath.Join(opts.StateDir, stagingIndexDir, testVolumeID+".json"))

	// Removing twice must not fail: unstage is retried.
	assert.NoError(t, store.Remove(stagingPath, testVolumeID))
}

func TestStagingStore_ListIndexed(t *testing.T) {
	store, _, stagingPath := newTestStagingStore(t)

	for i, volID := range []string{"vol-a", "vol-b"} {
		rec := newStagingRecord(volID, "att", labConnectionInfo(), mappedDevice(i), 1, 0)
		rec.Pool = "cinder-volumes"
		rec.Image = volID
		require.NoError(t, store.Write(filepath.Join(stagingPath, volID), rec))
	}

	recs, err := store.ListIndexed()
	require.NoError(t, err)
	require.Len(t, recs, 2)

	ids := []string{recs[0].VolumeID, recs[1].VolumeID}
	assert.ElementsMatch(t, []string{"vol-a", "vol-b"}, ids)
}

// One corrupt index entry must not block reconciliation of every other volume.
func TestStagingStore_ListIndexedSkipsCorruptEntries(t *testing.T) {
	store, opts, stagingPath := newTestStagingStore(t)
	rec := newStagingRecord("vol-good", "att", labConnectionInfo(), mappedDevice(1), 1, 0)
	require.NoError(t, store.Write(stagingPath, rec))

	indexDir := filepath.Join(opts.StateDir, stagingIndexDir)
	require.NoError(t, os.WriteFile(filepath.Join(indexDir, "vol-bad.json"), []byte("{corrupt"), 0o600))
	// A non-JSON file must simply be ignored.
	require.NoError(t, os.WriteFile(filepath.Join(indexDir, "README"), []byte("notes"), 0o600))

	recs, err := store.ListIndexed()
	require.NoError(t, err)
	require.Len(t, recs, 1)
	assert.Equal(t, "vol-good", recs[0].VolumeID)
}

func TestStagingStore_ListIndexedAbsentDirIsEmpty(t *testing.T) {
	store, _, _ := newTestStagingStore(t)

	recs, err := store.ListIndexed()
	require.NoError(t, err)
	assert.Empty(t, recs)
}

// The generation counter distinguishes "the mapping I recorded" from "a later
// mapping of the same image" across repeated CDI stage/unstage cycles.
func TestStagingStore_NextGenerationIncrements(t *testing.T) {
	store, _, stagingPath := newTestStagingStore(t)

	assert.Equal(t, 1, store.NextGeneration(testVolumeID))

	rec := newStagingRecord(testVolumeID, "att", labConnectionInfo(), mappedDevice(5), 1, 0)
	require.NoError(t, store.Write(stagingPath, rec))
	assert.Equal(t, 2, store.NextGeneration(testVolumeID))

	rec.MapGeneration = 7
	require.NoError(t, store.Write(stagingPath, rec))
	assert.Equal(t, 8, store.NextGeneration(testVolumeID))
}

// ── Ownership intent ─────────────────────────────────────────────────────────

func TestStagingStore_WriteIntentRoundTrip(t *testing.T) {
	store, _, _ := newTestStagingStore(t)

	rec := newMapIntentRecord(testVolumeID, "att", labConnectionInfo(), 3)
	require.NoError(t, store.WriteIntent(rec))

	got, err := store.ReadIndex(testVolumeID)
	require.NoError(t, err)
	assert.Equal(t, PhaseMapPending, got.Phase)
	assert.Equal(t, testVolumeID, got.VolumeID)
	assert.Equal(t, 3, got.MapGeneration)
	// The intent is written before the device exists, so it cannot name one.
	assert.Empty(t, got.DevicePath)
	assert.Zero(t, got.DeviceID)
}

// The intent lives in the node-scoped index because that is what startup
// reconciliation enumerates; a kubelet staging directory may not survive a reboot.
func TestStagingStore_WriteIntentIsNodeScoped(t *testing.T) {
	store, _, stagingPath := newTestStagingStore(t)

	require.NoError(t, store.WriteIntent(newMapIntentRecord(testVolumeID, "att", labConnectionInfo(), 1)))

	_, err := store.ReadIndex(testVolumeID)
	require.NoError(t, err)
	_, err = store.Read(stagingPath)
	require.Error(t, err, "the intent must not depend on the kubelet staging path")
}

func TestStagingStore_WriteIntentRejectsNonPendingPhase(t *testing.T) {
	store, _, _ := newTestStagingStore(t)

	staged := newStagingRecord(testVolumeID, "att", labConnectionInfo(), mappedDevice(5), 1, 0)
	err := store.WriteIntent(staged)
	require.Error(t, err, "only a map-pending record is an intent")
	assert.Contains(t, err.Error(), string(PhaseMapPending))
}

func TestStagingRecord_ProvesOwnershipOf(t *testing.T) {
	want := ImageIdentity{
		ClusterFSID: testFSID, ClusterName: "ceph",
		Pool: "cinder-volumes", Image: testVolumeID,
	}
	valid := newMapIntentRecord(testVolumeID, "att", labConnectionInfo(), 1)

	otherImage := *valid
	otherImage.Image = "someone-elses-image"

	otherFSID := *valid
	otherFSID.ClusterFSID = "a-different-cluster"

	foreignDriver := *valid
	foreignDriver.Driver = "rbd.csi.ceph.com"

	badPhase := *valid
	badPhase.Phase = "half-done"

	noVolume := *valid
	noVolume.VolumeID = ""

	for _, tc := range []struct {
		name     string
		rec      *StagingRecord
		volumeID string
		want     bool
	}{
		{"valid intent", valid, testVolumeID, true},
		{"staged record also proves ownership",
			newStagingRecord(testVolumeID, "att", labConnectionInfo(), mappedDevice(5), 1, 0),
			testVolumeID, true},
		{"nil record proves nothing", nil, testVolumeID, false},
		{"another volume's intent", valid, "some-other-volume", false},
		{"empty volume ID", &noVolume, testVolumeID, false},
		{"different image", &otherImage, testVolumeID, false},
		{"different cluster", &otherFSID, testVolumeID, false},
		{"written by another driver", &foreignDriver, testVolumeID, false},
		{"unknown phase", &badPhase, testVolumeID, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.rec.ProvesOwnershipOf(tc.volumeID, want))
		})
	}
}

// An incomplete requested identity can never be matched: refusing it keeps a
// half-populated publish context from authorizing reuse.
func TestStagingRecord_ProvesOwnershipRejectsIncompleteIdentity(t *testing.T) {
	rec := newMapIntentRecord(testVolumeID, "att", labConnectionInfo(), 1)
	assert.False(t, rec.ProvesOwnershipOf(testVolumeID, ImageIdentity{Pool: "cinder-volumes"}))
}

// Records written before the phase field existed were only ever written at the
// end of a successful stage, so they read back as staged.
func TestStagingStore_RecordWithoutPhaseReadsAsStaged(t *testing.T) {
	store, _, stagingPath := newTestStagingStore(t)

	rec := newStagingRecord(testVolumeID, "att", labConnectionInfo(), mappedDevice(5), 1, 0)
	require.NoError(t, store.Write(stagingPath, rec))

	// Strip the phase the way an older build would have left it.
	path := store.recordPath(stagingPath)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	delete(m, "phase")
	stripped, err := json.Marshal(m)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, stripped, 0o600))

	got, err := store.Read(stagingPath)
	require.NoError(t, err)
	assert.Equal(t, PhaseStaged, got.Phase)
}

func TestStagingStore_UnknownPhaseIsRejected(t *testing.T) {
	store, _, stagingPath := newTestStagingStore(t)

	rec := newStagingRecord(testVolumeID, "att", labConnectionInfo(), mappedDevice(5), 1, 0)
	rec.Phase = "somewhere-in-between"
	require.NoError(t, os.MkdirAll(stagingPath, 0o750))
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(store.recordPath(stagingPath), data, 0o600))

	_, readErr := store.Read(stagingPath)
	require.Error(t, readErr)
	assert.Contains(t, readErr.Error(), "unknown phase")
}

// A staged record that names no device cannot authorize an unstage, so it is
// rejected rather than silently treated as usable.
func TestStagingStore_StagedRecordWithoutDeviceIsRejected(t *testing.T) {
	store, _, stagingPath := newTestStagingStore(t)

	rec := newStagingRecord(testVolumeID, "att", labConnectionInfo(), mappedDevice(5), 1, 0)
	rec.DevicePath = ""
	require.NoError(t, os.MkdirAll(stagingPath, 0o750))
	data, err := json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(store.recordPath(stagingPath), data, 0o600))

	_, readErr := store.Read(stagingPath)
	require.Error(t, readErr)
	assert.Contains(t, readErr.Error(), "names no device")
}

func TestStagingRecord_Identity(t *testing.T) {
	rec := newStagingRecord(testVolumeID, "att", labConnectionInfo(), mappedDevice(5), 1, 0)

	id := rec.Identity()
	assert.True(t, id.IsComplete())
	assert.Equal(t, testFSID, id.ClusterFSID)
	assert.Equal(t, "cinder-volumes", id.Pool)
}

func TestStagingRecord_MonitorAddrsRoundTrip(t *testing.T) {
	ci := labConnectionInfo()
	rec := newStagingRecord(testVolumeID, "att", ci, mappedDevice(5), 1, 0)

	addrs := rec.MonitorAddrs()
	require.Len(t, addrs, len(ci.Monitors))
	for i := range addrs {
		assert.Equal(t, ci.Monitors[i].Host, addrs[i].Host)
		assert.Equal(t, ci.Monitors[i].Port, addrs[i].Port)
	}
}

// IPv6 monitors must survive the string round-trip through the record.
func TestStagingRecord_MonitorAddrsIPv6(t *testing.T) {
	ci := labConnectionInfo()
	ci.Monitors = []openstack.MonAddr{{Host: "fd00::1", Port: "6789"}}
	rec := newStagingRecord(testVolumeID, "att", ci, mappedDevice(5), 1, 0)

	addrs := rec.MonitorAddrs()
	require.Len(t, addrs, 1)
	assert.Equal(t, "fd00::1", addrs[0].Host)
	assert.Equal(t, "6789", addrs[0].Port)
}

func TestStagingRecord_MonitorAddrsSkipsUnparseable(t *testing.T) {
	rec := &StagingRecord{Monitors: []string{"10.0.0.1:6789", "no-port-here", ""}}

	addrs := rec.MonitorAddrs()
	require.Len(t, addrs, 1)
	assert.Equal(t, "10.0.0.1", addrs[0].Host)
}

// ── Atomic write ─────────────────────────────────────────────────────────────

// The temp file must be created with the final mode before content is written,
// so a partially written keyring is never readable.
func TestWriteFileAtomic_ModeAndContent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keyring")

	require.NoError(t, writeFileAtomic(path, []byte("secret-content"), 0o400))

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o400), info.Mode().Perm())

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "secret-content", string(content))
}

// No temporary files may be left behind, or the runtime directory accumulates
// readable fragments of key material.
func TestWriteFileAtomic_LeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()

	require.NoError(t, writeFileAtomic(filepath.Join(dir, "a"), []byte("x"), 0o600))
	require.NoError(t, writeFileAtomic(filepath.Join(dir, "b"), []byte("y"), 0o600))

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	for _, e := range entries {
		assert.False(t, len(e.Name()) > 0 && e.Name()[0] == '.',
			"temporary file %s was left behind", e.Name())
	}
	assert.Len(t, entries, 2)
}

func TestWriteFileAtomic_OverwritesExisting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conf")

	require.NoError(t, writeFileAtomic(path, []byte("first"), 0o600))
	require.NoError(t, writeFileAtomic(path, []byte("second"), 0o600))

	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "second", string(content))
}

func TestWriteFileAtomic_CreatesParentDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "deeper", "file")

	require.NoError(t, writeFileAtomic(path, []byte("x"), 0o600))
	assert.FileExists(t, path)
}
