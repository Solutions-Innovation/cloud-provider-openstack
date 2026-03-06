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

package iscsi

import (
	"context"
	"fmt"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-iscsi/openstack"
	"k8s.io/cloud-provider-openstack/pkg/util"
	cpoerrors "k8s.io/cloud-provider-openstack/pkg/util/errors"
	"k8s.io/klog/v2"
)

// controllerServer implements csi.ControllerServer for iSCSI-Cinder volumes.
// Uses Cinder v3 attachment API — no Nova dependency.
type controllerServer struct {
	Driver *Driver
	Cloud  openstack.IOpenStackISCSI
	csi.UnimplementedControllerServer
}

// ── Metadata Keys ────────────────────────────────────────────────────────────
// Metadata key suffixes stored in Cinder volume metadata. The full key is
// built by metadataKey() using the configured prefix (default "csi").
const (
	metaKeySuffixAttachmentID  = "attachment_id"
	metaKeySuffixCleanupVolume = "cleanupVolume"
	defaultMetadataPrefix      = "csi"
)

// metadataKey builds a Cinder metadata key from the configured prefix and a
// suffix. If prefix is empty, the default "csi" prefix is used.
func metadataKey(prefix, suffix string) string {
	if prefix == "" {
		prefix = defaultMetadataPrefix
	}
	return prefix + "." + suffix
}

// ── CreateVolume ─────────────────────────────────────────────────────────────

