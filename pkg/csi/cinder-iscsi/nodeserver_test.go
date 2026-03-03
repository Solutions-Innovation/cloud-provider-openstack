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
	"os"
	"path/filepath"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-iscsi/openstack"
	mountutil "k8s.io/cloud-provider-openstack/pkg/util/mount"
)

// ── Test Helpers ──────────────────────────────────────────────────────────────

func fakeDriver() *Driver {
	d := &Driver{
		name:      driverName,
		fqVersion: "1.0.0@test",
	}
	// Add node capabilities
	_ = d.AddNodeServiceCapabilities([]csi.NodeServiceCapability_RPC_Type{
		csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
		csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
	})
	return d
}

func fakeNodeServer(iscsiMock *ISCSIInitiatorMock, mountMock *mountutil.MountMock) *nodeServer {
	return &nodeServer{
		Driver: fakeDriver(),
		Opts: openstack.ISCSIOpts{
			LoginTimeout:      30,
			DeviceWaitTimeout: 2, // Short for tests
			ISCSIInterface:    "default",
		},
		ISCSI:   iscsiMock,
		Mounter: mountMock,
	}
}

func blockCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Block{
			Block: &csi.VolumeCapability_BlockVolume{},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
}

func mountCapability() *csi.VolumeCapability {
	return &csi.VolumeCapability{
		AccessType: &csi.VolumeCapability_Mount{
			Mount: &csi.VolumeCapability_MountVolume{FsType: "ext4"},
		},
		AccessMode: &csi.VolumeCapability_AccessMode{
			Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		},
	}
}

func stagePublishContext() map[string]string {
	return map[string]string{
		PublishContextTargetPortal: "10.0.0.1:3260",
		PublishContextTargetIQN:    "iqn.2010-10.org.openstack:volume-vol-001",
		PublishContextTargetLUN:    "1",
	}
}

func stageCHAPPublishContext() map[string]string {
	ctx := stagePublishContext()
	ctx[PublishContextAuthMethod] = "CHAP"
	ctx[PublishContextAuthUsername] = "user1"
	ctx[PublishContextAuthPassword] = "secret1"
	return ctx
}

// ── NodeGetInfo Tests ────────────────────────────────────────────────────────

func TestNodeGetInfo_Success(t *testing.T) {
	ns := fakeNodeServer(&ISCSIInitiatorMock{}, &mountutil.MountMock{})

	// Write a fake initiator name file
	iqnFile := filepath.Join(t.TempDir(), "initiatorname.iscsi")
	assert.NoError(t, os.WriteFile(iqnFile, []byte("InitiatorName=iqn.1993-08.org.debian:01:test123\n"), 0644))
	ns.initiatorNamePath = iqnFile

	// Inject hostname and IP functions
	ns.hostnameFunc = func() (string, error) { return "worker-1", nil }
	ns.getInterfaceIPFunc = func(_ string) (string, error) { return "10.0.0.42", nil }

	resp, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	assert.NoError(t, err)
	assert.Equal(t, "worker-1;iqn.1993-08.org.debian:01:test123;10.0.0.42", resp.NodeId)
}

func TestNodeGetInfo_MissingIQN(t *testing.T) {
	ns := fakeNodeServer(&ISCSIInitiatorMock{}, &mountutil.MountMock{})

	// Point to a non-existent file
	ns.initiatorNamePath = "/nonexistent/initiatorname.iscsi"
	ns.hostnameFunc = func() (string, error) { return "worker-1", nil }
	ns.getInterfaceIPFunc = func(_ string) (string, error) { return "10.0.0.42", nil }

	_, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read initiator IQN")
}

func TestNodeGetInfo_HostnameError(t *testing.T) {
	ns := fakeNodeServer(&ISCSIInitiatorMock{}, &mountutil.MountMock{})

	iqnFile := filepath.Join(t.TempDir(), "initiatorname.iscsi")
	assert.NoError(t, os.WriteFile(iqnFile, []byte("InitiatorName=iqn.test\n"), 0644))
	ns.initiatorNamePath = iqnFile
	ns.hostnameFunc = func() (string, error) { return "", fmt.Errorf("hostname unavailable") }
	ns.getInterfaceIPFunc = func(_ string) (string, error) { return "10.0.0.42", nil }

	_, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get hostname")
}

