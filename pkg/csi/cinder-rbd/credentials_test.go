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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-rbd/openstack"
)

// redactedKey stands in for a real Ceph key. Tests assert it never escapes.
const redactedKey = "AQBSECRETKEYVALUE1234567890abcdefghij=="

// writeCredentialDir builds a fake projected Secret volume.
func writeCredentialDir(t *testing.T, userID, key string) string {
	t.Helper()
	dir := t.TempDir()
	if userID != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, credentialUserIDFile), []byte(userID), 0o400))
	}
	if key != "" {
		require.NoError(t, os.WriteFile(filepath.Join(dir, credentialKeyFile), []byte(key), 0o400))
	}
	return dir
}

func TestFileCredentialProvider_Load(t *testing.T) {
	dir := writeCredentialDir(t, "cinder", redactedKey)
	p := NewFileCredentialProvider(dir)

	cred, err := p.Load(context.Background(), "cinder")
	require.NoError(t, err)
	assert.Equal(t, "cinder", cred.UserID)
	assert.Equal(t, "client.cinder", cred.Entity())
	assert.False(t, cred.IsZero())
}

// Secret data commonly carries a trailing newline, which is not part of the
// value. A key with a stray newline would fail Ceph authentication obscurely.
func TestFileCredentialProvider_TrimsWhitespace(t *testing.T) {
	dir := writeCredentialDir(t, "cinder\n", redactedKey+"\n")
	p := NewFileCredentialProvider(dir)

	cred, err := p.Load(context.Background(), "cinder")
	require.NoError(t, err)
	assert.Equal(t, "cinder", cred.UserID)
}

// The entity match must fail before any map attempt: krbd requires --id and the
// keyring entity to be the same identity, and a mismatch would otherwise appear
// as an opaque Ceph auth error.
func TestFileCredentialProvider_EntityMismatch(t *testing.T) {
	dir := writeCredentialDir(t, "wrcp-csi", redactedKey)
	p := NewFileCredentialProvider(dir)

	_, err := p.Load(context.Background(), "cinder")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrCredentialEntityMismatch))
	// The message must name both identities so the operator can fix it...
	assert.Contains(t, err.Error(), "wrcp-csi")
	assert.Contains(t, err.Error(), "cinder")
	// ...but must never contain the key.
	assert.NotContains(t, err.Error(), redactedKey)
}

// An empty wantUserID skips the check, which is what startup validation needs
// before any connection_info exists.
func TestFileCredentialProvider_EmptyWantSkipsEntityCheck(t *testing.T) {
	dir := writeCredentialDir(t, "anything", redactedKey)
	p := NewFileCredentialProvider(dir)

	cred, err := p.Load(context.Background(), "")
	require.NoError(t, err)
	assert.Equal(t, "anything", cred.UserID)
}

func TestFileCredentialProvider_Missing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		userID string
		key    string
	}{
		{"no files at all", "", ""},
		{"userID only", "cinder", ""},
		{"key only", "", redactedKey},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := NewFileCredentialProvider(writeCredentialDir(t, tc.userID, tc.key))

			_, err := p.Load(context.Background(), "cinder")
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrCredentialMissing))

			assert.Error(t, p.Available(context.Background()))
		})
	}
}

func TestFileCredentialProvider_EmptyFileIsMissing(t *testing.T) {
	dir := writeCredentialDir(t, "cinder", "")
	require.NoError(t, os.WriteFile(filepath.Join(dir, credentialKeyFile), []byte("   \n"), 0o400))
	p := NewFileCredentialProvider(dir)

	err := p.Available(context.Background())
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrCredentialMissing))
}

func TestFileCredentialProvider_Available(t *testing.T) {
	p := NewFileCredentialProvider(writeCredentialDir(t, "cinder", redactedKey))
	assert.NoError(t, p.Available(context.Background()))
}

func TestFileCredentialProvider_NonexistentDir(t *testing.T) {
	p := NewFileCredentialProvider(filepath.Join(t.TempDir(), "does-not-exist"))
	assert.Error(t, p.Available(context.Background()))
	_, err := p.Load(context.Background(), "cinder")
	assert.Error(t, err)
}

// The key must be unprintable by accident. This is the guard behind the design
// rule that credentials never reach logs: %v, %+v, %#v and %s must all redact.
func TestCephCredential_NeverPrintsKey(t *testing.T) {
	cred := NewTestCredential("cinder", redactedKey)

	for _, format := range []string{"%v", "%+v", "%#v", "%s"} {
		rendered := fmt.Sprintf(format, cred)
		assert.NotContains(t, rendered, redactedKey,
			"format %q leaked the key", format)
		assert.Contains(t, rendered, "redacted",
			"format %q should mark the key as redacted", format)
	}

	// Also verify when embedded in a larger structure, which is how a logger
	// would most plausibly reach it.
	wrapper := struct{ Cred *CephCredential }{cred}
	assert.NotContains(t, fmt.Sprintf("%+v", wrapper), redactedKey)
}

