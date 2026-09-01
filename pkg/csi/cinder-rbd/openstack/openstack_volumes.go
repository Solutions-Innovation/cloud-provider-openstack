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

// volumeDescription is stamped on every volume this driver creates, so an
// operator can tell driver-created volumes from hand-made ones.
const volumeDescription = "Created by Cinder RBD CSI driver"

// Backoff parameters for WaitVolumeTargetStatus.
const (
	operationFinishInitDelay = 1 * time.Second
	operationFinishFactor    = 1.1
	operationFinishMinSteps  = 10

	// defaultWaitTimeoutSeconds applies when the caller passes <= 0.
	defaultWaitTimeoutSeconds = 300
)

// volumeErrorStates are terminal failures: waiting longer cannot help, so the
// waiter fails fast instead of burning the whole timeout.
var volumeErrorStates = []string{"error", "error_extending", "error_deleting", "error_restoring"}

// ── Volume CRUD ──────────────────────────────────────────────────────────────

// CreateVolume creates a Cinder volume.
//
// Cinder API: POST /v3/volumes
func (os *OpenStackRBD) CreateVolume(ctx context.Context, opts *volumes.CreateOpts,
	schedulerHints volumes.SchedulerHintOptsBuilder) (*volumes.Volume, error) {
	if opts == nil {
		return nil, fmt.Errorf("openstack: create volume: opts must not be nil")
	}

	blockstorageClient, err := os.blockStorageClient(MvSelfServiceAttach)
	if err != nil {
		return nil, err
	}

	opts.Description = volumeDescription

	mc := metrics.NewMetricContext("volume", "create")
	vol, err := volumes.Create(ctx, blockstorageClient, opts, schedulerHints).Extract()
	if mc.ObserveRequest(err) != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusRequestEntityTooLarge) {
			return nil, fmt.Errorf("openstack: create volume %q: quota exceeded: %w", opts.Name, err)
		}
		return nil, fmt.Errorf("openstack: create volume %q: %w", opts.Name, err)
	}

	klog.V(4).Infof("CreateVolume: created volume %s (%s) size=%dGiB type=%s az=%s",
		vol.ID, vol.Name, vol.Size, vol.VolumeType, vol.AvailabilityZone)
	return vol, nil
}

// DeleteVolume deletes a Cinder volume. A missing volume is success, so a
// retried DeleteVolume RPC converges.
//
// Cinder API: DELETE /v3/volumes/{id}
func (os *OpenStackRBD) DeleteVolume(ctx context.Context, volumeID string) error {
	mc := metrics.NewMetricContext("volume", "delete")
	err := volumes.Delete(ctx, os.blockstorage, volumeID, nil).ExtractErr()
	if mc.ObserveRequest(err) != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			klog.V(3).Infof("DeleteVolume: volume %s already deleted", volumeID)
			return nil
		}
		return fmt.Errorf("openstack: delete volume %s: %w", volumeID, err)
	}

	klog.V(4).Infof("DeleteVolume: deleted volume %s", volumeID)
	return nil
}

// GetVolume reads a Cinder volume.
//
// The error is returned unwrapped so callers can use cpoerrors.IsNotFound on it.
//
// Cinder API: GET /v3/volumes/{id}
func (os *OpenStackRBD) GetVolume(ctx context.Context, volumeID string) (*volumes.Volume, error) {
	mc := metrics.NewMetricContext("volume", "get")
	vol, err := volumes.Get(ctx, os.blockstorage, volumeID).Extract()
	if mc.ObserveRequest(err) != nil {
		return nil, err
	}
	return vol, nil
}

// GetVolumesByName lists volumes with an exact name match, supporting
// CreateVolume idempotency.
//
// The server-side name filter is requested, but the result is also filtered
// exactly on the client. That matters for correctness rather than tidiness: if
// a backend ignores or loosens the filter, an unfiltered list would make
// CreateVolume see multiple "matches" for a unique name and fail, or worse,
// adopt an unrelated volume. Filtering locally makes the outcome independent of
// server behaviour, and removes any microversion dependency for this call.
//
// Cinder API: GET /v3/volumes?name={name}
func (os *OpenStackRBD) GetVolumesByName(ctx context.Context, name string) ([]volumes.Volume, error) {
	mc := metrics.NewMetricContext("volume", "list")
	pages, err := volumes.List(os.blockstorage, volumes.ListOpts{Name: name}).AllPages(ctx)
	if mc.ObserveRequest(err) != nil {
		return nil, fmt.Errorf("openstack: list volumes by name %q: %w", name, err)
	}

	vols, err := volumes.ExtractVolumes(pages)
	if err != nil {
		return nil, fmt.Errorf("openstack: extract volumes for name %q: %w", name, err)
	}

	exact := make([]volumes.Volume, 0, len(vols))
	for _, v := range vols {
		if v.Name == name {
			exact = append(exact, v)
		}
	}
	if len(exact) != len(vols) {
		klog.V(4).Infof("GetVolumesByName: server returned %d volume(s) for name %q, %d matched exactly",
			len(vols), name, len(exact))
	}
	return exact, nil
}

