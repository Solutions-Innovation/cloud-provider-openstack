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
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/kubernetes-csi/csi-lib-utils/protosanitizer"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-rbd/openstack"
	"k8s.io/cloud-provider-openstack/pkg/util/mount"
	"k8s.io/klog/v2"
)

type nodeServer struct {
	Driver      *Driver
	Opts        openstack.RBDOpts
	VolumeOpts  openstack.VolumeOpts
	Mapper      RBDMapper
	Credentials CephCredentialProvider
	Mounter     mount.IMount
	Staging     *stagingStore
	Isolation   *isolationSet

	// Injection point for unit testing NodeGetInfo. There is deliberately no
	// getInterfaceIPFunc: the Cinder RBD connector requires only "host".
	hostnameFunc func() (string, error)

	csi.UnimplementedNodeServer
}

// NodeGetInfo reports this node's identity.
//
// The node ID is the bare hostname. Unlike the iSCSI sibling there is no
// initiator identity to carry, and the validated backend accepts a connector
// containing only "host".
func (ns *nodeServer) NodeGetInfo(_ context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	klog.V(5).Info("NodeGetInfo called")

	hostnameFn := ns.hostnameFunc
	if hostnameFn == nil {
		hostnameFn = os.Hostname
	}
	hostname, err := hostnameFn()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to determine hostname: %v", err)
	}
	if hostname == "" {
		return nil, status.Error(codes.Internal, "hostname is empty")
	}

	return &csi.NodeGetInfoResponse{
		NodeId:            BuildNodeID(hostname, ""),
		MaxVolumesPerNode: ns.Opts.MaxVolumesPerNode,
	}, nil
}

// NodeGetCapabilities returns the advertised node capabilities.
func (ns *nodeServer) NodeGetCapabilities(_ context.Context,
	_ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	klog.V(5).Info("NodeGetCapabilities called")
	return &csi.NodeGetCapabilitiesResponse{Capabilities: ns.Driver.nscap}, nil
}

// ── NodeStageVolume ──────────────────────────────────────────────────────────

