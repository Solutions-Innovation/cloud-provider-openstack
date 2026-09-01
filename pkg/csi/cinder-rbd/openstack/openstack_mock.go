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

	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/stretchr/testify/mock"
)

// OpenStackRBDMock is a testify mock for IOpenStackRBD.
type OpenStackRBDMock struct {
	mock.Mock
}

var _ IOpenStackRBD = &OpenStackRBDMock{}

// CreateVolume mocks Cinder volume creation.
func (m *OpenStackRBDMock) CreateVolume(ctx context.Context, opts *volumes.CreateOpts,
	schedulerHints volumes.SchedulerHintOptsBuilder) (*volumes.Volume, error) {
	ret := m.Called(ctx, opts, schedulerHints)
	if rf, ok := ret.Get(0).(func() *volumes.Volume); ok {
		return rf(), ret.Error(1)
	}
	if v, ok := ret.Get(0).(*volumes.Volume); ok {
		return v, ret.Error(1)
	}
	return nil, ret.Error(1)
}

// DeleteVolume mocks Cinder volume deletion.
func (m *OpenStackRBDMock) DeleteVolume(ctx context.Context, volumeID string) error {
	return m.Called(ctx, volumeID).Error(0)
}

// GetVolume mocks reading a Cinder volume.
func (m *OpenStackRBDMock) GetVolume(ctx context.Context, volumeID string) (*volumes.Volume, error) {
	ret := m.Called(ctx, volumeID)
	if rf, ok := ret.Get(0).(func() *volumes.Volume); ok {
		return rf(), ret.Error(1)
	}
	if v, ok := ret.Get(0).(*volumes.Volume); ok {
		return v, ret.Error(1)
	}
	return nil, ret.Error(1)
}

// GetVolumesByName mocks the idempotency lookup.
func (m *OpenStackRBDMock) GetVolumesByName(ctx context.Context, name string) ([]volumes.Volume, error) {
	ret := m.Called(ctx, name)
	if v, ok := ret.Get(0).([]volumes.Volume); ok {
		return v, ret.Error(1)
	}
	return nil, ret.Error(1)
}

// WaitVolumeTargetStatus mocks the status waiter.
func (m *OpenStackRBDMock) WaitVolumeTargetStatus(ctx context.Context, volumeID string,
	tStatus []string, timeoutSeconds int) error {
	return m.Called(ctx, volumeID, tStatus, timeoutSeconds).Error(0)
}

// SetVolumeMetadata mocks the metadata merge.
func (m *OpenStackRBDMock) SetVolumeMetadata(ctx context.Context, volumeID string, metadata map[string]string) error {
	return m.Called(ctx, volumeID, metadata).Error(0)
}

// DeleteVolumeMetadata mocks metadata key removal.
func (m *OpenStackRBDMock) DeleteVolumeMetadata(ctx context.Context, volumeID string, keys []string) error {
	return m.Called(ctx, volumeID, keys).Error(0)
}

// CreateAttachment mocks reserved attachment record creation.
func (m *OpenStackRBDMock) CreateAttachment(ctx context.Context, volumeID string) (string, error) {
	ret := m.Called(ctx, volumeID)
	return ret.String(0), ret.Error(1)
}

// UpdateAttachmentConnector mocks the connector update.
func (m *OpenStackRBDMock) UpdateAttachmentConnector(ctx context.Context, attachmentID string,
	connector *AttachmentConnector) (*RBDConnectionInfo, error) {
	ret := m.Called(ctx, attachmentID, connector)
	if rf, ok := ret.Get(0).(func() *RBDConnectionInfo); ok {
		return rf(), ret.Error(1)
	}
	if v, ok := ret.Get(0).(*RBDConnectionInfo); ok {
		return v, ret.Error(1)
	}
	return nil, ret.Error(1)
}

// CompleteAttachment mocks os-complete.
func (m *OpenStackRBDMock) CompleteAttachment(ctx context.Context, attachmentID string) error {
	return m.Called(ctx, attachmentID).Error(0)
}

// GetAttachment mocks reading one attachment record.
func (m *OpenStackRBDMock) GetAttachment(ctx context.Context, attachmentID string) (*Attachment, error) {
	ret := m.Called(ctx, attachmentID)
	if v, ok := ret.Get(0).(*Attachment); ok {
		return v, ret.Error(1)
	}
	return nil, ret.Error(1)
}

// ListAttachmentsByVolume mocks listing a volume's attachment records.
func (m *OpenStackRBDMock) ListAttachmentsByVolume(ctx context.Context, volumeID string) ([]Attachment, error) {
	ret := m.Called(ctx, volumeID)
	if v, ok := ret.Get(0).([]Attachment); ok {
		return v, ret.Error(1)
	}
	return nil, ret.Error(1)
}

// DeleteAttachment mocks attachment record deletion.
func (m *OpenStackRBDMock) DeleteAttachment(ctx context.Context, attachmentID string) error {
	return m.Called(ctx, attachmentID).Error(0)
}

// DiscoverCinderCapabilities mocks the microversion probe.
func (m *OpenStackRBDMock) DiscoverCinderCapabilities(ctx context.Context) (*CinderCapabilities, error) {
	ret := m.Called(ctx)
	if v, ok := ret.Get(0).(*CinderCapabilities); ok {
		return v, ret.Error(1)
	}
	return nil, ret.Error(1)
}

// GetRBDOpts mocks the [RBD] accessor.
func (m *OpenStackRBDMock) GetRBDOpts() RBDOpts {
	return m.Called().Get(0).(RBDOpts)
}

// GetVolumeOpts mocks the [Volume] accessor.
func (m *OpenStackRBDMock) GetVolumeOpts() VolumeOpts {
	return m.Called().Get(0).(VolumeOpts)
}

// GetCinderCapabilities mocks the cached capability accessor.
func (m *OpenStackRBDMock) GetCinderCapabilities() *CinderCapabilities {
	ret := m.Called()
	if v, ok := ret.Get(0).(*CinderCapabilities); ok {
		return v
	}
	return nil
}