func TestNodeGetInfo_InterfaceIPError(t *testing.T) {
	ns := fakeNodeServer(&ISCSIInitiatorMock{}, &mountutil.MountMock{})

	iqnFile := filepath.Join(t.TempDir(), "initiatorname.iscsi")
	assert.NoError(t, os.WriteFile(iqnFile, []byte("InitiatorName=iqn.test\n"), 0644))
	ns.initiatorNamePath = iqnFile
	ns.hostnameFunc = func() (string, error) { return "worker-1", nil }
	ns.getInterfaceIPFunc = func(_ string) (string, error) { return "", fmt.Errorf("no iface") }

	_, err := ns.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to get interface IP")
}

// ── NodeStageVolume Tests ────────────────────────────────────────────────────

func TestNodeStageVolume_MissingVolumeID(t *testing.T) {
	ns := fakeNodeServer(&ISCSIInitiatorMock{}, &mountutil.MountMock{})
	_, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		StagingTargetPath: "/tmp/staging",
		VolumeCapability:  blockCapability(),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "VolumeId must be provided")
}

func TestNodeStageVolume_MissingStagingPath(t *testing.T) {
	ns := fakeNodeServer(&ISCSIInitiatorMock{}, &mountutil.MountMock{})
	_, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:         "vol-001",
		VolumeCapability: blockCapability(),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "StagingTargetPath must be provided")
}

func TestNodeStageVolume_RejectsMountCapability(t *testing.T) {
	ns := fakeNodeServer(&ISCSIInitiatorMock{}, &mountutil.MountMock{})
	_, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "vol-001",
		StagingTargetPath: "/tmp/staging",
		VolumeCapability:  mountCapability(),
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "filesystem (mount) volume mode is not supported")
}

func TestNodeStageVolume_MissingPublishContextFields(t *testing.T) {
	ns := fakeNodeServer(&ISCSIInitiatorMock{}, &mountutil.MountMock{})
	_, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "vol-001",
		StagingTargetPath: "/tmp/staging",
		VolumeCapability:  blockCapability(),
		PublishContext:    map[string]string{}, // missing fields
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "publish_context must contain")
}

func TestNodeStageVolume_IdempotentSessionActive(t *testing.T) {
	iscsiMock := &ISCSIInitiatorMock{}
	mountMock := &mountutil.MountMock{}
	ns := fakeNodeServer(iscsiMock, mountMock)

	stagingDir := t.TempDir()
	portal := "10.0.0.1:3260"
	iqn := "iqn.2010-10.org.openstack:volume-vol-001"

	// Override devicePathPrefix to avoid writing to the real /dev/disk/by-path/.
	origPrefix := devicePathPrefix
	devicePathPrefix = t.TempDir()
	t.Cleanup(func() { devicePathPrefix = origPrefix })

	devicePath := BuildDevicePath(portal, iqn, 1)

	// Create the device path so os.Stat succeeds for the idempotency check
	assert.NoError(t, os.MkdirAll(filepath.Dir(devicePath), 0755))
	assert.NoError(t, os.WriteFile(devicePath, []byte("x"), 0644))

	// Mock: session is active
	iscsiMock.On("IsSessionActive", mock.Anything, iqn, portal).Return(true, nil)

	resp, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "vol-001",
		StagingTargetPath: stagingDir,
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)

	// Verify device path file was written (idempotent)
	data, readErr := os.ReadFile(filepath.Join(stagingDir, devicePathFile))
	assert.NoError(t, readErr)
	assert.Equal(t, devicePath, string(data))

	iscsiMock.AssertExpectations(t)
}

