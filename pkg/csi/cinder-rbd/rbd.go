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
	"time"

	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-rbd/openstack"
)

// Sentinel errors for the node data path. They exist so the node server can
// map failures to gRPC codes without string matching.
var (
	// ErrExclusiveLockDenied means another writable client holds the Ceph
	// exclusive lock. There is deliberately no fallback path that retries
	// without --exclusive.
	ErrExclusiveLockDenied = errors.New("rbd: exclusive lock denied")

	// ErrIdentityMismatch means a device does not map the image the caller
	// expected. The caller must isolate, never unmap.
	ErrIdentityMismatch = errors.New("rbd: mapped image identity mismatch")

	// ErrNotMapped means no kernel mapping exists for the requested image.
	ErrNotMapped = errors.New("rbd: image is not mapped")

	// ErrDeviceBusy means unmap could not proceed because the device is in use.
	ErrDeviceBusy = errors.New("rbd: device busy")
)

// ImageIdentity is the tuple that must match before a mapping is reused,
// published to a pod, or unmapped.
//
// ClusterFSID is authoritative and is read from the kernel: on the validated
// platform (Linux 6.6) /sys/bus/rbd/devices/<N>/cluster_fsid is present.
type ImageIdentity struct {
	ClusterFSID string
	ClusterName string
	Pool        string
	Image       string
}

// String renders the identity for logs. It contains no credential material.
func (i ImageIdentity) String() string {
	return fmt.Sprintf("%s/%s@%s", i.Pool, i.Image, i.ClusterFSID)
}

// Matches reports whether other refers to the same Ceph image.
// ClusterName is intentionally excluded: it is a local alias, whereas the FSID
// is the cluster's real identity.
func (i ImageIdentity) Matches(other ImageIdentity) bool {
	return i.ClusterFSID == other.ClusterFSID &&
		i.Pool == other.Pool &&
		i.Image == other.Image
}

// IsComplete reports whether every field required for verification is set.
// An incomplete identity must never be used to authorize an unmap.
func (i ImageIdentity) IsComplete() bool {
	return i.ClusterFSID != "" && i.Pool != "" && i.Image != ""
}

// MapRequest describes one exclusive kernel mapping.
type MapRequest struct {
	Identity    ImageIdentity
	Monitors    []openstack.MonAddr
	UserID      string // must equal connection_info.auth_username
	ConfPath    string // generated ceph.conf
	KeyringPath string // generated keyring, mode 0400
	Exclusive   bool   // always true for writable maps
	ReadOnly    bool
	Timeout     time.Duration
}

// MappedDevice is one live kernel RBD mapping.
type MappedDevice struct {
	ID          int
	Pool        string
	Namespace   string
	Image       string
	Snap        string
	DevicePath  string
	ClusterFSID string
}

// Identity returns the ImageIdentity of the mapping.
func (d MappedDevice) Identity() ImageIdentity {
	return ImageIdentity{ClusterFSID: d.ClusterFSID, Pool: d.Pool, Image: d.Image}
}

// RBDMapper is the node data-path abstraction over the bundled Ceph rbd CLI
// and the kernel RBD sysfs interface.
//
// Implementations must obey these invariants (design §6.6):
//
//  1. Map never falls back to a non-exclusive mapping.
//  2. /dev/rbdN is not an identity. Device numbers are recycled, so every
//     reuse, publish and unmap is gated on VerifyIdentity.
//  3. ListMapped is the inventory of record. A missing staging record is not
//     proof that no mapping exists.
//  4. Every rbd invocation passes --conf. The WRCP host ships an
//     unsubstituted ceph.conf template, so a fallback read pollutes stderr and
//     can misreport the cluster.
//  5. Success is decided from exit status and parsed stdout, never from empty
//     stderr.
type RBDMapper interface {
	// Map performs an exclusive kernel map and returns the created device.
	Map(ctx context.Context, req MapRequest) (MappedDevice, error)

	// Unmap releases a device. Callers must verify identity first.
	Unmap(ctx context.Context, devicePath string, timeout time.Duration) error

	// ListMapped returns every kernel RBD mapping on this host, including
	// mappings owned by other clients such as the platform Ceph-CSI.
	ListMapped(ctx context.Context) ([]MappedDevice, error)

	// VerifyIdentity confirms devicePath currently maps want, returning
	// ErrIdentityMismatch when it does not.
	VerifyIdentity(ctx context.Context, devicePath string, want ImageIdentity) error

	// DeviceSize returns the block device size in bytes.
	DeviceSize(ctx context.Context, devicePath string) (int64, error)

	// Flush issues a buffer flush before unmap.
	Flush(ctx context.Context, devicePath string) error

	// LockHolders returns the current exclusive-lock holders for an image.
	// An empty slice means the image is unlocked.
	LockHolders(ctx context.Context, req MapRequest) ([]string, error)

	// CheckClient reports the bundled rbd CLI version string. Used by the
	// node Probe and by startup validation.
	CheckClient(ctx context.Context) (string, error)
}