// NodeStageVolume maps the RBD image exclusively and records the mapping.
//
// Idempotency is keyed on live kernel state, not on the staging record: a
// missing record is never proof that no mapping exists.
func (ns *nodeServer) NodeStageVolume(ctx context.Context,
	req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	klog.V(4).Infof("NodeStageVolume: called with args %+v", protosanitizer.StripSecrets(req))

	volumeID := req.GetVolumeId()
	stagingPath := req.GetStagingTargetPath()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID must be provided")
	}
	if stagingPath == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path must be provided")
	}
	capability := req.GetVolumeCapability()
	if capability == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability must be provided")
	}
	if err := validateBlockOnlyCapabilities([]*csi.VolumeCapability{capability}); err != nil {
		return nil, err
	}

	// Step 2: rebuild and validate the connection information.
	ci, attachmentID, err := ParsePublishContext(req.GetPublishContext())
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid publish context: %v", err)
	}
	if err := ValidateRBDConnectionInfo(ci, ns.Opts); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"unusable RBD connection information: %v", err)
	}
	want := ImageIdentity{
		ClusterFSID: ci.ClusterFSID,
		ClusterName: ci.ClusterName,
		Pool:        ci.Pool,
		Image:       ci.Image,
	}

	// Step 3: load the credential and enforce the entity match BEFORE any map
	// attempt, so a configuration error is reported as such rather than as an
	// opaque Ceph authentication failure.
	cred, err := ns.Credentials.Load(ctx, ci.AuthUsername)
	if err != nil {
		if errors.Is(err, ErrCredentialEntityMismatch) {
			return nil, status.Errorf(codes.FailedPrecondition, "%v", err)
		}
		return nil, status.Errorf(codes.FailedPrecondition, "ceph credential unavailable: %v", err)
	}

	// A volume isolated by reconciliation is refused up front. Re-discovering
	// the same conflict on every stage attempt would produce identical failures
	// with no indication that the node is degraded.
	if entry, isolated := ns.Isolation.Get(volumeID); isolated {
		return nil, status.Errorf(codes.FailedPrecondition,
			"volume %s is isolated on this node and will not be served: %s. "+
				"Operator resolution is required; see the operator runbook.",
			volumeID, entry.Detail)
	}

	// Step 4: reuse a live mapping only if this driver can prove it created it.
	//
	// The intent is read before the kernel is consulted so that a mapping found
	// without one is treated as foreign rather than as something to explain away.
	intent, intentErr := ns.Staging.ReadIndex(volumeID)
	if intentErr != nil {
		if !errors.Is(intentErr, os.ErrNotExist) {
			// An unreadable intent cannot prove ownership. Do not fall back to
			// identity alone: that is exactly the adoption this guards against.
			klog.Warningf("NodeStageVolume: ownership intent for volume %s is unusable (%v); "+
				"treating it as absent", volumeID, intentErr)
		}
		intent = nil
	}

	reused, err := ns.reuseOwnedMapping(ctx, volumeID, want, intent)
	if err != nil {
		return nil, err
	}
	if reused != nil {
		klog.V(2).Infof("NodeStageVolume: reusing driver-owned mapping %s for %s (intent phase %s)",
			reused.DevicePath, want, intent.Phase)
		// Finalize at the intent's generation: the mapping is not new, so
		// allocating a fresh generation would misreport it as a remap.
		if err := ns.recordStaging(ctx, stagingPath, volumeID, attachmentID, ci,
			*reused, intent.MapGeneration); err != nil {
			return nil, err
		}
		return &csi.NodeStageVolumeResponse{}, nil
	}

	// Step 5: materialize private credential files.
	rf, err := materializeRuntimeFiles(ns.Opts, volumeID, ci, cred)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to prepare Ceph configuration: %v", err)
	}

	// Step 6: persist the ownership intent BEFORE mapping.
	//
	// This is the only durable evidence that a mapping found later belongs to
	// this driver. Kernel and sysfs state can prove *what* an image is; nothing
	// in them records *who* mapped it, and platform Ceph-CSI maps through the
	// same kernel interface on these nodes. Writing the intent first means a
	// crash at any point after this leaves a recoverable state instead of an
	// unattributable mapping.
	generation := ns.Staging.NextGeneration(volumeID)
	if err := ns.Staging.WriteIntent(newMapIntentRecord(volumeID, attachmentID, ci, generation)); err != nil {
		_ = removeRuntimeFiles(ns.Opts, volumeID)
		return nil, status.Errorf(codes.Internal,
			"refusing to map %s without a durable ownership intent: %v", want, err)
	}

	// Step 7: exclusive map. No fallback exists.
	dev, err := ns.Mapper.Map(ctx, mapRequestFor(want, ci, cred, rf, ns.Opts))
	if err != nil {
		ns.discardIntentIfUnmapped(ctx, volumeID, want)
		_ = removeRuntimeFiles(ns.Opts, volumeID)
		if errors.Is(err, ErrExclusiveLockDenied) {
			mapReq := mapRequestFor(want, ci, cred, rf, ns.Opts)
			holders, lockErr := ns.Mapper.LockHolders(ctx, mapReq)
			if lockErr == nil && len(holders) > 0 {
				return nil, status.Errorf(codes.FailedPrecondition,
					"exclusive lock on %s is held by %v; refusing to map without exclusivity",
					want, holders)
			}
			return nil, status.Errorf(codes.FailedPrecondition,
				"could not acquire an exclusive mapping of %s: %v", want, err)
		}
		return nil, status.Errorf(codes.Unavailable, "failed to map %s: %v", want, err)
	}

	// Step 8: the identity gate. Any failure rolls back what we just created.
	sizeBytes, err := ns.identityGate(ctx, dev, want, mapRequestFor(want, ci, cred, rf, ns.Opts))
	if err != nil {
		return nil, ns.rollbackMap(ctx, volumeID, dev, want, err)
	}

	// Step 9: record the mapping and advance the intent to staged.
	if err := ns.recordStagingWithSize(stagingPath, volumeID, attachmentID, ci,
		dev, generation, sizeBytes); err != nil {
		// The mapping cannot be left live. With ownership proven only by an
		// intent, a mapping whose completed record never landed would be reused
		// on the strength of that intent — but nothing would have verified the
		// size or recorded the device for unstage. Roll it back so the retry is
		// a clean map.
		return nil, ns.rollbackMap(ctx, volumeID, dev, want,
			status.Errorf(codes.Internal,
				"mapped %s but failed to persist staging state: %v", dev.DevicePath, err))
	}

	klog.V(2).Infof("NodeStageVolume: staged volume %s as %s (%s)", volumeID, dev.DevicePath, want)
	return &csi.NodeStageVolumeResponse{}, nil
}

