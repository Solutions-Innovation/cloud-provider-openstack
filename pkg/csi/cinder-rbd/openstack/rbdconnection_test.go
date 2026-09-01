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
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// labConnectionInfoJSON is the verbatim connection_info returned by the
// validated WRCP 24.09 backend for a ceph-rook-store volume. Keeping the real
// payload as the baseline fixture means a backend change shows up as a test
// failure rather than as a production surprise.
const labConnectionInfoJSON = `{
  "driver_volume_type": "rbd",
  "cluster_name": "ceph",
  "name": "cinder-volumes/3018df26-0ba3-45a3-adfd-4a84ed59fff1",
  "auth_enabled": true,
  "auth_username": "cinder",
  "secret_type": "ceph",
  "secret_uuid": "c5f7876d-258c-4152-b26a-a3ab532fda28",
  "volume_id": "3018df26-0ba3-45a3-adfd-4a84ed59fff1",
  "attachment_id": "8d6cd8d7-1e6f-4bfd-8753-42332b4bc42d",
  "hosts": ["10.107.190.121", "10.106.210.60", "10.98.180.79"],
  "ports": ["6789", "6789", "6789"],
  "access_mode": "rw",
  "discard": true,
  "encrypted": false,
  "cacheable": false,
  "qos_specs": null
}`

func rawFromJSON(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	require.NoError(t, json.Unmarshal([]byte(s), &m))
	return m
}

// flatPayload returns a minimal valid flat payload that individual cases mutate.
func flatPayload() map[string]any {
	return map[string]any{
		"driver_volume_type": "rbd",
		"cluster_name":       "ceph",
		"name":               "cinder-volumes/img-1",
		"auth_enabled":       true,
		"auth_username":      "cinder",
		"secret_uuid":        "fsid-1",
		"volume_id":          "vol-1",
		"hosts":              []any{"10.0.0.1", "10.0.0.2"},
		"ports":              []any{"6789", "6789"},
	}
}

func TestParseRBDConnectionInfo_LabPayload(t *testing.T) {
	ci, err := parseRBDConnectionInfo(rawFromJSON(t, labConnectionInfoJSON))
	require.NoError(t, err)

	assert.Equal(t, DriverVolumeTypeRBD, ci.DriverVolumeType)
	assert.Equal(t, "ceph", ci.ClusterName)
	assert.Equal(t, "c5f7876d-258c-4152-b26a-a3ab532fda28", ci.ClusterFSID)
	// The pool and image come from `name` verbatim: no "volume-" prefix is
	// invented and the pool is not assumed.
	assert.Equal(t, "cinder-volumes", ci.Pool)
	assert.Equal(t, "3018df26-0ba3-45a3-adfd-4a84ed59fff1", ci.Image)
	assert.True(t, ci.AuthEnabled)
	assert.Equal(t, "cinder", ci.AuthUsername)
	assert.Equal(t, "3018df26-0ba3-45a3-adfd-4a84ed59fff1", ci.VolumeID)
	assert.Equal(t, "8d6cd8d7-1e6f-4bfd-8753-42332b4bc42d", ci.AttachmentID)
	assert.Equal(t, "rw", ci.AccessMode)
	assert.True(t, ci.Discard)

	require.Len(t, ci.Monitors, 3)
	assert.Equal(t, "10.107.190.121:6789", ci.Monitors[0].String())
	assert.Equal(t, "10.107.190.121:6789,10.106.210.60:6789,10.98.180.79:6789", ci.MonitorList())
	assert.Equal(t, "cinder-volumes/3018df26-0ba3-45a3-adfd-4a84ed59fff1", ci.ImageSpec())
}

func TestParseRBDConnectionInfo_NameSplitting(t *testing.T) {
	tests := []struct {
		name      string
		nameField string
		wantPool  string
		wantImage string
		wantErr   bool
	}{
		{name: "simple pool and image", nameField: "cinder-volumes/img-1", wantPool: "cinder-volumes", wantImage: "img-1"},
		{name: "splits on the first slash only", nameField: "pool/dir/img-1", wantPool: "pool", wantImage: "dir/img-1"},
		{name: "image keeps a volume- prefix if present", nameField: "p/volume-abc", wantPool: "p", wantImage: "volume-abc"},
		{name: "no slash is rejected", nameField: "just-an-image", wantErr: true},
		{name: "empty pool is rejected", nameField: "/img-1", wantErr: true},
		{name: "empty image is rejected", nameField: "pool/", wantErr: true},
		{name: "empty name is rejected", nameField: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := flatPayload()
			raw["name"] = tt.nameField

			ci, err := parseRBDConnectionInfo(raw)
			if tt.wantErr {
				require.ErrorIs(t, err, ErrConnectionInfoIncomplete)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantPool, ci.Pool)
			assert.Equal(t, tt.wantImage, ci.Image)
		})
	}
}