// CreateVolume creates a Cinder volume and a reserved attachment (no connector).
// The reserved attachment "locks" the volume in status "reserved" without any
// compute resources (no Shadow VM, no Nova).
//
// Flow:
//  1. Validate request parameters (block-only enforcement)
//  2. Check for existing volume by name (idempotency)
//  3. Create Cinder volume
//  4. Wait for volume to become "available"
//  5. Create reserved attachment (no connector, no server)
//  6. Store attachment_id in Cinder volume metadata
//
// Returns: Volume ID, capacity, and attachment_id in volume context.
func (cs *controllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	klog.V(4).Infof("CreateVolume: called with args %+v", protosanitizer.StripSecrets(req))

	// ── 1. Validate request ──────────────────────────────────────────────
	volName := req.GetName()
	if len(volName) == 0 {
		return nil, status.Error(codes.InvalidArgument, "CreateVolume: volume name must be provided")
	}

	volCapabilities := req.GetVolumeCapabilities()
	if volCapabilities == nil {
		return nil, status.Error(codes.InvalidArgument, "CreateVolume: volume capabilities must be provided")
	}

	// Block-only enforcement: reject filesystem/mount requests
	for _, cap := range volCapabilities {
		if cap.GetMount() != nil {
			return nil, status.Error(codes.InvalidArgument,
				"CreateVolume: this driver only supports raw block volumes (volumeMode: Block). "+
					"Filesystem mount requests are not supported. "+
					"Set volumeMode: Block in your PVC spec.")
		}
	}

	// Volume size — default 1 GiB
	volSizeBytes := int64(1 * 1024 * 1024 * 1024)
	if req.GetCapacityRange() != nil {
		volSizeBytes = req.GetCapacityRange().GetRequiredBytes()
	}
	volSizeGB := int(util.RoundUpSize(volSizeBytes, 1024*1024*1024))

	// Volume type and AZ from parameters
	volParams := req.GetParameters()
	volType := volParams["type"]
	volAZ := volParams["availability"]

	cloud := cs.Cloud
	vopts := cloud.GetVolumeOpts()

	// Apply default volume type from driver.conf if not specified in
	// StorageClass parameters.
	if volType == "" {
		volType = vopts.DefaultVolumeType
	}

	attachmentKey := metadataKey(vopts.MetadataPrefix, metaKeySuffixAttachmentID)

	// ── 2. Idempotency check ─────────────────────────────────────────────
	existingVols, err := cloud.GetVolumesByName(ctx, volName)
	if err != nil {
		klog.Errorf("CreateVolume: failed to query existing volumes: %v", err)
		return nil, status.Errorf(codes.Internal, "CreateVolume: failed to get volumes: %v", err)
	}

	if len(existingVols) == 1 {
		vol := &existingVols[0]
		if volSizeGB != vol.Size {
			return nil, status.Error(codes.AlreadyExists,
				"CreateVolume: volume already exists with same name but different capacity")
		}
		klog.V(4).Infof("CreateVolume: volume %s already exists (size=%dGiB, az=%s)",
			vol.ID, vol.Size, vol.AvailabilityZone)

		// Return existing volume — attachment_id should already be in metadata
		volCtx := map[string]string{}
		if attachID, ok := vol.Metadata[attachmentKey]; ok {
			volCtx["attachment_id"] = attachID
		}

		return &csi.CreateVolumeResponse{
			Volume: &csi.Volume{
				VolumeId:      vol.ID,
				CapacityBytes: int64(vol.Size) * 1024 * 1024 * 1024,
				VolumeContext: volCtx,
			},
		}, nil
	}

	if len(existingVols) > 1 {
		return nil, status.Error(codes.Internal,
			"CreateVolume: multiple volumes with the same name found")
	}

	// ── 3. Create Cinder volume ──────────────────────────────────────────
	opts := &volumes.CreateOpts{
		Name:             volName,
		Size:             volSizeGB,
		VolumeType:       volType,
		AvailabilityZone: volAZ,
	}

	vol, err := cloud.CreateVolume(ctx, opts, nil)
	if err != nil {
		klog.Errorf("CreateVolume: failed to create volume: %v", err)
		return nil, status.Errorf(codes.Internal, "CreateVolume: Cinder create failed: %v", err)
	}

	// ── 4. Wait for volume to be "available" ─────────────────────────────
	err = cloud.WaitVolumeTargetStatus(ctx, vol.ID, []string{"available"}, vopts.CreateTimeout)
	if err != nil {
		klog.Errorf("CreateVolume: volume %s failed to become available: %v", vol.ID, err)
		// Attempt cleanup
		cleanupErr := cloud.DeleteVolume(ctx, vol.ID)
		if cleanupErr != nil {
			klog.Errorf("CreateVolume: failed to clean up volume %s: %v", vol.ID, cleanupErr)
		}
		return nil, status.Errorf(codes.Internal,
			"CreateVolume: volume %s did not become available: %v", vol.ID, err)
	}

	// ── 5. Create reserved attachment ────────────────────────────────────
	attachmentID, err := cloud.CreateAttachment(ctx, vol.ID)
	if err != nil {
		klog.Errorf("CreateVolume: failed to create attachment for volume %s: %v", vol.ID, err)
		// Attempt cleanup
		cleanupErr := cloud.DeleteVolume(ctx, vol.ID)
		if cleanupErr != nil {
			klog.Errorf("CreateVolume: failed to clean up volume %s: %v", vol.ID, cleanupErr)
		}
		return nil, status.Errorf(codes.Internal,
			"CreateVolume: failed to create attachment: %v", err)
	}

	// ── 6. Store attachment_id in volume metadata ────────────────────────
	err = cloud.SetVolumeMetadata(ctx, vol.ID, map[string]string{
		attachmentKey: attachmentID,
	})
	if err != nil {
		klog.Errorf("CreateVolume: failed to set metadata on volume %s: %v", vol.ID, err)
		// Non-fatal — the attachment_id is also returned in volume context
	}

	klog.V(2).Infof("CreateVolume: successfully created volume %s with attachment %s",
		vol.ID, attachmentID)

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      vol.ID,
			CapacityBytes: int64(vol.Size) * 1024 * 1024 * 1024,
			VolumeContext: map[string]string{
				"attachment_id": attachmentID,
			},
		},
	}, nil
}

// ── DeleteVolume ─────────────────────────────────────────────────────────────

