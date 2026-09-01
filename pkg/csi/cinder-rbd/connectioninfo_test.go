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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-rbd/openstack"
)

// The emitted node ID is the bare hostname. The validated Cinder RBD backend
// accepts a connector containing only "host", so there is nothing else to carry.
func TestBuildNodeID(t *testing.T) {
	assert.Equal(t, "worker-0", BuildNodeID("worker-0", ""))
	assert.Equal(t, "worker-0;10.0.0.5", BuildNodeID("worker-0", "10.0.0.5"))
}

func TestParseNodeID(t *testing.T) {
	for _, tc := range []struct {
		name     string
		nodeID   string
		wantHost string
		wantIP   string
		wantErr  bool
	}{
		{"bare hostname", "worker-0", "worker-0", "", false},
		{"fqdn", "worker-0.lab.example.com", "worker-0.lab.example.com", "", false},
		{"forward-compatible two-field form", "worker-0;10.0.0.5", "worker-0", "10.0.0.5", false},
		{"empty", "", "", "", true},
		{"separator only", ";", "", "", true},
		{"missing ip", "worker-0;", "", "", true},
		{"missing host", ";10.0.0.5", "", "", true},
		{"three fields is rejected", "worker-0;10.0.0.5;iqn.x", "", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, ip, err := ParseNodeID(tc.nodeID)
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantHost, host)
			assert.Equal(t, tc.wantIP, ip)
		})
	}
}

// Round-tripping matters because a later addition of an IP must not break nodes
// already registered with the one-field form.
func TestNodeID_RoundTrip(t *testing.T) {
	for _, tc := range []struct{ host, ip string }{
		{"worker-0", ""},
		{"worker-0", "10.0.0.5"},
	} {
		host, ip, err := ParseNodeID(BuildNodeID(tc.host, tc.ip))
		require.NoError(t, err)
		assert.Equal(t, tc.host, host)
		assert.Equal(t, tc.ip, ip)
	}
}

// publish_context travels through a world-readable VolumeAttachment object, so
// its key set must never include anything credential-like.
func TestPublishContextKeys_CarryNoCredentialMaterial(t *testing.T) {
	keys := []string{
		PublishContextDriverVolumeType,
		PublishContextClusterName,
		PublishContextClusterFSID,
		PublishContextPool,
		PublishContextImage,
		PublishContextMonitors,
		PublishContextAuthEnabled,
		PublishContextAuthUsername,
		PublishContextVolumeID,
		PublishContextAccessMode,
		PublishContextAttachmentID,
	}
	forbidden := []string{"key", "keyring", "secret", "password", "token", "userkey"}

	for _, k := range keys {
		for _, bad := range forbidden {
			assert.NotContains(t, k, bad,
				"publish_context key %q must not reference credential material", k)
		}
	}
	// auth_username is an identity, not a credential, and is required so the
	// node can verify its keyring entity matches.
	assert.Equal(t, "auth_username", PublishContextAuthUsername)
	// secret_uuid is surfaced as cluster_fsid because that is what it is.
	assert.Equal(t, "cluster_fsid", PublishContextClusterFSID)
}