func TestParseRBDConnectionInfo_DriverVolumeType(t *testing.T) {
	tests := []struct {
		name string
		dvt  any
	}{
		{name: "iscsi is rejected", dvt: "iscsi"},
		{name: "nfs is rejected", dvt: "nfs"},
		{name: "empty is rejected", dvt: ""},
		{name: "missing is rejected", dvt: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := flatPayload()
			if tt.dvt == nil {
				delete(raw, "driver_volume_type")
			} else {
				raw["driver_volume_type"] = tt.dvt
			}

			_, err := parseRBDConnectionInfo(raw)
			require.ErrorIs(t, err, ErrWrongDriverVolumeType)
		})
	}
}

// driver_volume_type must be read from the top level only. Honouring a nested
// value would let a payload smuggle in a different backend type.
func TestParseRBDConnectionInfo_DriverVolumeTypeNotTakenFromNestedData(t *testing.T) {
	raw := map[string]any{
		"data": map[string]any{"driver_volume_type": "rbd", "name": "p/i"},
	}
	_, err := parseRBDConnectionInfo(raw)
	require.ErrorIs(t, err, ErrWrongDriverVolumeType)
}

func TestParseRBDConnectionInfo_MonitorPairing(t *testing.T) {
	tests := []struct {
		name    string
		hosts   []any
		ports   []any
		want    []string
		wantErr bool
	}{
		{
			name:  "paired positionally",
			hosts: []any{"10.0.0.1", "10.0.0.2"},
			ports: []any{"6789", "6790"},
			want:  []string{"10.0.0.1:6789", "10.0.0.2:6790"},
		},
		{
			name:  "numeric ports are accepted",
			hosts: []any{"10.0.0.1"},
			ports: []any{float64(6789)},
			want:  []string{"10.0.0.1:6789"},
		},
		{
			name:  "ipv6 monitors are bracketed",
			hosts: []any{"fd00::1"},
			ports: []any{"6789"},
			want:  []string{"[fd00::1]:6789"},
		},
		{name: "more hosts than ports is rejected", hosts: []any{"a", "b"}, ports: []any{"6789"}, wantErr: true},
		{name: "more ports than hosts is rejected", hosts: []any{"a"}, ports: []any{"6789", "6789"}, wantErr: true},
		{name: "empty hosts is rejected", hosts: []any{}, ports: []any{}, wantErr: true},
		{name: "empty host entry is rejected", hosts: []any{""}, ports: []any{"6789"}, wantErr: true},
		{name: "empty port entry is rejected", hosts: []any{"a"}, ports: []any{""}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := flatPayload()
			raw["hosts"] = tt.hosts
			raw["ports"] = tt.ports

			ci, err := parseRBDConnectionInfo(raw)
			if tt.wantErr {
				require.ErrorIs(t, err, ErrConnectionInfoIncomplete)
				return
			}
			require.NoError(t, err)
			got := make([]string, 0, len(ci.Monitors))
			for _, m := range ci.Monitors {
				got = append(got, m.String())
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestParseRBDConnectionInfo_RequiredFields(t *testing.T) {
	tests := []struct {
		name   string
		delete string
	}{
		{name: "missing name", delete: "name"},
		{name: "missing hosts", delete: "hosts"},
		{name: "missing ports", delete: "ports"},
		{name: "missing secret_uuid", delete: "secret_uuid"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := flatPayload()
			delete(raw, tt.delete)

			_, err := parseRBDConnectionInfo(raw)
			require.ErrorIs(t, err, ErrConnectionInfoIncomplete)
		})
	}
}

// auth_username is what the node's keyring entity must match, so it cannot be
// missing while auth is on.
func TestParseRBDConnectionInfo_AuthUsernameRequiredWhenAuthEnabled(t *testing.T) {
	raw := flatPayload()
	raw["auth_enabled"] = true
	delete(raw, "auth_username")

	_, err := parseRBDConnectionInfo(raw)
	require.ErrorIs(t, err, ErrConnectionInfoIncomplete)
}

func TestParseRBDConnectionInfo_AuthDisabledAllowsNoUsername(t *testing.T) {
	raw := flatPayload()
	raw["auth_enabled"] = false
	delete(raw, "auth_username")

	ci, err := parseRBDConnectionInfo(raw)
	require.NoError(t, err)
	assert.False(t, ci.AuthEnabled)
	assert.Empty(t, ci.AuthUsername)
}

// ── Nested `data` compatibility ──────────────────────────────────────────────

func TestParseRBDConnectionInfo_NestedDataFillsAbsentFields(t *testing.T) {
	raw := map[string]any{
		"driver_volume_type": "rbd",
		"data": map[string]any{
			"name":          "cinder-volumes/img-9",
			"secret_uuid":   "fsid-9",
			"auth_enabled":  true,
			"auth_username": "cinder",
			"hosts":         []any{"10.0.0.9"},
			"ports":         []any{"6789"},
			"cluster_name":  "ceph",
		},
	}

	ci, err := parseRBDConnectionInfo(raw)
	require.NoError(t, err)
	assert.Equal(t, "cinder-volumes", ci.Pool)
	assert.Equal(t, "img-9", ci.Image)
	assert.Equal(t, "fsid-9", ci.ClusterFSID)
	assert.Equal(t, "10.0.0.9:6789", ci.MonitorList())
}

// The top level wins. A nested value that agrees is fine; one that disagrees is
// rejected outright, because preferring either could map the wrong image.
func TestParseRBDConnectionInfo_NestedDataNeverOverridesTopLevel(t *testing.T) {
	t.Run("agreeing duplicate is accepted", func(t *testing.T) {
		raw := flatPayload()
		raw["data"] = map[string]any{"name": "cinder-volumes/img-1"}

		ci, err := parseRBDConnectionInfo(raw)
		require.NoError(t, err)
		assert.Equal(t, "img-1", ci.Image)
	})

	t.Run("conflicting name is rejected", func(t *testing.T) {
		raw := flatPayload()
		raw["data"] = map[string]any{"name": "other-pool/other-image"}

		_, err := parseRBDConnectionInfo(raw)
		require.ErrorIs(t, err, ErrConnectionInfoConflict)
		// The field name is reported; values are not, since a regressed backend
		// could place something sensitive in either.
		assert.Contains(t, err.Error(), "name")
	})

	t.Run("conflicting secret_uuid is rejected", func(t *testing.T) {
		raw := flatPayload()
		raw["data"] = map[string]any{"secret_uuid": "a-different-fsid"}

		_, err := parseRBDConnectionInfo(raw)
		require.ErrorIs(t, err, ErrConnectionInfoConflict)
	})

	t.Run("equivalent string and numeric port is not a conflict", func(t *testing.T) {
		raw := flatPayload()
		raw["ports"] = []any{"6789", "6789"}
		raw["data"] = map[string]any{"ports": []any{float64(6789), float64(6789)}}

		ci, err := parseRBDConnectionInfo(raw)
		require.NoError(t, err)
		assert.Equal(t, "10.0.0.1:6789,10.0.0.2:6789", ci.MonitorList())
	})
}

// The parser must not mutate its input: the caller may parse the same payload
// again during recovery.
func TestParseRBDConnectionInfo_DoesNotMutateInput(t *testing.T) {
	raw := flatPayload()
	raw["data"] = map[string]any{"access_mode": "rw"}
	before := len(raw)

	_, err := parseRBDConnectionInfo(raw)
	require.NoError(t, err)

	assert.Len(t, raw, before)
	_, stillNested := raw["data"].(map[string]any)
	assert.True(t, stillNested, "nested data must be left intact")
	assert.NotContains(t, raw, "access_mode", "nested keys must not be hoisted into the caller's map")
}

// ── Credential rejection (CVE-2020-10755 class) ──────────────────────────────

// A Ceph key must never arrive through connection_info. If one does, the
// backend has regressed and the driver refuses the response rather than using
// a credential from an untrusted channel.
func TestParseRBDConnectionInfo_RejectsCredentialFields(t *testing.T) {
	for _, field := range []string{"keyring", "key", "secret", "secret_key", "password", "userkey", "user_key"} {
		t.Run("top-level "+field, func(t *testing.T) {
			raw := flatPayload()
			raw[field] = "AQBSECRET=="

			_, err := parseRBDConnectionInfo(raw)
			require.ErrorIs(t, err, ErrConnectionInfoHasSecret)
			assert.Contains(t, err.Error(), field)
			assert.NotContains(t, err.Error(), "AQBSECRET", "the value must never be echoed")
		})

		t.Run("nested "+field, func(t *testing.T) {
			raw := flatPayload()
			raw["data"] = map[string]any{field: "AQBSECRET=="}

			_, err := parseRBDConnectionInfo(raw)
			require.ErrorIs(t, err, ErrConnectionInfoHasSecret)
			assert.NotContains(t, err.Error(), "AQBSECRET")
		})
	}
}

// secret_type is diagnostic only and must not be mistaken for a credential.
func TestParseRBDConnectionInfo_SecretTypeIsNotACredential(t *testing.T) {
	raw := flatPayload()
	raw["secret_type"] = "ceph"

	ci, err := parseRBDConnectionInfo(raw)
	require.NoError(t, err)
	assert.Equal(t, "fsid-1", ci.ClusterFSID)
}

func TestParseRBDConnectionInfo_EmptyPayload(t *testing.T) {
	_, err := parseRBDConnectionInfo(nil)
	require.ErrorIs(t, err, ErrConnectionInfoIncomplete)

	_, err = parseRBDConnectionInfo(map[string]any{})
	require.ErrorIs(t, err, ErrConnectionInfoIncomplete)
}
