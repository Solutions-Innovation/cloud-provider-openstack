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
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	rbd "k8s.io/cloud-provider-openstack/pkg/csi/cinder-rbd"
)

// fakeMapper is an in-memory kernel RBD implementation.
//
// It enforces the two properties the real kernel enforces and that the driver
// depends on: an image may be mapped at most once (the exclusive lock), and a
// device path is backed by a real file so the driver's existence and size checks
// behave as they would on a node.
type fakeMapper struct {
	mu sync.Mutex

	// byImage maps "pool/image" to its device.
	byImage map[string]rbd.MappedDevice
	// deviceDir backs each device with a real file, since the node server stats
	// the path and bind-mounts it.
	deviceDir string
	nextID    int

	// sizeBytes is reported for every device.
	sizeBytes int64
}

var _ rbd.RBDMapper = &fakeMapper{}

func newFakeMapper(t interface{ TempDir() string }) *fakeMapper {
	return &fakeMapper{
		byImage:   make(map[string]rbd.MappedDevice),
		deviceDir: t.TempDir(),
		sizeBytes: 1 << 30,
	}
}

func imageKey(pool, image string) string { return pool + "/" + image }

// Map maps an image, refusing a second concurrent mapping.
func (m *fakeMapper) Map(_ context.Context, req rbd.MapRequest) (rbd.MappedDevice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !req.Identity.IsComplete() {
		return rbd.MappedDevice{}, fmt.Errorf("fake: incomplete identity %s", req.Identity)
	}
	if !req.Exclusive {
		return rbd.MappedDevice{}, fmt.Errorf("fake: %w: writable maps must be exclusive",
			rbd.ErrExclusiveLockDenied)
	}

	key := imageKey(req.Identity.Pool, req.Identity.Image)
	if existing, ok := m.byImage[key]; ok {
		// The Ceph exclusive lock: one writable mapping per image.
		return rbd.MappedDevice{}, fmt.Errorf("fake: %s already mapped as %s: %w",
			key, existing.DevicePath, rbd.ErrExclusiveLockDenied)
	}

	// A real file so os.Stat and the bind mount behave as on a node.
	path := filepath.Join(m.deviceDir, fmt.Sprintf("rbd%d", m.nextID))
	if err := os.WriteFile(path, make([]byte, 4096), 0o600); err != nil {
		return rbd.MappedDevice{}, fmt.Errorf("fake: create device file: %w", err)
	}

	dev := rbd.MappedDevice{
		ID:          m.nextID,
		Pool:        req.Identity.Pool,
		Image:       req.Identity.Image,
		DevicePath:  path,
		ClusterFSID: req.Identity.ClusterFSID,
	}
	m.nextID++
	m.byImage[key] = dev
	return dev, nil
}

func (m *fakeMapper) Unmap(_ context.Context, devicePath string, _ time.Duration) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	for key, dev := range m.byImage {
		if dev.DevicePath == devicePath {
			delete(m.byImage, key)
			_ = os.Remove(devicePath)
			return nil
		}
	}
	// Idempotent, as the real implementation is for an already-unmapped device.
	return nil
}

func (m *fakeMapper) ListMapped(_ context.Context) ([]rbd.MappedDevice, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]rbd.MappedDevice, 0, len(m.byImage))
	for _, d := range m.byImage {
		out = append(out, d)
	}
	return out, nil
}

func (m *fakeMapper) VerifyIdentity(_ context.Context, devicePath string, want rbd.ImageIdentity) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !want.IsComplete() {
		return fmt.Errorf("fake: %w: incomplete expectation", rbd.ErrIdentityMismatch)
	}
	dev, ok := m.byImage[imageKey(want.Pool, want.Image)]
	if !ok {
		return fmt.Errorf("fake: %w: %s is not mapped", rbd.ErrIdentityMismatch, want)
	}
	if dev.DevicePath != devicePath {
		return fmt.Errorf("fake: %w: %s maps a different device (%s)",
			rbd.ErrIdentityMismatch, devicePath, dev.DevicePath)
	}
	if dev.ClusterFSID != want.ClusterFSID {
		return fmt.Errorf("fake: %w: cluster FSID differs", rbd.ErrIdentityMismatch)
	}
	return nil
}

func (m *fakeMapper) DeviceSize(_ context.Context, devicePath string) (int64, error) {
	if _, err := os.Stat(devicePath); err != nil {
		return 0, fmt.Errorf("fake: %s: %w", devicePath, err)
	}
	return m.sizeBytes, nil
}

func (m *fakeMapper) Flush(_ context.Context, devicePath string) error {
	if _, err := os.Stat(devicePath); err != nil {
		return fmt.Errorf("fake: flush %s: %w", devicePath, err)
	}
	return nil
}

func (m *fakeMapper) LockHolders(_ context.Context, req rbd.MapRequest) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, ok := m.byImage[imageKey(req.Identity.Pool, req.Identity.Image)]; ok {
		return []string{"client.fake@127.0.0.1"}, nil
	}
	return nil, nil
}

func (m *fakeMapper) CheckClient(_ context.Context) (string, error) { return "18.2.8", nil }

// fakeCredentials returns a credential matching whatever entity is requested,
// so the sanity suite exercises the success path rather than the entity-mismatch
// guard (which has dedicated unit tests).
type fakeCredentials struct{ userID string }

var _ rbd.CephCredentialProvider = &fakeCredentials{}

func (f *fakeCredentials) Load(_ context.Context, wantUserID string) (*rbd.CephCredential, error) {
	id := f.userID
	if wantUserID != "" {
		id = wantUserID
	}
	return rbd.NewTestCredential(id, "AQFAKEKEYFORSANITYTESTS=="), nil
}

func (f *fakeCredentials) Available(_ context.Context) error { return nil }
