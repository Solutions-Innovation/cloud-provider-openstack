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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-iscsi/openstack"
	"k8s.io/cloud-provider-openstack/pkg/util/mount"
	"k8s.io/klog/v2"
)

// devicePathFile is the filename stored under stagingTargetPath to persist the
// iSCSI device path between NodeStageVolume and NodeUnstageVolume.
const devicePathFile = "devicepath"

// nodeServer implements csi.NodeServer for iSCSI-Cinder volumes.
type nodeServer struct {
	Driver  *Driver
	Opts    openstack.ISCSIOpts
	ISCSI   ISCSIInitiator
	Mounter mount.IMount

	// Injection points for unit testing NodeGetInfo.
	initiatorNamePath  string                       // default: /etc/iscsi/initiatorname.iscsi
	hostnameFunc       func() (string, error)       // default: os.Hostname
	getInterfaceIPFunc func(string) (string, error) // default: GetInterfaceIP

	csi.UnimplementedNodeServer
}

// ── NodeGetInfo ──────────────────────────────────────────────────────────────

// NodeGetInfo returns the node ID composed of hostname;iqn;ip and the
// accessible topology based on the configured availability zone.
func (ns *nodeServer) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	klog.V(4).Info("NodeGetInfo called")

	// 1. Read initiator IQN
	iqnPath := ns.initiatorNamePath
	if iqnPath == "" {
		iqnPath = defaultInitiatorNamePath
	}
	iqn, err := ReadInitiatorIQN(iqnPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to read initiator IQN: %v", err)
	}

	// 2. Get hostname
	hostnameFn := ns.hostnameFunc
	if hostnameFn == nil {
		hostnameFn = os.Hostname
	}
	host, err := hostnameFn()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get hostname: %v", err)
	}

	// 3. Get storage network IP
	getIP := ns.getInterfaceIPFunc
	if getIP == nil {
		getIP = GetInterfaceIP
	}
	ip, err := getIP(ns.Opts.StorageInterface)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get interface IP: %v", err)
	}

	nodeID := fmt.Sprintf("%s%s%s%s%s", host, nodeIDSeparator, iqn, nodeIDSeparator, ip)
	klog.V(2).Infof("NodeGetInfo: nodeID=%s", nodeID)

	// TODO: Populate AccessibleTopology with the availability zone when
	// topology-aware provisioning is configured. The topologyKey constant
	// is defined in driver.go but the zone source (Cinder AZ, node label,
	// or config) has not been decided yet.
	return &csi.NodeGetInfoResponse{
		NodeId: nodeID,
	}, nil
}

// ── NodeStageVolume ──────────────────────────────────────────────────────────