// mapRequestFor assembles the mapper request. Exclusivity is not negotiable.
func mapRequestFor(want ImageIdentity, ci *openstack.RBDConnectionInfo, cred *CephCredential,
	rf runtimeFiles, opts openstack.RBDOpts) MapRequest {
	return MapRequest{
		Identity:    want,
		Monitors:    ci.Monitors,
		UserID:      cred.UserID,
		ConfPath:    rf.ConfPath,
		KeyringPath: rf.KeyringPath,
		Exclusive:   true,
		Timeout:     opts.MapTimeoutDuration(),
	}
}

// reuseOwnedMapping returns a live mapping of want when, and only when, a valid
// ownership intent proves this driver created it.
//
// Two independent facts are required and neither substitutes for the other:
//
//   - the intent proves *authorship* — that this driver mapped this image for
//     this volume;
//   - kernel verification proves *identity* — that the device really is that
//     image on the expected cluster.
//
// A live mapping without a valid intent is isolated, never adopted and never
// unmapped. It may belong to platform Ceph-CSI, which shares the kernel RBD
// interface on these nodes; unmapping it could fault an unrelated workload, and
// adopting it would hand a foreign device to a migration pod. There is no
// deployment mode, all-in-one included, in which identity alone is sufficient.
func (ns *nodeServer) reuseOwnedMapping(ctx context.Context, volumeID string,
	want ImageIdentity, intent *StagingRecord) (*MappedDevice, error) {
	found, err := ns.locateMapping(ctx, want)
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, nil
	}

	if !intent.ProvesOwnershipOf(volumeID, want) {
		return nil, ns.isolateMapping(volumeID, want, *found, fmt.Sprintf(
			"device %s maps %s but no valid driver ownership intent covers it "+
				"(intent present: %t); it may belong to another Ceph client, so it is "+
				"neither adopted nor unmapped",
			found.DevicePath, want, intent != nil))
	}

	if err := ns.Mapper.VerifyIdentity(ctx, found.DevicePath, want); err != nil {
		return nil, ns.isolateMapping(volumeID, want, *found, fmt.Sprintf(
			"device %s is covered by an ownership intent but failed identity "+
				"verification (%v); it is neither adopted nor unmapped",
			found.DevicePath, err))
	}
	return found, nil
}

// locateMapping finds a live mapping by pool and image without judging it.
//
// Device numbers are never used to locate anything: they are reused, so
// /dev/rbd5 today need not be /dev/rbd5 from an hour ago.
func (ns *nodeServer) locateMapping(ctx context.Context, want ImageIdentity) (*MappedDevice, error) {
	mapped, err := ns.Mapper.ListMapped(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list kernel RBD mappings: %v", err)
	}
	for i := range mapped {
		if mapped[i].Pool == want.Pool && mapped[i].Image == want.Image {
			return &mapped[i], nil
		}
	}
	return nil, nil
}

