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

	"github.com/stretchr/testify/mock"
)

// ISCSIInitiatorMock is a testify/mock implementation of ISCSIInitiator.
type ISCSIInitiatorMock struct {
	mock.Mock
}

var _ ISCSIInitiator = &ISCSIInitiatorMock{}

func (m *ISCSIInitiatorMock) Discovery(ctx context.Context, portal string) error {
	args := m.Called(ctx, portal)
	return args.Error(0)
}

func (m *ISCSIInitiatorMock) SetCHAPAuth(ctx context.Context, iqn, portal, username, password string) error {
	args := m.Called(ctx, iqn, portal, username, password)
	return args.Error(0)
}

func (m *ISCSIInitiatorMock) Login(ctx context.Context, iqn, portal string) error {
	args := m.Called(ctx, iqn, portal)
	return args.Error(0)
}

func (m *ISCSIInitiatorMock) Logout(ctx context.Context, iqn, portal string) error {
	args := m.Called(ctx, iqn, portal)
	return args.Error(0)
}

func (m *ISCSIInitiatorMock) DeleteNode(ctx context.Context, iqn, portal string) error {
	args := m.Called(ctx, iqn, portal)
	return args.Error(0)
}

func (m *ISCSIInitiatorMock) IsSessionActive(ctx context.Context, iqn, portal string) (bool, error) {
	args := m.Called(ctx, iqn, portal)
	return args.Bool(0), args.Error(1)
}

func (m *ISCSIInitiatorMock) CheckIscsiadm(ctx context.Context) error {
	args := m.Called(ctx)
	return args.Error(0)
}
