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
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-rbd/openstack"
	"k8s.io/klog/v2"
	"k8s.io/utils/exec"
)

// Package vars so tests can substitute fakes. No test may invoke the real
// binaries or touch real devices.
var (
	rbdBinary      = "rbd"
	blockdevBinary = "blockdev"
)

// cephVersionRe matches the leading version triple of `rbd --version`, e.g.
//
//	ceph version 18.2.8 (a1b2c3) reef (stable)
var cephVersionRe = regexp.MustCompile(`ceph version (\d+)\.(\d+)\.(\d+)`)

// Recognised failure signatures from the rbd CLI and the kernel.
//
// Matching on message text is unavoidable here: the CLI does not expose distinct
// exit codes for these conditions. The patterns are deliberately narrow.
var (
	exclusiveLockDeniedPatterns = []string{
		"exclusive lock",
		"read-only file system", // krbd's confusing EROFS for an already-held image
		"already mapped",
		"device or resource busy",
	}
	notMappedPatterns = []string{
		"not mapped",
		"no such file or directory",
	}
	deviceBusyPatterns = []string{
		"device or resource busy",
		"target is busy",
	}
)

// rbdCLIMapper implements RBDMapper over the bundled Ceph rbd CLI plus the
// kernel sysfs interface.
type rbdCLIMapper struct {
	exec exec.Interface
	opts openstack.RBDOpts
}

// NewRBDCLIMapper returns an RBDMapper backed by the bundled rbd CLI.
func NewRBDCLIMapper(opts openstack.RBDOpts) RBDMapper {
	return &rbdCLIMapper{exec: exec.New(), opts: opts}
}

// run executes an rbd subcommand.
//
// Two rules are enforced here rather than at each call site:
//
//   - stdout and stderr are captured separately, and success is decided from the
//     exit status alone. The WRCP host ships /etc/ceph/ceph.conf as an
//     unsubstituted template whose fsid is the literal %CLUSTER_UUID%, so even
//     credential-free commands print a config parse error to stderr. Treating
//     stderr as failure would break every list and unmap on that platform.
//   - the command is bounded by the caller's context.
func (m *rbdCLIMapper) run(ctx context.Context, timeout time.Duration, args ...string) (string, string, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	cmd := m.exec.CommandContext(ctx, rbdBinary, args...)
	var stdout, stderr strings.Builder
	cmd.SetStdout(&stdout)
	cmd.SetStderr(&stderr)

	err := cmd.Run()
	so, se := stdout.String(), stderr.String()

	if se != "" {
		// Logged at a low level: on the validated platform this is routine
		// noise, not a fault.
		klog.V(5).Infof("rbd %s: stderr: %s", strings.Join(args, " "), truncate(se, 300))
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return so, se, fmt.Errorf("rbd %s: timed out after %s: %w",
				strings.Join(args, " "), timeout, ctxErr)
		}
		return so, se, fmt.Errorf("rbd %s: %w (stderr: %s)",
			strings.Join(args, " "), err, truncate(se, 300))
	}
	return so, se, nil
}

// clusterArgs are the flags every rbd invocation needs.
//
// --conf is mandatory on all of them, including list and unmap which need no
// credentials: without it the CLI falls back to the host config (see run).
func clusterArgs(clusterName, confPath, userID, keyringPath string) []string {
	args := make([]string, 0, 8)
	if clusterName != "" {
		args = append(args, "--cluster", clusterName)
	}
	if confPath != "" {
		args = append(args, "--conf", confPath)
	}
	if userID != "" {
		args = append(args, "--id", userID)
	}
	if keyringPath != "" {
		args = append(args, "--keyring", keyringPath)
	}
	return args
}

// CheckClient runs `rbd --version` and verifies the major version.
//
// This matters on the validated platform: the host ships Ceph 14 tooling while
// the cluster runs Ceph 18, which is why the driver bundles its own client. A
// silent mismatch would surface later as an obscure mapping failure.
func (m *rbdCLIMapper) CheckClient(ctx context.Context) (string, error) {
	out, _, err := m.run(ctx, 30*time.Second, "--version")
	if err != nil {
		return "", err
	}

	match := cephVersionRe.FindStringSubmatch(out)
	if match == nil {
		return "", fmt.Errorf("rbd: cannot parse ceph version from %q", truncate(out, 200))
	}
	major, err := strconv.Atoi(match[1])
	if err != nil {
		return "", fmt.Errorf("rbd: cannot parse ceph major version from %q: %w", match[1], err)
	}
	if want := m.opts.CephClientVersionMajor; want > 0 && major != want {
		return "", fmt.Errorf("rbd: bundled client major version is %d, expected %d "+
			"(configured via [RBD] ceph-client-version-major)", major, want)
	}
	return fmt.Sprintf("%s.%s.%s", match[1], match[2], match[3]), nil
}