// isolateMapping records a volume as unservable on this node and returns the
// error to fail the RPC with.
//
// Isolation is registered so that later stage attempts fail immediately with the
// same explanation, and so the condition is visible as a metric rather than only
// as a repeated RPC error.
func (ns *nodeServer) isolateMapping(volumeID string, want ImageIdentity,
	dev MappedDevice, detail string) error {
	ns.Isolation.Add(ReconcileEntry{
		Action:     ActionIsolated,
		VolumeID:   volumeID,
		Identity:   want,
		DevicePath: dev.DevicePath,
		Detail:     detail,
	})
	klog.Errorf("NodeStageVolume: isolating volume %s: %s", volumeID, detail)
	return status.Errorf(codes.FailedPrecondition,
		"%s. Operator resolution is required; see the operator runbook.", detail)
}

// rollbackMap undoes a mapping this operation created, then reports cause.
//
// Order is the whole point. The intent is removed only after absence is
// confirmed, so every intermediate failure leaves state that reconciliation can
// still act on:
//
//   - identity mismatch ⇒ do not unmap at all; the device is not ours to remove
//   - unmap failed ⇒ intent retained, mapping still owned, retry or reconcile
//   - absence unconfirmed ⇒ intent retained, because a mapping may survive
//
// Removing the intent first would convert any of these into an unattributable
// mapping that no later run may touch.
func (ns *nodeServer) rollbackMap(ctx context.Context, volumeID string, dev MappedDevice,
	want ImageIdentity, cause error) error {
	// Key material is re-materialized by the next stage and `rbd device unmap`
	// does not need it, so it never outlives a failed attempt.
	defer func() { _ = removeRuntimeFiles(ns.Opts, volumeID) }()

	// Never unmap on a device path alone: numbers are recycled, so the device at
	// this path may already be another client's image. If it cannot be confirmed
	// as ours, the intent is kept and reconciliation locates our mapping by
	// pool/image instead — which is the only lookup that stays valid.
	if err := ns.Mapper.VerifyIdentity(ctx, dev.DevicePath, want); err != nil {
		klog.Errorf("NodeStageVolume: not unmapping %s: it does not verify as %s (%v); "+
			"retaining the ownership intent so reconciliation can locate the mapping by image",
			dev.DevicePath, want, err)
		return status.Errorf(codes.FailedPrecondition,
			"%s; %s could not be confirmed as %s so it was left mapped (%v). The ownership "+
				"intent is retained. Operator resolution is required; see the operator runbook.",
			status.Convert(cause).Message(), dev.DevicePath, want, err)
	}

	if err := ns.Mapper.Unmap(ctx, dev.DevicePath, ns.Opts.UnmapTimeoutDuration()); err != nil {
		klog.Errorf("NodeStageVolume: failed to roll back mapping %s: %v; "+
			"retaining the ownership intent for reconciliation", dev.DevicePath, err)
		return status.Errorf(codes.Internal,
			"%s; rolling back the mapping failed (%v). The ownership intent is retained so the "+
				"mapping remains attributable and recoverable.",
			status.Convert(cause).Message(), err)
	}

	remaining, err := ns.locateMapping(ctx, want)
	if err != nil {
		return status.Errorf(codes.Internal,
			"%s; the mapping was unmapped but its absence could not be confirmed (%v). "+
				"The ownership intent is retained.", status.Convert(cause).Message(), err)
	}
	if remaining != nil {
		return status.Errorf(codes.Internal,
			"%s; %s still appears mapped as %s after unmap. The ownership intent is retained.",
			status.Convert(cause).Message(), want, remaining.DevicePath)
	}

	// Absence confirmed: only now is it safe to drop the ownership claim.
	if err := ns.Staging.RemoveIndexOnly(volumeID); err != nil {
		klog.Warningf("NodeStageVolume: failed to remove ownership intent for volume %s: %v",
			volumeID, err)
	}
	return cause
}

