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
	"math"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-rbd/openstack"
	"k8s.io/cloud-provider-openstack/pkg/util"
	cpoerrors "k8s.io/cloud-provider-openstack/pkg/util/errors"
	"k8s.io/klog/v2"
)

// Metadata key suffixes. The full key is <prefix>.<suffix> with prefix from
// [Volume] metadata-prefix, defaulting to "csi.rbd". A single prefix governs
// every driver-owned key so the sibling drivers cannot collide on one volume.
const (
	metaKeySuffixAttachmentID  = "attachment_id"
	metaKeySuffixCleanupVolume = "cleanupVolume"
)

// StorageClass parameter names.
const (
	paramVolumeType   = "type"
	paramAvailability = "availability"
)

// gibiByte is the Cinder allocation unit: volumes are sized in whole GiB.
const gibiByte int64 = 1024 * 1024 * 1024

// metadataKey composes a driver-owned Cinder volume metadata key.
func metadataKey(prefix, suffix string) string {
	if prefix == "" {
		prefix = openstack.DefaultMetadataPrefix
	}
	return prefix + "." + suffix
}

type controllerServer struct {
	Driver *Driver
	Cloud  openstack.IOpenStackRBD
	csi.UnimplementedControllerServer
}

// validateBlockOnlyCapabilities rejects everything this driver cannot serve.
//
// Filesystem mode is rejected because the driver only maps raw block devices,
// and any access mode beyond SINGLE_NODE_WRITER is rejected because the Ceph
// exclusive lock permits exactly one writer. Accepting either would bind a PVC
// the driver then fails to stage, which surfaces as a stuck pod rather than a
// clear provisioning error.
func validateBlockOnlyCapabilities(caps []*csi.VolumeCapability) error {
	if len(caps) == 0 {
		return status.Error(codes.InvalidArgument, "volume capabilities must be provided")
	}
	for _, c := range caps {
		if c.GetMount() != nil {
			return status.Error(codes.InvalidArgument,
				"filesystem volumes are not supported: this driver serves raw block volumes only "+
					"(set volumeMode: Block on the PVC)")
		}
		if c.GetBlock() == nil {
			return status.Error(codes.InvalidArgument, "only block volume capabilities are supported")
		}
		if mode := c.GetAccessMode(); mode != nil &&
			mode.Mode != csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER {
			return status.Errorf(codes.InvalidArgument,
				"access mode %s is not supported: only SINGLE_NODE_WRITER is supported", mode.Mode)
		}
	}
	return nil
}

// ── CreateVolume ─────────────────────────────────────────────────────────────

