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
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func boolPtr(b bool) *bool { return &b }

func TestRBDOpts_ApplyDefaults_Empty(t *testing.T) {
	var o RBDOpts
	require.NoError(t, o.ApplyDefaults())

	assert.Equal(t, MounterKRBD, o.Mounter)
	assert.Equal(t, "ceph", o.ExpectedClusterName)
	assert.Equal(t, 18, o.CephClientVersionMajor)
	assert.Equal(t, "/etc/cinder-rbd-csi/ceph", o.CredentialPath)
	assert.Equal(t, "/run/cinder-rbd-csi", o.RuntimeDir)
	assert.Equal(t, "/var/lib/cinder-rbd-csi", o.StateDir)
	assert.Equal(t, 120*time.Second, o.MapTimeoutDuration())
	assert.Equal(t, 120*time.Second, o.UnmapTimeoutDuration())
	assert.Equal(t, 60*time.Second, o.DeviceWaitTimeoutDuration())
	assert.Equal(t, int64(0), o.MaxVolumesPerNode)

	// Unset exclusive must mean enabled: the safe value is the default.
	assert.True(t, o.IsExclusive())
}

func TestRBDOpts_ApplyDefaults_PreservesExplicitValues(t *testing.T) {
	o := RBDOpts{
		Mounter:                MounterKRBD,
		ExpectedClusterName:    "otherceph",
		ExpectedFSID:           "fsid-123",
		CephClientVersionMajor: 19,
		CredentialPath:         "/custom/creds",
		RuntimeDir:             "/custom/run",
		StateDir:               "/custom/state",
		MapTimeout:             "30s",
		UnmapTimeout:           "45s",
		DeviceWaitTimeout:      "5s",
		MaxVolumesPerNode:      64,
	}
	require.NoError(t, o.ApplyDefaults())

	assert.Equal(t, "otherceph", o.ExpectedClusterName)
	assert.Equal(t, "fsid-123", o.ExpectedFSID)
	assert.Equal(t, 19, o.CephClientVersionMajor)
	assert.Equal(t, "/custom/creds", o.CredentialPath)
	assert.Equal(t, 30*time.Second, o.MapTimeoutDuration())
	assert.Equal(t, 45*time.Second, o.UnmapTimeoutDuration())
	assert.Equal(t, 5*time.Second, o.DeviceWaitTimeoutDuration())
	assert.Equal(t, int64(64), o.MaxVolumesPerNode)
}

// A malformed duration must be an error, not a silent fallback to the default:
// quietly substituting 120s for "12x" would hide a misconfiguration.
func TestRBDOpts_ApplyDefaults_RejectsBadDurations(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts RBDOpts
		want string
	}{
		{"malformed map-timeout", RBDOpts{MapTimeout: "12x"}, "map-timeout"},
		{"malformed unmap-timeout", RBDOpts{UnmapTimeout: "abc"}, "unmap-timeout"},
		{"malformed device-wait", RBDOpts{DeviceWaitTimeout: "-"}, "device-wait-timeout"},
		{"zero map-timeout", RBDOpts{MapTimeout: "0s"}, "map-timeout"},
		{"negative unmap-timeout", RBDOpts{UnmapTimeout: "-5s"}, "unmap-timeout"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.opts.ApplyDefaults()
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestRBDOpts_ApplyDefaults_RejectsNegativeMaxVolumes(t *testing.T) {
	o := RBDOpts{MaxVolumesPerNode: -1}
	err := o.ApplyDefaults()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "max-volumes-per-node")
}

// Validate must make the two unsafe configurations unrepresentable rather than
// merely discouraged.
func TestRBDOpts_Validate(t *testing.T) {
	base := func() RBDOpts {
		o := RBDOpts{}
		require.NoError(t, o.ApplyDefaults())
		return o
	}

	t.Run("defaults are valid", func(t *testing.T) {
		assert.NoError(t, base().Validate())
	})

	t.Run("rejects non-krbd mounter", func(t *testing.T) {
		o := base()
		o.Mounter = "nbd"
		err := o.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mounter")
	})

	t.Run("rejects exclusive=false", func(t *testing.T) {
		o := base()
		o.Exclusive = boolPtr(false)
		err := o.Validate()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exclusive")
	})

	t.Run("accepts explicit exclusive=true", func(t *testing.T) {
		o := base()
		o.Exclusive = boolPtr(true)
		assert.NoError(t, o.Validate())
	})
}

func TestVolumeOpts_ApplyDefaults(t *testing.T) {
	var o VolumeOpts
	require.NoError(t, o.ApplyDefaults())

	assert.Equal(t, 300, o.CreateTimeout)
	assert.Equal(t, 120, o.DetachTimeout)
	assert.Equal(t, DefaultMetadataPrefix, o.MetadataPrefix)
	assert.Equal(t, "csi.rbd", o.MetadataPrefix)
	// Retain is the migration contract: PVC deletion must not destroy the
	// Cinder volume the Blueprint still needs.
	assert.Equal(t, DeleteVolumeModeRetain, o.DeleteVolumeMode)
}

// The baseline treats any non-"delete" value as retain, which silently accepts
// typos. Here an unknown mode is rejected.
func TestVolumeOpts_Validate_RejectsUnknownMode(t *testing.T) {
	o := VolumeOpts{DeleteVolumeMode: "delet"}
	err := o.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "delete-volume-mode")
}

