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

// Package openstack implements Cinder volume CRUD and metadata operations for
// the iSCSI-Cinder CSI driver.

package openstack

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/cloud-provider-openstack/pkg/metrics"
	cpoerrors "k8s.io/cloud-provider-openstack/pkg/util/errors"
	"k8s.io/klog/v2"
)

const (
	VolumeAvailableStatus = "available"
	VolumeInUseStatus     = "in-use"
	VolumeReservedStatus  = "reserved"

	volumeDescription = "Created by iSCSI-Cinder CSI driver"

	// Backoff parameters for WaitVolumeTargetStatus
	operationFinishInitDelay = 1 * time.Second
	operationFinishFactor    = 1.1
	operationFinishMinSteps  = 10

	// Default timeout when caller passes timeoutSeconds <= 0.
	defaultWaitTimeoutSeconds = 300
)

var volumeErrorStates = [...]string{"error", "error_extending", "error_deleting"}

// ── Volume CRUD Operations ───────────────────────────────────────────────────

// CreateVolume creates a Cinder volume with the given options.
// Cinder API: POST /v3/volumes
func (os *OpenStackISCSI) CreateVolume(ctx context.Context, opts *volumes.CreateOpts,
	schedulerHints volumes.SchedulerHintOptsBuilder) (*volumes.Volume, error) {

	// Volume create does not require a specific microversion, but we use a
	// thread-safe copy to avoid mutating the shared client.
	blockstorageClient, err := os.blockStorageClient(MvSelfServiceAttach)
	if err != nil {
		return nil, err
	}

	mc := metrics.NewMetricContext("volume", "create")
	opts.Description = volumeDescription
	vol, err := volumes.Create(ctx, blockstorageClient, opts, schedulerHints).Extract()
	if mc.ObserveRequest(err) != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusRequestEntityTooLarge) {
			return nil, fmt.Errorf("volume creation failed — quota exceeded: %w", err)
		}
		return nil, fmt.Errorf("failed to create volume: %w", err)
	}

	klog.V(4).Infof("CreateVolume: created volume %s (%s) size=%dGiB az=%s",
		vol.ID, vol.Name, vol.Size, vol.AvailabilityZone)
	return vol, nil
}

// DeleteVolume deletes a Cinder volume by ID.
// Cinder API: DELETE /v3/volumes/{id}
func (os *OpenStackISCSI) DeleteVolume(ctx context.Context, volumeID string) error {
	mc := metrics.NewMetricContext("volume", "delete")
	err := volumes.Delete(ctx, os.blockstorage, volumeID, nil).ExtractErr()
	if mc.ObserveRequest(err) != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			klog.V(3).Infof("DeleteVolume: volume %s already deleted", volumeID)
			return nil
		}
		return fmt.Errorf("failed to delete volume %s: %w", volumeID, err)
	}

	klog.V(4).Infof("DeleteVolume: deleted volume %s", volumeID)
	return nil
}

// GetVolume retrieves a Cinder volume by ID.
// Cinder API: GET /v3/volumes/{id}
func (os *OpenStackISCSI) GetVolume(ctx context.Context, volumeID string) (*volumes.Volume, error) {
	mc := metrics.NewMetricContext("volume", "get")
	vol, err := volumes.Get(ctx, os.blockstorage, volumeID).Extract()
	if mc.ObserveRequest(err) != nil {
		return nil, err
	}
	return vol, nil
}

// GetVolumesByName lists Cinder volumes filtered by name.
// Cinder API: GET /v3/volumes?name={name} (microversion 3.34 for server-side filtering)
func (os *OpenStackISCSI) GetVolumesByName(ctx context.Context, name string) ([]volumes.Volume, error) {
	blockstorageClient, err := os.blockStorageClient(MvServerSideNameFilter)
	if err != nil {
		return nil, err
	}

	opts := volumes.ListOpts{Name: name}
	mc := metrics.NewMetricContext("volume", "list")
	pages, err := volumes.List(blockstorageClient, opts).AllPages(ctx)
	if mc.ObserveRequest(err) != nil {
		return nil, fmt.Errorf("failed to list volumes by name %q: %w", name, err)
	}

	vols, err := volumes.ExtractVolumes(pages)
	if err != nil {
		return nil, fmt.Errorf("failed to extract volumes: %w", err)
	}

	return vols, nil
}

// ExpandVolume extends a Cinder volume to a new size.
// Cinder API: POST /v3/volumes/{id}/action  { "os-extend": { "new_size": size } }
func (os *OpenStackISCSI) ExpandVolume(ctx context.Context, volumeID string, status string, newSize int) error {
	extendOpts := volumes.ExtendSizeOpts{
		NewSize: newSize,
	}

	switch status {
	case VolumeInUseStatus:
		// Online resize requires microversion 3.42
		blockstorageClient, err := os.blockStorageClient(MvOnlineResize)
		if err != nil {
			return err
		}

		mc := metrics.NewMetricContext("volume", "expand")
		return mc.ObserveRequest(volumes.ExtendSize(ctx, blockstorageClient, volumeID, extendOpts).ExtractErr())

	case VolumeAvailableStatus, VolumeReservedStatus:
		mc := metrics.NewMetricContext("volume", "expand")
		return mc.ObserveRequest(volumes.ExtendSize(ctx, os.blockstorage, volumeID, extendOpts).ExtractErr())
	}

	return fmt.Errorf("volume %s cannot be resized when status is %s", volumeID, status)
}

