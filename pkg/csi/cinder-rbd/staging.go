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
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-rbd/openstack"
	"k8s.io/klog/v2"
)

const (
	// stagingRecordFile is the per-volume record under the CSI staging path.
	stagingRecordFile = "rbd-staging.json"

	// stagingRecordSchema is bumped when the on-disk shape changes. An unknown
	// schema means "reconcile from the kernel", not "fail".
	stagingRecordSchema = 1

	// stagingRecordMode: the record holds identifiers only, never key material,
	// but it is still owner-only.
	stagingRecordMode = 0o600

	// stagingIndexDir holds a node-scoped copy of every record so startup
	// reconciliation can enumerate driver-owned volumes without walking every
	// kubelet staging directory.
	stagingIndexDir = "staged"
)

// StagingPhase records how far a stage got.
//
// The phase is what turns the record into an ownership *intent* rather than a
// mere hint. It is written before `rbd device map` and only advanced afterwards,
// so a crash anywhere in between still leaves durable proof that this driver —
// and not platform Ceph-CSI, which shares the same kernel path on these nodes —
// created any mapping that is found later.
type StagingPhase string

const (
	// PhaseMapPending means the driver is about to map, or mapped and did not
	// finish recording. A live mapping covered by this phase is driver-owned and
	// may be reused or rolled back.
	PhaseMapPending StagingPhase = "map-pending"

	// PhaseStaged means the map completed, passed the identity gate, and was
	// fully recorded.
	PhaseStaged StagingPhase = "staged"
)

// Valid reports whether the phase is one this driver writes.
func (p StagingPhase) Valid() bool {
	return p == PhaseMapPending || p == PhaseStaged
}

// StagingRecord is the durable per-volume staging record.
//
// It serves two distinct purposes, and conflating them is a mistake:
//
//   - As a *hint*, it records what the driver believes it mapped. It is
//     explicitly NOT the source of truth for identity — the kernel and Ceph are.
//   - As an *ownership intent* (see Phase), it is the only durable evidence that
//     this driver created a mapping. Kernel state can prove what an image is; it
//     cannot prove who mapped it.
//
// Both are needed to reuse a live mapping: the intent proves authorship, the
// kernel proves identity. Neither alone is sufficient.
//
// It carries no credential material — only the auth_username needed to rebuild
// a keyring path.
type StagingRecord struct {
	Schema int          `json:"schema"`
	Phase  StagingPhase `json:"phase,omitempty"`
	// VolumeID scopes the intent. A record found under one volume ID never
	// authorizes anything for another.
	VolumeID      string   `json:"volume_id"`
	AttachmentID  string   `json:"attachment_id,omitempty"`
	ClusterName   string   `json:"cluster_name,omitempty"`
	ClusterFSID   string   `json:"cluster_fsid"`
	Pool          string   `json:"pool"`
	Image         string   `json:"image"`
	Monitors      []string `json:"monitors,omitempty"`
	AuthUsername  string   `json:"auth_username,omitempty"`
	DevicePath    string   `json:"device_path"`
	DeviceID      int      `json:"device_id"`
	Exclusive     bool     `json:"exclusive"`
	MapGeneration int      `json:"map_generation"`
	SizeBytes     int64    `json:"size_bytes,omitempty"`
	StagedAt      string   `json:"staged_at"`
	Driver        string   `json:"driver"`
	// TargetPath records the raw-block bind target, so a restart can recreate a
	// missing link when the pod still needs it.
	TargetPath string `json:"target_path,omitempty"`
}

// ProvesOwnershipOf reports whether this record is valid driver ownership
// evidence for the given volume and image identity.
//
// Every condition matters:
//   - the driver name guards against a record written by something else that
//     happens to share the state directory;
//   - the volume ID guards against one volume's intent authorizing another's
//     mapping;
//   - the identity guards against an intent being reused after the volume was
//     deleted and its ID recycled onto a different image;
//   - the phase guards against a record shape this build does not understand.
func (r *StagingRecord) ProvesOwnershipOf(volumeID string, want ImageIdentity) bool {
	if r == nil || r.Driver != driverName || !r.Phase.Valid() {
		return false
	}
	if r.VolumeID == "" || r.VolumeID != volumeID {
		return false
	}
	return want.IsComplete() && r.Identity() == want
}

// Identity returns the ImageIdentity the record claims.
func (r *StagingRecord) Identity() ImageIdentity {
	return ImageIdentity{
		ClusterFSID: r.ClusterFSID,
		ClusterName: r.ClusterName,
		Pool:        r.Pool,
		Image:       r.Image,
	}
}