func TestCephCredential_NilSafe(t *testing.T) {
	var cred *CephCredential
	assert.Equal(t, "<nil>", cred.String())
	assert.True(t, cred.IsZero())
	assert.NotPanics(t, func() { _ = fmt.Sprintf("%v", cred) })
}

// ── Runtime credential materialization ───────────────────────────────────────

func testRBDOptsWithDirs(t *testing.T) openstack.RBDOpts {
	t.Helper()
	base := t.TempDir()

	var opts openstack.RBDOpts
	require.NoError(t, opts.ApplyDefaults())
	opts.RuntimeDir = filepath.Join(base, "run")
	opts.StateDir = filepath.Join(base, "state")
	opts.ExpectedFSID = "c5f7876d-258c-4152-b26a-a3ab532fda28"
	return opts
}

func testMaterializeConnInfo() *openstack.RBDConnectionInfo {
	return &openstack.RBDConnectionInfo{
		DriverVolumeType: openstack.DriverVolumeTypeRBD,
		ClusterName:      "ceph",
		ClusterFSID:      "c5f7876d-258c-4152-b26a-a3ab532fda28",
		Pool:             "cinder-volumes",
		Image:            "img-1",
		AuthEnabled:      true,
		AuthUsername:     "cinder",
		Monitors: []openstack.MonAddr{
			{Host: "10.107.190.121", Port: "6789"},
			{Host: "10.106.210.60", Port: "6789"},
		},
	}
}

func TestMaterializeRuntimeFiles_WritesConfAndKeyring(t *testing.T) {
	opts := testRBDOptsWithDirs(t)
	cred := NewTestCredential("cinder", redactedKey)

	rf, err := materializeRuntimeFiles(opts, "vol-1", testMaterializeConnInfo(), cred)
	require.NoError(t, err)

	assert.FileExists(t, rf.ConfPath)
	assert.FileExists(t, rf.KeyringPath)

	conf, err := os.ReadFile(rf.ConfPath)
	require.NoError(t, err)
	// The FSID recorded here is what binds the generated config to a verified
	// cluster identity.
	assert.Contains(t, string(conf), "fsid = c5f7876d-258c-4152-b26a-a3ab532fda28")
	assert.Contains(t, string(conf), "mon_host = 10.107.190.121:6789,10.106.210.60:6789")
	assert.Contains(t, string(conf), "auth_client_required = cephx")
	// The config must never carry the key.
	assert.NotContains(t, string(conf), redactedKey)

	keyring, err := os.ReadFile(rf.KeyringPath)
	require.NoError(t, err)
	assert.Contains(t, string(keyring), "[client.cinder]")
	assert.Contains(t, string(keyring), redactedKey)
}

// The keyring is the one file carrying key material, so its mode is asserted
// explicitly rather than assumed from the umask.
func TestMaterializeRuntimeFiles_StrictPermissions(t *testing.T) {
	opts := testRBDOptsWithDirs(t)

	rf, err := materializeRuntimeFiles(opts, "vol-1", testMaterializeConnInfo(),
		NewTestCredential("cinder", redactedKey))
	require.NoError(t, err)

	dirInfo, err := os.Stat(rf.Dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), dirInfo.Mode().Perm(), "runtime dir must be owner-only")

	confInfo, err := os.Stat(rf.ConfPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), confInfo.Mode().Perm())

	keyringInfo, err := os.Stat(rf.KeyringPath)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o400), keyringInfo.Mode().Perm(),
		"keyring must be read-only to the owner")
}

// A pre-existing directory with a loose mode must be tightened, otherwise an
// upgrade from a version with a different umask leaves key material readable.
func TestMaterializeRuntimeFiles_TightensLooseDirectoryMode(t *testing.T) {
	opts := testRBDOptsWithDirs(t)
	dir := volumeRuntimeDir(opts, "vol-1")
	require.NoError(t, os.MkdirAll(dir, 0o777))

	_, err := materializeRuntimeFiles(opts, "vol-1", testMaterializeConnInfo(),
		NewTestCredential("cinder", redactedKey))
	require.NoError(t, err)

	info, err := os.Stat(dir)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm())
}

