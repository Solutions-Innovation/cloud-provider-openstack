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
	"fmt"
	"sort"
	"sync"

	"k8s.io/klog/v2"
)

// ReconcileAction is what the reconciler decided for one record or mapping.
type ReconcileAction string

const (
	// ActionAdopted means a live mapping matched a record and was kept. The
	// recorded device path is refreshed, because kernel device numbers change
	// across a reboot.
	ActionAdopted ReconcileAction = "adopted"

	// ActionUnstaged means a record had no live mapping, so the record was
	// discarded. A later NodeStageVolume maps afresh.
	ActionUnstaged ReconcileAction = "unstaged"

	// ActionIsolated means a live mapping occupies a recorded pool/image but
	// failed identity verification. It is neither adopted nor unmapped: the
	// device may belong to another client, and unmapping it could fault an
	// unrelated workload.
	ActionIsolated ReconcileAction = "isolated"

	// ActionReported means a live mapping has no record. It is only reported.
	// Adoption requires proof of driver ownership, which a node-only process
	// cannot obtain.
	ActionReported ReconcileAction = "reported"
)

// ReconcileEntry is one decision, suitable for logging and for the isolation set.
type ReconcileEntry struct {
	Action     ReconcileAction
	VolumeID   string
	Identity   ImageIdentity
	DevicePath string
	Detail     string
}

// ReconcileResult summarises a reconciliation pass.
type ReconcileResult struct {
	Entries []ReconcileEntry

	Adopted  int
	Unstaged int
	Isolated int
	Reported int
}

// IsolatedIdentities returns the identities that failed verification.
func (r *ReconcileResult) IsolatedIdentities() []ImageIdentity {
	out := make([]ImageIdentity, 0, r.Isolated)
	for _, e := range r.Entries {
		if e.Action == ActionIsolated {
			out = append(out, e.Identity)
		}
	}
	return out
}

// isolationSet tracks volumes the driver refuses to serve until an operator
// intervenes.
//
// Without this, a mismatched mapping found at startup would be re-discovered on
// every stage attempt and produce the same failure with no record that the node
// is in a degraded state. Holding it explicitly lets the node fail fast and lets
// the metric reflect reality.
type isolationSet struct {
	mu    sync.RWMutex
	byVol map[string]ReconcileEntry
}

func newIsolationSet() *isolationSet {
	return &isolationSet{byVol: make(map[string]ReconcileEntry)}
}

// Add marks a volume as isolated.
//
// Nil-safe: a nil set means isolation tracking is disabled, which is valid for a
// hand-constructed nodeServer. Panicking here would turn a wiring gap into a
// crash in a privileged DaemonSet.
func (s *isolationSet) Add(e ReconcileEntry) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e.VolumeID == "" {
		// Keyed by identity when the volume ID is unknown, so an unrecorded
		// mapping can still be tracked.
		e.VolumeID = e.Identity.String()
	}
	s.byVol[e.VolumeID] = e
	isolatedMappings.Set(float64(len(s.byVol)))
}

// Get reports whether a volume is isolated. Nil-safe.
func (s *isolationSet) Get(volumeID string) (ReconcileEntry, bool) {
	if s == nil {
		return ReconcileEntry{}, false
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	e, ok := s.byVol[volumeID]
	return e, ok
}

// Clear removes a volume from the isolation set, for use once an operator has
// resolved the conflict and the volume verifies cleanly again. Nil-safe.
func (s *isolationSet) Clear(volumeID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.byVol, volumeID)
	isolatedMappings.Set(float64(len(s.byVol)))
}

// Len returns the number of isolated volumes. Nil-safe.
func (s *isolationSet) Len() int {
	if s == nil {
		return 0
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.byVol)
}

// reconciler compares durable staging records against live kernel state.
//
// The kernel is authoritative. Records only say where to look.
type reconciler struct {
	mapper    RBDMapper
	staging   *stagingStore
	isolation *isolationSet
	// ownedPools limits "unrecognized mapping" reporting to pools this driver
	// could plausibly own. Without it, every platform Ceph-CSI mapping on the
	// node would be reported as unrecognized on every restart.
	ownedPools map[string]bool
}