// discardIntentIfUnmapped drops the intent when the map demonstrably did not
// happen.
//
// A failed map call is not proof that nothing was mapped, so the kernel is
// checked. If a mapping exists, or cannot be listed, the intent is kept: an
// unattributable mapping is far worse than a stale intent, which the next stage
// or reconciliation resolves.
func (ns *nodeServer) discardIntentIfUnmapped(ctx context.Context, volumeID string, want ImageIdentity) {
	found, err := ns.locateMapping(ctx, want)
	if err != nil {
		klog.Warningf("NodeStageVolume: could not confirm that %s is unmapped after a failed map "+
			"(%v); retaining the ownership intent for volume %s", want, err, volumeID)
		return
	}
	if found != nil {
		klog.Warningf("NodeStageVolume: map of %s reported failure but %s is mapped; "+
			"retaining the ownership intent for volume %s", want, found.DevicePath, volumeID)
		return
	}
	if err := ns.Staging.RemoveIndexOnly(volumeID); err != nil {
		klog.Warningf("NodeStageVolume: failed to remove ownership intent for volume %s: %v",
			volumeID, err)
	}
}

// identityGate performs the five checks required before a writable device is
// exposed. It returns the verified device size.
func (ns *nodeServer) identityGate(ctx context.Context, dev MappedDevice,
	want ImageIdentity, mapReq MapRequest) (int64, error) {
	// Checks 1 and 2: FSID, pool and image, from sysfs cross-checked against
	// the kernel device list.
	if err := ns.Mapper.VerifyIdentity(ctx, dev.DevicePath, want); err != nil {
		return 0, status.Errorf(codes.FailedPrecondition,
			"mapped device %s does not match the expected image: %v", dev.DevicePath, err)
	}

	// Check 4: the block device must exist. udev creates it asynchronously, so
	// a bounded wait is required before stat.
	if err := waitForDevice(ctx, dev.DevicePath, ns.Opts.DeviceWaitTimeoutDuration()); err != nil {
		return 0, status.Errorf(codes.Internal, "%v", err)
	}
	sizeBytes, err := ns.Mapper.DeviceSize(ctx, dev.DevicePath)
	if err != nil {
		return 0, status.Errorf(codes.Internal,
			"could not determine size of %s: %v", dev.DevicePath, err)
	}
	if sizeBytes <= 0 {
		return 0, status.Errorf(codes.Internal,
			"device %s reported a non-positive size (%d)", dev.DevicePath, sizeBytes)
	}

	// Check 3: no conflicting writable client may hold the exclusive lock.
	// The map already succeeded with --exclusive, so this is a defence against
	// a lock acquired between map and publish. A lookup failure is not fatal:
	// exclusivity was already enforced by the kernel.
	holders, lockErr := ns.Mapper.LockHolders(ctx, mapReq)
	if lockErr != nil {
		klog.V(4).Infof("identityGate: could not read lock holders for %s: %v", want, lockErr)
	} else if len(holders) > 1 {
		return 0, status.Errorf(codes.FailedPrecondition,
			"image %s has %d exclusive-lock holders (%v); expected at most one",
			want, len(holders), holders)
	}

	// Check 5 was enforced before the map, when the credential was loaded.
	return sizeBytes, nil
}

// recordStaging writes the completed record, determining the size from the device.
func (ns *nodeServer) recordStaging(ctx context.Context, stagingPath, volumeID, attachmentID string,
	ci *openstack.RBDConnectionInfo, dev MappedDevice, generation int) error {
	sizeBytes, err := ns.Mapper.DeviceSize(ctx, dev.DevicePath)
	if err != nil {
		klog.V(4).Infof("recordStaging: size of %s unavailable: %v", dev.DevicePath, err)
	}
	return ns.recordStagingWithSize(stagingPath, volumeID, attachmentID, ci, dev, generation, sizeBytes)
}

