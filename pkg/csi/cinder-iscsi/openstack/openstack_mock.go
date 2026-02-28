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

package openstack

import (
	"context"

	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/snapshots"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/stretchr/testify/mock"
)

// OpenStackISCSIMock is a mock implementation of IOpenStackISCSI for unit tests.
// Generated from the IOpenStackISCSI interface using testify/mock patterns.
type OpenStackISCSIMock struct {
	mock.Mock
}

// Compile-time check that OpenStackISCSIMock implements IOpenStackISCSI
var _ IOpenStackISCSI = &OpenStackISCSIMock{}

// ── Volume Operations ────────────────────────────────────────────────────────

func (m *OpenStackISCSIMock) CreateVolume(ctx context.Context, opts *volumes.CreateOpts, schedulerHints volumes.SchedulerHintOptsBuilder) (*volumes.Volume, error) {
	ret := m.Called(ctx, opts, schedulerHints)
	var r0 *volumes.Volume
	if rf, ok := ret.Get(0).(func() *volumes.Volume); ok {
		r0 = rf()
	} else if ret.Get(0) != nil {
		r0 = ret.Get(0).(*volumes.Volume)
	}
	return r0, ret.Error(1)
}

func (m *OpenStackISCSIMock) DeleteVolume(ctx context.Context, volumeID string) error {
	ret := m.Called(ctx, volumeID)
	return ret.Error(0)
}

func (m *OpenStackISCSIMock) GetVolume(ctx context.Context, volumeID string) (*volumes.Volume, error) {
	ret := m.Called(ctx, volumeID)
	var r0 *volumes.Volume
	if rf, ok := ret.Get(0).(func() *volumes.Volume); ok {
		r0 = rf()
	} else if ret.Get(0) != nil {
		r0 = ret.Get(0).(*volumes.Volume)
	}
	return r0, ret.Error(1)
}

func (m *OpenStackISCSIMock) GetVolumesByName(ctx context.Context, name string) ([]volumes.Volume, error) {
	ret := m.Called(ctx, name)
	var r0 []volumes.Volume
	if rf, ok := ret.Get(0).(func() []volumes.Volume); ok {
		r0 = rf()
	} else if ret.Get(0) != nil {
		r0 = ret.Get(0).([]volumes.Volume)
	}
	return r0, ret.Error(1)
}

func (m *OpenStackISCSIMock) ExpandVolume(ctx context.Context, volumeID string, status string, newSize int) error {
	ret := m.Called(ctx, volumeID, status, newSize)
	return ret.Error(0)
}

func (m *OpenStackISCSIMock) WaitVolumeTargetStatus(ctx context.Context, volumeID string, tStatus []string, timeoutSeconds int) error {
	ret := m.Called(ctx, volumeID, tStatus, timeoutSeconds)
	return ret.Error(0)
}

func (m *OpenStackISCSIMock) SetVolumeMetadata(ctx context.Context, volumeID string, metadata map[string]string) error {
	ret := m.Called(ctx, volumeID, metadata)
	return ret.Error(0)
}

func (m *OpenStackISCSIMock) DeleteVolumeMetadata(ctx context.Context, volumeID string, keys []string) error {
	ret := m.Called(ctx, volumeID, keys)
	return ret.Error(0)
}

// ── Cinder v3 Attachment Operations ──────────────────────────────────────────

func (m *OpenStackISCSIMock) CreateAttachment(ctx context.Context, volumeID string) (string, error) {
	ret := m.Called(ctx, volumeID)
	return ret.String(0), ret.Error(1)
}

func (m *OpenStackISCSIMock) UpdateAttachmentConnector(ctx context.Context, attachmentID string, connector *AttachmentConnector) (*ISCSIConnectionInfo, error) {
	ret := m.Called(ctx, attachmentID, connector)
	var r0 *ISCSIConnectionInfo
	if rf, ok := ret.Get(0).(func() *ISCSIConnectionInfo); ok {
		r0 = rf()
	} else if ret.Get(0) != nil {
		r0 = ret.Get(0).(*ISCSIConnectionInfo)
	}
	return r0, ret.Error(1)
}

func (m *OpenStackISCSIMock) CompleteAttachment(ctx context.Context, attachmentID string) error {
	ret := m.Called(ctx, attachmentID)
	return ret.Error(0)
}

func (m *OpenStackISCSIMock) GetAttachment(ctx context.Context, attachmentID string) (*Attachment, error) {
	ret := m.Called(ctx, attachmentID)
	var r0 *Attachment
	if rf, ok := ret.Get(0).(func() *Attachment); ok {
		r0 = rf()
	} else if ret.Get(0) != nil {
		r0 = ret.Get(0).(*Attachment)
	}
	return r0, ret.Error(1)
}

func (m *OpenStackISCSIMock) DeleteAttachment(ctx context.Context, attachmentID string) error {
	ret := m.Called(ctx, attachmentID)
	return ret.Error(0)
}

// ── Snapshot Operations ──────────────────────────────────────────────────────

func (m *OpenStackISCSIMock) CreateSnapshot(ctx context.Context, name, volID string, tags map[string]string) (*snapshots.Snapshot, error) {
	ret := m.Called(ctx, name, volID, tags)
	var r0 *snapshots.Snapshot
	if rf, ok := ret.Get(0).(func() *snapshots.Snapshot); ok {
		r0 = rf()
	} else if ret.Get(0) != nil {
		r0 = ret.Get(0).(*snapshots.Snapshot)
	}
	return r0, ret.Error(1)
}

func (m *OpenStackISCSIMock) DeleteSnapshot(ctx context.Context, snapID string) error {
	ret := m.Called(ctx, snapID)
	return ret.Error(0)
}

func (m *OpenStackISCSIMock) GetSnapshotByID(ctx context.Context, snapshotID string) (*snapshots.Snapshot, error) {
	ret := m.Called(ctx, snapshotID)
	var r0 *snapshots.Snapshot
	if rf, ok := ret.Get(0).(func() *snapshots.Snapshot); ok {
		r0 = rf()
	} else if ret.Get(0) != nil {
		r0 = ret.Get(0).(*snapshots.Snapshot)
	}
	return r0, ret.Error(1)
}

// ── Discovery & Configuration ────────────────────────────────────────────────

func (m *OpenStackISCSIMock) DiscoverCinderCapabilities(ctx context.Context) (*CinderCapabilities, error) {
	ret := m.Called(ctx)
	var r0 *CinderCapabilities
	if rf, ok := ret.Get(0).(func() *CinderCapabilities); ok {
		r0 = rf()
	} else if ret.Get(0) != nil {
		r0 = ret.Get(0).(*CinderCapabilities)
	}
	return r0, ret.Error(1)
}

func (m *OpenStackISCSIMock) GetISCSIOpts() ISCSIOpts {
	ret := m.Called()
	return ret.Get(0).(ISCSIOpts)
}

func (m *OpenStackISCSIMock) GetVolumeOpts() VolumeOpts {
	ret := m.Called()
	return ret.Get(0).(VolumeOpts)
}

func (m *OpenStackISCSIMock) GetCinderCapabilities() *CinderCapabilities {
	ret := m.Called()
	var r0 *CinderCapabilities
	if rf, ok := ret.Get(0).(func() *CinderCapabilities); ok {
		r0 = rf()
	} else if ret.Get(0) != nil {
		r0 = ret.Get(0).(*CinderCapabilities)
	}
	return r0
}