func TestImageIdentity(t *testing.T) {
	a := ImageIdentity{ClusterFSID: "fsid-1", ClusterName: "ceph", Pool: "cinder-volumes", Image: "img-1"}

	t.Run("string form carries no secrets", func(t *testing.T) {
		assert.Equal(t, "cinder-volumes/img-1@fsid-1", a.String())
	})

	t.Run("matches itself", func(t *testing.T) {
		assert.True(t, a.Matches(a))
	})

	// ClusterName is a local alias; the FSID is the cluster's real identity, so
	// a differing alias must not defeat a match.
	t.Run("cluster name is not part of identity", func(t *testing.T) {
		b := a
		b.ClusterName = "someotheralias"
		assert.True(t, a.Matches(b))
	})

	for _, tc := range []struct {
		name   string
		mutate func(ImageIdentity) ImageIdentity
	}{
		{"different fsid", func(i ImageIdentity) ImageIdentity { i.ClusterFSID = "fsid-2"; return i }},
		{"different pool", func(i ImageIdentity) ImageIdentity { i.Pool = "kube-rbd"; return i }},
		{"different image", func(i ImageIdentity) ImageIdentity { i.Image = "img-2"; return i }},
	} {
		t.Run(tc.name+" does not match", func(t *testing.T) {
			assert.False(t, a.Matches(tc.mutate(a)))
		})
	}

	t.Run("completeness gates unmap authorization", func(t *testing.T) {
		assert.True(t, a.IsComplete())
		for _, incomplete := range []ImageIdentity{
			{Pool: "p", Image: "i"},        // no FSID
			{ClusterFSID: "f", Image: "i"}, // no pool
			{ClusterFSID: "f", Pool: "p"},  // no image
			{},                             // nothing
		} {
			assert.False(t, incomplete.IsComplete())
		}
	})
}

func TestMappedDevice_Identity(t *testing.T) {
	d := MappedDevice{
		ID: 5, Pool: "cinder-volumes", Image: "img-1",
		DevicePath: "/dev/rbd5", ClusterFSID: "fsid-1",
	}
	want := ImageIdentity{ClusterFSID: "fsid-1", Pool: "cinder-volumes", Image: "img-1"}
	assert.True(t, d.Identity().Matches(want))
}

// metadataKey encodes the decision that a single configurable prefix governs
// every driver-owned Cinder metadata key, so the sibling drivers cannot collide
// on one volume.
func TestMetadataKey(t *testing.T) {
	assert.Equal(t, "csi.rbd.attachment_id",
		metadataKey(openstack.DefaultMetadataPrefix, metaKeySuffixAttachmentID))
	assert.Equal(t, "csi.rbd.cleanupVolume",
		metadataKey(openstack.DefaultMetadataPrefix, metaKeySuffixCleanupVolume))

	t.Run("empty prefix falls back to the default", func(t *testing.T) {
		assert.Equal(t, "csi.rbd.attachment_id", metadataKey("", metaKeySuffixAttachmentID))
	})

	t.Run("custom prefix is honoured", func(t *testing.T) {
		assert.Equal(t, "mycorp.attachment_id", metadataKey("mycorp", metaKeySuffixAttachmentID))
	})

	// The iSCSI sibling uses the "csi" prefix. The RBD default must differ so
	// both drivers can own metadata on the same volume without collision.
	t.Run("differs from the iscsi driver prefix", func(t *testing.T) {
		assert.NotEqual(t, "csi.attachment_id",
			metadataKey(openstack.DefaultMetadataPrefix, metaKeySuffixAttachmentID))
	})
}

// ── publish_context codec ────────────────────────────────────────────────────

func testConnInfo() *openstack.RBDConnectionInfo {
	return &openstack.RBDConnectionInfo{
		DriverVolumeType: openstack.DriverVolumeTypeRBD,
		ClusterName:      "ceph",
		ClusterFSID:      "c5f7876d-258c-4152-b26a-a3ab532fda28",
		Pool:             "cinder-volumes",
		Image:            "3018df26-0ba3-45a3-adfd-4a84ed59fff1",
		AuthEnabled:      true,
		AuthUsername:     "cinder",
		Monitors: []openstack.MonAddr{
			{Host: "10.107.190.121", Port: "6789"},
			{Host: "10.106.210.60", Port: "6789"},
		},
		VolumeID:   "3018df26-0ba3-45a3-adfd-4a84ed59fff1",
		AccessMode: "rw",
	}
}