func TestNodeStageVolume_Success(t *testing.T) {
	iscsiMock := &ISCSIInitiatorMock{}
	mountMock := &mountutil.MountMock{}
	ns := fakeNodeServer(iscsiMock, mountMock)

	stagingDir := t.TempDir()
	portal := "10.0.0.1:3260"
	iqn := "iqn.2010-10.org.openstack:volume-vol-001"

	// Override devicePathPrefix to avoid writing to the real /dev/disk/by-path/.
	origPrefix := devicePathPrefix
	devicePathPrefix = t.TempDir()
	t.Cleanup(func() { devicePathPrefix = origPrefix })

	devicePath := BuildDevicePath(portal, iqn, 1)

	// Create the device file so WaitForDevice succeeds
	assert.NoError(t, os.MkdirAll(filepath.Dir(devicePath), 0755))
	assert.NoError(t, os.WriteFile(devicePath, []byte("x"), 0644))

	// Mock expectations
	iscsiMock.On("IsSessionActive", mock.Anything, iqn, portal).Return(false, nil)
	iscsiMock.On("Discovery", mock.Anything, portal).Return(nil)
	iscsiMock.On("Login", mock.Anything, iqn, portal).Return(nil)

	resp, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "vol-001",
		StagingTargetPath: stagingDir,
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)

	// Verify device path file was written
	data, err := os.ReadFile(filepath.Join(stagingDir, devicePathFile))
	assert.NoError(t, err)
	assert.Equal(t, devicePath, string(data))

	iscsiMock.AssertExpectations(t)
}

func TestNodeStageVolume_WithCHAP(t *testing.T) {
	iscsiMock := &ISCSIInitiatorMock{}
	mountMock := &mountutil.MountMock{}
	ns := fakeNodeServer(iscsiMock, mountMock)

	stagingDir := t.TempDir()
	portal := "10.0.0.1:3260"
	iqn := "iqn.2010-10.org.openstack:volume-vol-001"

	// Override devicePathPrefix to avoid writing to the real /dev/disk/by-path/.
	origPrefix := devicePathPrefix
	devicePathPrefix = t.TempDir()
	t.Cleanup(func() { devicePathPrefix = origPrefix })

	devicePath := BuildDevicePath(portal, iqn, 1)

	// Create the device file so WaitForDevice succeeds
	assert.NoError(t, os.MkdirAll(filepath.Dir(devicePath), 0755))
	assert.NoError(t, os.WriteFile(devicePath, []byte("x"), 0644))

	iscsiMock.On("IsSessionActive", mock.Anything, iqn, portal).Return(false, nil)
	iscsiMock.On("Discovery", mock.Anything, portal).Return(nil)
	iscsiMock.On("SetCHAPAuth", mock.Anything, iqn, portal, "user1", "secret1").Return(nil)
	iscsiMock.On("Login", mock.Anything, iqn, portal).Return(nil)

	resp, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "vol-001",
		StagingTargetPath: stagingDir,
		VolumeCapability:  blockCapability(),
		PublishContext:    stageCHAPPublishContext(),
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	iscsiMock.AssertExpectations(t)
}

func TestNodeStageVolume_CHAPMissingCredentials(t *testing.T) {
	iscsiMock := &ISCSIInitiatorMock{}
	mountMock := &mountutil.MountMock{}
	ns := fakeNodeServer(iscsiMock, mountMock)

	iscsiMock.On("IsSessionActive", mock.Anything, mock.Anything, mock.Anything).Return(false, nil)
	iscsiMock.On("Discovery", mock.Anything, mock.Anything).Return(nil)

	_, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "vol-001",
		StagingTargetPath: t.TempDir(),
		VolumeCapability:  blockCapability(),
		PublishContext: map[string]string{
			PublishContextTargetPortal: "10.0.0.1:3260",
			PublishContextTargetIQN:    "iqn.test",
			PublishContextTargetLUN:    "0",
			PublishContextAuthMethod:   "CHAP",
			// Missing username and password
		},
	})

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "auth_username or auth_password missing")
}

