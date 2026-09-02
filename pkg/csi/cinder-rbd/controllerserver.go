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
	"strings"

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
		// Returning here without checking the reservation would report success
		// for a volume that is not reserved — the "volume created, reservation
		// not created" boundary. Reconcile it before returning, otherwise a
		// retry after a partial create converges on an unusable volume.
		attachmentID, err := cs.ensureReservation(ctx, &vol, attachmentKey)
		if err != nil {
			return nil, err
		}
		klog.V(3).Infof("CreateVolume: reusing existing volume %s (%s) with attachment record %s",
			vol.ID, volName, attachmentID)
		return &csi.CreateVolumeResponse{
			Volume: &csi.Volume{
				VolumeId:      vol.ID,
				CapacityBytes: int64(vol.Size) * gibiByte,
				VolumeContext: map[string]string{metaKeySuffixAttachmentID: attachmentID},
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

	// Persisting the attachment ID is fatal, not advisory. Cinder volume
	// metadata is the only record of driver ownership, and a listed record is
	// never adopted, so a volume whose ID was not persisted is unusable. Roll
	// the record back and return a retryable error rather than reporting success
	// with an ownership record nobody can attribute.
	if err := cs.Cloud.SetVolumeMetadata(ctx, vol.ID,
		map[string]string{attachmentKey: attachmentID}); err != nil {
		rollbackErr := cs.rollbackAttachment(ctx, vol.ID, attachmentID,
			status.Errorf(codes.Aborted,
				"created volume %s and attachment record %s but could not persist the attachment ID "+
					"in volume metadata: %v", vol.ID, attachmentID, err))
		// The volume is freshly created and empty, so discarding it is safe and
		// leaves nothing for the retry to reconcile.
		return nil, cleanup(rollbackErr)
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

// ensureReservation reconciles the reservation for a volume found by name.
//
// A volume that exists without a persisted attachment ID is the "volume created,
// reservation not created" boundary. Returning it as-is would report success for
// an unreserved volume, and because a listed record is never adopted, the volume
// would then fail closed at publish. Reconciling here is what makes a retry after
// a partial create converge.
func (cs *controllerServer) ensureReservation(ctx context.Context,
	vol *volumes.Volume, attachmentKey string) (string, error) {
	if id := vol.Metadata[attachmentKey]; id != "" {
		return id, nil
	}

	records, err := cs.Cloud.ListAttachmentsByVolume(ctx, vol.ID)
	if err != nil {
		return "", status.Errorf(codes.Internal,
			"failed to list attachment records for volume %s: %v", vol.ID, err)
	}
	if len(records) > 0 {
		// Same reasoning as resolveMissingAttachment: an unattributable record
		// must not be adopted.
		ids := make([]string, 0, len(records))
		for _, r := range records {
			ids = append(ids, r.ID)
		}
		duplicateAttachmentRecords.WithLabelValues().Inc()
		return "", status.Errorf(codes.FailedPrecondition,
			"volume %q already exists with no attachment ID in its metadata but %d attachment "+
				"record(s) present (%v); the driver will not adopt an unattributable record. "+
				"Operator resolution is required; see the operator runbook.",
			vol.Name, len(records), ids)
	}

	klog.V(2).Infof("CreateVolume: existing volume %s has no reservation, creating one", vol.ID)
	return cs.createAndPersistAttachment(ctx, vol.ID, attachmentKey, attachReasonOnDemand)
}

// deleteAttachmentIdempotent removes an attachment record, treating a missing
// record as success.
//
// A 404 means the goal — no such record — is already met. Reporting it as an
// error would make DeleteVolume and ControllerUnpublishVolume unable to make
// progress after a lost response or an operator's manual cleanup: both are
// retried indefinitely by their sidecars, so every retry would fail on the
// record that is already gone.
func (cs *controllerServer) deleteAttachmentIdempotent(ctx context.Context,
	volumeID, attachmentID string) error {
	err := cs.Cloud.DeleteAttachment(ctx, attachmentID)
	switch {
	case err == nil:
		attachmentRecordsDeleted.WithLabelValues().Inc()
		return nil
	case cpoerrors.IsNotFound(err):
		// Not counted as a deletion: nothing was deleted by this call.
		klog.V(3).Infof("attachment record %s for volume %s is already gone", attachmentID, volumeID)
		return nil
	default:
		return status.Errorf(codes.Internal,
			"failed to delete attachment record %s for volume %s: %v", attachmentID, volumeID, err)
	}
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
		if err := cs.deleteAttachmentIdempotent(ctx, volumeID, attachmentID); err != nil {
			return nil, err
		}
	}

	// Wait for the volume to leave in-use/reserved before touching anything
	// else.
	//
	// Note what this does and does not prove. Cinder status reflects attachment
	// RECORDS, not kernel state: if ControllerUnpublishVolume succeeded while
	// NodeUnstageVolume did not — an unreachable node, a force detach — the
	// volume reads available while a worker still holds a krbd mapping and the
	// Ceph exclusive lock. So this is a necessary precondition for releasing the
	// volume, not evidence that every node has released it. Proving that
	// requires the cross-node no-map check still open as Q8.
	if err := cs.Cloud.WaitVolumeTargetStatus(ctx, volumeID,
		[]string{openstack.VolumeAvailableStatus}, vopts.DetachTimeout); err != nil {
		return nil, status.Errorf(codes.Aborted,
			"volume %s has not returned to %s; refusing to finish delete while a mapping may remain: %v",
			volumeID, openstack.VolumeAvailableStatus, err)
	}

	// Retain-only. Physical deletion of the Cinder volume is deliberately not
	// performed by this driver:
	//
	//   * The migration contract requires the volume to survive PVC deletion so
	//     the Blueprint can attach it to the target VM.
	//   * The controller cannot prove that no worker still holds a kernel RBD
	//     mapping (see the note above), and deleting an image out from under a
	//     live mapping is a data-corruption path rather than an error.
	//
	// Because delete-volume-mode is rejected at startup, this is the only
	// reachable path. A per-volume cleanupVolume request is reported and ignored
	// rather than failing: retain always succeeds, so nothing is left stuck.
	if strings.EqualFold(strings.TrimSpace(vol.Metadata[cleanupKey]), "true") {
		klog.Warningf("DeleteVolume: volume %s requests %s=true, but this driver retains volumes "+
			"only; the Cinder volume is being kept. Delete it out of band once you have confirmed "+
			"no node holds a mapping.", volumeID, cleanupKey)
	}

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

	// Recovery for a prior attempt whose response was lost: if the record
	// already carries usable connection information, reuse it instead of
	// updating again. A second update on a record that Cinder has already moved
	// to attached/in-use can be rejected outright, which would strand a volume
	// that is in fact correctly attached.
	connInfo, reused := cs.reuseStoredConnection(ctx, attachmentID)

	if !reused {
		connInfo, err = cs.Cloud.UpdateAttachmentConnector(ctx, attachmentID, connector)
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
			attachmentID, err = cs.createAndPersistAttachment(ctx, volumeID, attachmentKey,
				attachReasonReplacement)
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
	}

	// Validate BEFORE completing. Completion advances the record to in-use, and
	// Cinder can refuse a later update on a completed record — so completing
	// first would make an unusable attachment hard to retry out of.
	if err := ValidateRBDConnectionInfo(connInfo, cs.Cloud.GetRBDOpts()); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"unusable RBD connection information for volume %s: %v", volumeID, err)
	}

	// Completion moves the volume to in-use and needs microversion 3.44. It is
	// optional, so failures are logged and not returned.
	if err := cs.Cloud.CompleteAttachment(ctx, attachmentID); err != nil {
		klog.V(3).Infof("ControllerPublishVolume: completion of attachment record %s skipped or failed: %v",
			attachmentID, err)
	}

	klog.V(2).Infof("ControllerPublishVolume: volume %s published to node %s via attachment record %s (%s/%s@%s)",
		volumeID, host, attachmentID, connInfo.Pool, connInfo.Image, connInfo.ClusterFSID)

	return &csi.ControllerPublishVolumeResponse{
		PublishContext: BuildPublishContext(connInfo, attachmentID),
	}, nil
}