// CreateVolume creates a Cinder volume and an initial reserved attachment
// record, then records the attachment ID in Cinder volume metadata.
func (cs *controllerServer) CreateVolume(ctx context.Context,
	req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	klog.V(4).Infof("CreateVolume: called with args %+v", protosanitizer.StripSecrets(req))

	volName := req.GetName()
	if volName == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name must be provided")
	}
	if err := validateBlockOnlyCapabilities(req.GetVolumeCapabilities()); err != nil {
		return nil, err
	}

	vopts := cs.Cloud.GetVolumeOpts()
	attachmentKey := metadataKey(vopts.MetadataPrefix, metaKeySuffixAttachmentID)

	// Size: Cinder allocates whole GiB, so round up rather than truncate.
	sizeBytes := gibiByte
	if cr := req.GetCapacityRange(); cr != nil {
		required := cr.GetRequiredBytes()
		if required < 0 {
			return nil, status.Errorf(codes.InvalidArgument,
				"required_bytes must not be negative, got %d", required)
		}
		if required > 0 {
			sizeBytes = util.RoundUpSize(required, gibiByte) * gibiByte
		}
		// Rounding up to a whole GiB can push the volume past an explicit
		// limit. The CSI spec requires OUT_OF_RANGE when the range cannot be
		// satisfied; silently over-allocating would violate the PVC contract.
		if limit := cr.GetLimitBytes(); limit > 0 && sizeBytes > limit {
			return nil, status.Errorf(codes.OutOfRange,
				"cannot satisfy capacity range: %d bytes rounds up to %d bytes (whole GiB), "+
					"which exceeds limit_bytes %d", required, sizeBytes, limit)
		}
	}
	// Cinder sizes are int GiB. Guard the narrowing explicitly: the repo
	// cross-compiles to 32-bit arm, where int is 32 bits.
	sizeGiBInt64 := sizeBytes / gibiByte
	if sizeGiBInt64 <= 0 || sizeGiBInt64 > int64(math.MaxInt32) {
		return nil, status.Errorf(codes.OutOfRange,
			"requested size %d bytes (%d GiB) is out of the supported range", sizeBytes, sizeGiBInt64)
	}
	sizeGiB := int(sizeGiBInt64)

	// Volume type comes from the StorageClass, never hard-coded.
	params := req.GetParameters()
	volType := params[paramVolumeType]
	if volType == "" {
		volType = vopts.DefaultVolumeType
	}

	// Idempotency: the provisioner retries with the same name.
	existing, err := cs.Cloud.GetVolumesByName(ctx, volName)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to look up volume %q: %v", volName, err)
	}
	switch {
	case len(existing) > 1:
		return nil, status.Errorf(codes.Internal,
			"found %d volumes named %q; cannot determine which to use", len(existing), volName)
	case len(existing) == 1:
		vol := existing[0]
		if vol.Size != sizeGiB {
			return nil, status.Errorf(codes.AlreadyExists,
				"volume %q already exists with size %dGiB, requested %dGiB", volName, vol.Size, sizeGiB)
		}
		klog.V(3).Infof("CreateVolume: reusing existing volume %s (%s)", vol.ID, volName)
		return &csi.CreateVolumeResponse{
			Volume: &csi.Volume{
				VolumeId:      vol.ID,
				CapacityBytes: int64(vol.Size) * gibiByte,
				VolumeContext: map[string]string{metaKeySuffixAttachmentID: vol.Metadata[attachmentKey]},
			},
		}, nil
	}

	createOpts := &volumes.CreateOpts{
		Name:             volName,
		Size:             sizeGiB,
		VolumeType:       volType,
		AvailabilityZone: params[paramAvailability],
	}
	vol, err := cs.Cloud.CreateVolume(ctx, createOpts, nil)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create volume %q: %v", volName, err)
	}

	// Past this point a Cinder volume exists. Any failure attempts cleanup so a
	// retry does not accumulate orphans, but a failed cleanup is only logged:
	// the original error is what the caller needs to see.
	cleanup := func(reason error) error {
		if delErr := cs.Cloud.DeleteVolume(ctx, vol.ID); delErr != nil {
			klog.Errorf("CreateVolume: failed to clean up volume %s after error: %v", vol.ID, delErr)
		}
		return reason
	}

	if err := cs.Cloud.WaitVolumeTargetStatus(ctx, vol.ID,
		[]string{openstack.VolumeAvailableStatus}, vopts.CreateTimeout); err != nil {
		return nil, cleanup(status.Errorf(codes.Internal,
			"volume %s did not become available: %v", vol.ID, err))
	}

	attachmentID, err := cs.Cloud.CreateAttachment(ctx, vol.ID)
	if err != nil {
		return nil, cleanup(status.Errorf(codes.Internal,
			"failed to create reserved attachment record for volume %s: %v", vol.ID, err))
	}

	// A failed metadata write is not fatal: ControllerPublishVolume recovers by
	// listing the volume's attachment records. Failing here would leak the
	// record we just created.
	if err := cs.Cloud.SetVolumeMetadata(ctx, vol.ID,
		map[string]string{attachmentKey: attachmentID}); err != nil {
		klog.Warningf("CreateVolume: failed to persist %s on volume %s: %v; "+
			"publish will recover by listing attachment records", attachmentKey, vol.ID, err)
	}

	attachmentRecordsCreated.WithLabelValues(attachReasonCreateVolume).Inc()
	klog.V(2).Infof("CreateVolume: created volume %s (%s) with reserved attachment record %s",
		vol.ID, volName, attachmentID)

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      vol.ID,
			CapacityBytes: sizeBytes,
			// Informational only. This value goes stale after the first
			// unpublish; Cinder metadata is the source of truth.
			VolumeContext: map[string]string{metaKeySuffixAttachmentID: attachmentID},
		},
	}, nil
}

// ── DeleteVolume ─────────────────────────────────────────────────────────────