// Map performs an exclusive kernel map and returns the created device.
//
// There is deliberately no code path that removes --exclusive and retries: a
// writable non-exclusive mapping would defeat the single-writer guarantee that
// the Ceph exclusive lock provides.
func (m *rbdCLIMapper) Map(ctx context.Context, req MapRequest) (MappedDevice, error) {
	if !req.Identity.IsComplete() {
		return MappedDevice{}, fmt.Errorf("rbd: map: incomplete image identity %s", req.Identity)
	}
	if req.UserID == "" {
		return MappedDevice{}, fmt.Errorf("rbd: map %s: user ID must not be empty", req.Identity)
	}
	if !req.Exclusive && !req.ReadOnly {
		// Refuse rather than silently downgrade: the caller asked for something
		// this driver does not support.
		return MappedDevice{}, fmt.Errorf("rbd: map %s: %w — writable mappings must be exclusive",
			req.Identity, ErrExclusiveLockDenied)
	}

	args := []string{"device", "map", "--device-type", "krbd"}
	if req.Exclusive {
		args = append(args, "--exclusive")
	}
	if req.ReadOnly {
		args = append(args, "--read-only")
	}
	args = append(args, clusterArgs(req.Identity.ClusterName, req.ConfPath, req.UserID, req.KeyringPath)...)
	args = append(args, req.Identity.Pool+"/"+req.Identity.Image)

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = m.opts.MapTimeoutDuration()
	}

	start := time.Now()
	stdout, stderr, err := m.run(ctx, timeout, args...)
	if err != nil {
		combined := strings.ToLower(stdout + " " + stderr + " " + err.Error())
		if containsAny(combined, exclusiveLockDeniedPatterns) {
			observeMap(start, resultLockDenied)
			return MappedDevice{}, fmt.Errorf("rbd: map %s: %w: %v",
				req.Identity, ErrExclusiveLockDenied, err)
		}
		observeMap(start, resultError)
		return MappedDevice{}, err
	}
	observeMap(start, resultSuccess)

	devicePath := strings.TrimSpace(stdout)
	if devicePath == "" {
		return MappedDevice{}, fmt.Errorf("rbd: map %s: succeeded but returned no device path",
			req.Identity)
	}

	id, err := deviceIDFromPath(devicePath)
	if err != nil {
		return MappedDevice{}, err
	}

	klog.V(4).Infof("Map: mapped %s as %s", req.Identity, devicePath)
	return MappedDevice{
		ID:          id,
		Pool:        req.Identity.Pool,
		Image:       req.Identity.Image,
		DevicePath:  devicePath,
		ClusterFSID: req.Identity.ClusterFSID,
	}, nil
}

// Unmap releases a device.
//
// Callers must have verified identity first: this function cannot tell whether
// the device belongs to this driver.
func (m *rbdCLIMapper) Unmap(ctx context.Context, devicePath string, timeout time.Duration) error {
	if _, err := deviceIDFromPath(devicePath); err != nil {
		return err
	}
	if timeout <= 0 {
		timeout = m.opts.UnmapTimeoutDuration()
	}

	// --conf is required even here: unmap needs no credentials but still reads
	// ceph.conf, and the host template would emit a parse error.
	args := []string{"device", "unmap"}
	args = append(args, clusterArgs("", m.clusterConfPath(), "", "")...)
	args = append(args, devicePath)

	start := time.Now()
	_, stderr, err := m.run(ctx, timeout, args...)
	if err != nil {
		combined := strings.ToLower(stderr + " " + err.Error())
		switch {
		case containsAny(combined, notMappedPatterns):
			// Already gone: unmap is idempotent.
			observeUnmap(start, resultSuccess)
			klog.V(4).Infof("Unmap: %s was not mapped", devicePath)
			return nil
		case containsAny(combined, deviceBusyPatterns):
			observeUnmap(start, resultError)
			return fmt.Errorf("rbd: unmap %s: %w: %v", devicePath, ErrDeviceBusy, err)
		default:
			observeUnmap(start, resultError)
			return err
		}
	}
	observeUnmap(start, resultSuccess)

	klog.V(4).Infof("Unmap: unmapped %s", devicePath)
	return nil
}