// MonitorAddrs parses the recorded monitor list back into structured form.
//
// net.SplitHostPort is used rather than a manual colon split so bracketed IPv6
// literals round-trip correctly. An unparseable entry is skipped: a partial
// monitor list still works, whereas returning a malformed address would produce
// a confusing map failure.
func (r *StagingRecord) MonitorAddrs() []openstack.MonAddr {
	out := make([]openstack.MonAddr, 0, len(r.Monitors))
	for _, m := range r.Monitors {
		host, port, err := net.SplitHostPort(m)
		if err != nil || host == "" || port == "" {
			klog.V(5).Infof("staging: skipping unparseable monitor %q in record for volume %s",
				m, r.VolumeID)
			continue
		}
		out = append(out, openstack.MonAddr{Host: host, Port: port})
	}
	return out
}

// newMapIntentRecord builds the pre-map ownership intent.
//
// Device fields are deliberately absent: the intent is written before the map
// exists, so it can only claim *which image* the driver is about to map, not
// which device it became. Identity is re-derived from the kernel afterwards.
func newMapIntentRecord(volumeID, attachmentID string, ci *openstack.RBDConnectionInfo,
	generation int) *StagingRecord {
	monitors := make([]string, 0, len(ci.Monitors))
	for _, m := range ci.Monitors {
		monitors = append(monitors, m.String())
	}
	return &StagingRecord{
		Schema:        stagingRecordSchema,
		Phase:         PhaseMapPending,
		VolumeID:      volumeID,
		AttachmentID:  attachmentID,
		ClusterName:   ci.ClusterName,
		ClusterFSID:   ci.ClusterFSID,
		Pool:          ci.Pool,
		Image:         ci.Image,
		Monitors:      monitors,
		AuthUsername:  ci.AuthUsername,
		Exclusive:     true,
		MapGeneration: generation,
		StagedAt:      time.Now().UTC().Format(time.RFC3339),
		Driver:        driverName,
	}
}

// newStagingRecord builds a completed record from validated connection information.
func newStagingRecord(volumeID, attachmentID string, ci *openstack.RBDConnectionInfo,
	dev MappedDevice, generation int, sizeBytes int64) *StagingRecord {
	monitors := make([]string, 0, len(ci.Monitors))
	for _, m := range ci.Monitors {
		monitors = append(monitors, m.String())
	}
	return &StagingRecord{
		Schema:        stagingRecordSchema,
		Phase:         PhaseStaged,
		VolumeID:      volumeID,
		AttachmentID:  attachmentID,
		ClusterName:   ci.ClusterName,
		ClusterFSID:   ci.ClusterFSID,
		Pool:          ci.Pool,
		Image:         ci.Image,
		Monitors:      monitors,
		AuthUsername:  ci.AuthUsername,
		DevicePath:    dev.DevicePath,
		DeviceID:      dev.ID,
		Exclusive:     true,
		MapGeneration: generation,
		SizeBytes:     sizeBytes,
		StagedAt:      time.Now().UTC().Format(time.RFC3339),
		Driver:        driverName,
	}
}

// stagingStore reads and writes staging records.
type stagingStore struct {
	opts openstack.RBDOpts
}

func newStagingStore(opts openstack.RBDOpts) *stagingStore {
	return &stagingStore{opts: opts}
}

// recordPath returns the per-volume record path under the CSI staging path.
func (s *stagingStore) recordPath(stagingTargetPath string) string {
	return filepath.Join(stagingTargetPath, stagingRecordFile)
}

// indexPath returns the node-scoped copy for this volume.
func (s *stagingStore) indexPath(volumeID string) string {
	return filepath.Join(s.opts.StateDir, stagingIndexDir, volumeID+".json")
}

// WriteIntent durably persists a pre-map ownership intent.
//
// Only the node-scoped index is written. The intent must be findable after a
// crash by startup reconciliation, which enumerates the index; the kubelet
// staging directory is not a dependable place for that, since it can be absent
// or recreated across a reboot.
//
// The write is synchronous and errors are returned, never logged and ignored:
// mapping without a durable intent is precisely the state this prevents.
func (s *stagingStore) WriteIntent(rec *StagingRecord) error {
	if rec.Phase != PhaseMapPending {
		return fmt.Errorf("refusing to write intent for volume %s in phase %q, want %q",
			rec.VolumeID, rec.Phase, PhaseMapPending)
	}
	return s.WriteIndexOnly(rec)
}

// Write persists the record to both the staging path and the node-scoped index.
//
// Both copies are written atomically. If the index write fails the staging copy
// is kept: losing the index degrades startup reconciliation, whereas losing the
// staging copy would hide the mapping from unstage.
func (s *stagingStore) Write(stagingTargetPath string, rec *StagingRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal staging record: %w", err)
	}
	data = append(data, '\n')

	if err := os.MkdirAll(stagingTargetPath, 0o750); err != nil {
		return fmt.Errorf("create staging path %s: %w", stagingTargetPath, err)
	}
	if err := writeFileAtomic(s.recordPath(stagingTargetPath), data, stagingRecordMode); err != nil {
		return fmt.Errorf("write staging record: %w", err)
	}

	if err := writeFileAtomic(s.indexPath(rec.VolumeID), data, stagingRecordMode); err != nil {
		klog.Warningf("staging: failed to write node index for volume %s: %v; "+
			"startup reconciliation may not see this volume", rec.VolumeID, err)
	}
	return nil
}

