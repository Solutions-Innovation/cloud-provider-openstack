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
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-rbd/openstack"
	"k8s.io/utils/exec"
	testingexec "k8s.io/utils/exec/testing"
)

// recordingExec captures the argv of every command and replays scripted output.
//
// Capturing argv is the point: several invariants in this driver are properties
// of the command line (--exclusive always present, --conf on every call), and
// only argv inspection can prove them.
type recordingExec struct {
	calls   [][]string
	outputs []scriptedOutput
	idx     int
}

type scriptedOutput struct {
	stdout string
	stderr string
	err    error
}

func (r *recordingExec) Command(cmd string, args ...string) exec.Cmd {
	return r.CommandContext(nil, cmd, args...) //nolint:staticcheck // nil ctx unused by the fake
}

func (r *recordingExec) CommandContext(_ context.Context, cmd string, args ...string) exec.Cmd {
	r.calls = append(r.calls, append([]string{cmd}, args...))

	var out scriptedOutput
	if r.idx < len(r.outputs) {
		out = r.outputs[r.idx]
	}
	r.idx++

	fake := &testingexec.FakeCmd{
		CombinedOutputScript: []testingexec.FakeAction{
			func() ([]byte, []byte, error) { return []byte(out.stdout), []byte(out.stderr), out.err },
		},
		RunScript: []testingexec.FakeAction{
			func() ([]byte, []byte, error) { return []byte(out.stdout), []byte(out.stderr), out.err },
		},
	}
	return testingexec.InitFakeCmd(fake, cmd, args...)
}

func (r *recordingExec) LookPath(file string) (string, error) { return file, nil }

// lastCall returns the argv of the most recent invocation.
func (r *recordingExec) lastCall() []string {
	if len(r.calls) == 0 {
		return nil
	}
	return r.calls[len(r.calls)-1]
}

func (r *recordingExec) argvString(i int) string {
	if i >= len(r.calls) {
		return ""
	}
	return strings.Join(r.calls[i], " ")
}

func newRecordingMapper(t *testing.T, outputs ...scriptedOutput) (*rbdCLIMapper, *recordingExec) {
	t.Helper()
	var o openstack.RBDOpts
	require.NoError(t, o.ApplyDefaults())

	rec := &recordingExec{outputs: outputs}
	return &rbdCLIMapper{exec: rec, opts: o}, rec
}

func testIdentity() ImageIdentity {
	return ImageIdentity{
		ClusterFSID: "c5f7876d-258c-4152-b26a-a3ab532fda28",
		ClusterName: "ceph",
		Pool:        "cinder-volumes",
		Image:       "3018df26-0ba3-45a3-adfd-4a84ed59fff1",
	}
}

func testMapRequest() MapRequest {
	return MapRequest{
		Identity:    testIdentity(),
		Monitors:    []openstack.MonAddr{{Host: "10.0.0.1", Port: "6789"}},
		UserID:      "cinder",
		ConfPath:    "/run/cinder-rbd-csi/vol/ceph.conf",
		KeyringPath: "/run/cinder-rbd-csi/vol/keyring",
		Exclusive:   true,
		Timeout:     30 * time.Second,
	}
}

// ── CheckClient ──────────────────────────────────────────────────────────────

func TestCheckClient_ParsesBundledClientVersion(t *testing.T) {
	m, _ := newRecordingMapper(t, scriptedOutput{
		stdout: "ceph version 18.2.8 (a1b2c3d4e5f6) reef (stable)\n",
	})

	v, err := m.CheckClient(t.Context())
	require.NoError(t, err)
	assert.Equal(t, "18.2.8", v)
}

// The whole reason the driver bundles a client: the host ships Ceph 14 while the
// cluster runs Ceph 18. A mismatch must fail loudly at startup.
func TestCheckClient_RejectsHostCeph14(t *testing.T) {
	m, _ := newRecordingMapper(t, scriptedOutput{
		stdout: "ceph version 14.2.22 (0adeed6d58c3) nautilus (stable)\n",
	})

	_, err := m.CheckClient(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "14")
	assert.Contains(t, err.Error(), "ceph-client-version-major")
}

func TestCheckClient_UnparseableOutput(t *testing.T) {
	for _, tt := range []struct{ name, out string }{
		{name: "empty output", out: ""},
		{name: "unrelated text", out: "command not found\n"},
		{name: "truncated version", out: "ceph version 18\n"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := newRecordingMapper(t, scriptedOutput{stdout: tt.out})
			_, err := m.CheckClient(t.Context())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "cannot parse ceph version")
		})
	}
}

// ── Map ──────────────────────────────────────────────────────────────────────

