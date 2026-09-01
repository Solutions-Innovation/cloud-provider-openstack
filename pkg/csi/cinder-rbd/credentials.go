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

	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-rbd/openstack"
)

// Credential Secret file names, projected read-only into the node container.
// The operator duplicates the existing platform client.cinder key into this
// Secret; the driver never derives, discovers, or copies a key at runtime.
const (
	credentialUserIDFile = "userID"
	credentialKeyFile    = "userKey"
)

var (
	// ErrCredentialMissing means the projected Secret is absent or unreadable.
	ErrCredentialMissing = errors.New("ceph credential not available")

	// ErrCredentialEntityMismatch means the configured entity differs from the
	// auth_username Cinder returned. krbd requires --id and the keyring entity
	// to be the same identity, so this must fail before any map attempt.
	ErrCredentialEntityMismatch = errors.New("ceph credential entity does not match Cinder auth_username")
)

// CephCredential is an operator-managed Ceph identity.
//
// The key is unexported and String/GoString are overridden so the value cannot
// be printed by accident through %v, %+v, %s or a logger reflecting over the
// struct. Only writeKeyring formats it.
type CephCredential struct {
	// UserID is the entity without the "client." prefix, e.g. "cinder".
	UserID string
	key    string
}

// String implements fmt.Stringer without exposing the key.
func (c *CephCredential) String() string {
	if c == nil {
		return "<nil>"
	}
	return fmt.Sprintf("CephCredential{UserID:%q, key:<redacted>}", c.UserID)
}

// GoString implements fmt.GoStringer so %#v is also safe.
func (c *CephCredential) GoString() string { return c.String() }

// Entity returns the full Ceph entity name, e.g. "client.cinder".
func (c *CephCredential) Entity() string { return "client." + c.UserID }

// IsZero reports whether the credential carries no key.
func (c *CephCredential) IsZero() bool { return c == nil || c.key == "" }

// CephCredentialProvider supplies the Ceph key used for kernel RBD mapping.
type CephCredentialProvider interface {
	// Load returns the credential, failing when its entity does not match
	// wantUserID. wantUserID comes from connection_info.auth_username.
	Load(ctx context.Context, wantUserID string) (*CephCredential, error)

	// Available reports whether a credential can be read at all. Used by the
	// node Probe so a missing Secret surfaces as not-ready instead of as a
	// per-volume staging failure.
	Available(ctx context.Context) error
}

// fileCredentialProvider reads the credential from a projected Secret volume.
//
// This is chosen over CSI node-stage secrets (which would place the key in an
// RPC payload) and over a Kubernetes API read (which would need a client and
// Secret RBAC). kubelet refreshes a projected Secret in place, so re-reading on
// every stage picks up a rotated key without restarting the pod.
type fileCredentialProvider struct {
	dir string
}

// NewFileCredentialProvider returns a provider reading userID and userKey from
// dir.
func NewFileCredentialProvider(dir string) CephCredentialProvider {
	return &fileCredentialProvider{dir: dir}
}

// readTrimmed reads a credential file and trims surrounding whitespace.
// Trailing newlines are common in Secret data and are not part of the value.
func (p *fileCredentialProvider) readTrimmed(name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(p.dir, name))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

// Available reports whether both credential files can be read.
func (p *fileCredentialProvider) Available(_ context.Context) error {
	for _, f := range []string{credentialUserIDFile, credentialKeyFile} {
		v, err := p.readTrimmed(f)
		if err != nil {
			return fmt.Errorf("%w: %s: %v", ErrCredentialMissing, filepath.Join(p.dir, f), err)
		}
		if v == "" {
			return fmt.Errorf("%w: %s is empty", ErrCredentialMissing, filepath.Join(p.dir, f))
		}
	}
	return nil
}

// Load reads the credential and enforces the entity match.
//
// The error message names both entities because a mismatch is a configuration
// error the operator must fix, and an opaque Ceph authentication failure later
// would be much harder to diagnose. Neither message contains the key.
func (p *fileCredentialProvider) Load(_ context.Context, wantUserID string) (*CephCredential, error) {
	userID, err := p.readTrimmed(credentialUserIDFile)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCredentialMissing, err)
	}
	key, err := p.readTrimmed(credentialKeyFile)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCredentialMissing, err)
	}
	if userID == "" || key == "" {
		return nil, fmt.Errorf("%w: userID or userKey is empty", ErrCredentialMissing)
	}
	if wantUserID != "" && userID != wantUserID {
		return nil, fmt.Errorf("%w: configured %q, Cinder returned %q",
			ErrCredentialEntityMismatch, userID, wantUserID)
	}
	return &CephCredential{UserID: userID, key: key}, nil
}

// ── Runtime credential materialization ───────────────────────────────────────

// File modes for generated runtime files. The keyring is 0400: read-only, owner
// only, because it is the one file that carries key material.
const (
	runtimeDirMode = 0o700
	cephConfMode   = 0o600
	keyringMode    = 0o400
)

// clusterConfPath returns the cluster-wide generated ceph.conf.
//
// Credential-free commands (`rbd device list`, `rbd device unmap`) still read a
// config file. Without this the CLI falls back to the host's
// /etc/ceph/ceph.conf, which on the validated platform is an unsubstituted
// template. Writing one cluster-scoped config at startup means those commands
// work even when no volume is staged.
func clusterConfPath(opts openstack.RBDOpts) string {
	return filepath.Join(opts.RuntimeDir, "ceph.conf")
}

// volumeRuntimeDir returns the private per-volume runtime directory.
func volumeRuntimeDir(opts openstack.RBDOpts, volumeID string) string {
	return filepath.Join(opts.RuntimeDir, volumeID)
}