// Retain-only is enforced at startup rather than by failing DeleteVolume.
// Rejecting the RPC would leave the PersistentVolume in a delete loop that never
// completes; rejecting the configuration tells the operator immediately and
// strands nothing.
func TestVolumeOpts_Validate_RejectsDeleteMode(t *testing.T) {
	o := VolumeOpts{DeleteVolumeMode: DeleteVolumeModeDelete}
	err := o.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not implemented")
	assert.Contains(t, err.Error(), DeleteVolumeModeRetain)
}

func TestVolumeOpts_Validate_AcceptsRetain(t *testing.T) {
	o := VolumeOpts{DeleteVolumeMode: DeleteVolumeModeRetain}
	require.NoError(t, o.Validate())
}

// ApplyDefaults must produce a configuration that Validate accepts, or an
// operator supplying no [Volume] section at all cannot start the driver.
func TestVolumeOpts_DefaultsAreValid(t *testing.T) {
	var o VolumeOpts
	require.NoError(t, o.ApplyDefaults())
	require.NoError(t, o.Validate())
}

func TestMonAddr_String(t *testing.T) {
	for _, tc := range []struct {
		name string
		addr MonAddr
		want string
	}{
		{"ipv4", MonAddr{Host: "10.0.0.1", Port: "6789"}, "10.0.0.1:6789"},
		{"hostname", MonAddr{Host: "mon-a", Port: "6789"}, "mon-a:6789"},
		{"no port", MonAddr{Host: "10.0.0.1"}, "10.0.0.1"},
		{"ipv6 is bracketed", MonAddr{Host: "fd00::1", Port: "6789"}, "[fd00::1]:6789"},
		{"already bracketed ipv6", MonAddr{Host: "[fd00::1]", Port: "6789"}, "[fd00::1]:6789"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.addr.String())
		})
	}
}

func TestRBDConnectionInfo_Helpers(t *testing.T) {
	ci := &RBDConnectionInfo{
		Pool:  "cinder-volumes",
		Image: "3018df26-0ba3-45a3-adfd-4a84ed59fff1",
		Monitors: []MonAddr{
			{Host: "10.107.190.121", Port: "6789"},
			{Host: "10.106.210.60", Port: "6789"},
		},
	}
	assert.Equal(t, "cinder-volumes/3018df26-0ba3-45a3-adfd-4a84ed59fff1", ci.ImageSpec())
	assert.Equal(t, "10.107.190.121:6789,10.106.210.60:6789", ci.MonitorList())
}

// TestNoDeadConfigFields guards the rule that every configuration field must
// have a reader somewhere in the driver. The sibling iSCSI driver accumulated
// three knobs that are parsed and never read; this test makes that regression
// visible at review time.
//
// It cannot detect reads automatically, so it enforces a weaker but useful
// invariant: adding a gcfg-tagged field without registering the code that
// consumes it fails the build.
func TestNoDeadConfigFields(t *testing.T) {
	rbdReaders := map[string]string{
		"Mounter":                "RBDOpts.Validate",
		"Exclusive":              "RBDOpts.IsExclusive / Validate",
		"ExpectedClusterName":    "connection-info validation (phase 2)",
		"ExpectedFSID":           "identity gate check 1 (phase 3)",
		"CephClientVersionMajor": "rbdCLIMapper.CheckClient",
		"CredentialPath":         "Driver.SetupNodeService -> NewFileCredentialProvider",
		"RuntimeDir":             "credential materialization (phase 3)",
		"StateDir":               "staging index (phase 3)",
		"MapTimeout":             "RBDOpts.MapTimeoutDuration",
		"UnmapTimeout":           "RBDOpts.UnmapTimeoutDuration",
		"DeviceWaitTimeout":      "RBDOpts.DeviceWaitTimeoutDuration",
		"MaxVolumesPerNode":      "nodeServer.NodeGetInfo",
	}
	volumeReaders := map[string]string{
		"CreateTimeout":     "CreateVolume wait (phase 2)",
		"DetachTimeout":     "DeleteVolume / ControllerUnpublishVolume wait (phase 2)",
		"DefaultVolumeType": "CreateVolume volume type resolution (phase 2)",
		"MetadataPrefix":    "metadataKey",
		"DeleteVolumeMode":  "VolumeOpts.Validate (retain-only enforcement)",
	}

	check := func(t *testing.T, typ reflect.Type, readers map[string]string, section string) {
		t.Helper()
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			tag, ok := f.Tag.Lookup("gcfg")
			if !ok {
				continue // resolved/unexported fields are not configuration
			}
			if _, registered := readers[f.Name]; !registered {
				t.Errorf("[%s] field %s (gcfg:%q) has no registered reader. "+
					"Either wire it up or remove it — do not add dead configuration.",
					section, f.Name, tag)
			}
		}
		for name := range readers {
			if _, found := typ.FieldByName(name); !found {
				t.Errorf("[%s] registered reader for %s but the field no longer exists",
					section, name)
			}
		}
	}

	check(t, reflect.TypeOf(RBDOpts{}), rbdReaders, "RBD")
	check(t, reflect.TypeOf(VolumeOpts{}), volumeReaders, "Volume")
}