func newReconciler(mapper RBDMapper, staging *stagingStore, isolation *isolationSet) *reconciler {
	return &reconciler{mapper: mapper, staging: staging, isolation: isolation}
}

// Reconcile performs one pass and returns its decisions.
//
// It is a pure function of (records, live mappings, sysfs identity) plus the
// side effects of refreshing or removing records, which makes it testable
// without a kernel.
func (r *reconciler) Reconcile(ctx context.Context) (*ReconcileResult, error) {
	records, err := r.staging.ListIndexed()
	if err != nil {
		return nil, fmt.Errorf("reconcile: read staging index: %w", err)
	}

	live, err := r.mapper.ListMapped(ctx)
	if err != nil {
		// Without the kernel inventory nothing can be decided safely. Returning
		// an error keeps the node not-ready rather than acting on assumptions.
		return nil, fmt.Errorf("reconcile: list kernel mappings: %w", err)
	}

	result := &ReconcileResult{}
	r.ownedPools = make(map[string]bool, len(records))
	claimed := make(map[int]bool, len(live))

	// Deterministic order so logs and tests are stable.
	sort.Slice(records, func(i, j int) bool { return records[i].VolumeID < records[j].VolumeID })

	for _, rec := range records {
		r.ownedPools[rec.Pool] = true
		entry := r.reconcileRecord(ctx, rec, live, claimed)
		result.Entries = append(result.Entries, entry)

		switch entry.Action {
		case ActionAdopted:
			result.Adopted++
		case ActionUnstaged:
			result.Unstaged++
		case ActionIsolated:
			result.Isolated++
			r.isolation.Add(entry)
		case ActionReported:
			result.Reported++
		}
	}

	// Live mappings not covered by any record.
	for i := range live {
		d := live[i]
		if claimed[d.ID] {
			continue
		}
		// Only pools the driver could own are interesting. The platform
		// Ceph-CSI uses the same kernel path on these nodes, and reporting its
		// mappings would be noise at best and alarming at worst.
		if !r.ownedPools[d.Pool] {
			klog.V(5).Infof("reconcile: ignoring foreign mapping %s (%s/%s)",
				d.DevicePath, d.Pool, d.Image)
			continue
		}
		entry := ReconcileEntry{
			Action:     ActionReported,
			Identity:   d.Identity(),
			DevicePath: d.DevicePath,
			Detail: "live mapping in a driver-owned pool has no staging record; " +
				"ownership cannot be proven without a Cinder client, so it is left untouched",
		}
		klog.Warningf("reconcile: %s %s (%s/%s): %s",
			entry.Action, d.DevicePath, d.Pool, d.Image, entry.Detail)
		result.Entries = append(result.Entries, entry)
		result.Reported++
	}

	orphanedStagingRecords.Set(float64(result.Unstaged))
	unrecognizedMappings.Set(float64(result.Reported))
	stagedVolumes.Set(float64(result.Adopted))

	klog.V(2).Infof("reconcile: %d adopted, %d unstaged, %d isolated, %d reported",
		result.Adopted, result.Unstaged, result.Isolated, result.Reported)
	return result, nil
}

