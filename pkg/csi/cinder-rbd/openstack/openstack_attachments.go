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
	"net/http"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/attachments"
	"k8s.io/cloud-provider-openstack/pkg/metrics"
	"k8s.io/klog/v2"
)

// DiscoverCinderCapabilities probes the Cinder microversions the driver needs.
//
// 3.27 (self-service attachment records) is mandatory: without it the driver
// cannot reserve a volume without Nova, so the error is returned and startup
// fails. 3.44 (os-complete) is advisory: CompleteAttachment is skipped when it
// is unavailable, because connection discovery only needs 3.27.
//
// Verified on WRCP 24.09: both probes return HTTP 200.
func (os *OpenStackRBD) DiscoverCinderCapabilities(ctx context.Context) (*CinderCapabilities, error) {
	caps := &CinderCapabilities{}

	client327, err := os.blockStorageClient(MvSelfServiceAttach)
	if err != nil {
		return nil, err
	}

	mc := metrics.NewMetricContext("cinder", "discover_capabilities")
	_, err = attachments.List(client327, attachments.ListOpts{Limit: 1}).AllPages(ctx)
	if mc.ObserveRequest(err) != nil {
		return nil, fmt.Errorf("cinder does not support microversion %s (self-service attachment records): %w",
			MvSelfServiceAttach, err)
	}
	caps.SupportsV327 = true
	klog.V(2).Infof("DiscoverCinderCapabilities: microversion %s supported", MvSelfServiceAttach)

	client344, err := os.blockStorageClient(MvAttachComplete)
	if err != nil {
		return nil, err
	}
	if _, err = attachments.List(client344, attachments.ListOpts{Limit: 1}).AllPages(ctx); err != nil {
		klog.V(2).Infof("DiscoverCinderCapabilities: microversion %s NOT supported; "+
			"attachment completion will be skipped", MvAttachComplete)
		caps.SupportsV344 = false
	} else {
		klog.V(2).Infof("DiscoverCinderCapabilities: microversion %s supported", MvAttachComplete)
		caps.SupportsV344 = true
	}

	os.cinderCaps = caps
	return caps, nil
}

// ── Attachment record lifecycle ──────────────────────────────────────────────

// reservedAttachmentCreateOpts builds POST /v3/attachments with only
// volume_uuid.
//
// The upstream gophercloud CreateOpts serialises InstanceUUID as
// `"instance_uuid":""` when empty, and Cinder rejects that with HTTP 400
// because an empty string is not a valid UUID. Omitting the field entirely is
// what produces a reserved record with no Nova instance.
//
// Verified on WRCP 24.09: the record is created with status "reserved" and
// instance null, and the volume moves available -> reserved.
type reservedAttachmentCreateOpts struct {
	VolumeUUID string `json:"volume_uuid"`
}

// ToAttachmentCreateMap implements attachments.CreateOptsBuilder.
func (o reservedAttachmentCreateOpts) ToAttachmentCreateMap() (map[string]any, error) {
	return gophercloud.BuildRequestBody(o, "attachment")
}

// CreateAttachment creates a reserved attachment record, reserving the volume
// without consuming compute resources.
//
// Cinder API: POST /v3/attachments (microversion >= 3.27)
func (os *OpenStackRBD) CreateAttachment(ctx context.Context, volumeID string) (string, error) {
	blockstorageClient, err := os.blockStorageClient(MvSelfServiceAttach)
	if err != nil {
		return "", err
	}

	mc := metrics.NewMetricContext("attachment", "create")
	result, err := attachments.Create(ctx, blockstorageClient,
		reservedAttachmentCreateOpts{VolumeUUID: volumeID}).Extract()
	if mc.ObserveRequest(err) != nil {
		return "", fmt.Errorf("openstack: create attachment record for volume %s: %w", volumeID, err)
	}

	klog.V(4).Infof("CreateAttachment: created attachment record %s for volume %s (status %s)",
		result.ID, volumeID, result.Status)
	return result.ID, nil
}