// reuseStoredConnection returns the connection information already recorded on
// an attachment, when it is present and usable.
//
// This is the recovery path for "connector update succeeded, CSI response lost".
// Falling through to a fresh update is always safe, so any doubt returns false.
func (cs *controllerServer) reuseStoredConnection(ctx context.Context,
	attachmentID string) (*openstack.RBDConnectionInfo, bool) {
	att, err := cs.Cloud.GetAttachment(ctx, attachmentID)
	if err != nil {
		// Includes NotFound: the update path handles a stale record by
		// replacing it, so nothing is decided here.
		klog.V(4).Infof("ControllerPublishVolume: attachment record %s not readable for reuse: %v",
			attachmentID, err)
		return nil, false
	}
	if att.ConnectionInfo == nil {
		// A reserved record legitimately has none yet.
		return nil, false
	}
	if err := ValidateRBDConnectionInfo(att.ConnectionInfo, cs.Cloud.GetRBDOpts()); err != nil {
		// Stored information is unusable. An update may return corrected
		// information, so try that rather than failing here.
		klog.V(3).Infof("ControllerPublishVolume: stored connection information on record %s "+
			"is unusable (%v); re-updating the connector", attachmentID, err)
		return nil, false
	}

	// Reuse is sound here only because RBD connection information is
	// host-independent: pool, image, monitors and FSID do not vary by connector
	// host. Do NOT copy this pattern into a driver whose connection information
	// is per-initiator, such as the iSCSI sibling, where reusing a record
	// attached to another host would hand back the wrong target.
	klog.V(2).Infof("ControllerPublishVolume: reusing stored connection information on record %s",
		attachmentID)
	return att.ConnectionInfo, true
}

