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

// StagingRecord is the durable per-volume staging hint.
//
// It records what the driver believes it mapped. It is explicitly NOT the source
// of truth: the kernel and Ceph are. Its purpose is to locate expected state so
// reconciliation can compare it against reality.
//
// It carries no credential material — only the auth_username needed to rebuild
// a keyring path.
type StagingRecord struct {
	Schema        int      `json:"schema"`
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

// newStagingRecord builds a record from validated connection information.
func newStagingRecord(volumeID, attachmentID string, ci *openstack.RBDConnectionInfo,
	dev MappedDevice, generation int, sizeBytes int64) *StagingRecord {
	monitors := make([]string, 0, len(ci.Monitors))
	for _, m := range ci.Monitors {
		monitors = append(monitors, m.String())
	}
	return &StagingRecord{
		Schema:        stagingRecordSchema,
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
	if rec.VolumeID == "" || rec.Pool == "" || rec.Image == "" {
		return nil, fmt.Errorf("staging record %s is missing required fields", path)
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