// WaitVolumeTargetStatus polls until the volume reaches one of the target
// statuses. timeoutSeconds controls the total wait time; if <= 0 the default
// (300 s) is used. The number of exponential-backoff steps is computed from
// the requested timeout so the backoff covers the full window.
func (os *OpenStackISCSI) WaitVolumeTargetStatus(ctx context.Context, volumeID string, tStatus []string, timeoutSeconds int) error {
	if timeoutSeconds <= 0 {
		timeoutSeconds = defaultWaitTimeoutSeconds
	}

	// Compute steps so the cumulative backoff covers the timeout.
	// Total ≈ initDelay * (factor^steps - 1) / (factor - 1)
	// Solving for steps: ceil(ln(timeout*(factor-1)/initDelay + 1) / ln(factor))
	initSec := operationFinishInitDelay.Seconds()
	n := math.Log(float64(timeoutSeconds)*(operationFinishFactor-1)/initSec+1) / math.Log(operationFinishFactor)
	steps := int(math.Ceil(n))
	if steps < operationFinishMinSteps {
		steps = operationFinishMinSteps
	}

	backoff := wait.Backoff{
		Duration: operationFinishInitDelay,
		Factor:   operationFinishFactor,
		Steps:    steps,
	}

	waitErr := wait.ExponentialBackoff(backoff, func() (bool, error) {
		vol, err := os.GetVolume(ctx, volumeID)
		if err != nil {
			return false, err
		}
		for _, t := range tStatus {
			if vol.Status == t {
				return true, nil
			}
		}
		for _, eState := range volumeErrorStates {
			if vol.Status == eState {
				return false, fmt.Errorf("volume %s is in error state: %s", volumeID, vol.Status)
			}
		}
		return false, nil
	})

	if wait.Interrupted(waitErr) {
		waitErr = fmt.Errorf("timeout waiting for volume %s to reach status %v (waited %ds)",
			volumeID, tStatus, timeoutSeconds)
	}

	return waitErr
}

// ── Volume Metadata Operations ───────────────────────────────────────────────

// SetVolumeMetadata sets (merges) metadata key-value pairs on a Cinder volume.
// Existing keys not in the provided map are preserved by first reading the
// current metadata, merging, then updating.
//
// Cinder API: PUT /v3/volumes/{id}  { "volume": { "metadata": {...} } }
func (os *OpenStackISCSI) SetVolumeMetadata(ctx context.Context, volumeID string, metadata map[string]string) error {
	// Read current volume to get existing metadata
	vol, err := os.GetVolume(ctx, volumeID)
	if err != nil {
		return fmt.Errorf("failed to get volume %s for metadata update: %w", volumeID, err)
	}

	// Merge: existing + new (new overwrites existing on conflict)
	merged := make(map[string]string, len(vol.Metadata)+len(metadata))
	for k, v := range vol.Metadata {
		merged[k] = v
	}
	for k, v := range metadata {
		merged[k] = v
	}

	opts := volumes.UpdateOpts{
		Metadata: merged,
	}

	mc := metrics.NewMetricContext("volume", "metadata_set")
	_, err = volumes.Update(ctx, os.blockstorage, volumeID, opts).Extract()
	if mc.ObserveRequest(err) != nil {
		return fmt.Errorf("failed to set metadata on volume %s: %w", volumeID, err)
	}

	klog.V(4).Infof("SetVolumeMetadata: set %d keys on volume %s", len(metadata), volumeID)
	return nil
}

// DeleteVolumeMetadata removes the specified metadata keys from a Cinder volume.
// Keys that don't exist are silently ignored.
//
// Strategy: read current metadata, remove specified keys, update with remaining.
func (os *OpenStackISCSI) DeleteVolumeMetadata(ctx context.Context, volumeID string, keys []string) error {
	vol, err := os.GetVolume(ctx, volumeID)
	if err != nil {
		if cpoerrors.IsNotFound(err) {
			klog.V(3).Infof("DeleteVolumeMetadata: volume %s not found, skipping", volumeID)
			return nil
		}
		return fmt.Errorf("failed to get volume %s for metadata delete: %w", volumeID, err)
	}

	// Build metadata map without the specified keys
	remaining := make(map[string]string, len(vol.Metadata))
	keysToRemove := make(map[string]bool, len(keys))
	for _, k := range keys {
		keysToRemove[k] = true
	}
	for k, v := range vol.Metadata {
		if !keysToRemove[k] {
			remaining[k] = v
		}
	}

	// If no keys were actually removed, skip the update
	if len(remaining) == len(vol.Metadata) {
		klog.V(4).Infof("DeleteVolumeMetadata: no matching keys to remove from volume %s", volumeID)
		return nil
	}

	opts := volumes.UpdateOpts{
		Metadata: remaining,
	}

	mc := metrics.NewMetricContext("volume", "metadata_delete")
	_, err = volumes.Update(ctx, os.blockstorage, volumeID, opts).Extract()
	if mc.ObserveRequest(err) != nil {
		return fmt.Errorf("failed to delete metadata keys from volume %s: %w", volumeID, err)
	}

	klog.V(4).Infof("DeleteVolumeMetadata: removed %d keys from volume %s", len(keys), volumeID)
	return nil
}

// ── Snapshot Operations (stubs — not needed for Phase 2) ─────────────────────
// Snapshot operations remain as stubs. They will be implemented if/when
// CREATE_DELETE_SNAPSHOT capability is added.