// recordStagingWithSize persists the completed record, advancing the intent from
// map-pending to staged.
//
// The generation is passed in rather than derived here: it was allocated before
// the map, and re-deriving it would read back the intent this call is about to
// overwrite, double-counting the mapping as a remap.
func (ns *nodeServer) recordStagingWithSize(stagingPath, volumeID, attachmentID string,
	ci *openstack.RBDConnectionInfo, dev MappedDevice, generation int, sizeBytes int64) error {
	rec := newStagingRecord(volumeID, attachmentID, ci, dev, generation, sizeBytes)
	if err := ns.Staging.Write(stagingPath, rec); err != nil {
		return status.Errorf(codes.Internal, "failed to write staging record: %v", err)
	}
	return nil
}

// ── NodeUnstageVolume ────────────────────────────────────────────────────────

// NodeUnstageVolume flushes and unmaps the RBD image.
//
// No Cinder call is made here: deleting the attachment record is
// ControllerUnpublishVolume's job, and keeping that split is what makes a Cinder
// outage during unstage tractable.
func (ns *nodeServer) NodeUnstageVolume(ctx context.Context,
	req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	klog.V(4).Infof("NodeUnstageVolume: called with args %+v", protosanitizer.StripSecrets(req))

	volumeID := req.GetVolumeId()
	stagingPath := req.GetStagingTargetPath()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID must be provided")
	}
	if stagingPath == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path must be provided")
	}

	// Step 2: the record locates expected state, but a missing record is NOT
	// proof that no mapping exists. Fall back to the node index, then to the
	// kernel.
	rec, err := ns.Staging.Read(stagingPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			klog.Warningf("NodeUnstageVolume: staging record for %s unusable (%v); "+
				"reconciling from the kernel", volumeID, err)
		}
		rec, err = ns.Staging.ReadIndex(volumeID)
		if err != nil {
			rec = nil
		}
	}

	want, ok := ns.expectedIdentity(rec)
	if !ok {
		// Nothing on this node describes the volume. Without an expected
		// identity an unmap cannot be authorized, so report success rather than
		// guessing: a mapping that exists will be found by reconciliation.
		klog.V(3).Infof("NodeUnstageVolume: no staging state for volume %s; nothing to unmap", volumeID)
		_ = ns.cleanupStaging(stagingPath, volumeID)
		return &csi.NodeUnstageVolumeResponse{}, nil
	}

	// Step 3: locate the device by identity, never by recorded device number.
	dev, err := ns.findDeviceForIdentity(ctx, want)
	if err != nil {
		return nil, err
	}
	if dev == nil {
		klog.V(3).Infof("NodeUnstageVolume: %s is not mapped; cleaning up state", want)
		_ = removeRuntimeFiles(ns.Opts, volumeID)
		_ = ns.cleanupStaging(stagingPath, volumeID)
		return &csi.NodeUnstageVolumeResponse{}, nil
	}

	// Step 4: flush before unmap so the tail of a migration copy is not lost.
	if err := ns.Mapper.Flush(ctx, dev.DevicePath); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to flush %s before unmap: %v",
			dev.DevicePath, err)
	}

	// Step 5: unmap. On timeout or busy, keep state and let kubelet retry.
	if err := ns.Mapper.Unmap(ctx, dev.DevicePath, ns.Opts.UnmapTimeoutDuration()); err != nil {
		if errors.Is(err, ErrDeviceBusy) {
			return nil, status.Errorf(codes.Aborted,
				"device %s is busy; keeping staging state for retry: %v", dev.DevicePath, err)
		}
		return nil, status.Errorf(codes.Aborted,
			"failed to unmap %s; keeping staging state for retry: %v", dev.DevicePath, err)
	}

	// Step 6: confirm absence before discarding state.
	if remaining, listErr := ns.findDeviceForIdentity(ctx, want); listErr == nil && remaining != nil {
		return nil, status.Errorf(codes.Aborted,
			"%s still appears mapped as %s after unmap; keeping staging state",
			want, remaining.DevicePath)
	}

	// Steps 7 and 8: remove credential files, then the staging state.
	if err := removeRuntimeFiles(ns.Opts, volumeID); err != nil {
		klog.Warningf("NodeUnstageVolume: failed to remove runtime files for %s: %v", volumeID, err)
	}
	if err := ns.cleanupStaging(stagingPath, volumeID); err != nil {
		klog.Warningf("NodeUnstageVolume: failed to remove staging state for %s: %v", volumeID, err)
	}

	klog.V(2).Infof("NodeUnstageVolume: unstaged volume %s (%s)", volumeID, want)
	return &csi.NodeUnstageVolumeResponse{}, nil
}