func TestNodeStageVolume_WaitForDeviceTimeout(t *testing.T) {
	iscsiMock := &ISCSIInitiatorMock{}
	mountMock := &mountutil.MountMock{}
	ns := fakeNodeServer(iscsiMock, mountMock)
	ns.Opts.DeviceWaitTimeout = 1 // 1 second — device file never created

	portal := "10.0.0.1:3260"
	iqn := "iqn.2010-10.org.openstack:volume-vol-001"

	iscsiMock.On("IsSessionActive", mock.Anything, iqn, portal).Return(false, nil)
	iscsiMock.On("Discovery", mock.Anything, portal).Return(nil)
	iscsiMock.On("Login", mock.Anything, iqn, portal).Return(nil)
	// Cleanup calls after WaitForDevice failure
	iscsiMock.On("Logout", mock.Anything, iqn, portal).Return(nil)
	iscsiMock.On("DeleteNode", mock.Anything, iqn, portal).Return(nil)

	_, err := ns.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "vol-001",
		StagingTargetPath: t.TempDir(),
		VolumeCapability:  blockCapability(),
		PublishContext:    stagePublishContext(),
	})

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "did not appear")
	// Verify cleanup: both Logout and DeleteNode were called
	iscsiMock.AssertCalled(t, "Logout", mock.Anything, iqn, portal)
	iscsiMock.AssertCalled(t, "DeleteNode", mock.Anything, iqn, portal)
}

// ── NodeUnstageVolume Tests ──────────────────────────────────────────────────

func TestNodeUnstageVolume_MissingVolumeID(t *testing.T) {
	ns := fakeNodeServer(&ISCSIInitiatorMock{}, &mountutil.MountMock{})
	_, err := ns.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		StagingTargetPath: "/tmp/staging",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "VolumeId must be provided")
}

func TestNodeUnstageVolume_IdempotentNoStagingDir(t *testing.T) {
	iscsiMock := &ISCSIInitiatorMock{}
	ns := fakeNodeServer(iscsiMock, &mountutil.MountMock{})

	resp, err := ns.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          "vol-001",
		StagingTargetPath: "/nonexistent/path/that/does/not/exist",
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)
}

func TestNodeUnstageVolume_Success(t *testing.T) {
	iscsiMock := &ISCSIInitiatorMock{}
	ns := fakeNodeServer(iscsiMock, &mountutil.MountMock{})

	stagingDir := t.TempDir()
	portal := "10.0.0.1:3260"
	iqn := "iqn.2010-10.org.openstack:volume-vol-001"
	devicePath := BuildDevicePath(portal, iqn, 1)

	// Write the device path file
	assert.NoError(t, os.WriteFile(filepath.Join(stagingDir, devicePathFile), []byte(devicePath), 0640))

	iscsiMock.On("Logout", mock.Anything, iqn, portal).Return(nil)
	iscsiMock.On("DeleteNode", mock.Anything, iqn, portal).Return(nil)

	resp, err := ns.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          "vol-001",
		StagingTargetPath: stagingDir,
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)

	// Verify staging dir was cleaned up
	_, statErr := os.Stat(filepath.Join(stagingDir, devicePathFile))
	assert.True(t, os.IsNotExist(statErr))

	iscsiMock.AssertExpectations(t)
}

func TestNodeUnstageVolume_LogoutFails(t *testing.T) {
	iscsiMock := &ISCSIInitiatorMock{}
	ns := fakeNodeServer(iscsiMock, &mountutil.MountMock{})

	stagingDir := t.TempDir()
	portal := "10.0.0.1:3260"
	iqn := "iqn.2010-10.org.openstack:volume-vol-001"
	devicePath := BuildDevicePath(portal, iqn, 1)

	assert.NoError(t, os.WriteFile(filepath.Join(stagingDir, devicePathFile), []byte(devicePath), 0640))

	iscsiMock.On("Logout", mock.Anything, iqn, portal).Return(fmt.Errorf("iscsiadm: session not found"))

	_, err := ns.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          "vol-001",
		StagingTargetPath: stagingDir,
	})

	assert.Error(t, err)
	assert.Contains(t, err.Error(), "iSCSI logout failed")
	iscsiMock.AssertExpectations(t)
}

// ── NodePublishVolume Tests ──────────────────────────────────────────────────

func TestNodePublishVolume_MissingVolumeID(t *testing.T) {
	ns := fakeNodeServer(&ISCSIInitiatorMock{}, &mountutil.MountMock{})
	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		StagingTargetPath: "/tmp/staging",
		TargetPath:        "/tmp/target",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "VolumeId must be provided")
}