// WaitVolumeTargetStatus polls until the volume reaches one of tStatus.
//
// timeoutSeconds <= 0 selects the default. The number of backoff steps is
// derived from the timeout so the exponential schedule spans the whole window
// rather than expiring early.
func (os *OpenStackRBD) WaitVolumeTargetStatus(ctx context.Context, volumeID string,
	tStatus []string, timeoutSeconds int) error {
	if timeoutSeconds <= 0 {
		timeoutSeconds = defaultWaitTimeoutSeconds
	}

	// Total ~= initDelay * (factor^steps - 1) / (factor - 1); solve for steps.
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

	var lastStatus string
	waitErr := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		vol, err := os.GetVolume(ctx, volumeID)
		if err != nil {
			return false, err
		}
		lastStatus = vol.Status

		for _, t := range tStatus {
			if vol.Status == t {
				return true, nil
			}
		}
		for _, e := range volumeErrorStates {
			if vol.Status == e {
				return false, fmt.Errorf("openstack: volume %s entered error state %q while waiting for %v",
					volumeID, vol.Status, tStatus)
			}
		}
		return false, nil
	})

	if wait.Interrupted(waitErr) {
		return fmt.Errorf("openstack: timeout after %ds waiting for volume %s to reach %v (last status %q)",
			timeoutSeconds, volumeID, tStatus, lastStatus)
	}
	return waitErr
}

// ── Volume metadata ──────────────────────────────────────────────────────────
//
// Cinder's metadata sub-resource is not used. Both operations are
// read-modify-write over volumes.Update so unrelated keys survive. Key
// prefixing is the caller's job.

// SetVolumeMetadata merges keys into the volume metadata; supplied keys win on
// conflict.
func (os *OpenStackRBD) SetVolumeMetadata(ctx context.Context, volumeID string, metadata map[string]string) error {
	if len(metadata) == 0 {
		return nil
	}

	vol, err := os.GetVolume(ctx, volumeID)
	if err != nil {
		return fmt.Errorf("openstack: get volume %s for metadata update: %w", volumeID, err)
	}

	merged := make(map[string]string, len(vol.Metadata)+len(metadata))
	for k, v := range vol.Metadata {
		merged[k] = v
	}
	for k, v := range metadata {
		merged[k] = v
	}

	mc := metrics.NewMetricContext("volume", "metadata_set")
	_, err = volumes.Update(ctx, os.blockstorage, volumeID, volumes.UpdateOpts{Metadata: merged}).Extract()
	if mc.ObserveRequest(err) != nil {
		return fmt.Errorf("openstack: set metadata on volume %s: %w", volumeID, err)
	}

	klog.V(4).Infof("SetVolumeMetadata: set %d key(s) on volume %s", len(metadata), volumeID)
	return nil
}

// DeleteVolumeMetadata removes keys from the volume metadata. Absent keys are
// ignored, and a missing volume is success.
func (os *OpenStackRBD) DeleteVolumeMetadata(ctx context.Context, volumeID string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}

	vol, err := os.GetVolume(ctx, volumeID)
	if err != nil {
		if cpoerrors.IsNotFound(err) {
			klog.V(3).Infof("DeleteVolumeMetadata: volume %s not found, nothing to clear", volumeID)
			return nil
		}
		return fmt.Errorf("openstack: get volume %s for metadata delete: %w", volumeID, err)
	}

	remove := make(map[string]bool, len(keys))
	for _, k := range keys {
		remove[k] = true
	}

	remaining := make(map[string]string, len(vol.Metadata))
	for k, v := range vol.Metadata {
		if !remove[k] {
			remaining[k] = v
		}
	}

	// Nothing matched: skip the write rather than issuing a no-op PUT.
	if len(remaining) == len(vol.Metadata) {
		klog.V(4).Infof("DeleteVolumeMetadata: no matching keys on volume %s", volumeID)
		return nil
	}

	mc := metrics.NewMetricContext("volume", "metadata_delete")
	_, err = volumes.Update(ctx, os.blockstorage, volumeID, volumes.UpdateOpts{Metadata: remaining}).Extract()
	if mc.ObserveRequest(err) != nil {
		return fmt.Errorf("openstack: delete metadata keys from volume %s: %w", volumeID, err)
	}

	klog.V(4).Infof("DeleteVolumeMetadata: removed %d key(s) from volume %s",
		len(vol.Metadata)-len(remaining), volumeID)
	return nil
}