// resolveMissingAttachment decides what to do when volume metadata carries no
// attachment ID.
//
// Only the zero-record case may proceed. A record found by listing is NOT
// adopted: a connector-less reserved attachment carries no ownership marker —
// its fields are id, volume_uuid, status and a nil instance, none of which the
// driver can stamp or read back to prove authorship. "Exactly one record exists"
// is evidence of a record, not of ownership; a second driver deployment against
// the same Cinder project, or an operator running the CLI, produces the same
// shape. Adopting it could attach a record another actor owns.
func (cs *controllerServer) resolveMissingAttachment(ctx context.Context,
	volumeID, attachmentKey string) (string, error) {
	records, err := cs.Cloud.ListAttachmentsByVolume(ctx, volumeID)
	if err != nil {
		return "", status.Errorf(codes.Internal,
			"failed to list attachment records for volume %s: %v", volumeID, err)
	}

	if len(records) == 0 {
		klog.V(2).Infof("ControllerPublishVolume: volume %s has no attachment record, creating one", volumeID)
		return cs.createAndPersistAttachment(ctx, volumeID, attachmentKey, attachReasonOnDemand)
	}

	// Fail closed. This is an operator-resolution boundary, not a recoverable
	// one; see the operator runbook.
	ids := make([]string, 0, len(records))
	for _, r := range records {
		ids = append(ids, r.ID)
	}
	duplicateAttachmentRecords.WithLabelValues().Inc()
	return "", status.Errorf(codes.FailedPrecondition,
		"volume %s has no attachment ID in its metadata but %d attachment record(s) exist (%v). "+
			"A connector-less Cinder attachment carries no ownership marker, so the driver will not "+
			"adopt one. Operator resolution is required; see the operator runbook.",
		volumeID, len(records), ids)
}

// createAndPersistAttachment creates a reserved record and stores its ID.
//
// Persisting the ID is fatal: without it the record is unattributable, and
// because a listed record is never adopted, the volume would be permanently
// unusable. On failure the record is rolled back so the retry starts clean.
func (cs *controllerServer) createAndPersistAttachment(ctx context.Context,
	volumeID, attachmentKey, reason string) (string, error) {
	attachmentID, err := cs.Cloud.CreateAttachment(ctx, volumeID)
	if err != nil {
		return "", status.Errorf(codes.Internal,
			"failed to create attachment record for volume %s: %v", volumeID, err)
	}

	if err := cs.Cloud.SetVolumeMetadata(ctx, volumeID,
		map[string]string{attachmentKey: attachmentID}); err != nil {
		return "", cs.rollbackAttachment(ctx, volumeID, attachmentID,
			status.Errorf(codes.Aborted,
				"created attachment record %s for volume %s but could not persist its ID in volume "+
					"metadata (%v); rolled back so the retry starts clean", attachmentID, volumeID, err))
	}

	attachmentRecordsCreated.WithLabelValues(reason).Inc()
	return attachmentID, nil
}

// rollbackAttachment deletes a record whose ownership could not be recorded.
//
// If the deletion cannot be confirmed, the returned error says so explicitly:
// an unattributable record is left behind, and the next publish will fail closed
// on it rather than adopt it. That is the intended outcome — it is visible and
// requires a decision, instead of silently attaching a record nobody owns.
func (cs *controllerServer) rollbackAttachment(ctx context.Context,
	volumeID, attachmentID string, cause error) error {
	// A record that is already gone satisfies the rollback, so 404 is success
	// here too; treating it as failure would escalate a clean state to operator
	// resolution.
	if delErr := cs.deleteAttachmentIdempotent(ctx, volumeID, attachmentID); delErr != nil {
		klog.Errorf("ControllerPublishVolume: could not roll back attachment record %s "+
			"for volume %s: %v", attachmentID, volumeID, delErr)
		return status.Errorf(codes.FailedPrecondition,
			"%s; additionally, the record could not be deleted (%v), so volume %s now has an "+
				"unattributable attachment record. Operator resolution is required; "+
				"see the operator runbook.",
			status.Convert(cause).Message(), status.Convert(delErr).Message(), volumeID)
	}
	return cause
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
	} else if err := cs.deleteAttachmentIdempotent(ctx, volumeID, attachmentID); err != nil {
		return nil, err
	}

	// Clearing the metadata is fatal, not advisory.
	//
	// This key is the driver's authoritative ownership record. Leaving it naming
	// a record that no longer exists means the driver's own source of truth is
	// wrong, and the migration handoff reads that metadata. Returning an error
	// has the attacher retry until Cinder accepts the change; the retry is safe
	// because deleting an already-deleted record now succeeds, so the loop
	// converges instead of failing on the missing record.
	//
	// Ordering is load-bearing and must not be reversed: the record is deleted
	// first so that a failure here leaves metadata pointing at nothing, which the
	// next publish repairs. Clearing metadata first and then failing to delete
	// the record would strand an unattributable record that no publish may adopt.
	if err := cs.Cloud.DeleteVolumeMetadata(ctx, volumeID, []string{attachmentKey}); err != nil {
		return nil, status.Errorf(codes.Aborted,
			"detached volume %s but failed to clear %s from its metadata: %v",
			volumeID, attachmentKey, err)
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