// DeleteVolume handles PVC deletion. Behavior depends on the delete-volume-mode
// driver configuration (default: "retain") and the per-volume csi.cleanupVolume
// metadata override:
//
// Precedence:
//  1. Per-volume metadata csi.cleanupVolume ("true" = delete) takes priority
//  2. Otherwise, driver.conf [Volume] delete-volume-mode is used
//  3. If neither is set, default is "retain"
//
// Modes:
//   - delete: Delete the Cinder volume entirely (error/cleanup path)
//   - retain (default): Delete the attachment, remove CSI metadata, and
//     leave the volume "available" for Blueprint to create the target VM (success path)
//
// Flow:
//  1. Get volume from Cinder (not found → success, idempotent)
//  2. Extract attachment_id and cleanupVolume from metadata
//  3. Resolve effective delete mode (per-volume override > driver config > retain)
//  4. Delete attachment if present
//  5. Delete volume OR remove CSI metadata (based on resolved mode)
func (cs *controllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	klog.V(4).Infof("DeleteVolume: called with args %+v", protosanitizer.StripSecrets(req))

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "DeleteVolume: volume ID must be provided")
	}

	cloud := cs.Cloud

	// ── 1. Get volume ────────────────────────────────────────────────────
	vol, err := cloud.GetVolume(ctx, volumeID)
	if err != nil {
		if cpoerrors.IsNotFound(err) {
			klog.V(3).Infof("DeleteVolume: volume %s already deleted", volumeID)
			return &csi.DeleteVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal, "DeleteVolume: failed to get volume %s: %v", volumeID, err)
	}

	vopts := cloud.GetVolumeOpts()
	attachmentKey := metadataKey(vopts.MetadataPrefix, metaKeySuffixAttachmentID)
	cleanupKey := metadataKey(vopts.MetadataPrefix, metaKeySuffixCleanupVolume)

	// ── 2. Extract metadata ──────────────────────────────────────────────
	attachmentID := vol.Metadata[attachmentKey]
	cleanupVolume := vol.Metadata[cleanupKey]

	// ── 3. Resolve effective delete mode ─────────────────────────────────
	// Per-volume metadata overrides the driver-level default.
	shouldDelete := false
	if cleanupVolume != "" {
		// Explicit per-volume override
		shouldDelete = (cleanupVolume == "true")
	} else {
		// Fall back to driver.conf delete-volume-mode (default: retain)
		shouldDelete = (vopts.DeleteVolumeMode == openstack.DeleteVolumeModeDelete)
	}

	// ── 4. Delete attachment if present ──────────────────────────────────
	if attachmentID != "" {
		err = cloud.DeleteAttachment(ctx, attachmentID)
		if err != nil {
			klog.Errorf("DeleteVolume: failed to delete attachment %s: %v", attachmentID, err)
			return nil, status.Errorf(codes.Internal,
				"DeleteVolume: failed to delete attachment %s: %v", attachmentID, err)
		}
		klog.V(4).Infof("DeleteVolume: deleted attachment %s for volume %s", attachmentID, volumeID)
	}

	// ── 5. Cleanup or handoff ────────────────────────────────────────────
	if shouldDelete {
		// Full cleanup — delete the Cinder volume
		err = cloud.DeleteVolume(ctx, volumeID)
		if err != nil {
			if cpoerrors.IsNotFound(err) {
				klog.V(3).Infof("DeleteVolume: volume %s already deleted during cleanup", volumeID)
				return &csi.DeleteVolumeResponse{}, nil
			}
			return nil, status.Errorf(codes.Internal,
				"DeleteVolume: failed to delete volume %s: %v", volumeID, err)
		}
		klog.V(2).Infof("DeleteVolume: fully deleted volume %s (cleanup mode)", volumeID)
	} else {
		// Success path — leave volume available for Blueprint
		// Remove CSI metadata so the volume is clean for target VM creation
		err = cloud.DeleteVolumeMetadata(ctx, volumeID, []string{
			attachmentKey,
			cleanupKey,
		})
		if err != nil {
			klog.Errorf("DeleteVolume: failed to clean metadata on volume %s: %v", volumeID, err)
			// Non-fatal — volume is still usable
		}
		klog.V(2).Infof("DeleteVolume: released volume %s (available for Blueprint)", volumeID)
	}

	return &csi.DeleteVolumeResponse{}, nil
}

// ── ControllerPublishVolume ──────────────────────────────────────────────────