// reconcileRecord decides the fate of one staging record.
func (r *reconciler) reconcileRecord(ctx context.Context, rec *StagingRecord,
	live []MappedDevice, claimed map[int]bool) ReconcileEntry {
	want := rec.Identity()

	base := ReconcileEntry{VolumeID: rec.VolumeID, Identity: want, DevicePath: rec.DevicePath}

	if !want.IsComplete() {
		// A record that cannot describe an identity cannot authorize anything.
		base.Action = ActionUnstaged
		base.Detail = "staging record is incomplete and cannot be verified; discarding it"
		r.discardRecord(rec)
		klog.Warningf("reconcile: volume %s: %s", rec.VolumeID, base.Detail)
		return base
	}

	// Locate by pool/image, never by the recorded device number: numbers are
	// reused after a reboot, so /dev/rbd5 may now be someone else's image.
	var found *MappedDevice
	for i := range live {
		if live[i].Pool == want.Pool && live[i].Image == want.Image {
			found = &live[i]
			break
		}
	}

	if found == nil {
		base.Action = ActionUnstaged
		if rec.Phase == PhaseMapPending {
			// The intent outlived its purpose: nothing was mapped, or a rollback
			// completed without getting to remove it. Discarding is correct and
			// safe precisely because there is no mapping to leave orphaned.
			base.Detail = "ownership intent has no live mapping; the map did not happen " +
				"or was rolled back, so the intent is discarded"
		} else {
			base.Detail = "no live kernel mapping for the recorded image; a later stage will map afresh"
		}
		r.discardRecord(rec)
		klog.V(3).Infof("reconcile: volume %s (%s): %s", rec.VolumeID, want, base.Detail)
		return base
	}

	claimed[found.ID] = true

	if err := r.mapper.VerifyIdentity(ctx, found.DevicePath, want); err != nil {
		base.Action = ActionIsolated
		base.DevicePath = found.DevicePath
		base.Detail = fmt.Sprintf("device %s occupies %s but failed identity verification: %v; "+
			"not adopted and not unmapped", found.DevicePath, want, err)
		klog.Errorf("reconcile: volume %s: %s", rec.VolumeID, base.Detail)
		return base
	}

	// A map-pending record with a verified live mapping is the crash window
	// between `rbd device map` and the completed record. The intent proves the
	// mapping is ours, so it is adopted and finalized rather than left in limbo —
	// this is what makes a mid-stage crash recoverable instead of isolating.
	if rec.Phase == PhaseMapPending {
		base.Action = ActionAdopted
		base.DevicePath = found.DevicePath
		base.Detail = fmt.Sprintf("ownership intent covers live mapping %s; "+
			"finalizing the interrupted stage", found.DevicePath)
		klog.V(2).Infof("reconcile: volume %s: %s", rec.VolumeID, base.Detail)
		r.finalizeIntent(rec, *found)
		return base
	}

	// Adopt. The device number may have changed across a restart, so the record
	// is refreshed rather than trusted.
	base.Action = ActionAdopted
	base.DevicePath = found.DevicePath
	if found.DevicePath != rec.DevicePath {
		base.Detail = fmt.Sprintf("device path changed from %s to %s; record refreshed",
			rec.DevicePath, found.DevicePath)
		klog.V(2).Infof("reconcile: volume %s: %s", rec.VolumeID, base.Detail)
	}
	r.refreshRecord(rec, *found)
	return base
}

// finalizeIntent advances an interrupted map-pending record to staged.
//
// Only the node-scoped index is written, for the same reason refreshRecord gives:
// the staging-path copy lives under a kubelet directory that may not exist yet,
// and NodeStageVolume rewrites it on the next call. The device is recorded from
// the kernel, never from the intent, which never knew it.
func (r *reconciler) finalizeIntent(rec *StagingRecord, dev MappedDevice) {
	updated := *rec
	updated.Phase = PhaseStaged
	updated.DevicePath = dev.DevicePath
	updated.DeviceID = dev.ID
	if err := r.staging.WriteIndexOnly(&updated); err != nil {
		// The intent survives, so the mapping stays attributable and the next
		// stage or reconciliation can finish the job.
		klog.Warningf("reconcile: failed to finalize ownership intent for volume %s: %v; "+
			"the intent is retained", rec.VolumeID, err)
	}
}

// refreshRecord rewrites the node-scoped index with the current device path.
//
// Only the index is updated: the staging-path copy lives under a kubelet
// directory that may not exist yet after a reboot, and NodeStageVolume rewrites
// it on the next call anyway.
func (r *reconciler) refreshRecord(rec *StagingRecord, dev MappedDevice) {
	if rec.DevicePath == dev.DevicePath && rec.DeviceID == dev.ID {
		return
	}
	updated := *rec
	updated.DevicePath = dev.DevicePath
	updated.DeviceID = dev.ID
	if err := r.staging.WriteIndexOnly(&updated); err != nil {
		klog.Warningf("reconcile: failed to refresh index for volume %s: %v", rec.VolumeID, err)
	}
}

// discardRecord removes a record whose mapping is gone.
func (r *reconciler) discardRecord(rec *StagingRecord) {
	if err := r.staging.RemoveIndexOnly(rec.VolumeID); err != nil {
		klog.Warningf("reconcile: failed to remove stale index for volume %s: %v", rec.VolumeID, err)
	}
}