// Every writable map must carry --exclusive and --device-type krbd. This is the
// single-writer guarantee, so it is asserted on the argv rather than trusted.
func TestMap_AlwaysUsesExclusiveKRBD(t *testing.T) {
	m, rec := newRecordingMapper(t, scriptedOutput{stdout: "/dev/rbd5\n"})

	dev, err := m.Map(t.Context(), testMapRequest())
	require.NoError(t, err)

	argv := strings.Join(rec.lastCall(), " ")
	assert.Contains(t, argv, "--exclusive")
	assert.Contains(t, argv, "--device-type krbd")
	assert.Contains(t, argv, "--id cinder")
	assert.Contains(t, argv, "--conf /run/cinder-rbd-csi/vol/ceph.conf")
	assert.Contains(t, argv, "--keyring /run/cinder-rbd-csi/vol/keyring")
	assert.Contains(t, argv, "--cluster ceph")
	assert.Contains(t, argv, "cinder-volumes/3018df26-0ba3-45a3-adfd-4a84ed59fff1")

	assert.Equal(t, "/dev/rbd5", dev.DevicePath)
	assert.Equal(t, 5, dev.ID)
	assert.Equal(t, "cinder-volumes", dev.Pool)
	assert.Equal(t, testIdentity().ClusterFSID, dev.ClusterFSID)
}

// No image name may be invented: the pool/image pair goes to the CLI verbatim.
func TestMap_UsesImageNameVerbatim(t *testing.T) {
	m, rec := newRecordingMapper(t, scriptedOutput{stdout: "/dev/rbd0\n"})

	req := testMapRequest()
	req.Identity.Image = "volume-already-prefixed"
	_, err := m.Map(t.Context(), req)
	require.NoError(t, err)

	argv := strings.Join(rec.lastCall(), " ")
	assert.Contains(t, argv, "cinder-volumes/volume-already-prefixed")
	assert.NotContains(t, argv, "volume-volume-")
}

// A lock conflict must surface as ErrExclusiveLockDenied and must NOT trigger a
// second invocation without --exclusive.
func TestMap_LockDeniedDoesNotRetryWithoutExclusive(t *testing.T) {
	m, rec := newRecordingMapper(t, scriptedOutput{
		stderr: "rbd: map failed: (30) Read-only file system",
		err:    errors.New("exit status 1"),
	})

	_, err := m.Map(t.Context(), testMapRequest())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrExclusiveLockDenied))

	require.Len(t, rec.calls, 1, "a denied exclusive map must not be retried")
	assert.Contains(t, rec.argvString(0), "--exclusive")
}

// The same-host duplicate case observed in the lab: "already mapped as /dev/rbd5"
// plus a misleading EROFS.
func TestMap_AlreadyMappedIsLockDenied(t *testing.T) {
	m, _ := newRecordingMapper(t, scriptedOutput{
		stderr: "rbd: warning: image already mapped as /dev/rbd5\nrbd: map failed: (30) Read-only file system",
		err:    errors.New("exit status 1"),
	})

	_, err := m.Map(t.Context(), testMapRequest())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrExclusiveLockDenied))
}

// A writable non-exclusive mapping is not representable: the request is refused
// before any command runs.
func TestMap_RefusesNonExclusiveWritable(t *testing.T) {
	m, rec := newRecordingMapper(t)

	req := testMapRequest()
	req.Exclusive = false
	_, err := m.Map(t.Context(), req)

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrExclusiveLockDenied))
	assert.Empty(t, rec.calls, "no command may run for an unsupported request")
}

func TestMap_RejectsIncompleteIdentity(t *testing.T) {
	m, rec := newRecordingMapper(t)

	req := testMapRequest()
	req.Identity.ClusterFSID = ""
	_, err := m.Map(t.Context(), req)

	require.Error(t, err)
	assert.Empty(t, rec.calls)
}

func TestMap_EmptyDevicePathIsAnError(t *testing.T) {
	m, _ := newRecordingMapper(t, scriptedOutput{stdout: "\n"})

	_, err := m.Map(t.Context(), testMapRequest())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no device path")
}

// ── stderr is not a failure signal ───────────────────────────────────────────

// The WRCP host ships an unsubstituted ceph.conf template, so even successful
// credential-free commands print a parse error. Treating stderr as failure would
// break every list and unmap on the validated platform.
func TestRun_ConfigParseErrorOnStderrWithExitZeroIsSuccess(t *testing.T) {
	m, _ := newRecordingMapper(t, scriptedOutput{
		stdout: "/dev/rbd5\n",
		stderr: "parse error setting 'fsid' to '%CLUSTER_UUID%'\n",
		err:    nil,
	})

	dev, err := m.Map(t.Context(), testMapRequest())
	require.NoError(t, err, "exit 0 with stderr noise must be treated as success")
	assert.Equal(t, "/dev/rbd5", dev.DevicePath)
}

// ── ListMapped ───────────────────────────────────────────────────────────────