// ControllerPublishVolume attaches a Cinder volume to a node by creating (if
// needed) and updating a Cinder attachment with the node's initiator connector
// (IQN, IP, host). This triggers Cinder to call the backend's
// initialize_connection(), creating an iSCSI target for this specific initiator.
//
// Flow:
//  1. Parse node identity (hostname;iqn;ip)
//  2. Get attachment_id from Cinder metadata, or create a new one
//  3. Update attachment with initiator connector
//     - If the attachment is gone (404), create a new one and retry
//  4. Optionally complete attachment (microversion >= 3.44)
//  5. Validate driver_volume_type == "iscsi"
//
// Returns: publish_context with iSCSI target details (portal, IQN, LUN, CHAP).
func (cs *controllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	klog.V(4).Infof("ControllerPublishVolume: called with args %+v", protosanitizer.StripSecrets(req))

	volumeID := req.GetVolumeId()
	nodeID := req.GetNodeId()
	volumeCapability := req.GetVolumeCapability()

	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume: volume ID must be provided")
	}
	if len(nodeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume: node ID must be provided")
	}
	if volumeCapability == nil {
		return nil, status.Error(codes.InvalidArgument, "ControllerPublishVolume: volume capability must be provided")
	}

	// Block-only enforcement
	if volumeCapability.GetMount() != nil {
		return nil, status.Error(codes.InvalidArgument,
			"ControllerPublishVolume: this driver only supports raw block volumes")
	}

	cloud := cs.Cloud

	// ── 1. Parse node identity ───────────────────────────────────────────
	host, iqn, ip, err := ParseNodeID(nodeID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument,
			"ControllerPublishVolume: invalid node ID: %v", err)
	}
	klog.V(4).Infof("ControllerPublishVolume: node=%s iqn=%s ip=%s", host, iqn, ip)

	// ── 2. Get or create attachment ───────────────────────────────────────
	// Read attachment_id from Cinder volume metadata first (source of truth).
	// If not present (e.g., after ControllerUnpublishVolume cleared it, or
	// on first publish after re-scheduling), create a new reserved attachment.
	// Fall back to PV volumeContext only as last resort.
	attachmentID := ""
	vol, err := cloud.GetVolume(ctx, volumeID)
	if err != nil {
		if cpoerrors.IsNotFound(err) {
			return nil, status.Errorf(codes.NotFound,
				"ControllerPublishVolume: volume %s not found", volumeID)
		}
		return nil, status.Errorf(codes.Internal,
			"ControllerPublishVolume: failed to get volume %s: %v", volumeID, err)
	}
	vopts := cloud.GetVolumeOpts()
	attachmentKey := metadataKey(vopts.MetadataPrefix, metaKeySuffixAttachmentID)
	attachmentID = vol.Metadata[attachmentKey]

	// No attachment in metadata — create one on-demand.
	// This happens after ControllerUnpublishVolume clears the metadata, or on
	// first publish of a volume that lost its metadata. Do NOT fall back to
	// PV volumeContext — it may contain a stale attachment_id from CreateVolume
	// that was already deleted by ControllerUnpublishVolume.
	if attachmentID == "" {
		klog.V(2).Infof("ControllerPublishVolume: no attachment found for volume %s, creating one", volumeID)
		newAttachmentID, createErr := cloud.CreateAttachment(ctx, volumeID)
		if createErr != nil {
			return nil, status.Errorf(codes.Internal,
				"ControllerPublishVolume: failed to create attachment for volume %s: %v", volumeID, createErr)
		}
		// Persist in Cinder metadata so ControllerUnpublishVolume can find it
		metaErr := cloud.SetVolumeMetadata(ctx, volumeID, map[string]string{
			attachmentKey: newAttachmentID,
		})
		if metaErr != nil {
			klog.Warningf("ControllerPublishVolume: failed to persist attachment_id in metadata: %v", metaErr)
		}
		attachmentID = newAttachmentID
	}

	klog.V(4).Infof("ControllerPublishVolume: using attachment %s for volume %s", attachmentID, volumeID)

	// ── 3. Update attachment with connector ──────────────────────────────
	iscsiOpts := cloud.GetISCSIOpts()

	platform := iscsiOpts.Platform
	if platform == "" {
		platform = openstack.DefaultConnectorPlatform
	}
	osType := iscsiOpts.OSType
	if osType == "" {
		osType = openstack.DefaultConnectorOSType
	}

	connector := &openstack.AttachmentConnector{
		Initiator: iqn,
		IP:        ip,
		Host:      host,
		Multipath: iscsiOpts.EnableMultipath,
		Platform:  platform,
		OSType:    osType,
	}

	connInfo, err := cloud.UpdateAttachmentConnector(ctx, attachmentID, connector)
	// If the attachment was deleted externally (e.g., stale metadata), create
	// a fresh one and retry the update once.
	if err != nil && cpoerrors.IsNotFound(err) {
		klog.Warningf("ControllerPublishVolume: attachment %s not found (deleted externally?), creating new one", attachmentID)
		newAttachmentID, createErr := cloud.CreateAttachment(ctx, volumeID)
		if createErr != nil {
			return nil, status.Errorf(codes.Internal,
				"ControllerPublishVolume: failed to create replacement attachment for volume %s: %v", volumeID, createErr)
		}
		metaErr := cloud.SetVolumeMetadata(ctx, volumeID, map[string]string{
			attachmentKey: newAttachmentID,
		})
		if metaErr != nil {
			klog.Warningf("ControllerPublishVolume: failed to persist replacement attachment_id in metadata: %v", metaErr)
		}
		attachmentID = newAttachmentID
		connInfo, err = cloud.UpdateAttachmentConnector(ctx, attachmentID, connector)
	}
	if err != nil {
		klog.Errorf("ControllerPublishVolume: failed to update connector on attachment %s: %v",
			attachmentID, err)
		return nil, status.Errorf(codes.Internal,
			"ControllerPublishVolume: failed to update attachment connector: %v", err)
	}

	// ── 4. Optionally complete attachment ────────────────────────────────
	err = cloud.CompleteAttachment(ctx, attachmentID)
	if err != nil {
		klog.Warningf("ControllerPublishVolume: CompleteAttachment failed for %s: %v (non-fatal)",
			attachmentID, err)
		// Non-fatal — some Cinder versions don't support 3.44
	}

	// ── 5. Validate driver_volume_type ───────────────────────────────────
	if err := ValidateISCSIConnectionInfo(connInfo); err != nil {
		return nil, status.Errorf(codes.Internal,
			"ControllerPublishVolume: %v", err)
	}

	// Build publish context for NodeStageVolume
	publishContext := BuildPublishContext(connInfo)

	klog.V(2).Infof("ControllerPublishVolume: volume %s published to node %s (portal=%s iqn=%s)",
		volumeID, host, connInfo.TargetPortal, connInfo.TargetIQN)

	return &csi.ControllerPublishVolumeResponse{
		PublishContext: publishContext,
	}, nil
}

