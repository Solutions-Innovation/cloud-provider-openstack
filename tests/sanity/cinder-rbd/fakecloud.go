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
	"sync"

	"github.com/google/uuid"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-rbd/openstack"
	cpoerrors "k8s.io/cloud-provider-openstack/pkg/util/errors"
)

// fakeCloud is an in-memory Cinder implementing IOpenStackRBD.
//
// It models the behaviour measured on the validated WRCP 24.09 backend rather
// than an idealised Cinder: a reserved attachment record has a nil instance, the
// connector update moves the volume to "attaching" (not straight to in-use), and
// completion is what makes it "in-use".
type fakeCloud struct {
	mu sync.Mutex

	volumes     map[string]*volumes.Volume
	attachments map[string]*openstack.Attachment

	rbdOpts    openstack.RBDOpts
	volumeOpts openstack.VolumeOpts
	caps       *openstack.CinderCapabilities
}

var _ openstack.IOpenStackRBD = &fakeCloud{}

const (
	fakePool        = "cinder-volumes"
	fakeClusterFSID = "c5f7876d-258c-4152-b26a-a3ab532fda28"
	fakeClusterName = "ceph"
	fakeAuthUser    = "cinder"
)

func newFakeCloud(rbdOpts openstack.RBDOpts, volumeOpts openstack.VolumeOpts) *fakeCloud {
	return &fakeCloud{
		volumes:     make(map[string]*volumes.Volume),
		attachments: make(map[string]*openstack.Attachment),
		rbdOpts:     rbdOpts,
		volumeOpts:  volumeOpts,
		caps:        &openstack.CinderCapabilities{SupportsV327: true, SupportsV344: true},
	}
}

// ── Volume operations ────────────────────────────────────────────────────────

func (f *fakeCloud) CreateVolume(_ context.Context, opts *volumes.CreateOpts,
	_ volumes.SchedulerHintOptsBuilder) (*volumes.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	vol := &volumes.Volume{
		ID:               uuid.NewString(),
		Name:             opts.Name,
		Size:             opts.Size,
		VolumeType:       opts.VolumeType,
		AvailabilityZone: opts.AvailabilityZone,
		Status:           openstack.VolumeAvailableStatus,
		Metadata:         map[string]string{},
	}
	f.volumes[vol.ID] = vol
	return vol, nil
}

func (f *fakeCloud) DeleteVolume(_ context.Context, volumeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.volumes[volumeID]; !ok {
		// Idempotent, matching the real driver's tolerance of HTTP 404.
		return nil
	}
	delete(f.volumes, volumeID)
	return nil
}

func (f *fakeCloud) GetVolume(_ context.Context, volumeID string) (*volumes.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	vol, ok := f.volumes[volumeID]
	if !ok {
		// Must satisfy cpoerrors.IsNotFound, which the driver relies on to map
		// this to codes.NotFound.
		return nil, cpoerrors.ErrNotFound
	}
	copied := *vol
	copied.Metadata = copyMap(vol.Metadata)
	return &copied, nil
}

func (f *fakeCloud) GetVolumesByName(_ context.Context, name string) ([]volumes.Volume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []volumes.Volume
	for _, v := range f.volumes {
		if v.Name == name {
			copied := *v
			copied.Metadata = copyMap(v.Metadata)
			out = append(out, copied)
		}
	}
	return out, nil
}

func (f *fakeCloud) WaitVolumeTargetStatus(_ context.Context, volumeID string,
	tStatus []string, _ int) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	vol, ok := f.volumes[volumeID]
	if !ok {
		return cpoerrors.ErrNotFound
	}
	for _, s := range tStatus {
		if vol.Status == s {
			return nil
		}
	}
	return fmt.Errorf("fake: volume %s is %q, want one of %v", volumeID, vol.Status, tStatus)
}

func (f *fakeCloud) SetVolumeMetadata(_ context.Context, volumeID string, metadata map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	vol, ok := f.volumes[volumeID]
	if !ok {
		return cpoerrors.ErrNotFound
	}
	if vol.Metadata == nil {
		vol.Metadata = map[string]string{}
	}
	for k, v := range metadata {
		vol.Metadata[k] = v
	}
	return nil
}