// expectedIdentity extracts a complete identity from a record.
func (ns *nodeServer) expectedIdentity(rec *StagingRecord) (ImageIdentity, bool) {
	if rec == nil {
		return ImageIdentity{}, false
	}
	want := rec.Identity()
	if !want.IsComplete() {
		return ImageIdentity{}, false
	}
	return want, true
}

// findDeviceForIdentity locates a live mapping of want, verifying identity.
//
// A device occupying the same pool/image but failing verification is isolated:
// unmapping it could fault an unrelated workload, so the error is surfaced and
// nothing is touched.
func (ns *nodeServer) findDeviceForIdentity(ctx context.Context, want ImageIdentity) (*MappedDevice, error) {
	mapped, err := ns.Mapper.ListMapped(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to list kernel RBD mappings: %v", err)
	}

	for i := range mapped {
		d := mapped[i]
		if d.Pool != want.Pool || d.Image != want.Image {
			continue
		}
		if err := ns.Mapper.VerifyIdentity(ctx, d.DevicePath, want); err != nil {
			return nil, status.Errorf(codes.FailedPrecondition,
				"device %s occupies %s/%s but does not match the staged image (%v); "+
					"refusing to unmap it — operator resolution required",
				d.DevicePath, d.Pool, d.Image, err)
		}
		return &d, nil
	}
	return nil, nil
}

// cleanupStaging removes the staging record and the staging directory.
func (ns *nodeServer) cleanupStaging(stagingPath, volumeID string) error {
	if err := ns.Staging.Remove(stagingPath, volumeID); err != nil {
		return err
	}
	// The directory itself is kubelet's; removing it when empty is best effort.
	if err := os.Remove(stagingPath); err != nil && !os.IsNotExist(err) {
		klog.V(5).Infof("cleanupStaging: staging path %s not removed: %v", stagingPath, err)
	}
	return nil
}

// ── NodePublishVolume ────────────────────────────────────────────────────────