// ── ControllerUnpublishVolume ────────────────────────────────────────────────

// ControllerUnpublishVolume deletes the current Cinder attachment and clears
// the attachment_id metadata. After this call, the Cinder volume returns to
// "available" state — ready for another ControllerPublishVolume or for direct
// use by OpenStack (e.g., Nova boot-from-volume).
//
// This is NOT a no-op (unlike the NFS driver). iSCSI targets are per-initiator,
// so the attachment must be deleted to trigger terminate_connection and remove
// the iSCSI target.
//
// Flow:
//  1. Get current attachment_id from volume metadata
//  2. Delete current attachment (triggers terminate_connection → iSCSI target removed)
//  3. Clear attachment_id from volume metadata
//
// Idempotency: If the old attachment is already deleted, succeed.
func (cs *controllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	klog.V(4).Infof("ControllerUnpublishVolume: called with args %+v", protosanitizer.StripSecrets(req))

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ControllerUnpublishVolume: volume ID must be provided")
	}

	cloud := cs.Cloud

	// ── 1. Get current attachment_id ─────────────────────────────────────
	vol, err := cloud.GetVolume(ctx, volumeID)
	if err != nil {
		if cpoerrors.IsNotFound(err) {
			klog.V(3).Infof("ControllerUnpublishVolume: volume %s not found, assuming already cleaned up", volumeID)
			return &csi.ControllerUnpublishVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal,
			"ControllerUnpublishVolume: failed to get volume %s: %v", volumeID, err)
	}

	vopts := cloud.GetVolumeOpts()
	attachmentKey := metadataKey(vopts.MetadataPrefix, metaKeySuffixAttachmentID)
	attachmentID := vol.Metadata[attachmentKey]

	// ── 2. Delete current attachment ─────────────────────────────────────
	if attachmentID != "" {
		err = cloud.DeleteAttachment(ctx, attachmentID)
		if err != nil {
			klog.Errorf("ControllerUnpublishVolume: failed to delete attachment %s: %v",
				attachmentID, err)
			return nil, status.Errorf(codes.Internal,
				"ControllerUnpublishVolume: failed to delete attachment %s: %v", attachmentID, err)
		}
		klog.V(4).Infof("ControllerUnpublishVolume: deleted attachment %s", attachmentID)
	} else {
		klog.V(3).Infof("ControllerUnpublishVolume: no attachment_id in metadata for volume %s, skipping delete", volumeID)
	}

	// ── 3. Clear attachment_id from metadata ─────────────────────────────
	err = cloud.DeleteVolumeMetadata(ctx, volumeID, []string{attachmentKey})
	if err != nil {
		klog.Errorf("ControllerUnpublishVolume: failed to clear metadata on volume %s: %v",
			volumeID, err)
		// Non-fatal — attachment is deleted, volume is available
	}

	klog.V(2).Infof("ControllerUnpublishVolume: detached volume %s (attachment %s deleted, volume now available)",
		volumeID, attachmentID)

	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// ── Capability Validation ────────────────────────────────────────────────────