func TestNodePublishVolume_MissingTargetPath(t *testing.T) {
	ns := fakeNodeServer(&ISCSIInitiatorMock{}, &mountutil.MountMock{})
	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "vol-001",
		StagingTargetPath: "/tmp/staging",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "TargetPath must be provided")
}

func TestNodePublishVolume_IdempotentAlreadyMounted(t *testing.T) {
	iscsiMock := &ISCSIInitiatorMock{}
	mountMock := &mountutil.MountMock{}
	ns := fakeNodeServer(iscsiMock, mountMock)

	stagingDir := t.TempDir()
	devicePath := "/dev/disk/by-path/ip-10.0.0.1:3260-iscsi-iqn.test-lun-0"
	assert.NoError(t, os.WriteFile(filepath.Join(stagingDir, devicePathFile), []byte(devicePath), 0640))

	// Create a real file for the symlink target, or just skip EvalSymlinks issue
	// by directly testing the mount check
	targetPath := filepath.Join(t.TempDir(), "block-device")

	// Mock: IsLikelyNotMountPointAttach returns false (already mounted)
	mountMock.On("IsLikelyNotMountPointAttach", targetPath).Return(false, nil)

	resp, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "vol-001",
		StagingTargetPath: stagingDir,
		TargetPath:        targetPath,
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	mountMock.AssertExpectations(t)
}

func TestNodePublishVolume_RejectsMount(t *testing.T) {
	ns := fakeNodeServer(&ISCSIInitiatorMock{}, &mountutil.MountMock{})
	_, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "vol-001",
		StagingTargetPath: "/tmp/staging",
		TargetPath:        "/tmp/target",
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "filesystem (mount) volume mode is not supported")
}

func TestNodePublishVolume_BindMount(t *testing.T) {
	iscsiMock := &ISCSIInitiatorMock{}
	mountMock := &mountutil.MountMock{}
	ns := fakeNodeServer(iscsiMock, mountMock)

	// Set up staging dir with a device path file
	stagingDir := t.TempDir()
	targetDir := t.TempDir()
	targetPath := filepath.Join(targetDir, "block-device")

	// Create a real file to act as the block device (symlink target)
	realDevice := filepath.Join(t.TempDir(), "sda")
	assert.NoError(t, os.WriteFile(realDevice, []byte{}, 0640))

	// Create a symlink that looks like a /dev/disk/by-path entry
	deviceSymlink := filepath.Join(t.TempDir(), "ip-10.0.0.1:3260-iscsi-iqn.test-lun-0")
	assert.NoError(t, os.Symlink(realDevice, deviceSymlink))

	// Write the symlink path into the staging device-path file
	assert.NoError(t, os.WriteFile(filepath.Join(stagingDir, devicePathFile), []byte(deviceSymlink), 0640))

	// Mock: not yet mounted.
	// Note: MakeFile() and Mounter() on MountMock are hardcoded stubs that
	// bypass _m.Called(), so they cannot be mocked via On()/Return().
	// MakeFile always returns nil; Mounter() returns a fresh FakeMounter.
	mountMock.On("IsLikelyNotMountPointAttach", targetPath).Return(true, nil)

	resp, err := ns.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "vol-001",
		StagingTargetPath: stagingDir,
		TargetPath:        targetPath,
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Block{
				Block: &csi.VolumeCapability_BlockVolume{},
			},
			AccessMode: &csi.VolumeCapability_AccessMode{
				Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
			},
		},
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	mountMock.AssertExpectations(t)
}

// ── NodeUnpublishVolume Tests ────────────────────────────────────────────────

func TestNodeUnpublishVolume_MissingVolumeID(t *testing.T) {
	ns := fakeNodeServer(&ISCSIInitiatorMock{}, &mountutil.MountMock{})
	_, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		TargetPath: "/tmp/target",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "VolumeId must be provided")
}

func TestNodeUnpublishVolume_MissingTargetPath(t *testing.T) {
	ns := fakeNodeServer(&ISCSIInitiatorMock{}, &mountutil.MountMock{})
	_, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId: "vol-001",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "TargetPath must be provided")
}

