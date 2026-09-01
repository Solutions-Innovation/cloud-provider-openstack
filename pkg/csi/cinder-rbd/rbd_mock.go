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
	"time"

	"github.com/stretchr/testify/mock"
)

// RBDMapperMock is a testify mock for RBDMapper. It lets the node server be
// tested without a kernel, a Ceph cluster, or the rbd CLI.
type RBDMapperMock struct {
	mock.Mock
}

var _ RBDMapper = &RBDMapperMock{}

// Map mocks an exclusive kernel map.
func (m *RBDMapperMock) Map(ctx context.Context, req MapRequest) (MappedDevice, error) {
	ret := m.Called(ctx, req)
	return ret.Get(0).(MappedDevice), ret.Error(1)
}

// Unmap mocks releasing a device.
func (m *RBDMapperMock) Unmap(ctx context.Context, devicePath string, timeout time.Duration) error {
	return m.Called(ctx, devicePath, timeout).Error(0)
}

// ListMapped mocks the kernel mapping inventory.
func (m *RBDMapperMock) ListMapped(ctx context.Context) ([]MappedDevice, error) {
	ret := m.Called(ctx)
	if rf, ok := ret.Get(0).(func() []MappedDevice); ok {
		return rf(), ret.Error(1)
	}
	if v, ok := ret.Get(0).([]MappedDevice); ok {
		return v, ret.Error(1)
	}
	return nil, ret.Error(1)
}

// VerifyIdentity mocks the identity check.
func (m *RBDMapperMock) VerifyIdentity(ctx context.Context, devicePath string, want ImageIdentity) error {
	return m.Called(ctx, devicePath, want).Error(0)
}

// DeviceSize mocks reading the device size.
func (m *RBDMapperMock) DeviceSize(ctx context.Context, devicePath string) (int64, error) {
	ret := m.Called(ctx, devicePath)
	return ret.Get(0).(int64), ret.Error(1)
}

// Flush mocks flushing buffers.
func (m *RBDMapperMock) Flush(ctx context.Context, devicePath string) error {
	return m.Called(ctx, devicePath).Error(0)
}

// LockHolders mocks reading exclusive-lock holders.
func (m *RBDMapperMock) LockHolders(ctx context.Context, req MapRequest) ([]string, error) {
	ret := m.Called(ctx, req)
	if v, ok := ret.Get(0).([]string); ok {
		return v, ret.Error(1)
	}
	return nil, ret.Error(1)
}

// CheckClient mocks the rbd CLI version check.
func (m *RBDMapperMock) CheckClient(ctx context.Context) (string, error) {
	ret := m.Called(ctx)
	return ret.String(0), ret.Error(1)
}

// CephCredentialProviderMock is a testify mock for CephCredentialProvider.
type CephCredentialProviderMock struct {
	mock.Mock
}

var _ CephCredentialProvider = &CephCredentialProviderMock{}

// Load mocks reading the Ceph credential.
func (m *CephCredentialProviderMock) Load(ctx context.Context, wantUserID string) (*CephCredential, error) {
	ret := m.Called(ctx, wantUserID)
	if v, ok := ret.Get(0).(*CephCredential); ok {
		return v, ret.Error(1)
	}
	return nil, ret.Error(1)
}

// Available mocks the credential availability check.
func (m *CephCredentialProviderMock) Available(ctx context.Context) error {
	return m.Called(ctx).Error(0)
}

// NewTestCredential builds a CephCredential with a key, for tests only.
// The key stays unexported in production code, so tests in this package use
// this helper rather than reaching into the struct.
func NewTestCredential(userID, key string) *CephCredential {
	return &CephCredential{UserID: userID, key: key}
}