func TestMaterializeRuntimeFiles_IsIdempotent(t *testing.T) {
	opts := testRBDOptsWithDirs(t)
	ci := testMaterializeConnInfo()
	cred := NewTestCredential("cinder", redactedKey)

	first, err := materializeRuntimeFiles(opts, "vol-1", ci, cred)
	require.NoError(t, err)
	second, err := materializeRuntimeFiles(opts, "vol-1", ci, cred)
	require.NoError(t, err)

	assert.Equal(t, first.ConfPath, second.ConfPath)
	assert.FileExists(t, second.KeyringPath)
}

func TestRemoveRuntimeFiles_DeletesEverything(t *testing.T) {
	opts := testRBDOptsWithDirs(t)
	rf, err := materializeRuntimeFiles(opts, "vol-1", testMaterializeConnInfo(),
		NewTestCredential("cinder", redactedKey))
	require.NoError(t, err)

	require.NoError(t, removeRuntimeFiles(opts, "vol-1"))

	assert.NoFileExists(t, rf.KeyringPath)
	assert.NoFileExists(t, rf.ConfPath)
	assert.NoDirExists(t, rf.Dir)
}

// Key material must not outlive its use, so removal must succeed even when
// called twice (unstage is retried).
func TestRemoveRuntimeFiles_IsIdempotent(t *testing.T) {
	opts := testRBDOptsWithDirs(t)
	_, err := materializeRuntimeFiles(opts, "vol-1", testMaterializeConnInfo(),
		NewTestCredential("cinder", redactedKey))
	require.NoError(t, err)

	require.NoError(t, removeRuntimeFiles(opts, "vol-1"))
	assert.NoError(t, removeRuntimeFiles(opts, "vol-1"))
}

// A half-materialized directory is worse than none: the next map would fail on a
// missing keyring. Verify the conf is cleaned up when the keyring cannot be
// written.
func TestMaterializeRuntimeFiles_CleansUpOnKeyringFailure(t *testing.T) {
	opts := testRBDOptsWithDirs(t)
	dir := volumeRuntimeDir(opts, "vol-1")
	require.NoError(t, os.MkdirAll(dir, 0o700))

	// Make the keyring path a directory so writing a file there fails.
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "keyring"), 0o700))

	_, err := materializeRuntimeFiles(opts, "vol-1", testMaterializeConnInfo(),
		NewTestCredential("cinder", redactedKey))
	require.Error(t, err)
	assert.NoDirExists(t, dir, "a failed materialization must leave nothing behind")
}

func TestBuildCephConf_AuthDisabled(t *testing.T) {
	conf := string(buildCephConf("fsid-1", []openstack.MonAddr{{Host: "10.0.0.1", Port: "6789"}}, false))

	assert.Contains(t, conf, "auth_client_required = none")
	assert.NotContains(t, conf, "cephx")
}

func TestBuildCephConf_IPv6Monitors(t *testing.T) {
	conf := string(buildCephConf("fsid-1", []openstack.MonAddr{{Host: "fd00::1", Port: "6789"}}, true))

	assert.Contains(t, conf, "mon_host = [fd00::1]:6789")
}

// The cluster-scoped conf exists so credential-free commands never fall back to
// the host's unsubstituted template. It must contain no key material.
func TestWriteClusterConf(t *testing.T) {
	opts := testRBDOptsWithDirs(t)
	require.NoError(t, os.MkdirAll(opts.RuntimeDir, 0o700))

	require.NoError(t, writeClusterConf(opts, []openstack.MonAddr{{Host: "10.0.0.1", Port: "6789"}}))

	content, err := os.ReadFile(clusterConfPath(opts))
	require.NoError(t, err)
	assert.Contains(t, string(content), "fsid = c5f7876d-258c-4152-b26a-a3ab532fda28")
	assert.NotContains(t, string(content), "key =")
}

// The redaction guarantee end to end: the only file that may contain the key is
// the keyring, and nothing else written during staging may reference it.
func TestKeyMaterialConfinedToKeyring(t *testing.T) {
	opts := testRBDOptsWithDirs(t)
	rf, err := materializeRuntimeFiles(opts, "vol-1", testMaterializeConnInfo(),
		NewTestCredential("cinder", redactedKey))
	require.NoError(t, err)

	var filesWithKey []string
	err = filepath.Walk(opts.RuntimeDir, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return nil //nolint:nilerr // unreadable entries are not a test failure
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return nil
		}
		if strings.Contains(string(content), redactedKey) {
			filesWithKey = append(filesWithKey, path)
		}
		return nil
	})
	require.NoError(t, err)

	require.Len(t, filesWithKey, 1, "exactly one file may contain the key")
	assert.Equal(t, rf.KeyringPath, filesWithKey[0])
}