// DeleteVolume removes the attachment record and then either retains or deletes
// the Cinder volume.
//
// Retain is the default because the migration Blueprint attaches the volume to
// the target VM after the PVC is gone.
func (cs *controllerServer) DeleteVolume(ctx context.Context,
	req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	klog.V(4).Infof("DeleteVolume: called with args %+v", protosanitizer.StripSecrets(req))

	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID must be provided")
	}

	vol, err := cs.Cloud.GetVolume(ctx, volumeID)
	if err != nil {
		if cpoerrors.IsNotFound(err) {
			klog.V(3).Infof("DeleteVolume: volume %s already gone", volumeID)
			return &csi.DeleteVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal, "failed to get volume %s: %v", volumeID, err)
	}

	vopts := cs.Cloud.GetVolumeOpts()
	attachmentKey := metadataKey(vopts.MetadataPrefix, metaKeySuffixAttachmentID)
	cleanupKey := metadataKey(vopts.MetadataPrefix, metaKeySuffixCleanupVolume)
	attachmentID := vol.Metadata[attachmentKey]

	if attachmentID != "" {
		if err := cs.Cloud.DeleteAttachment(ctx, attachmentID); err != nil {
			return nil, status.Errorf(codes.Internal,
				"failed to delete attachment record %s for volume %s: %v", attachmentID, volumeID, err)
		}
	}

	// Wait for the volume to leave in-use/reserved before deciding anything
	// else. Deleting a Cinder volume whose RBD image is still mapped by a
	// kernel client risks data corruption, not merely an API error, so a
	// retryable Aborted is returned instead of proceeding hopefully.
	if err := cs.Cloud.WaitVolumeTargetStatus(ctx, volumeID,
		[]string{openstack.VolumeAvailableStatus}, vopts.DetachTimeout); err != nil {
		return nil, status.Errorf(codes.Aborted,
			"volume %s has not returned to %s; refusing to finish delete while a mapping may remain: %v",
			volumeID, openstack.VolumeAvailableStatus, err)
	}

	if vopts.ShouldDeleteVolume(vol.Metadata[cleanupKey]) {
		if err := cs.Cloud.DeleteVolume(ctx, volumeID); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to delete volume %s: %v", volumeID, err)
		}
		volumesDeleted.WithLabelValues().Inc()
		klog.V(2).Infof("DeleteVolume: deleted Cinder volume %s (explicit cleanup)", volumeID)
		return &csi.DeleteVolumeResponse{}, nil
	}

	// Retain: strip CSI ownership only, leaving the volume available.
	if err := cs.Cloud.DeleteVolumeMetadata(ctx, volumeID, []string{attachmentKey, cleanupKey}); err != nil {
		klog.Warningf("DeleteVolume: failed to strip CSI metadata from retained volume %s: %v",
			volumeID, err)
	}
	volumesRetained.WithLabelValues().Inc()
	klog.V(2).Infof("DeleteVolume: retained Cinder volume %s for migration handoff", volumeID)
	return &csi.DeleteVolumeResponse{}, nil
}

// ── ControllerPublishVolume ──────────────────────────────────────────────────

// ControllerPublishVolume attaches the node's connector to an attachment record
// and returns the RBD connection information.
func (cs *controllerServer) ControllerPublishVolume(ctx context.Context,
	req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	klog.V(4).Infof("ControllerPublishVolume: called with args %+v", protosanitizer.StripSecrets(req))

	volumeID := req.GetVolumeId()
	nodeID := req.GetNodeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID must be provided")
	}
	if nodeID == "" {
		return nil, status.Error(codes.InvalidArgument, "node ID must be provided")
	}
	if capability := req.GetVolumeCapability(); capability == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability must be provided")
	} else if err := validateBlockOnlyCapabilities([]*csi.VolumeCapability{capability}); err != nil {
		return nil, err
	}

	host, _, err := ParseNodeID(nodeID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid node ID %q: %v", nodeID, err)
	}

	vol, err := cs.Cloud.GetVolume(ctx, volumeID)
	if err != nil {
		if cpoerrors.IsNotFound(err) {
			return nil, status.Errorf(codes.NotFound, "volume %s not found", volumeID)
		}
		return nil, status.Errorf(codes.Internal, "failed to get volume %s: %v", volumeID, err)
	}

	vopts := cs.Cloud.GetVolumeOpts()
	attachmentKey := metadataKey(vopts.MetadataPrefix, metaKeySuffixAttachmentID)

	// Cinder metadata is the only source of truth. The attachment_id in the PV
	// volumeContext is immutable and goes stale after the first unpublish, so it
	// is deliberately not consulted.
	attachmentID := vol.Metadata[attachmentKey]
	if attachmentID == "" {
		attachmentID, err = cs.resolveMissingAttachment(ctx, volumeID, attachmentKey)
		if err != nil {
			return nil, err
		}
	}

	connector := &openstack.AttachmentConnector{Host: host}

	connInfo, err := cs.Cloud.UpdateAttachmentConnector(ctx, attachmentID, connector)
	if err != nil {
		if !cpoerrors.IsNotFound(err) {
			return nil, status.Errorf(codes.Internal,
				"failed to update attachment record %s for volume %s: %v", attachmentID, volumeID, err)
		}
		// The record was deleted behind our back. Create a replacement and
		// retry exactly once: retrying indefinitely would mask a backend that
		// deletes records as fast as we create them.
		klog.V(2).Infof("ControllerPublishVolume: attachment record %s is gone, creating a replacement",
			attachmentID)
		attachmentID, err = cs.createAndPersistAttachment(ctx, volumeID, attachmentKey)
		if err != nil {
			return nil, err
		}
		connInfo, err = cs.Cloud.UpdateAttachmentConnector(ctx, attachmentID, connector)
		if err != nil {
			return nil, status.Errorf(codes.Internal,
				"failed to update replacement attachment record %s for volume %s: %v",
				attachmentID, volumeID, err)
		}
	}

	// Completion moves the volume to in-use and needs microversion 3.44. It is
	// optional, so failures are logged and not returned.
	if err := cs.Cloud.CompleteAttachment(ctx, attachmentID); err != nil {
		klog.V(3).Infof("ControllerPublishVolume: completion of attachment record %s skipped or failed: %v",
			attachmentID, err)
	}

	if err := ValidateRBDConnectionInfo(connInfo, cs.Cloud.GetRBDOpts()); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"unusable RBD connection information for volume %s: %v", volumeID, err)
	}

	klog.V(2).Infof("ControllerPublishVolume: volume %s published to node %s via attachment record %s (%s/%s@%s)",
		volumeID, host, attachmentID, connInfo.Pool, connInfo.Image, connInfo.ClusterFSID)

	return &csi.ControllerPublishVolumeResponse{
		PublishContext: BuildPublishContext(connInfo, attachmentID),
	}, nil
}