// writeFileAtomic writes data via a temporary file and a rename.
//
// The temp file is created with the final mode before any content is written,
// so a partially written keyring is never readable. The rename is atomic within
// a directory, so a reader sees either the old file or the complete new one.
func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, runtimeDirMode); err != nil {
		return fmt.Errorf("create runtime directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()

	// Any early return must not leave the temp file behind.
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("chmod temporary file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	// fsync before rename: a crash must not leave an empty file under the
	// final name, which would fail authentication in a confusing way.
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpName, path, err)
	}
	return nil
}

// buildCephConf renders a minimal ceph.conf for one cluster.
//
// The fsid recorded here is the value already validated against
// [RBD] expected-fsid, which is what binds a generated config to a verified
// cluster identity.
func buildCephConf(fsid string, monitors []openstack.MonAddr, authEnabled bool) []byte {
	var b strings.Builder
	b.WriteString("# Generated by cinder-rbd.csi.windriver.com. Do not edit.\n")
	b.WriteString("[global]\n")
	if fsid != "" {
		fmt.Fprintf(&b, "fsid = %s\n", fsid)
	}
	if len(monitors) > 0 {
		hosts := make([]string, 0, len(monitors))
		for _, m := range monitors {
			hosts = append(hosts, m.String())
		}
		fmt.Fprintf(&b, "mon_host = %s\n", strings.Join(hosts, ","))
	}
	if authEnabled {
		b.WriteString("auth_cluster_required = cephx\n")
		b.WriteString("auth_service_required = cephx\n")
		b.WriteString("auth_client_required = cephx\n")
	} else {
		b.WriteString("auth_cluster_required = none\n")
		b.WriteString("auth_service_required = none\n")
		b.WriteString("auth_client_required = none\n")
	}
	return []byte(b.String())
}

// buildKeyring renders a keyring for one identity.
//
// This is the only place the key is formatted, which is what makes the
// redaction guarantee in CephCredential auditable.
func buildKeyring(cred *CephCredential) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "[%s]\n\tkey = %s\n", cred.Entity(), cred.key)
	return []byte(b.String())
}

// runtimeFiles are the paths handed to the rbd CLI for one volume.
type runtimeFiles struct {
	Dir         string
	ConfPath    string
	KeyringPath string
}

// materializeRuntimeFiles writes the per-volume ceph.conf and keyring.
//
// Both are written atomically with restrictive modes into a private,
// preferably memory-backed directory. They are removed after unmap, and after a
// failed map, so key material does not outlive its use.
func materializeRuntimeFiles(opts openstack.RBDOpts, volumeID string,
	ci *openstack.RBDConnectionInfo, cred *CephCredential) (runtimeFiles, error) {
	dir := volumeRuntimeDir(opts, volumeID)
	rf := runtimeFiles{
		Dir:         dir,
		ConfPath:    filepath.Join(dir, "ceph.conf"),
		KeyringPath: filepath.Join(dir, "keyring"),
	}

	if err := os.MkdirAll(dir, runtimeDirMode); err != nil {
		return runtimeFiles{}, fmt.Errorf("create runtime directory %s: %w", dir, err)
	}
	// MkdirAll leaves an existing directory's mode alone, so tighten it in case
	// it was created with a looser umask by an earlier version.
	if err := os.Chmod(dir, runtimeDirMode); err != nil {
		return runtimeFiles{}, fmt.Errorf("chmod runtime directory %s: %w", dir, err)
	}

	conf := buildCephConf(ci.ClusterFSID, ci.Monitors, ci.AuthEnabled)
	if err := writeFileAtomic(rf.ConfPath, conf, cephConfMode); err != nil {
		return runtimeFiles{}, fmt.Errorf("write ceph.conf: %w", err)
	}

	if cred != nil && !cred.IsZero() {
		if err := writeFileAtomic(rf.KeyringPath, buildKeyring(cred), keyringMode); err != nil {
			// Remove the conf too: a half-materialized directory is worse than
			// none, because the next map would fail on a missing keyring.
			_ = removeRuntimeFiles(opts, volumeID)
			return runtimeFiles{}, fmt.Errorf("write keyring: %w", err)
		}
	} else {
		rf.KeyringPath = ""
	}

	return rf, nil
}

// writeClusterConf writes the cluster-scoped ceph.conf used by credential-free
// commands. It carries no key material.
func writeClusterConf(opts openstack.RBDOpts, monitors []openstack.MonAddr) error {
	return writeFileAtomic(clusterConfPath(opts),
		buildCephConf(opts.ExpectedFSID, monitors, true), cephConfMode)
}

// removeRuntimeFiles deletes a volume's generated credential files.
//
// The keyring is overwritten before unlinking. On a memory-backed filesystem
// that is belt-and-braces, but the runtime directory is operator-configurable
// and may land on disk.
func removeRuntimeFiles(opts openstack.RBDOpts, volumeID string) error {
	dir := volumeRuntimeDir(opts, volumeID)

	keyring := filepath.Join(dir, "keyring")
	if info, err := os.Stat(keyring); err == nil && info.Mode().IsRegular() {
		if f, openErr := os.OpenFile(keyring, os.O_WRONLY, keyringMode); openErr == nil {
			zeros := make([]byte, info.Size())
			_, _ = f.Write(zeros)
			_ = f.Sync()
			_ = f.Close()
		}
	}

	if err := os.RemoveAll(dir); err != nil {
		return fmt.Errorf("remove runtime directory %s: %w", dir, err)
	}
	return nil
}