// NodeStageVolume performs iSCSI discovery, CHAP auth, login, waits for the
// block device to appear, and records the device path at the staging directory.
func (ns *nodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	klog.V(4).Infof("NodeStageVolume: volumeID=%s stagingTargetPath=%s", req.GetVolumeId(), req.GetStagingTargetPath())

	// ── Validate required fields ─────────────────────────────────────────
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "VolumeId must be provided")
	}
	stagingPath := req.GetStagingTargetPath()
	if stagingPath == "" {
		return nil, status.Error(codes.InvalidArgument, "StagingTargetPath must be provided")
	}
	volCap := req.GetVolumeCapability()
	if volCap == nil {
		return nil, status.Error(codes.InvalidArgument, "VolumeCapability must be provided")
	}

	// ── Block-only enforcement ───────────────────────────────────────────
	if volCap.GetMount() != nil {
		return nil, status.Error(codes.InvalidArgument,
			"cinder-iscsi.csi.windriver.com: filesystem (mount) volume mode is not supported; "+
				"this driver only supports volumeMode: Block for migration workloads")
	}
	if volCap.GetBlock() == nil {
		return nil, status.Error(codes.InvalidArgument,
			"volume capability must specify block access type")
	}

	// ── Parse publish context ────────────────────────────────────────────
	pubCtx := req.GetPublishContext()
	portal := pubCtx[PublishContextTargetPortal]
	iqn := pubCtx[PublishContextTargetIQN]
	lunStr := pubCtx[PublishContextTargetLUN]
	authMethod := pubCtx[PublishContextAuthMethod]
	authUser := pubCtx[PublishContextAuthUsername]
	authPass := pubCtx[PublishContextAuthPassword]

	if portal == "" || iqn == "" || lunStr == "" {
		return nil, status.Error(codes.InvalidArgument,
			"publish_context must contain target_portal, target_iqn, and target_lun")
	}
	lun, err := strconv.Atoi(lunStr)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid target_lun %q: %v", lunStr, err)
	}

	devicePath := BuildDevicePath(portal, iqn, lun)

	// ── Idempotency: check if session already active + device exists ─────
	sessionActive, err := ns.ISCSI.IsSessionActive(ctx, iqn, portal)
	if err != nil {
		klog.V(3).Infof("NodeStageVolume: session check error (proceeding): %v", err)
	}
	if sessionActive {
		if _, statErr := os.Stat(devicePath); statErr == nil {
			klog.V(2).Infof("NodeStageVolume: session already active and device %s exists for volume %s (idempotent)", devicePath, volumeID)
			// Ensure the devicepath file exists at the staging location
			if writeErr := ns.writeDevicePath(stagingPath, devicePath); writeErr != nil {
				return nil, status.Errorf(codes.Internal, "failed to write device path file: %v", writeErr)
			}
			return &csi.NodeStageVolumeResponse{}, nil
		}
	}

	// ── iSCSI Discovery ──────────────────────────────────────────────────
	if err := ns.ISCSI.Discovery(ctx, portal); err != nil {
		return nil, status.Errorf(codes.Internal, "iSCSI discovery failed: %v", err)
	}

	// ── Set CHAP auth if required ────────────────────────────────────────
	if strings.EqualFold(authMethod, "CHAP") {
		if authUser == "" || authPass == "" {
			return nil, status.Error(codes.InvalidArgument,
				"CHAP auth_method specified but auth_username or auth_password missing")
		}
		if err := ns.ISCSI.SetCHAPAuth(ctx, iqn, portal, authUser, authPass); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to set CHAP auth: %v", err)
		}
	}

	// ── iSCSI Login ──────────────────────────────────────────────────────
	if err := ns.ISCSI.Login(ctx, iqn, portal); err != nil {
		return nil, status.Errorf(codes.Internal, "iSCSI login failed: %v", err)
	}

	// ── Wait for block device ────────────────────────────────────────────
	if err := WaitForDevice(ctx, devicePath, ns.Opts.DeviceWaitTimeout); err != nil {
		// Attempt cleanup on failure — mirror the Logout + DeleteNode
		// sequence from NodeUnstageVolume to avoid stale iscsiadm state.
		_ = ns.ISCSI.Logout(ctx, iqn, portal)
		_ = ns.ISCSI.DeleteNode(ctx, iqn, portal)
		return nil, status.Errorf(codes.Internal, "device %s did not appear: %v", devicePath, err)
	}

	// ── Store device path at staging target path ─────────────────────────
	if err := ns.writeDevicePath(stagingPath, devicePath); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to write device path file: %v", err)
	}

	klog.V(2).Infof("NodeStageVolume: volume %s staged at %s, device=%s", volumeID, stagingPath, devicePath)
	return &csi.NodeStageVolumeResponse{}, nil
}

// writeDevicePath ensures the staging directory exists and writes the device
// path string to the devicepath file within it.
func (ns *nodeServer) writeDevicePath(stagingPath, devicePath string) error {
	if err := os.MkdirAll(stagingPath, 0750); err != nil {
		return fmt.Errorf("failed to create staging directory %s: %w", stagingPath, err)
	}
	dpFile := filepath.Join(stagingPath, devicePathFile)
	if err := os.WriteFile(dpFile, []byte(devicePath), 0640); err != nil {
		return fmt.Errorf("failed to write device path to %s: %w", dpFile, err)
	}
	return nil
}

// readDevicePath reads the device path string from the staging directory.
// The underlying error is wrapped with %w so callers can inspect it with
// errors.Is(err, os.ErrNotExist) to distinguish "never staged" from read failures.
func (ns *nodeServer) readDevicePath(stagingPath string) (string, error) {
	dpFile := filepath.Join(stagingPath, devicePathFile)
	data, err := os.ReadFile(dpFile)
	if err != nil {
		return "", fmt.Errorf("failed to read device path from %s: %w", dpFile, err)
	}
	return strings.TrimSpace(string(data)), nil
}

// ── NodeUnstageVolume ────────────────────────────────────────────────────────