const labDeviceListJSON = `[
  {"id":"0","pool":"kube-rbd","namespace":"","name":"csi-vol-746d4c96","snap":"-","device":"/dev/rbd0"},
  {"id":"5","pool":"cinder-volumes","namespace":"","name":"3018df26-0ba3-45a3-adfd-4a84ed59fff1","snap":"-","device":"/dev/rbd5"}
]`

func TestListMapped_ParsesLabOutputAndIncludesForeignMappings(t *testing.T) {
	withFakeSysfs(t, cinderDevice(5))
	m, rec := newRecordingMapper(t, scriptedOutput{stdout: labDeviceListJSON})

	devs, err := m.ListMapped(t.Context())
	require.NoError(t, err)
	require.Len(t, devs, 2)

	// --conf is required even though list needs no credentials.
	assert.Contains(t, strings.Join(rec.lastCall(), " "), "--conf")
	assert.Contains(t, strings.Join(rec.lastCall(), " "), "--format json")

	assert.Equal(t, "kube-rbd", devs[0].Pool, "platform Ceph-CSI mappings must be listed")
	assert.Equal(t, "cinder-volumes", devs[1].Pool)
	assert.Equal(t, "/dev/rbd5", devs[1].DevicePath)
	// snap "-" means no snapshot.
	assert.Empty(t, devs[1].Snap)
	// The FSID is not in the CLI output and is filled from sysfs.
	assert.Equal(t, "c5f7876d-258c-4152-b26a-a3ab532fda28", devs[1].ClusterFSID)
}

func TestListMapped_EmptyOutputs(t *testing.T) {
	for _, tt := range []struct{ name, out string }{
		{name: "empty string", out: ""},
		{name: "whitespace", out: "  \n"},
		{name: "json null", out: "null"},
		{name: "empty array", out: "[]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			m, _ := newRecordingMapper(t, scriptedOutput{stdout: tt.out})
			devs, err := m.ListMapped(t.Context())
			require.NoError(t, err)
			assert.Empty(t, devs)
		})
	}
}

func TestListMapped_MalformedJSONIsAnError(t *testing.T) {
	m, _ := newRecordingMapper(t, scriptedOutput{stdout: "{not json"})

	_, err := m.ListMapped(t.Context())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse device list")
}

func TestListMapped_HandlesNamespace(t *testing.T) {
	m, _ := newRecordingMapper(t, scriptedOutput{
		stdout: `[{"id":"1","pool":"p","namespace":"ns1","name":"img","snap":"-","device":"/dev/rbd1"}]`,
	})

	devs, err := m.ListMapped(t.Context())
	require.NoError(t, err)
	require.Len(t, devs, 1)
	assert.Equal(t, "ns1", devs[0].Namespace)
}

// ── VerifyIdentity ───────────────────────────────────────────────────────────

func TestVerifyIdentity_MatchingDevicePasses(t *testing.T) {
	withFakeSysfs(t, cinderDevice(5))
	m, _ := newRecordingMapper(t, scriptedOutput{stdout: labDeviceListJSON})

	require.NoError(t, m.VerifyIdentity(t.Context(), "/dev/rbd5", testIdentity()))
}

func TestVerifyIdentity_Mismatches(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(ImageIdentity) ImageIdentity
	}{
		{name: "different fsid", mutate: func(i ImageIdentity) ImageIdentity {
			i.ClusterFSID = "another-cluster"
			return i
		}},
		{name: "different pool", mutate: func(i ImageIdentity) ImageIdentity {
			i.Pool = "kube-rbd"
			return i
		}},
		{name: "different image", mutate: func(i ImageIdentity) ImageIdentity {
			i.Image = "someone-elses-image"
			return i
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			withFakeSysfs(t, cinderDevice(5))
			m, _ := newRecordingMapper(t, scriptedOutput{stdout: labDeviceListJSON})

			err := m.VerifyIdentity(t.Context(), "/dev/rbd5", tt.mutate(testIdentity()))
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrIdentityMismatch))
		})
	}
}

// An incomplete expected identity must never authorize anything: this is what
// prevents an unmap driven by a partially populated staging record.
func TestVerifyIdentity_IncompleteExpectationFailsClosed(t *testing.T) {
	withFakeSysfs(t, cinderDevice(5))
	m, _ := newRecordingMapper(t)

	err := m.VerifyIdentity(t.Context(), "/dev/rbd5", ImageIdentity{Pool: "cinder-volumes"})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrIdentityMismatch))
}

// A device absent from sysfs cannot be verified, so it must not be trusted.
func TestVerifyIdentity_UnreadableDeviceFailsClosed(t *testing.T) {
	withFakeSysfs(t) // empty tree
	m, _ := newRecordingMapper(t)

	err := m.VerifyIdentity(t.Context(), "/dev/rbd7", testIdentity())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrIdentityMismatch))
}

