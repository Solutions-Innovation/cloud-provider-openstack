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

// Package openstack implements Cinder v3 attachment operations for the
// iSCSI-Cinder CSI driver. These are thin wrappers around the Gophercloud
// blockstorage/v3/attachments SDK — no Nova dependency.

package openstack

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/attachments"
	"k8s.io/cloud-provider-openstack/pkg/metrics"
	"k8s.io/klog/v2"
)

// ── Cinder v3 Attachment Operations ──────────────────────────────────────────

// CreateAttachment creates a reserved Cinder v3 attachment with no connector
// and no instance UUID. This "locks" the volume in status "reserved" without
// consuming any compute resources (unlike Nova attach or Shadow VM).
//
// Cinder API: POST /v3/attachments  { volume_uuid: volumeID }
// Requires microversion >= 3.27.
func (os *OpenStackISCSI) CreateAttachment(ctx context.Context, volumeID string) (string, error) {
	blockstorageClient, err := os.blockStorageClient(MvSelfServiceAttach)
	if err != nil {
		return "", err
	}

	opts := attachments.CreateOpts{
		VolumeUUID: volumeID,
		// No InstanceUUID, no Connector — creates a "reserved" attachment
	}

	mc := metrics.NewMetricContext("attachment", "create")
	result, err := attachments.Create(ctx, blockstorageClient, opts).Extract()
	if mc.ObserveRequest(err) != nil {
		return "", fmt.Errorf("failed to create attachment for volume %s: %w", volumeID, err)
	}

	klog.V(4).Infof("CreateAttachment: created attachment %s for volume %s (status: %s)",
		result.ID, volumeID, result.Status)
	return result.ID, nil
}

// UpdateAttachmentConnector updates a reserved attachment with the initiator's
// connector information (IQN, IP, host). This triggers Cinder to call the
// backend's initialize_connection(), which creates the iSCSI target and returns
// connection_info with target portal, IQN, LUN, and CHAP credentials.
//
// Cinder API: PUT /v3/attachments/{id}  { connector: {...} }
// Returns parsed ISCSIConnectionInfo from the attachment's connection_info.
func (os *OpenStackISCSI) UpdateAttachmentConnector(ctx context.Context, attachmentID string,
	connector *AttachmentConnector) (*ISCSIConnectionInfo, error) {

	blockstorageClient, err := os.blockStorageClient(MvSelfServiceAttach)
	if err != nil {
		return nil, err
	}

	connectorMap := map[string]any{
		"initiator": connector.Initiator,
		"ip":        connector.IP,
		"host":      connector.Host,
		"multipath": connector.Multipath,
		"platform":  connector.Platform,
		"os_type":   connector.OSType,
	}

	opts := attachments.UpdateOpts{
		Connector: connectorMap,
	}

	mc := metrics.NewMetricContext("attachment", "update")
	result, err := attachments.Update(ctx, blockstorageClient, attachmentID, opts).Extract()
	if mc.ObserveRequest(err) != nil {
		return nil, fmt.Errorf("failed to update attachment %s connector: %w", attachmentID, err)
	}

	klog.V(4).Infof("UpdateAttachmentConnector: updated attachment %s (status: %s)",
		attachmentID, result.Status)

	// Parse connection_info from the response
	connInfo, err := parseISCSIConnectionInfo(result.ConnectionInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to parse connection_info for attachment %s: %w", attachmentID, err)
	}

	return connInfo, nil
}

// CompleteAttachment marks an attachment as "complete" (in-use).
// Available only with microversion >= 3.44.
//
// Cinder API: POST /v3/attachments/{id}/action  { "os-complete": null }
func (os *OpenStackISCSI) CompleteAttachment(ctx context.Context, attachmentID string) error {
	if os.cinderCaps != nil && !os.cinderCaps.SupportsV344 {
		klog.V(4).Infof("CompleteAttachment: skipping — Cinder does not support microversion 3.44")
		return nil
	}

	blockstorageClient, err := os.blockStorageClient(MvAttachComplete)
	if err != nil {
		return err
	}

	mc := metrics.NewMetricContext("attachment", "complete")
	err = attachments.Complete(ctx, blockstorageClient, attachmentID).ExtractErr()
	if mc.ObserveRequest(err) != nil {
		return fmt.Errorf("failed to complete attachment %s: %w", attachmentID, err)
	}

	klog.V(4).Infof("CompleteAttachment: completed attachment %s", attachmentID)
	return nil
}

// GetAttachment retrieves a Cinder v3 attachment by ID.
//
// Cinder API: GET /v3/attachments/{id}
func (os *OpenStackISCSI) GetAttachment(ctx context.Context, attachmentID string) (*Attachment, error) {
	blockstorageClient, err := os.blockStorageClient(MvSelfServiceAttach)
	if err != nil {
		return nil, err
	}

	mc := metrics.NewMetricContext("attachment", "get")
	result, err := attachments.Get(ctx, blockstorageClient, attachmentID).Extract()
	if mc.ObserveRequest(err) != nil {
		return nil, fmt.Errorf("failed to get attachment %s: %w", attachmentID, err)
	}

	att := &Attachment{
		ID:       result.ID,
		VolumeID: result.VolumeID,
		Status:   result.Status,
	}
	if result.Instance != "" {
		inst := result.Instance
		att.Instance = &inst
	}

	// Parse connection_info if present
	if result.ConnectionInfo != nil {
		connInfo, err := parseISCSIConnectionInfo(result.ConnectionInfo)
		if err != nil {
			klog.V(3).Infof("GetAttachment: could not parse connection_info for %s: %v", attachmentID, err)
			// Non-fatal — connection_info may not be populated for reserved attachments
		} else {
			att.ConnectionInfo = connInfo
		}
	}

	return att, nil
}