// NodeUnstageVolume logs out of the iSCSI session, removes the node DB entry,
// and cleans up the staging directory.
func (ns *nodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	klog.V(4).Infof("NodeUnstageVolume: volumeID=%s stagingTargetPath=%s", req.GetVolumeId(), req.GetStagingTargetPath())

	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "VolumeId must be provided")
	}
	stagingPath := req.GetStagingTargetPath()
	if stagingPath == "" {
		return nil, status.Error(codes.InvalidArgument, "StagingTargetPath must be provided")
	}

	// ── Read device info from staging path ───────────────────────────────
	devicePath, err := ns.readDevicePath(stagingPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			klog.V(2).Infof("NodeUnstageVolume: device path file does not exist for volume %s (idempotent)", volumeID)
			return &csi.NodeUnstageVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal, "failed to read device path: %v", err)
	}

	// ── Parse portal + IQN from device path ──────────────────────────────
	portal, iqn, _, err := ParseDevicePath(devicePath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to parse device path %q: %v", devicePath, err)
	}

	// ── iSCSI Logout ─────────────────────────────────────────────────────
	if err := ns.ISCSI.Logout(ctx, iqn, portal); err != nil {
		return nil, status.Errorf(codes.Internal, "iSCSI logout failed: %v", err)
	}

	// ── Delete node DB entry ─────────────────────────────────────────────
	if err := ns.ISCSI.DeleteNode(ctx, iqn, portal); err != nil {
		return nil, status.Errorf(codes.Internal, "iSCSI delete node failed: %v", err)
	}

	// ── Cleanup staging directory ────────────────────────────────────────
	dpFile := filepath.Join(stagingPath, devicePathFile)
	if err := os.Remove(dpFile); err != nil && !os.IsNotExist(err) {
		klog.Warningf("NodeUnstageVolume: failed to remove %s: %v", dpFile, err)
	}
	if err := os.Remove(stagingPath); err != nil && !os.IsNotExist(err) {
		klog.Warningf("NodeUnstageVolume: failed to remove staging dir %s: %v", stagingPath, err)
	}

	klog.V(2).Infof("NodeUnstageVolume: volume %s unstaged from %s", volumeID, stagingPath)
	return &csi.NodeUnstageVolumeResponse{}, nil
}

// ── NodePublishVolume ────────────────────────────────────────────────────────

// NodePublishVolume bind-mounts the block device from the staging path to the
// pod's target path for raw block access.
func (ns *nodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	klog.V(4).Infof("NodePublishVolume: volumeID=%s targetPath=%s", req.GetVolumeId(), req.GetTargetPath())

	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "VolumeId must be provided")
	}
	stagingPath := req.GetStagingTargetPath()
	if stagingPath == "" {
		return nil, status.Error(codes.InvalidArgument, "StagingTargetPath must be provided")
	}
	targetPath := req.GetTargetPath()
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "TargetPath must be provided")
	}

	// ── Block-only enforcement ───────────────────────────────────────────
	volCap := req.GetVolumeCapability()
	if volCap == nil {
		return nil, status.Error(codes.InvalidArgument, "VolumeCapability must be provided")
	}
	if volCap.GetMount() != nil {
		return nil, status.Error(codes.InvalidArgument,
			"cinder-iscsi.csi.windriver.com: filesystem (mount) volume mode is not supported; "+
				"this driver only supports volumeMode: Block for migration workloads")
	}

	// ── Idempotency: check if already bind-mounted ───────────────────────
	notMounted, err := ns.Mounter.IsLikelyNotMountPointAttach(targetPath)
	if err == nil && !notMounted {
		klog.V(2).Infof("NodePublishVolume: %s already mounted for volume %s (idempotent)", targetPath, volumeID)
		return &csi.NodePublishVolumeResponse{}, nil
	}

	// ── Read device path from staging ────────────────────────────────────
	devicePath, err := ns.readDevicePath(stagingPath)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"failed to read device path from staging %s: %v", stagingPath, err)
	}

	// ── Resolve symlink to real device path ──────────────────────────────
	realDevicePath, err := filepath.EvalSymlinks(devicePath)
	if err != nil {
		return nil, status.Errorf(codes.Internal,
			"failed to resolve device symlink %s: %v", devicePath, err)
	}

	// ── Create the target file for block bind-mount ──────────────────────
	if err := ns.Mounter.MakeFile(targetPath); err != nil {
		return nil, status.Errorf(codes.Internal,
			"failed to create target file %s: %v", targetPath, err)
	}

	// ── Bind mount device to target ──────────────────────────────────────
	mounter := ns.Mounter.Mounter()
	if err := mounter.Mount(realDevicePath, targetPath, "", []string{"bind"}); err != nil {
		// Clean up created file on failure
		_ = os.Remove(targetPath)
		return nil, status.Errorf(codes.Internal,
			"failed to bind mount %s to %s: %v", realDevicePath, targetPath, err)
	}

	klog.V(2).Infof("NodePublishVolume: volume %s published at %s (device=%s)", volumeID, targetPath, realDevicePath)
	return &csi.NodePublishVolumeResponse{}, nil
}

