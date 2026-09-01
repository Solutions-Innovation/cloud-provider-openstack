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
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeSysfsDevice describes one device directory to create.
type fakeSysfsDevice struct {
	id    int
	attrs map[string]string
}

// withFakeSysfs redirects sysfsRBDRoot at a temporary tree for the duration of
// the test.
//
// Tests must never read the real /sys/bus/rbd: on a developer host with live
// Ceph mappings, code that writes there could disturb real kernel state.
func withFakeSysfs(t *testing.T, devices ...fakeSysfsDevice) string {
	t.Helper()

	root := t.TempDir()
	for _, d := range devices {
		dir := filepath.Join(root, strconv.Itoa(d.id))
		require.NoError(t, os.MkdirAll(dir, 0o755))
		for k, v := range d.attrs {
			// The kernel appends a newline; reproduce that so the trimming is
			// actually exercised.
			require.NoError(t, os.WriteFile(filepath.Join(dir, k), []byte(v+"\n"), 0o644))
		}
	}

	orig := sysfsRBDRoot
	sysfsRBDRoot = root
	t.Cleanup(func() { sysfsRBDRoot = orig })
	return root
}

// cinderDevice is a device directory matching what the lab reported for a
// Cinder-created image.
func cinderDevice(id int) fakeSysfsDevice {
	return fakeSysfsDevice{id: id, attrs: map[string]string{
		sysfsAttrClusterFSID: "c5f7876d-258c-4152-b26a-a3ab532fda28",
		sysfsAttrPool:        "cinder-volumes",
		sysfsAttrPoolNS:      "",
		sysfsAttrName:        "3018df26-0ba3-45a3-adfd-4a84ed59fff1",
		sysfsAttrImageID:     "3337ebc9501c13",
		sysfsAttrSize:        "1073741824",
		sysfsAttrFeatures:    "0x000000000000003d",
		sysfsAttrCurrentSnap: "-",
	}}
}

func TestDeviceIDFromPath(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		want    int
		wantErr bool
	}{
		{name: "single digit", path: "/dev/rbd0", want: 0},
		{name: "multi digit", path: "/dev/rbd42", want: 42},
		{name: "partition suffix is rejected", path: "/dev/rbd0p1", wantErr: true},
		{name: "named path is rejected", path: "/dev/rbd/pool/image", wantErr: true},
		{name: "unrelated device is rejected", path: "/dev/sda", wantErr: true},
		{name: "empty is rejected", path: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := deviceIDFromPath(tt.path)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			assert.Equal(t, tt.path, devicePathFromID(got))
		})
	}
}

func TestReadSysfsDevice_ReadsLabAttributeSet(t *testing.T) {
	withFakeSysfs(t, cinderDevice(5))

	dev, err := readSysfsDevice(5)
	require.NoError(t, err)

	assert.Equal(t, 5, dev.ID)
	assert.Equal(t, "c5f7876d-258c-4152-b26a-a3ab532fda28", dev.ClusterFSID)
	assert.Equal(t, "cinder-volumes", dev.Pool)
	assert.Equal(t, "3018df26-0ba3-45a3-adfd-4a84ed59fff1", dev.Image)
	assert.Equal(t, "3337ebc9501c13", dev.ImageID)
	assert.Equal(t, int64(1073741824), dev.SizeBytes)
	// current_snap of "-" means no snapshot, not a snapshot named "-".
	assert.Empty(t, dev.Snap)
}

// The identity attributes are mandatory. A kernel that does not expose them
// cannot be verified, so reading must fail rather than yield a partial identity
// that could authorize an unmap.
func TestReadSysfsDevice_MissingIdentityAttributeFailsClosed(t *testing.T) {
	for _, missing := range []string{sysfsAttrClusterFSID, sysfsAttrPool, sysfsAttrName} {
		t.Run("missing "+missing, func(t *testing.T) {
			d := cinderDevice(1)
			delete(d.attrs, missing)
			withFakeSysfs(t, d)

			_, err := readSysfsDevice(1)
			require.Error(t, err)
			assert.Contains(t, err.Error(), missing)
		})
	}
}

func TestReadSysfsDevice_EmptyIdentityAttributeFailsClosed(t *testing.T) {
	d := cinderDevice(1)
	d.attrs[sysfsAttrClusterFSID] = ""
	withFakeSysfs(t, d)

	_, err := readSysfsDevice(1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), sysfsAttrClusterFSID)
}

func TestReadSysfsDevice_MissingOptionalAttributesAreTolerated(t *testing.T) {
	withFakeSysfs(t, fakeSysfsDevice{id: 2, attrs: map[string]string{
		sysfsAttrClusterFSID: "fsid-1",
		sysfsAttrPool:        "p",
		sysfsAttrName:        "i",
	}})

	dev, err := readSysfsDevice(2)
	require.NoError(t, err)
	assert.Equal(t, "fsid-1", dev.ClusterFSID)
	assert.Zero(t, dev.SizeBytes)
	assert.Empty(t, dev.Features)
}

// The platform Ceph-CSI shares the same kernel path, so listing must surface
// mappings this driver does not own rather than hiding them.
func TestListSysfsDevices_IncludesForeignMappings(t *testing.T) {
	withFakeSysfs(t,
		fakeSysfsDevice{id: 0, attrs: map[string]string{
			sysfsAttrClusterFSID: "c5f7876d-258c-4152-b26a-a3ab532fda28",
			sysfsAttrPool:        "kube-rbd",
			sysfsAttrName:        "csi-vol-746d4c96",
		}},
		cinderDevice(5),
	)

	devs, err := listSysfsDevices()
	require.NoError(t, err)
	require.Len(t, devs, 2)

	pools := map[string]string{}
	for _, d := range devs {
		pools[d.Pool] = d.Image
	}
	assert.Contains(t, pools, "kube-rbd", "platform Ceph-CSI mappings must be visible")
	assert.Contains(t, pools, "cinder-volumes")
}

// One unreadable device must not block reconciliation of every other volume.
func TestListSysfsDevices_SkipsUnreadableEntries(t *testing.T) {
	root := withFakeSysfs(t, cinderDevice(5))
	// A device directory with no attributes, as seen mid-unmap.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "9"), 0o755))
	// A non-numeric entry, which sysfs also contains.
	require.NoError(t, os.MkdirAll(filepath.Join(root, "not-a-device"), 0o755))

	devs, err := listSysfsDevices()
	require.NoError(t, err)
	require.Len(t, devs, 1)
	assert.Equal(t, 5, devs[0].ID)
}

// A host with no kernel RBD support must yield an empty list, not an error:
// the node plugin has to start and report not-ready rather than crash.
func TestListSysfsDevices_AbsentRootIsNotAnError(t *testing.T) {
	orig := sysfsRBDRoot
	sysfsRBDRoot = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { sysfsRBDRoot = orig })

	devs, err := listSysfsDevices()
	require.NoError(t, err)
	assert.Empty(t, devs)
}

func TestSysfsDevice_Identity(t *testing.T) {
	withFakeSysfs(t, cinderDevice(5))

	dev, err := readSysfsDevice(5)
	require.NoError(t, err)

	want := ImageIdentity{
		ClusterFSID: "c5f7876d-258c-4152-b26a-a3ab532fda28",
		Pool:        "cinder-volumes",
		Image:       "3018df26-0ba3-45a3-adfd-4a84ed59fff1",
	}
	assert.True(t, dev.Identity().Matches(want))
	assert.True(t, dev.Identity().IsComplete())
}