// resolveMissingAttachment recovers when volume metadata carries no attachment
// ID: either a reservation exists that we lost track of, or none exists.
//
// Listing first is what prevents duplicate records. Creating unconditionally
// would leave the earlier reservation holding the volume forever.
func (cs *controllerServer) resolveMissingAttachment(ctx context.Context,
	volumeID, attachmentKey string) (string, error) {
	records, err := cs.Cloud.ListAttachmentsByVolume(ctx, volumeID)
	if err != nil {
		return "", status.Errorf(codes.Internal,
			"failed to list attachment records for volume %s: %v", volumeID, err)
	}

	switch len(records) {
	case 0:
		klog.V(2).Infof("ControllerPublishVolume: volume %s has no attachment record, creating one", volumeID)
		return cs.createAndPersistAttachment(ctx, volumeID, attachmentKey)
	case 1:
		adopted := records[0].ID
		attachmentRecordsCreated.WithLabelValues(attachReasonAdopted).Inc()
		klog.V(2).Infof("ControllerPublishVolume: adopting existing attachment record %s for volume %s "+
			"and restoring its metadata", adopted, volumeID)
		if err := cs.Cloud.SetVolumeMetadata(ctx, volumeID,
			map[string]string{attachmentKey: adopted}); err != nil {
			klog.Warningf("ControllerPublishVolume: failed to restore %s on volume %s: %v",
				attachmentKey, volumeID, err)
		}
		return adopted, nil
	default:
		// Choosing arbitrarily could attach a record another actor owns, so
		// this requires operator resolution and no record is mutated.
		ids := make([]string, 0, len(records))
		for _, r := range records {
			ids = append(ids, r.ID)
		}
		duplicateAttachmentRecords.WithLabelValues().Inc()
		return "", status.Errorf(codes.FailedPrecondition,
			"volume %s has %d attachment records (%v); refusing to guess which one to use — "+
				"operator resolution required", volumeID, len(records), ids)
	}
}

// createAndPersistAttachment creates a reserved record and stores its ID.
func (cs *controllerServer) createAndPersistAttachment(ctx context.Context,
	volumeID, attachmentKey string) (string, error) {
	attachmentID, err := cs.Cloud.CreateAttachment(ctx, volumeID)
	if err != nil {
		return "", status.Errorf(codes.Internal,
			"failed to create attachment record for volume %s: %v", volumeID, err)
	}
	if err := cs.Cloud.SetVolumeMetadata(ctx, volumeID,
		map[string]string{attachmentKey: attachmentID}); err != nil {
		// Not fatal: the next publish recovers by listing records.
		klog.Warningf("ControllerPublishVolume: failed to persist %s on volume %s: %v",
			attachmentKey, volumeID, err)
	}
	return attachmentID, nil
}