// ── NodeUnpublishVolume ──────────────────────────────────────────────────────

// NodeUnpublishVolume unmounts the block device from the pod's target path.
func (ns *nodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	klog.V(4).Infof("NodeUnpublishVolume: volumeID=%s targetPath=%s", req.GetVolumeId(), req.GetTargetPath())

	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "VolumeId must be provided")
	}
	targetPath := req.GetTargetPath()
	if targetPath == "" {
		return nil, status.Error(codes.InvalidArgument, "TargetPath must be provided")
	}

	// ── Unmount ──────────────────────────────────────────────────────────
	if err := ns.Mounter.UnmountPath(targetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to unmount %s: %v", targetPath, err)
	}

	klog.V(2).Infof("NodeUnpublishVolume: volume %s unpublished from %s", volumeID, targetPath)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// ── NodeGetVolumeStats ───────────────────────────────────────────────────────

// NodeGetVolumeStats returns capacity statistics for the block device at the
// given volume path.
func (ns *nodeServer) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	klog.V(4).Infof("NodeGetVolumeStats: volumeID=%s volumePath=%s", req.GetVolumeId(), req.GetVolumePath())

	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "VolumeId must be provided")
	}
	volumePath := req.GetVolumePath()
	if volumePath == "" {
		return nil, status.Error(codes.InvalidArgument, "VolumePath must be provided")
	}

	// Check that the volume path exists
	if _, err := os.Stat(volumePath); err != nil {
		if os.IsNotExist(err) {
			return nil, status.Errorf(codes.NotFound, "volume path %s not found", volumePath)
		}
		return nil, status.Errorf(codes.Internal, "failed to stat volume path %s: %v", volumePath, err)
	}

	stats, err := ns.Mounter.GetDeviceStats(volumePath)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get device stats for %s: %v", volumePath, err)
	}

	if stats.Block {
		// Block device — report total capacity only (used/available not meaningful for raw block)
		return &csi.NodeGetVolumeStatsResponse{
			Usage: []*csi.VolumeUsage{
				{
					Total: stats.TotalBytes,
					Unit:  csi.VolumeUsage_BYTES,
				},
			},
		}, nil
	}

	// Filesystem — shouldn't happen for this block-only driver, but handle gracefully
	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Available: stats.AvailableBytes,
				Total:     stats.TotalBytes,
				Used:      stats.UsedBytes,
				Unit:      csi.VolumeUsage_BYTES,
			},
			{
				Available: stats.AvailableInodes,
				Total:     stats.TotalInodes,
				Used:      stats.UsedInodes,
				Unit:      csi.VolumeUsage_INODES,
			},
		},
	}, nil
}

// ── NodeExpandVolume ─────────────────────────────────────────────────────────

// NodeExpandVolume returns Unimplemented. iSCSI volume expansion is handled
// entirely on the controller side via Cinder os-extend; the iSCSI initiator
// automatically sees the new LUN size after the Cinder extend completes.
func (ns *nodeServer) NodeExpandVolume(ctx context.Context, req *csi.NodeExpandVolumeRequest) (*csi.NodeExpandVolumeResponse, error) {
	return nil, status.Error(codes.Unimplemented, "NodeExpandVolume is not supported for iSCSI volumes")
}

// ── NodeGetCapabilities ──────────────────────────────────────────────────────

// NodeGetCapabilities returns the set of node capabilities supported by this
// driver (STAGE_UNSTAGE_VOLUME, GET_VOLUME_STATS).
func (ns *nodeServer) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	klog.V(5).Infof("NodeGetCapabilities called")

	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: ns.Driver.nscap,
	}, nil
}