func (f *fakeCloud) DeleteVolumeMetadata(_ context.Context, volumeID string, keys []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	vol, ok := f.volumes[volumeID]
	if !ok {
		return nil
	}
	for _, k := range keys {
		delete(vol.Metadata, k)
	}
	return nil
}

// ── Attachment records ───────────────────────────────────────────────────────

func (f *fakeCloud) CreateAttachment(_ context.Context, volumeID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	vol, ok := f.volumes[volumeID]
	if !ok {
		return "", cpoerrors.ErrNotFound
	}

	att := &openstack.Attachment{
		ID:       uuid.NewString(),
		VolumeID: volumeID,
		Status:   "reserved",
		// Nil instance: a self-service reservation has no Nova server.
		Instance: nil,
	}
	f.attachments[att.ID] = att
	vol.Status = openstack.VolumeReservedStatus
	return att.ID, nil
}

func (f *fakeCloud) UpdateAttachmentConnector(_ context.Context, attachmentID string,
	connector *openstack.AttachmentConnector) (*openstack.RBDConnectionInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	att, ok := f.attachments[attachmentID]
	if !ok {
		// Drives the driver's single replace-and-retry path.
		return nil, cpoerrors.ErrNotFound
	}
	if connector == nil || connector.Host == "" {
		// The real backend rejects an empty connector.
		return nil, fmt.Errorf("fake: connector must carry a host")
	}

	vol, ok := f.volumes[att.VolumeID]
	if !ok {
		return nil, cpoerrors.ErrNotFound
	}

	// Measured behaviour: the volume moves to "attaching", not "in-use".
	att.Status = openstack.VolumeAttachingStatus
	vol.Status = openstack.VolumeAttachingStatus

	ci := &openstack.RBDConnectionInfo{
		DriverVolumeType: openstack.DriverVolumeTypeRBD,
		ClusterName:      fakeClusterName,
		ClusterFSID:      fakeClusterFSID,
		Pool:             fakePool,
		Image:            att.VolumeID,
		AuthEnabled:      true,
		AuthUsername:     fakeAuthUser,
		Monitors: []openstack.MonAddr{
			{Host: "10.107.190.121", Port: "6789"},
			{Host: "10.106.210.60", Port: "6789"},
		},
		VolumeID:     att.VolumeID,
		AttachmentID: att.ID,
		AccessMode:   "rw",
	}
	att.ConnectionInfo = ci
	return ci, nil
}

func (f *fakeCloud) CompleteAttachment(_ context.Context, attachmentID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	att, ok := f.attachments[attachmentID]
	if !ok {
		return cpoerrors.ErrNotFound
	}
	att.Status = "attached"
	if vol, ok := f.volumes[att.VolumeID]; ok {
		vol.Status = openstack.VolumeInUseStatus
	}
	return nil
}

func (f *fakeCloud) GetAttachment(_ context.Context, attachmentID string) (*openstack.Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	att, ok := f.attachments[attachmentID]
	if !ok {
		return nil, cpoerrors.ErrNotFound
	}
	copied := *att
	return &copied, nil
}

func (f *fakeCloud) ListAttachmentsByVolume(_ context.Context, volumeID string) ([]openstack.Attachment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []openstack.Attachment
	for _, a := range f.attachments {
		if a.VolumeID == volumeID {
			out = append(out, *a)
		}
	}
	return out, nil
}

func (f *fakeCloud) DeleteAttachment(_ context.Context, attachmentID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	att, ok := f.attachments[attachmentID]
	if !ok {
		return nil // idempotent
	}
	delete(f.attachments, attachmentID)

	// Deleting the record returns the volume to available, as measured.
	if vol, ok := f.volumes[att.VolumeID]; ok {
		vol.Status = openstack.VolumeAvailableStatus
	}
	return nil
}

// ── Discovery and configuration ──────────────────────────────────────────────

func (f *fakeCloud) DiscoverCinderCapabilities(_ context.Context) (*openstack.CinderCapabilities, error) {
	return f.caps, nil
}

func (f *fakeCloud) GetRBDOpts() openstack.RBDOpts                        { return f.rbdOpts }
func (f *fakeCloud) GetVolumeOpts() openstack.VolumeOpts                  { return f.volumeOpts }
func (f *fakeCloud) GetCinderCapabilities() *openstack.CinderCapabilities { return f.caps }

func copyMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