// ValidateVolumeCapabilities checks if the requested capabilities match what
// the driver supports. Part of the CSI spec required RPCs.
func (cs *controllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	klog.V(4).Infof("ValidateVolumeCapabilities: called with args %+v", protosanitizer.StripSecrets(req))

	volumeID := req.GetVolumeId()
	if len(volumeID) == 0 {
		return nil, status.Error(codes.InvalidArgument, "ValidateVolumeCapabilities: volume ID must be provided")
	}

	reqCaps := req.GetVolumeCapabilities()
	if reqCaps == nil {
		return nil, status.Error(codes.InvalidArgument, "ValidateVolumeCapabilities: volume capabilities must be provided")
	}

	cloud := cs.Cloud

	// Verify the volume exists
	_, err := cloud.GetVolume(ctx, volumeID)
	if err != nil {
		if cpoerrors.IsNotFound(err) {
			return nil, status.Errorf(codes.NotFound, "ValidateVolumeCapabilities: volume %s not found", volumeID)
		}
		return nil, status.Errorf(codes.Internal, "ValidateVolumeCapabilities: failed to get volume %s: %v", volumeID, err)
	}

	// Check each requested capability
	for _, cap := range reqCaps {
		// Reject filesystem/mount requests
		if cap.GetMount() != nil {
			return &csi.ValidateVolumeCapabilitiesResponse{
				Message: "this driver only supports raw block volumes (volumeMode: Block)",
			}, nil
		}

		// Check access mode
		if cap.GetAccessMode() != nil {
			mode := cap.GetAccessMode().GetMode()
			supported := false
			for _, vcap := range cs.Driver.GetVolumeCapabilityAccessModes() {
				if mode == vcap.GetMode() {
					supported = true
					break
				}
			}
			if !supported {
				return &csi.ValidateVolumeCapabilitiesResponse{
					Message: fmt.Sprintf("access mode %v not supported", mode),
				}, nil
			}
		}
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: reqCaps,
		},
	}, nil
}

// ControllerGetCapabilities returns the capabilities of this controller service.
func (cs *controllerServer) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	klog.V(5).Infof("ControllerGetCapabilities called with req: %+v", req)
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: cs.Driver.cscap,
	}, nil
}

// ControllerModifyVolume is not supported.
func (cs *controllerServer) ControllerModifyVolume(ctx context.Context, req *csi.ControllerModifyVolumeRequest) (*csi.ControllerModifyVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "ControllerModifyVolume not supported")
}