func TestBuildPublishContext(t *testing.T) {
	pc := BuildPublishContext(testConnInfo(), "att-1")

	assert.Equal(t, "rbd", pc[PublishContextDriverVolumeType])
	assert.Equal(t, "ceph", pc[PublishContextClusterName])
	assert.Equal(t, "c5f7876d-258c-4152-b26a-a3ab532fda28", pc[PublishContextClusterFSID])
	assert.Equal(t, "cinder-volumes", pc[PublishContextPool])
	assert.Equal(t, "3018df26-0ba3-45a3-adfd-4a84ed59fff1", pc[PublishContextImage])
	assert.Equal(t, "10.107.190.121:6789,10.106.210.60:6789", pc[PublishContextMonitors])
	assert.Equal(t, "true", pc[PublishContextAuthEnabled])
	assert.Equal(t, "cinder", pc[PublishContextAuthUsername])
	assert.Equal(t, "att-1", pc[PublishContextAttachmentID])
}

// Optional fields are omitted rather than emitted empty, so the node can tell
// "absent" from "empty" during validation.
func TestBuildPublishContext_OmitsEmptyOptionalFields(t *testing.T) {
	ci := testConnInfo()
	ci.ClusterName = ""
	ci.AccessMode = ""
	ci.VolumeID = ""

	pc := BuildPublishContext(ci, "")

	assert.NotContains(t, pc, PublishContextClusterName)
	assert.NotContains(t, pc, PublishContextAccessMode)
	assert.NotContains(t, pc, PublishContextVolumeID)
	assert.NotContains(t, pc, PublishContextAttachmentID)
	// Required identity fields are always present.
	assert.Contains(t, pc, PublishContextClusterFSID)
	assert.Contains(t, pc, PublishContextPool)
	assert.Contains(t, pc, PublishContextImage)
}

func TestPublishContext_RoundTrip(t *testing.T) {
	original := testConnInfo()

	ci, attachmentID, err := ParsePublishContext(BuildPublishContext(original, "att-1"))
	require.NoError(t, err)

	assert.Equal(t, "att-1", attachmentID)
	assert.Equal(t, original.DriverVolumeType, ci.DriverVolumeType)
	assert.Equal(t, original.ClusterName, ci.ClusterName)
	assert.Equal(t, original.ClusterFSID, ci.ClusterFSID)
	assert.Equal(t, original.Pool, ci.Pool)
	assert.Equal(t, original.Image, ci.Image)
	assert.Equal(t, original.AuthEnabled, ci.AuthEnabled)
	assert.Equal(t, original.AuthUsername, ci.AuthUsername)
	assert.Equal(t, original.MonitorList(), ci.MonitorList())
}

func TestPublishContext_RoundTripIPv6(t *testing.T) {
	ci := testConnInfo()
	ci.Monitors = []openstack.MonAddr{{Host: "fd00::1", Port: "6789"}, {Host: "fd00::2", Port: "6790"}}

	parsed, _, err := ParsePublishContext(BuildPublishContext(ci, ""))
	require.NoError(t, err)

	require.Len(t, parsed.Monitors, 2)
	assert.Equal(t, "fd00::1", parsed.Monitors[0].Host)
	assert.Equal(t, "6789", parsed.Monitors[0].Port)
	assert.Equal(t, "fd00::2", parsed.Monitors[1].Host)
	assert.Equal(t, "6790", parsed.Monitors[1].Port)
}

func TestParsePublishContext_Errors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{name: "empty context", mutate: func(pc map[string]string) {
			for k := range pc {
				delete(pc, k)
			}
		}},
		{name: "missing monitors", mutate: func(pc map[string]string) {
			delete(pc, PublishContextMonitors)
		}},
		{name: "monitor without a port", mutate: func(pc map[string]string) {
			pc[PublishContextMonitors] = "10.0.0.1"
		}},
		{name: "non-boolean auth_enabled", mutate: func(pc map[string]string) {
			pc[PublishContextAuthEnabled] = "maybe"
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pc := BuildPublishContext(testConnInfo(), "att-1")
			tt.mutate(pc)

			_, _, err := ParsePublishContext(pc)
			require.Error(t, err)
		})
	}
}