func TestNodeUnpublishVolume_Success(t *testing.T) {
	iscsiMock := &ISCSIInitiatorMock{}
	mountMock := &mountutil.MountMock{}
	ns := fakeNodeServer(iscsiMock, mountMock)

	targetPath := "/tmp/target/block"
	mountMock.On("UnmountPath", targetPath).Return(nil)

	resp, err := ns.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "vol-001",
		TargetPath: targetPath,
	})

	assert.NoError(t, err)
	assert.NotNil(t, resp)
	mountMock.AssertExpectations(t)
}

// ── NodeGetVolumeStats Tests ─────────────────────────────────────────────────

func TestNodeGetVolumeStats_MissingVolumeID(t *testing.T) {
	ns := fakeNodeServer(&ISCSIInitiatorMock{}, &mountutil.MountMock{})
	_, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumePath: "/dev/sda",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "VolumeId must be provided")
}

func TestNodeGetVolumeStats_MissingVolumePath(t *testing.T) {
	ns := fakeNodeServer(&ISCSIInitiatorMock{}, &mountutil.MountMock{})
	_, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId: "vol-001",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "VolumePath must be provided")
}

func TestNodeGetVolumeStats_PathNotFound(t *testing.T) {
	ns := fakeNodeServer(&ISCSIInitiatorMock{}, &mountutil.MountMock{})
	_, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "vol-001",
		VolumePath: "/nonexistent/device/path",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}

func TestNodeGetVolumeStats_BlockDevice(t *testing.T) {
	mountMock := &mountutil.MountMock{}
	ns := fakeNodeServer(&ISCSIInitiatorMock{}, mountMock)

	// Create a temp file to act as volume path
	tmpFile := filepath.Join(t.TempDir(), "device")
	assert.NoError(t, os.WriteFile(tmpFile, []byte("x"), 0644))

	mountMock.On("GetDeviceStats", tmpFile).Return(&mountutil.DeviceStats{
		Block:      true,
		TotalBytes: 10737418240, // 10 GiB
	}, nil)

	resp, err := ns.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "vol-001",
		VolumePath: tmpFile,
	})

	assert.NoError(t, err)
	assert.Len(t, resp.Usage, 1)
	assert.Equal(t, int64(10737418240), resp.Usage[0].Total)
	assert.Equal(t, csi.VolumeUsage_BYTES, resp.Usage[0].Unit)
	mountMock.AssertExpectations(t)
}

// ── NodeExpandVolume Tests ───────────────────────────────────────────────────

func TestNodeExpandVolume_Unimplemented(t *testing.T) {
	ns := fakeNodeServer(&ISCSIInitiatorMock{}, &mountutil.MountMock{})
	_, err := ns.NodeExpandVolume(context.Background(), &csi.NodeExpandVolumeRequest{
		VolumeId:   "vol-001",
		VolumePath: "/dev/sda",
	})
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "Unimplemented")
}

// ── NodeGetCapabilities Tests ────────────────────────────────────────────────

func TestNodeGetCapabilities(t *testing.T) {
	ns := fakeNodeServer(&ISCSIInitiatorMock{}, &mountutil.MountMock{})
	resp, err := ns.NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})

	assert.NoError(t, err)
	assert.Len(t, resp.Capabilities, 2)

	capTypes := make(map[csi.NodeServiceCapability_RPC_Type]bool)
	for _, cap := range resp.Capabilities {
		capTypes[cap.GetRpc().GetType()] = true
	}
	assert.True(t, capTypes[csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME])
	assert.True(t, capTypes[csi.NodeServiceCapability_RPC_GET_VOLUME_STATS])
}

// ── ISCSIInitiator Helper Tests ──────────────────────────────────────────────

func TestBuildDevicePath(t *testing.T) {
	path := BuildDevicePath("10.0.0.1:3260", "iqn.2010-10.org.openstack:volume-test", 1)
	assert.Equal(t, "/dev/disk/by-path/ip-10.0.0.1:3260-iscsi-iqn.2010-10.org.openstack:volume-test-lun-1", path)
}