// Read loads the record from the staging path.
//
// A missing record returns os.ErrNotExist so callers can use errors.Is. An
// unknown schema is reported as unusable rather than as a hard error, because
// the correct response is to reconcile from the kernel.
func (s *stagingStore) Read(stagingTargetPath string) (*StagingRecord, error) {
	return readStagingRecordFile(s.recordPath(stagingTargetPath))
}

// ReadIndex loads the node-scoped copy for a volume.
func (s *stagingStore) ReadIndex(volumeID string) (*StagingRecord, error) {
	return readStagingRecordFile(s.indexPath(volumeID))
}

func readStagingRecordFile(path string) (*StagingRecord, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		// Wrapped with %w so errors.Is(err, os.ErrNotExist) works: a missing
		// record is an expected, meaningful state.
		return nil, fmt.Errorf("read staging record %s: %w", path, err)
	}

	var rec StagingRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("parse staging record %s: %w", path, err)
	}
	if rec.Schema != stagingRecordSchema {
		return nil, fmt.Errorf("staging record %s has unsupported schema %d (want %d)",
			path, rec.Schema, stagingRecordSchema)
	}
	// An absent phase means the record predates the phase field. Every such
	// record was written at the end of a successful stage — that was the only
	// place the store was called — so treating it as staged is accurate rather
	// than merely convenient.
	if rec.Phase == "" {
		rec.Phase = PhaseStaged
	}
	if !rec.Phase.Valid() {
		return nil, fmt.Errorf("staging record %s has unknown phase %q", path, rec.Phase)
	}
	if rec.VolumeID == "" || rec.Pool == "" || rec.Image == "" {
		return nil, fmt.Errorf("staging record %s is missing required fields", path)
	}
	// A completed record must name the device it completed on; a map-pending one
	// must not be trusted to, because it is written before the device exists.
	if rec.Phase == PhaseStaged && rec.DevicePath == "" {
		return nil, fmt.Errorf("staging record %s is %s but names no device", path, PhaseStaged)
	}
	return &rec, nil
}

// Remove deletes both copies of the record.
func (s *stagingStore) Remove(stagingTargetPath, volumeID string) error {
	var firstErr error
	if err := os.Remove(s.recordPath(stagingTargetPath)); err != nil && !os.IsNotExist(err) {
		firstErr = fmt.Errorf("remove staging record: %w", err)
	}
	if err := os.Remove(s.indexPath(volumeID)); err != nil && !os.IsNotExist(err) {
		if firstErr == nil {
			firstErr = fmt.Errorf("remove staging index: %w", err)
		}
	}
	return firstErr
}

// ListIndexed returns every record in the node-scoped index.
//
// An unreadable record is skipped with a warning rather than aborting: one
// corrupt file must not block reconciliation of every other volume.
func (s *stagingStore) ListIndexed() ([]*StagingRecord, error) {
	dir := filepath.Join(s.opts.StateDir, stagingIndexDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read staging index %s: %w", dir, err)
	}

	out := make([]*StagingRecord, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		rec, readErr := readStagingRecordFile(filepath.Join(dir, e.Name()))
		if readErr != nil {
			klog.Warningf("staging: skipping unusable index entry %s: %v", e.Name(), readErr)
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

// NextGeneration returns the map generation to use for a new mapping.
//
// The counter makes "the mapping I recorded" distinguishable from "a later
// mapping of the same image" in logs and metrics, which matters when diagnosing
// repeated stage/unstage cycles across CDI phases.
func (s *stagingStore) NextGeneration(volumeID string) int {
	if rec, err := s.ReadIndex(volumeID); err == nil {
		return rec.MapGeneration + 1
	}
	return 1
}

// WriteIndexOnly updates just the node-scoped index copy.
//
// Used by startup reconciliation: the staging-path copy lives under a kubelet
// directory that may not exist yet after a reboot, and NodeStageVolume rewrites
// it on the next call regardless.
func (s *stagingStore) WriteIndexOnly(rec *StagingRecord) error {
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal staging record: %w", err)
	}
	data = append(data, '\n')
	return writeFileAtomic(s.indexPath(rec.VolumeID), data, stagingRecordMode)
}

// RemoveIndexOnly deletes just the node-scoped index copy.
func (s *stagingStore) RemoveIndexOnly(volumeID string) error {
	if err := os.Remove(s.indexPath(volumeID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove staging index: %w", err)
	}
	return nil
}