// A bare address must not be silently given port 6789: guessing could point the
// node at a different cluster.
func TestParsePublishContext_DoesNotGuessMonitorPort(t *testing.T) {
	pc := BuildPublishContext(testConnInfo(), "")
	pc[PublishContextMonitors] = "10.0.0.1,10.0.0.2"

	_, _, err := ParsePublishContext(pc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "host:port")
}

// ── ValidateRBDConnectionInfo ────────────────────────────────────────────────

func TestValidateRBDConnectionInfo_Valid(t *testing.T) {
	var opts openstack.RBDOpts
	require.NoError(t, opts.ApplyDefaults())

	require.NoError(t, ValidateRBDConnectionInfo(testConnInfo(), opts))
}

func TestValidateRBDConnectionInfo_Invalid(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*openstack.RBDConnectionInfo)
	}{
		{name: "wrong driver volume type", mutate: func(c *openstack.RBDConnectionInfo) {
			c.DriverVolumeType = "iscsi"
		}},
		{name: "empty cluster fsid", mutate: func(c *openstack.RBDConnectionInfo) {
			c.ClusterFSID = ""
		}},
		{name: "empty pool", mutate: func(c *openstack.RBDConnectionInfo) { c.Pool = "" }},
		{name: "empty image", mutate: func(c *openstack.RBDConnectionInfo) { c.Image = "" }},
		{name: "no monitors", mutate: func(c *openstack.RBDConnectionInfo) { c.Monitors = nil }},
		{name: "monitor with empty port", mutate: func(c *openstack.RBDConnectionInfo) {
			c.Monitors = []openstack.MonAddr{{Host: "10.0.0.1"}}
		}},
		{name: "auth enabled without username", mutate: func(c *openstack.RBDConnectionInfo) {
			c.AuthEnabled = true
			c.AuthUsername = ""
		}},
	}

	var opts openstack.RBDOpts
	require.NoError(t, opts.ApplyDefaults())

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ci := testConnInfo()
			tt.mutate(ci)
			require.Error(t, ValidateRBDConnectionInfo(ci, opts))
		})
	}
}

func TestValidateRBDConnectionInfo_NilIsRejected(t *testing.T) {
	var opts openstack.RBDOpts
	require.NoError(t, opts.ApplyDefaults())
	require.Error(t, ValidateRBDConnectionInfo(nil, opts))
}

// The expected-FSID check is the driver's protection against a backend
// re-pointed at another Ceph cluster.
func TestValidateRBDConnectionInfo_ExpectedFSID(t *testing.T) {
	var opts openstack.RBDOpts
	require.NoError(t, opts.ApplyDefaults())

	t.Run("matching fsid passes", func(t *testing.T) {
		opts.ExpectedFSID = "c5f7876d-258c-4152-b26a-a3ab532fda28"
		require.NoError(t, ValidateRBDConnectionInfo(testConnInfo(), opts))
	})

	t.Run("mismatching fsid fails", func(t *testing.T) {
		opts.ExpectedFSID = "some-other-fsid"
		err := ValidateRBDConnectionInfo(testConnInfo(), opts)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "expected-fsid")
	})

	t.Run("unset expected fsid skips the check", func(t *testing.T) {
		opts.ExpectedFSID = ""
		require.NoError(t, ValidateRBDConnectionInfo(testConnInfo(), opts))
	})
}

func TestValidateRBDConnectionInfo_ExpectedClusterName(t *testing.T) {
	var opts openstack.RBDOpts
	require.NoError(t, opts.ApplyDefaults())
	opts.ExpectedClusterName = "ceph"

	t.Run("mismatching cluster name fails", func(t *testing.T) {
		ci := testConnInfo()
		ci.ClusterName = "otherceph"
		require.Error(t, ValidateRBDConnectionInfo(ci, opts))
	})

	// cluster_name is optional in the payload, so absence is not a mismatch.
	t.Run("absent cluster name is tolerated", func(t *testing.T) {
		ci := testConnInfo()
		ci.ClusterName = ""
		require.NoError(t, ValidateRBDConnectionInfo(ci, opts))
	})
}