// UpdateAttachmentConnector attaches the connector, causing Cinder to call the
// backend's initialize_connection and return RBD connection information.
//
// The connector carries only "host": the deployed backend rejects an empty
// connector and needs nothing else. Sending speculative fields would be
// untested risk.
//
// A NotFound error is returned unwrapped enough for cpoerrors.IsNotFound to
// detect it, which is what drives the caller's single replace-and-retry.
//
// Cinder API: PUT /v3/attachments/{id} (microversion >= 3.27)
func (os *OpenStackRBD) UpdateAttachmentConnector(ctx context.Context, attachmentID string,
	connector *AttachmentConnector) (*RBDConnectionInfo, error) {
	if connector == nil || connector.Host == "" {
		return nil, fmt.Errorf("openstack: update attachment %s: connector host must not be empty",
			attachmentID)
	}

	blockstorageClient, err := os.blockStorageClient(MvSelfServiceAttach)
	if err != nil {
		return nil, err
	}

	opts := attachments.UpdateOpts{
		Connector: map[string]any{"host": connector.Host},
	}

	mc := metrics.NewMetricContext("attachment", "update")
	result, err := attachments.Update(ctx, blockstorageClient, attachmentID, opts).Extract()
	if mc.ObserveRequest(err) != nil {
		// Returned unwrapped so the caller can classify HTTP 404.
		return nil, err
	}

	klog.V(4).Infof("UpdateAttachmentConnector: updated attachment record %s (status %s)",
		attachmentID, result.Status)

	connInfo, err := parseRBDConnectionInfo(result.ConnectionInfo)
	if err != nil {
		return nil, fmt.Errorf("openstack: attachment %s: %w", attachmentID, err)
	}
	// The response omits attachment_id on some paths; the caller's own ID is
	// authoritative for correlation.
	if connInfo.AttachmentID == "" {
		connInfo.AttachmentID = attachmentID
	}
	return connInfo, nil
}

// CompleteAttachment issues os-complete, moving the volume to in-use.
//
// Best effort by design: completion needs microversion 3.44 while connection
// discovery needs only 3.27, so an unsupported backend is not an error.
//
// Cinder API: POST /v3/attachments/{id}/action {"os-complete": {}}
func (os *OpenStackRBD) CompleteAttachment(ctx context.Context, attachmentID string) error {
	if os.cinderCaps != nil && !os.cinderCaps.SupportsV344 {
		klog.V(4).Infof("CompleteAttachment: skipping attachment %s — Cinder lacks microversion %s",
			attachmentID, MvAttachComplete)
		return nil
	}

	blockstorageClient, err := os.blockStorageClient(MvAttachComplete)
	if err != nil {
		return err
	}

	mc := metrics.NewMetricContext("attachment", "complete")
	err = attachments.Complete(ctx, blockstorageClient, attachmentID).ExtractErr()
	if mc.ObserveRequest(err) != nil {
		return fmt.Errorf("openstack: complete attachment %s: %w", attachmentID, err)
	}

	klog.V(4).Infof("CompleteAttachment: completed attachment record %s", attachmentID)
	return nil
}

// GetAttachment reads one attachment record.
//
// connection_info is normalized when present but a malformed payload is not
// fatal here: callers use this to discover whether a record exists at all, and
// failing the lookup would block recovery.
//
// Cinder API: GET /v3/attachments/{id}
func (os *OpenStackRBD) GetAttachment(ctx context.Context, attachmentID string) (*Attachment, error) {
	blockstorageClient, err := os.blockStorageClient(MvSelfServiceAttach)
	if err != nil {
		return nil, err
	}

	mc := metrics.NewMetricContext("attachment", "get")
	result, err := attachments.Get(ctx, blockstorageClient, attachmentID).Extract()
	if mc.ObserveRequest(err) != nil {
		return nil, err
	}
	return toAttachment(result.ID, result.VolumeID, result.Status, result.Instance, result.ConnectionInfo), nil
}

