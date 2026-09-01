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

package sanity

import (
	"os"

	cpomount "k8s.io/cloud-provider-openstack/pkg/util/mount"
	"k8s.io/mount-utils"
	exectesting "k8s.io/utils/exec/testing"
)

// fakeMount is an in-memory IMount for the sanity suite.
//
// Only the methods this driver actually calls are meaningful:
// IsLikelyNotMountPointAttach, MakeFile, Mounter().Mount, UnmountPath and
// GetDeviceStats. The rest satisfy the interface.
type fakeMount struct {
	base    *mount.SafeFormatAndMount
	fake    *mount.FakeMounter
	wrapped *cpomount.Mount
}

var _ cpomount.IMount = &fakeMount{}

// getFakeMountProvider returns a fresh fake mounter.
//
// Each call gets its own FakeMounter so parallel sanity cases cannot see each
// other's mount points.
func getFakeMountProvider() cpomount.IMount {
	fm := &mount.FakeMounter{MountPoints: []mount.MountPoint{}}
	base := &mount.SafeFormatAndMount{
		Interface: fm,
		Exec:      &exectesting.FakeExec{DisableScripts: true},
	}
	return &fakeMount{
		base:    base,
		fake:    fm,
		wrapped: &cpomount.Mount{BaseMounter: base},
	}
}

func (m *fakeMount) Mounter() *mount.SafeFormatAndMount { return m.base }

func (m *fakeMount) ScanForAttach(_ string) error { return nil }

func (m *fakeMount) IsLikelyNotMountPointAttach(targetpath string) (bool, error) {
	return m.wrapped.IsLikelyNotMountPointAttach(targetpath)
}

func (m *fakeMount) UnmountPath(mountPath string) error {
	return m.wrapped.UnmountPath(mountPath)
}

func (m *fakeMount) GetInstanceID() (string, error) { return "fake-instance", nil }

func (m *fakeMount) GetDevicePath(_ string) (string, error) { return "", nil }

func (m *fakeMount) MakeDir(pathname string) error {
	return os.MkdirAll(pathname, 0o750)
}

// MakeFile creates the raw-block bind target, replacing a kubelet-created
// directory as the production implementation does.
func (m *fakeMount) MakeFile(pathname string) error {
	if info, err := os.Stat(pathname); err == nil && info.IsDir() {
		if err := os.Remove(pathname); err != nil {
			return err
		}
	}
	f, err := os.OpenFile(pathname, os.O_CREATE, 0o640)
	if err != nil {
		if os.IsExist(err) {
			return nil
		}
		return err
	}
	return f.Close()
}

// GetDeviceStats reports a fixed capacity for a raw block device.
func (m *fakeMount) GetDeviceStats(_ string) (*cpomount.DeviceStats, error) {
	return &cpomount.DeviceStats{Block: true, TotalBytes: 1 << 30}, nil
}

func (m *fakeMount) GetMountFs(_ string) ([]byte, error) { return []byte("ext4"), nil }