// DeleteAttachment deletes a Cinder v3 attachment. This triggers the backend's
// terminate_connection(), removing the iSCSI target for this initiator.
// The volume status transitions back to "available".
//
// Cinder API: DELETE /v3/attachments/{id}
func (os *OpenStackISCSI) DeleteAttachment(ctx context.Context, attachmentID string) error {
	blockstorageClient, err := os.blockStorageClient(MvSelfServiceAttach)
	if err != nil {
		return err
	}

	mc := metrics.NewMetricContext("attachment", "delete")
	err = attachments.Delete(ctx, blockstorageClient, attachmentID).ExtractErr()
	if mc.ObserveRequest(err) != nil {
		// If attachment is already deleted, treat as success (idempotent)
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			klog.V(3).Infof("DeleteAttachment: attachment %s already deleted", attachmentID)
			return nil
		}
		return fmt.Errorf("failed to delete attachment %s: %w", attachmentID, err)
	}

	klog.V(4).Infof("DeleteAttachment: deleted attachment %s", attachmentID)
	return nil
}

// ── Cinder Capabilities Discovery ────────────────────────────────────────────

// DiscoverCinderCapabilities probes the Cinder API version endpoint to
// determine supported microversions. This is called at driver startup.
func (os *OpenStackISCSI) DiscoverCinderCapabilities(ctx context.Context) (*CinderCapabilities, error) {
	caps := &CinderCapabilities{}

	// Probe 3.27 — self-service attachments
	client327, err := os.blockStorageClient(MvSelfServiceAttach)
	if err != nil {
		return nil, err
	}

	mc := metrics.NewMetricContext("cinder", "discover_capabilities")
	_, err = attachments.List(client327, attachments.ListOpts{Limit: 1}).AllPages(ctx)
	if mc.ObserveRequest(err) != nil {
		return nil, fmt.Errorf("cinder does not support microversion %s (self-service attachments): %w",
			MvSelfServiceAttach, err)
	}
	caps.SupportsV327 = true
	klog.V(2).Infof("DiscoverCinderCapabilities: Cinder supports microversion %s (self-service attachments)",
		MvSelfServiceAttach)

	// Probe 3.44 — os-complete action
	client344, err := os.blockStorageClient(MvAttachComplete)
	if err != nil {
		return nil, err
	}
	_, err = attachments.List(client344, attachments.ListOpts{Limit: 1}).AllPages(ctx)
	if err != nil {
		klog.V(2).Infof("DiscoverCinderCapabilities: Cinder does NOT support microversion %s (os-complete)",
			MvAttachComplete)
		caps.SupportsV344 = false
	} else {
		klog.V(2).Infof("DiscoverCinderCapabilities: Cinder supports microversion %s (os-complete)",
			MvAttachComplete)
		caps.SupportsV344 = true
	}

	os.cinderCaps = caps
	return caps, nil
}

// ── connection_info Parser ───────────────────────────────────────────────────

// parseISCSIConnectionInfo converts the raw connection_info map from a Cinder
// attachment response into a structured ISCSIConnectionInfo.
//
// Cinder returns connection_info as:
//
//	{
//	  "driver_volume_type": "iscsi",
//	  "data": {
//	    "target_portal": "69.167.149.97:3260",
//	    "target_iqn": "iqn.2010-10.org.openstack:volume-xxx",
//	    "target_lun": 0,
//	    "auth_method": "CHAP",
//	    "auth_username": "...",
//	    "auth_password": "...",
//	    ...
//	  }
//	}
func parseISCSIConnectionInfo(connInfo map[string]any) (*ISCSIConnectionInfo, error) {
	if connInfo == nil {
		return nil, fmt.Errorf("connection_info is nil")
	}

	info := &ISCSIConnectionInfo{}

	// Extract driver_volume_type from top level
	if dvt, ok := connInfo["driver_volume_type"].(string); ok {
		info.DriverVolumeType = dvt
	}

	// The actual connection details are nested under "data"
	data, ok := connInfo["data"].(map[string]any)
	if !ok {
		// Some backends put fields at the top level without "data" nesting
		data = connInfo
	}

	// Marshal data to JSON and unmarshal into our struct for clean parsing
	jsonBytes, err := json.Marshal(data)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal connection_info data: %w", err)
	}

	if err := json.Unmarshal(jsonBytes, info); err != nil {
		return nil, fmt.Errorf("failed to unmarshal connection_info data: %w", err)
	}

	// Ensure driver_volume_type is set (may come from top-level)
	if info.DriverVolumeType == "" {
		if dvt, ok := connInfo["driver_volume_type"].(string); ok {
			info.DriverVolumeType = dvt
		}
	}

	klog.V(5).Infof("parseISCSIConnectionInfo: portal=%s iqn=%s lun=%d auth=%s type=%s",
		info.TargetPortal, info.TargetIQN, info.TargetLUN, info.AuthMethod, info.DriverVolumeType)

	return info, nil
}