// ListAttachmentsByVolume lists the attachment records for one volume.
//
// This has no equivalent in the sibling iSCSI driver, and is required by two
// recovery boundaries: a reservation created but whose metadata write failed,
// and a connector update that succeeded but whose response was lost. Without
// it, the controller cannot tell "no record exists" from "a record exists that
// I forgot about", and would create duplicates.
//
// Cinder API: GET /v3/attachments?volume_id={id}
func (os *OpenStackRBD) ListAttachmentsByVolume(ctx context.Context, volumeID string) ([]Attachment, error) {
	if volumeID == "" {
		return nil, fmt.Errorf("openstack: list attachment records: volume ID must not be empty")
	}

	blockstorageClient, err := os.blockStorageClient(MvSelfServiceAttach)
	if err != nil {
		return nil, err
	}

	mc := metrics.NewMetricContext("attachment", "list")
	pages, err := attachments.List(blockstorageClient, attachments.ListOpts{VolumeID: volumeID}).AllPages(ctx)
	if mc.ObserveRequest(err) != nil {
		return nil, fmt.Errorf("openstack: list attachment records for volume %s: %w", volumeID, err)
	}

	extracted, err := attachments.ExtractAttachments(pages)
	if err != nil {
		return nil, fmt.Errorf("openstack: extract attachment records for volume %s: %w", volumeID, err)
	}

	// Filter locally on volume ID as well: correctness must not depend on the
	// backend honouring the query filter, since adopting another volume's
	// record would attach the wrong image.
	out := make([]Attachment, 0, len(extracted))
	for i := range extracted {
		a := &extracted[i]
		if a.VolumeID != volumeID {
			continue
		}
		out = append(out, *toAttachment(a.ID, a.VolumeID, a.Status, a.Instance, a.ConnectionInfo))
	}

	klog.V(4).Infof("ListAttachmentsByVolume: volume %s has %d attachment record(s)", volumeID, len(out))
	return out, nil
}

// DeleteAttachment removes an attachment record, returning the volume to
// available. A missing record is success so a retried unpublish converges.
//
// Cinder API: DELETE /v3/attachments/{id}
func (os *OpenStackRBD) DeleteAttachment(ctx context.Context, attachmentID string) error {
	blockstorageClient, err := os.blockStorageClient(MvSelfServiceAttach)
	if err != nil {
		return err
	}

	mc := metrics.NewMetricContext("attachment", "delete")
	err = attachments.Delete(ctx, blockstorageClient, attachmentID).ExtractErr()
	if mc.ObserveRequest(err) != nil {
		if gophercloud.ResponseCodeIs(err, http.StatusNotFound) {
			klog.V(3).Infof("DeleteAttachment: attachment record %s already gone", attachmentID)
			return nil
		}
		return fmt.Errorf("openstack: delete attachment record %s: %w", attachmentID, err)
	}

	klog.V(4).Infof("DeleteAttachment: deleted attachment record %s", attachmentID)
	return nil
}

// toAttachment converts a gophercloud attachment into the driver's type,
// normalizing connection_info when it is present and well-formed.
func toAttachment(id, volumeID, status string, instance string, rawConnInfo map[string]any) *Attachment {
	att := &Attachment{ID: id, VolumeID: volumeID, Status: status}
	if instance != "" {
		inst := instance
		att.Instance = &inst
	}
	if len(rawConnInfo) > 0 {
		ci, err := parseRBDConnectionInfo(rawConnInfo)
		if err != nil {
			// Not fatal: existence and status are what recovery needs, and a
			// reserved record legitimately has no usable connection_info yet.
			klog.V(4).Infof("toAttachment: attachment %s connection_info not usable: %v", id, err)
		} else {
			att.ConnectionInfo = ci
		}
	}
	return att
}
