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
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// sysfsRBDRoot is a package var so tests can redirect it to t.TempDir().
// No test may read or write the real /sys/bus/rbd.
var sysfsRBDRoot = "/sys/bus/rbd/devices"

// sysfs attribute names, verified present on the WRCP 24.09 kernel (Linux 6.6).
//
// Note there is no "snap" attribute: it is current_snap. A namespace is
// pool_ns. Getting these names wrong would silently skip an identity check,
// so they are constants rather than inline strings.
const (
	sysfsAttrClusterFSID = "cluster_fsid"
	sysfsAttrPool        = "pool"
	sysfsAttrPoolNS      = "pool_ns"
	sysfsAttrName        = "name"
	sysfsAttrImageID     = "image_id"
	sysfsAttrSize        = "size"
	sysfsAttrFeatures    = "features"
	sysfsAttrCurrentSnap = "current_snap"
)

// devicePathRe matches a kernel RBD device path and captures its index.
var devicePathRe = regexp.MustCompile(`^/dev/rbd(\d+)$`)

// deviceIDFromPath extracts N from /dev/rbdN.
//
// The number is only an index into sysfs, never an identity: kernel device
// numbers are reused after unmap, so the attributes found there must still be
// compared against the expected image.
func deviceIDFromPath(devicePath string) (int, error) {
	m := devicePathRe.FindStringSubmatch(devicePath)
	if m == nil {
		return 0, fmt.Errorf("rbd: %q is not a kernel RBD device path (/dev/rbdN)", devicePath)
	}
	id, err := strconv.Atoi(m[1])
	if err != nil {
		return 0, fmt.Errorf("rbd: cannot parse device index from %q: %w", devicePath, err)
	}
	return id, nil
}

// devicePathFromID renders /dev/rbdN.
func devicePathFromID(id int) string {
	return "/dev/rbd" + strconv.Itoa(id)
}

// sysfsDevice is the kernel's view of one mapping.
type sysfsDevice struct {
	ID          int
	ClusterFSID string
	Pool        string
	Namespace   string
	Image       string
	ImageID     string
	Snap        string
	SizeBytes   int64
	Features    string
}

// Identity returns the ImageIdentity the kernel reports.
func (d sysfsDevice) Identity() ImageIdentity {
	return ImageIdentity{ClusterFSID: d.ClusterFSID, Pool: d.Pool, Image: d.Image}
}

// readSysfsAttr reads one attribute, trimming the trailing newline the kernel
// appends.
func readSysfsAttr(id int, attr string) (string, error) {
	// filepath.Join cleans the path, and both components are driver-controlled
	// (an integer and a constant), so no traversal is possible here.
	p := filepath.Join(sysfsRBDRoot, strconv.Itoa(id), attr)
	b, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// readSysfsDevice reads the identity attributes of /dev/rbd<id>.
//
// cluster_fsid, pool and name are mandatory: they are the identity. A kernel
// that does not expose them cannot be verified, and the caller must fail closed
// rather than assume a match.
func readSysfsDevice(id int) (sysfsDevice, error) {
	dev := sysfsDevice{ID: id}

	for _, req := range []struct {
		attr string
		dst  *string
	}{
		{sysfsAttrClusterFSID, &dev.ClusterFSID},
		{sysfsAttrPool, &dev.Pool},
		{sysfsAttrName, &dev.Image},
	} {
		v, err := readSysfsAttr(id, req.attr)
		if err != nil {
			return sysfsDevice{}, fmt.Errorf("rbd: read %s for /dev/rbd%d: %w", req.attr, id, err)
		}
		if v == "" {
			return sysfsDevice{}, fmt.Errorf("rbd: %s for /dev/rbd%d is empty", req.attr, id)
		}
		*req.dst = v
	}

	// Optional attributes: useful for diagnostics, never required for identity.
	dev.Namespace, _ = readSysfsAttr(id, sysfsAttrPoolNS)
	dev.ImageID, _ = readSysfsAttr(id, sysfsAttrImageID)
	dev.Features, _ = readSysfsAttr(id, sysfsAttrFeatures)
	if snap, err := readSysfsAttr(id, sysfsAttrCurrentSnap); err == nil && snap != "-" {
		dev.Snap = snap
	}
	if raw, err := readSysfsAttr(id, sysfsAttrSize); err == nil {
		if n, convErr := strconv.ParseInt(raw, 10, 64); convErr == nil {
			dev.SizeBytes = n
		}
	}

	return dev, nil
}

// listSysfsDevices enumerates every kernel RBD mapping on the host, including
// mappings owned by other clients such as the platform Ceph-CSI.
//
// A directory whose attributes cannot be read is skipped rather than failing
// the whole listing: a device may be unmapped concurrently, and refusing to
// enumerate would block reconciliation for every other volume.
func listSysfsDevices() ([]sysfsDevice, error) {
	entries, err := os.ReadDir(sysfsRBDRoot)
	if err != nil {
		if os.IsNotExist(err) {
			// No kernel RBD support, or nothing has ever been mapped.
			return nil, nil
		}
		return nil, fmt.Errorf("rbd: read %s: %w", sysfsRBDRoot, err)
	}

	out := make([]sysfsDevice, 0, len(entries))
	for _, e := range entries {
		id, convErr := strconv.Atoi(e.Name())
		if convErr != nil {
			continue // not a device directory
		}
		dev, readErr := readSysfsDevice(id)
		if readErr != nil {
			continue // raced with an unmap, or an incomplete device
		}
		out = append(out, dev)
	}
	return out, nil
}