// ── ControllerUnpublishVolume ────────────────────────────────────────────────

// ControllerUnpublishVolume deletes the current attachment record and clears its
// metadata, returning the Cinder volume to available.
//
// This is not a no-op and not a rotation: no replacement record is created. The
// available window between migration pods is required so a multi-phase CDI
// workflow can advance.
func (cs *controllerServer) ControllerUnpublishVolume(ctx context.Context,
	req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	klog.V(4).Infof("ControllerUnpublishVolume: called with args %+v", protosanitizer.StripSecrets(req))

	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID must be provided")
	}

	vol, err := cs.Cloud.GetVolume(ctx, volumeID)
	if err != nil {
		if cpoerrors.IsNotFound(err) {
			klog.V(3).Infof("ControllerUnpublishVolume: volume %s not found, nothing to detach", volumeID)
			return &csi.ControllerUnpublishVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal, "failed to get volume %s: %v", volumeID, err)
	}

	vopts := cs.Cloud.GetVolumeOpts()
	attachmentKey := metadataKey(vopts.MetadataPrefix, metaKeySuffixAttachmentID)
	attachmentID := vol.Metadata[attachmentKey]

	if attachmentID == "" {
		klog.V(3).Infof("ControllerUnpublishVolume: volume %s has no attachment record in metadata", volumeID)
	} else if err := cs.Cloud.DeleteAttachment(ctx, attachmentID); err != nil {
		return nil, status.Errorf(codes.Internal,
			"failed to delete attachment record %s for volume %s: %v", attachmentID, volumeID, err)
	} else {
		attachmentRecordsDeleted.WithLabelValues().Inc()
	}

	if err := cs.Cloud.DeleteVolumeMetadata(ctx, volumeID, []string{attachmentKey}); err != nil {
		// Non-fatal: the record is gone, which is what releases the volume. A
		// stale metadata key is recovered from on the next publish.
		klog.Warningf("ControllerUnpublishVolume: failed to clear %s on volume %s: %v",
			attachmentKey, volumeID, err)
	}

	// The next migration pod must not be published while the volume is still
	// reserved or in-use, so wait for available and let the attacher retry.
	if err := cs.Cloud.WaitVolumeTargetStatus(ctx, volumeID,
		[]string{openstack.VolumeAvailableStatus}, vopts.DetachTimeout); err != nil {
		return nil, status.Errorf(codes.Aborted,
			"volume %s has not returned to %s after detaching: %v",
			volumeID, openstack.VolumeAvailableStatus, err)
	}

	klog.V(2).Infof("ControllerUnpublishVolume: volume %s detached and returned to %s",
		volumeID, openstack.VolumeAvailableStatus)
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}

// ── Capability RPCs ──────────────────────────────────────────────────────────

// ValidateVolumeCapabilities reports whether the requested capabilities are
// supported.
//
// Unsupported capabilities produce a response with no Confirmed field plus a
// human-readable Message, per the CSI spec, rather than a gRPC error.
func (cs *controllerServer) ValidateVolumeCapabilities(ctx context.Context,
	req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	klog.V(4).Infof("ValidateVolumeCapabilities: called with args %+v", protosanitizer.StripSecrets(req))

	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID must be provided")
	}
	caps := req.GetVolumeCapabilities()
	if len(caps) == 0 {
		return nil, status.Error(codes.InvalidArgument, "volume capabilities must be provided")
	}

	if _, err := cs.Cloud.GetVolume(ctx, volumeID); err != nil {
		if cpoerrors.IsNotFound(err) {
			return nil, status.Errorf(codes.NotFound, "volume %s not found", volumeID)
		}
		return nil, status.Errorf(codes.Internal, "failed to get volume %s: %v", volumeID, err)
	}

	if err := validateBlockOnlyCapabilities(caps); err != nil {
		return &csi.ValidateVolumeCapabilitiesResponse{
			Message: fmt.Sprintf("unsupported capability: %v", status.Convert(err).Message()),
		}, nil
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: caps,
		},
	}, nil
}

// ControllerGetCapabilities returns the advertised controller capabilities.
func (cs *controllerServer) ControllerGetCapabilities(_ context.Context,
	_ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	klog.V(5).Info("ControllerGetCapabilities called")
	return &csi.ControllerGetCapabilitiesResponse{Capabilities: cs.Driver.cscap}, nil
}

// ControllerExpandVolume, snapshots and clones stay Unimplemented via the
// embedded UnimplementedControllerServer: they are non-goals and are not
// advertised, so a stub that pretends otherwise would be misleading.