// sysfs and the CLI listing must agree. A disagreement between two kernel views
// is never safe to act on.
func TestVerifyIdentity_SysfsAndDeviceListDisagree(t *testing.T) {
	withFakeSysfs(t, cinderDevice(5))
	// The listing claims device 5 is a different image.
	m, _ := newRecordingMapper(t, scriptedOutput{
		stdout: `[{"id":"5","pool":"cinder-volumes","namespace":"","name":"a-different-image","snap":"-","device":"/dev/rbd5"}]`,
	})

	err := m.VerifyIdentity(t.Context(), "/dev/rbd5", testIdentity())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrIdentityMismatch))
}

// Present in sysfs but absent from the device list is unverifiable.
func TestVerifyIdentity_AbsentFromDeviceList(t *testing.T) {
	withFakeSysfs(t, cinderDevice(5))
	m, _ := newRecordingMapper(t, scriptedOutput{stdout: "[]"})

	err := m.VerifyIdentity(t.Context(), "/dev/rbd5", testIdentity())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrIdentityMismatch))
}

// ── Unmap ────────────────────────────────────────────────────────────────────

// Unmap needs no credentials but still reads ceph.conf, so --conf must be
// present or the CLI falls back to the broken host template.
func TestUnmap_PassesConf(t *testing.T) {
	m, rec := newRecordingMapper(t, scriptedOutput{})

	require.NoError(t, m.Unmap(t.Context(), "/dev/rbd5", time.Second))
	argv := strings.Join(rec.lastCall(), " ")
	assert.Contains(t, argv, "--conf")
	assert.Contains(t, argv, "device unmap")
	assert.Contains(t, argv, "/dev/rbd5")
}

func TestUnmap_AlreadyGoneIsIdempotent(t *testing.T) {
	m, _ := newRecordingMapper(t, scriptedOutput{
		stderr: "rbd: /dev/rbd5 is not mapped",
		err:    errors.New("exit status 22"),
	})

	require.NoError(t, m.Unmap(t.Context(), "/dev/rbd5", time.Second))
}

func TestUnmap_BusyIsRetryable(t *testing.T) {
	m, _ := newRecordingMapper(t, scriptedOutput{
		stderr: "rbd: unmap failed: (16) Device or resource busy",
		err:    errors.New("exit status 1"),
	})

	err := m.Unmap(t.Context(), "/dev/rbd5", time.Second)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrDeviceBusy))
}

func TestUnmap_RejectsNonRBDPath(t *testing.T) {
	m, rec := newRecordingMapper(t)

	err := m.Unmap(t.Context(), "/dev/sda", time.Second)
	require.Error(t, err)
	assert.Empty(t, rec.calls, "a non-RBD path must never reach the CLI")
}

// ── LockHolders ──────────────────────────────────────────────────────────────

// Exact `rbd status` output captured from the lab while a device was mapped.
const labRBDStatusLocked = `Watchers:
	watcher=192.168.206.2:0/76629272 client.56193456 cookie=18446462598732840961
There is 1 exclusive lock on this image.
Locker           ID                         Address
client.56193456  auto 18446462598732840961  192.168.206.2:0/76629272
`

func TestLockHolders_ParsesLabOutput(t *testing.T) {
	m, rec := newRecordingMapper(t, scriptedOutput{stdout: labRBDStatusLocked})

	holders, err := m.LockHolders(t.Context(), testMapRequest())
	require.NoError(t, err)
	require.Len(t, holders, 1)
	assert.Contains(t, holders[0], "client.56193456")
	assert.Contains(t, holders[0], "192.168.206.2:0/76629272")
	assert.Contains(t, strings.Join(rec.lastCall(), " "), "--conf")
}

func TestLockHolders_UnlockedImage(t *testing.T) {
	m, _ := newRecordingMapper(t, scriptedOutput{stdout: "Watchers: none\n"})

	holders, err := m.LockHolders(t.Context(), testMapRequest())
	require.NoError(t, err)
	assert.Empty(t, holders)
}

func TestLockHolders_RejectsIncompleteIdentity(t *testing.T) {
	m, rec := newRecordingMapper(t)

	req := testMapRequest()
	req.Identity.Pool = ""
	_, err := m.LockHolders(t.Context(), req)

	require.Error(t, err)
	assert.Empty(t, rec.calls)
}

// ── helpers ──────────────────────────────────────────────────────────────────

func TestTruncate(t *testing.T) {
	assert.Equal(t, "abc", truncate("abc", 10))
	assert.Equal(t, "abc", truncate("abc", 3))
	assert.Equal(t, "ab...", truncate("abcdef", 2))
}

func TestContainsAny(t *testing.T) {
	assert.True(t, containsAny("device or resource busy", deviceBusyPatterns))
	assert.False(t, containsAny("all good", deviceBusyPatterns))
}