// NodePublishVolume bind-mounts the staged device at the pod's target path.
//
// No Cinder or Ceph interaction happens here.
func (ns *nodeServer) NodePublishVolume(ctx context.Context,
	req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	klog.V(4).Infof("NodePublishVolume: called with args %+v", protosanitizer.StripSecrets(req))

	volumeID := req.GetVolumeId()
	stagingPath := req.GetStagingTargetPath()
	targetPath := req.GetTargetPath()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID must be provided")
	}
	if stagingPath == "" {
		return nil, status.Error(codes.InvalidArgument, "staging target path must be provided")
	}
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "target path must be provided")
	}
	capability := req.GetVolumeCapability()
	if capability == nil {
		return nil, status.Error(codes.InvalidArgument, "volume capability must be provided")
	}
	if err := validateBlockOnlyCapabilities([]*csi.VolumeCapability{capability}); err != nil {
		return nil, err
	}

	// Idempotency: already bind-mounted.
	notMnt, err := ns.Mounter.IsLikelyNotMountPointAttach(targetPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to inspect target path %s: %v", targetPath, err)
	}
	if !notMnt {
		klog.V(4).Infof("NodePublishVolume: %s is already published", targetPath)
		return &csi.NodePublishVolumeResponse{}, nil
	}

	rec, err := ns.Staging.Read(stagingPath)
	if err != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"volume %s is not staged at %s: %v", volumeID, stagingPath, err)
	}
	// Only a completed stage may be published. A map-pending record describes an
	// interrupted attempt: it names no device, and bind-mounting on the strength
	// of one would expose a device nothing has verified. WriteIntent is
	// index-only so this should be unreachable, which is exactly why it is
	// checked rather than assumed.
	if rec.Phase != PhaseStaged {
		return nil, status.Errorf(codes.FailedPrecondition,
			"volume %s is staged only as %s at %s; the stage did not complete",
			volumeID, rec.Phase, stagingPath)
	}
	want, ok := ns.expectedIdentity(rec)
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition,
			"staging record for volume %s is incomplete", volumeID)
	}

	// A recycled /dev/rbdN must never be bind-mounted into a pod, so the
	// recorded device is re-verified rather than trusted.
	if err := ns.Mapper.VerifyIdentity(ctx, rec.DevicePath, want); err != nil {
		return nil, status.Errorf(codes.FailedPrecondition,
			"staged device %s no longer maps %s: %v", rec.DevicePath, want, err)
	}

	realDevice, err := filepath.EvalSymlinks(rec.DevicePath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to resolve %s: %v", rec.DevicePath, err)
	}

	// kubelet may have created a directory at the block publish path; MakeFile
	// replaces it with a regular file suitable for a bind mount.
	if err := ns.Mounter.MakeFile(targetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create target file %s: %v", targetPath, err)
	}

	if err := ns.Mounter.Mounter().Mount(realDevice, targetPath, "", []string{"bind"}); err != nil {
		if rmErr := os.Remove(targetPath); rmErr != nil && !os.IsNotExist(rmErr) {
			klog.Errorf("NodePublishVolume: failed to clean up %s after mount failure: %v",
				targetPath, rmErr)
		}
		return nil, status.Errorf(codes.Internal,
			"failed to bind mount %s at %s: %v", realDevice, targetPath, err)
	}

	klog.V(2).Infof("NodePublishVolume: published %s at %s", realDevice, targetPath)
	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume removes the raw-block bind target.
func (ns *nodeServer) NodeUnpublishVolume(_ context.Context,
	req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	klog.V(4).Infof("NodeUnpublishVolume: called with args %+v", protosanitizer.StripSecrets(req))

	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID must be provided")
	}
	targetPath := req.GetTargetPath()
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "target path must be provided")
	}

	if err := ns.Mounter.UnmountPath(targetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to unmount %s: %v", targetPath, err)
	}

	klog.V(2).Infof("NodeUnpublishVolume: unpublished %s", targetPath)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// ── NodeGetVolumeStats ───────────────────────────────────────────────────────

// NodeGetVolumeStats reports capacity for a raw block volume.
//
// Block devices have no inode statistics, so only a BYTES usage entry is
// returned.
func (ns *nodeServer) NodeGetVolumeStats(_ context.Context,
	req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	klog.V(4).Infof("NodeGetVolumeStats: called with args %+v", protosanitizer.StripSecrets(req))

	volumePath := req.GetVolumePath()
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID must be provided")
	}
	if volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "volume path must be provided")
	}

	if _, err := os.Stat(volumePath); err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "volume path %s does not exist", volumePath)
		}
		return nil, status.Errorf(codes.Internal, "failed to stat %s: %v", volumePath, err)
	}

	stats, err := ns.Mounter.GetDeviceStats(volumePath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get stats for %s: %v", volumePath, err)
	}

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{{
			Total: stats.TotalBytes,
			Unit:  csi.VolumeUsage_BYTES,
		}},
	}, nil
}

// NodeExpandVolume stays Unimplemented: expansion is a non-goal.
func (ns *nodeServer) NodeExpandVolume(_ context.Context,
	_ *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "volume expansion is not supported by this driver")
}