// clusterConfPath returns the cluster-wide generated ceph.conf.
//
// The reconciler writes this at startup so credential-free commands (list,
// unmap) never fall back to the broken host config, even when no per-volume
// config exists.
func (m *rbdCLIMapper) clusterConfPath() string {
	return clusterConfPath(m.opts)
}

// rbdDeviceListEntry mirrors one element of `rbd device list --format json`.
// Field names verified against the bundled client output.
type rbdDeviceListEntry struct {
	ID        string `json:"id"`
	Pool      string `json:"pool"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Snap      string `json:"snap"`
	Device    string `json:"device"`
}

// ListMapped returns every kernel RBD mapping on the host.
//
// This is the inventory of record. A missing staging record is never proof that
// no mapping exists, so reconciliation always starts here.
//
// The cluster FSID is filled in from sysfs, because `rbd device list` does not
// report it and FSID is the authoritative half of the identity.
func (m *rbdCLIMapper) ListMapped(ctx context.Context) ([]MappedDevice, error) {
	args := []string{"device", "list", "--format", "json"}
	args = append(args, clusterArgs("", m.clusterConfPath(), "", "")...)

	stdout, _, err := m.run(ctx, 30*time.Second, args...)
	if err != nil {
		return nil, err
	}

	trimmed := strings.TrimSpace(stdout)
	if trimmed == "" || trimmed == "null" {
		return nil, nil
	}

	var entries []rbdDeviceListEntry
	if err := json.Unmarshal([]byte(trimmed), &entries); err != nil {
		return nil, fmt.Errorf("rbd: parse device list: %w (output: %s)", err, truncate(trimmed, 300))
	}

	// sysfs is read once and indexed, rather than per device, so a long device
	// list does not turn into N directory walks.
	fsidByID := make(map[int]string)
	if devs, sysErr := listSysfsDevices(); sysErr == nil {
		for _, d := range devs {
			fsidByID[d.ID] = d.ClusterFSID
		}
	} else {
		klog.V(4).Infof("ListMapped: sysfs unavailable, cluster FSID will be empty: %v", sysErr)
	}

	out := make([]MappedDevice, 0, len(entries))
	for _, e := range entries {
		id, convErr := strconv.Atoi(e.ID)
		if convErr != nil {
			// Fall back to the device path, which is the same index.
			if id, convErr = deviceIDFromPath(e.Device); convErr != nil {
				klog.V(4).Infof("ListMapped: skipping entry with unparseable id %q", e.ID)
				continue
			}
		}
		snap := e.Snap
		if snap == "-" {
			snap = ""
		}
		out = append(out, MappedDevice{
			ID:          id,
			Pool:        e.Pool,
			Namespace:   e.Namespace,
			Image:       e.Name,
			Snap:        snap,
			DevicePath:  e.Device,
			ClusterFSID: fsidByID[id],
		})
	}
	return out, nil
}

// VerifyIdentity confirms that devicePath currently maps want.
//
// This is the gate in front of every reuse, publish and unmap. sysfs is the
// source of truth because it is the kernel's own view; `rbd device list` is
// cross-checked so a disagreement between the two is caught rather than
// averaged.
func (m *rbdCLIMapper) VerifyIdentity(ctx context.Context, devicePath string, want ImageIdentity) error {
	if !want.IsComplete() {
		// Refusing here is what prevents an unmap authorized by a partially
		// populated record.
		return fmt.Errorf("rbd: verify %s: %w: expected identity is incomplete (%s)",
			devicePath, ErrIdentityMismatch, want)
	}

	id, err := deviceIDFromPath(devicePath)
	if err != nil {
		return err
	}

	dev, err := readSysfsDevice(id)
	if err != nil {
		// An unreadable device cannot be verified, so it must not be trusted.
		return fmt.Errorf("rbd: verify %s: %w: %v", devicePath, ErrIdentityMismatch, err)
	}

	got := dev.Identity()
	if !got.Matches(want) {
		return fmt.Errorf("rbd: verify %s: %w: kernel reports %s, expected %s",
			devicePath, ErrIdentityMismatch, got, want)
	}

	// Cross-check the CLI inventory. A mismatch means the two kernel views
	// disagree, which is never safe to act on.
	mapped, listErr := m.ListMapped(ctx)
	if listErr != nil {
		klog.V(4).Infof("VerifyIdentity: device list unavailable for cross-check of %s: %v",
			devicePath, listErr)
		return nil
	}
	for _, d := range mapped {
		if d.ID != id {
			continue
		}
		if d.Pool != want.Pool || d.Image != want.Image {
			return fmt.Errorf("rbd: verify %s: %w: device list reports %s/%s, sysfs reports %s/%s",
				devicePath, ErrIdentityMismatch, d.Pool, d.Image, dev.Pool, dev.Image)
		}
		return nil
	}
	// Present in sysfs but absent from the CLI listing: treat as unverifiable.
	return fmt.Errorf("rbd: verify %s: %w: device is not in the kernel device list",
		devicePath, ErrIdentityMismatch)
}

// DeviceSize returns the block device size in bytes.
//
// sysfs reports size in 512-byte sectors, so blockdev is preferred and sysfs is
// the fallback. Getting the unit wrong would fail the size check in the identity
// gate for every volume.
func (m *rbdCLIMapper) DeviceSize(ctx context.Context, devicePath string) (int64, error) {
	cmd := m.exec.CommandContext(ctx, blockdevBinary, "--getsize64", devicePath)
	out, err := cmd.CombinedOutput()
	if err == nil {
		if n, convErr := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); convErr == nil {
			return n, nil
		}
	}

	id, idErr := deviceIDFromPath(devicePath)
	if idErr != nil {
		return 0, idErr
	}
	dev, sysErr := readSysfsDevice(id)
	if sysErr != nil {
		return 0, fmt.Errorf("rbd: size of %s: blockdev failed (%v) and sysfs failed: %w",
			devicePath, err, sysErr)
	}
	if dev.SizeBytes <= 0 {
		return 0, fmt.Errorf("rbd: size of %s: kernel reported %d bytes", devicePath, dev.SizeBytes)
	}
	return dev.SizeBytes, nil
}

// Flush pushes buffered writes to the cluster before unmap.
//
// Skipping this risks losing the tail of a migration copy, so a failure is
// returned rather than logged.
func (m *rbdCLIMapper) Flush(ctx context.Context, devicePath string) error {
	cmd := m.exec.CommandContext(ctx, blockdevBinary, "--flushbufs", devicePath)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("rbd: flush %s: %w (output: %s)", devicePath, err, truncate(string(out), 200))
	}
	klog.V(5).Infof("Flush: flushed %s", devicePath)
	return nil
}

// lockLineRe matches a locker row of `rbd status`, e.g.
//
//	client.56193456  auto 18446462598732840961  192.168.206.2:0/76629272
var lockLineRe = regexp.MustCompile(`^(client\.\d+)\s+(.+?)\s+(\S+)$`)

// LockHolders returns the current exclusive-lock holders for an image.
//
// An empty slice means unlocked. The output shape was captured on the validated
// platform: a "Watchers:" block, then "There is 1 exclusive lock on this
// image.", then a Locker table.
func (m *rbdCLIMapper) LockHolders(ctx context.Context, req MapRequest) ([]string, error) {
	if !req.Identity.IsComplete() {
		return nil, fmt.Errorf("rbd: lock holders: incomplete image identity %s", req.Identity)
	}

	args := []string{"status"}
	args = append(args, clusterArgs(req.Identity.ClusterName, req.ConfPath, req.UserID, req.KeyringPath)...)
	args = append(args, req.Identity.Pool+"/"+req.Identity.Image)

	stdout, _, err := m.run(ctx, 30*time.Second, args...)
	if err != nil {
		return nil, err
	}

	holders := make([]string, 0, 1)
	inLockerTable := false
	for _, line := range strings.Split(stdout, "\n") {
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "":
			continue
		case strings.HasPrefix(trimmed, "Locker"):
			inLockerTable = true
			continue
		case strings.HasPrefix(trimmed, "Watchers"):
			inLockerTable = false
			continue
		}
		if !inLockerTable {
			continue
		}
		if m := lockLineRe.FindStringSubmatch(trimmed); m != nil {
			holders = append(holders, fmt.Sprintf("%s@%s", m[1], m[3]))
		}
	}
	return holders, nil
}

// containsAny reports whether haystack contains any of the needles.
func containsAny(haystack string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(haystack, n) {
			return true
		}
	}
	return false
}

// truncate bounds untrusted command output before it reaches an error message.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// waitForDevice waits until devicePath exists.
//
// The kernel creates /dev/rbdN asynchronously via udev, so a map can return
// before the node is usable.
func waitForDevice(ctx context.Context, devicePath string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	deadline := time.Now().Add(timeout)

	for {
		if _, err := os.Stat(devicePath); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("rbd: device %s did not appear within %s", devicePath, timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