func TestParseDevicePath_Valid(t *testing.T) {
	path := "/dev/disk/by-path/ip-10.0.0.1:3260-iscsi-iqn.2010-10.org.openstack:volume-test-lun-1"
	portal, iqn, lun, err := ParseDevicePath(path)
	assert.NoError(t, err)
	assert.Equal(t, "10.0.0.1:3260", portal)
	assert.Equal(t, "iqn.2010-10.org.openstack:volume-test", iqn)
	assert.Equal(t, 1, lun)
}

func TestParseDevicePath_Invalid(t *testing.T) {
	_, _, _, err := ParseDevicePath("/dev/sda")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "cannot parse")
}

func TestBuildParseDevicePath_Roundtrip(t *testing.T) {
	portal := "192.168.1.100:3260"
	iqn := "iqn.2024-01.com.example:storage.target1"
	lun := 5

	path := BuildDevicePath(portal, iqn, lun)
	pPortal, pIQN, pLUN, err := ParseDevicePath(path)
	assert.NoError(t, err)
	assert.Equal(t, portal, pPortal)
	assert.Equal(t, iqn, pIQN)
	assert.Equal(t, lun, pLUN)
}

func TestReadInitiatorIQN_Valid(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "initiatorname.iscsi")
	content := `## Generated by /sbin/iscsi-iname
InitiatorName=iqn.1993-08.org.debian:01:aabbccdd
`
	assert.NoError(t, os.WriteFile(tmpFile, []byte(content), 0644))

	iqn, err := ReadInitiatorIQN(tmpFile)
	assert.NoError(t, err)
	assert.Equal(t, "iqn.1993-08.org.debian:01:aabbccdd", iqn)
}

func TestReadInitiatorIQN_MissingFile(t *testing.T) {
	_, err := ReadInitiatorIQN("/nonexistent/file")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to open")
}

func TestReadInitiatorIQN_EmptyName(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "initiatorname.iscsi")
	content := "InitiatorName=\n"
	assert.NoError(t, os.WriteFile(tmpFile, []byte(content), 0644))

	_, err := ReadInitiatorIQN(tmpFile)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty InitiatorName")
}

func TestReadInitiatorIQN_NoInitiatorNameLine(t *testing.T) {
	tmpFile := filepath.Join(t.TempDir(), "initiatorname.iscsi")
	content := "# Just comments\n# No InitiatorName line\n"
	assert.NoError(t, os.WriteFile(tmpFile, []byte(content), 0644))

	_, err := ReadInitiatorIQN(tmpFile)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "InitiatorName not found")
}

// ── Probe Node Health Tests ──────────────────────────────────────────────────

func TestProbe_NodeOnlyMode_IscsiadmAvailable(t *testing.T) {
	iscsiMock := &ISCSIInitiatorMock{}
	ids := &identityServer{
		Driver:    fakeDriver(),
		cloud:     nil, // node-only mode
		iscsiInit: iscsiMock,
	}

	iscsiMock.On("CheckIscsiadm", mock.Anything).Return(nil)

	resp, err := ids.Probe(context.Background(), &csi.ProbeRequest{})
	assert.NoError(t, err)
	assert.True(t, resp.Ready.Value)
	iscsiMock.AssertExpectations(t)
}

func TestProbe_NodeOnlyMode_IscsiadmUnavailable(t *testing.T) {
	iscsiMock := &ISCSIInitiatorMock{}
	ids := &identityServer{
		Driver:    fakeDriver(),
		cloud:     nil,
		iscsiInit: iscsiMock,
	}

	iscsiMock.On("CheckIscsiadm", mock.Anything).Return(assert.AnError)

	resp, err := ids.Probe(context.Background(), &csi.ProbeRequest{})
	assert.NoError(t, err) // Probe returns nil error with Ready=false
	assert.False(t, resp.Ready.Value)
	iscsiMock.AssertExpectations(t)
}

func TestProbe_NodeOnlyMode_NoISCSIInit(t *testing.T) {
	ids := &identityServer{
		Driver:    fakeDriver(),
		cloud:     nil,
		iscsiInit: nil, // no iSCSI initiator wired — SetupNodeService not called
	}

	resp, err := ids.Probe(context.Background(), &csi.ProbeRequest{})
	assert.NoError(t, err)
	assert.False(t, resp.Ready.Value, "should report not-ready when iscsiInit is nil in node-only mode")
}
