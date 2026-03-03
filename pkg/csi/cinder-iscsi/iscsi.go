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
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"k8s.io/klog/v2"
	"k8s.io/utils/exec"
)

// ── ISCSIInitiator Interface ─────────────────────────────────────────────────

// ISCSIInitiator abstracts iscsiadm commands for unit testing.
// The concrete implementation (iscsiadmInitiator) shells out to iscsiadm;
// tests inject a mock via testify/mock.
type ISCSIInitiator interface {
	// Discovery runs iSCSI SendTargets discovery against the portal.
	//   iscsiadm -m discovery -t sendtargets -p <portal>
	Discovery(ctx context.Context, portal string) error

	// SetCHAPAuth configures CHAP credentials on an iscsiadm node entry.
	//   iscsiadm -m node -T <iqn> -p <portal> --op update -n node.session.auth.authmethod -v CHAP
	//   iscsiadm -m node -T <iqn> -p <portal> --op update -n node.session.auth.username  -v <username>
	//   iscsiadm -m node -T <iqn> -p <portal> --op update -n node.session.auth.password  -v <password>
	SetCHAPAuth(ctx context.Context, iqn, portal, username, password string) error

	// Login performs an iSCSI login to the target.
	//   iscsiadm -m node -T <iqn> -p <portal> --login
	Login(ctx context.Context, iqn, portal string) error

	// Logout performs an iSCSI logout from the target.
	//   iscsiadm -m node -T <iqn> -p <portal> --logout
	Logout(ctx context.Context, iqn, portal string) error

	// DeleteNode removes the iscsiadm node DB entry.
	//   iscsiadm -m node -T <iqn> -p <portal> --op delete
	DeleteNode(ctx context.Context, iqn, portal string) error

	// IsSessionActive checks whether an iSCSI session is active for the
	// given IQN and portal by inspecting `iscsiadm -m session` output.
	IsSessionActive(ctx context.Context, iqn, portal string) (bool, error)

	// CheckIscsiadm verifies that iscsiadm is available (for Probe health check).
	//   iscsiadm --version
	CheckIscsiadm(ctx context.Context) error
}

// ── iscsiadmInitiator — concrete implementation ──────────────────────────────

// iscsiadmInitiator implements ISCSIInitiator by shelling out to iscsiadm
// via k8s.io/utils/exec.Interface.
type iscsiadmInitiator struct {
	exec         exec.Interface
	loginTimeout int // seconds; 0 means use iscsiadm default
}

// NewISCSIInitiator creates a concrete ISCSIInitiator backed by iscsiadm.
func NewISCSIInitiator(loginTimeout int) ISCSIInitiator {
	return &iscsiadmInitiator{
		exec:         exec.New(),
		loginTimeout: loginTimeout,
	}
}

func (i *iscsiadmInitiator) Discovery(ctx context.Context, portal string) error {
	args := []string{"-m", "discovery", "-t", "sendtargets", "-p", portal}
	klog.V(4).Infof("iscsiadm discovery: portal=%s", portal)
	out, err := i.exec.CommandContext(ctx, "iscsiadm", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iscsiadm discovery failed (portal=%s): %w, output: %s", portal, err, string(out))
	}
	klog.V(5).Infof("iscsiadm discovery output: %s", string(out))
	return nil
}

func (i *iscsiadmInitiator) SetCHAPAuth(ctx context.Context, iqn, portal, username, password string) error {
	updates := []struct {
		name, value string
	}{
		{"node.session.auth.authmethod", "CHAP"},
		{"node.session.auth.username", username},
		{"node.session.auth.password", password},
	}
	for _, u := range updates {
		args := []string{"-m", "node", "-T", iqn, "-p", portal, "--op", "update", "-n", u.name, "-v", u.value}
		out, err := i.exec.CommandContext(ctx, "iscsiadm", args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("iscsiadm set %s failed (iqn=%s portal=%s): %w, output: %s",
				u.name, iqn, portal, err, string(out))
		}
	}
	klog.V(4).Infof("iscsiadm CHAP auth set: iqn=%s portal=%s", iqn, portal)
	return nil
}

func (i *iscsiadmInitiator) Login(ctx context.Context, iqn, portal string) error {
	// iscsiadm node mode: --login and --op update are mutually exclusive
	// option groups (see iscsiadm(8) SYNOPSIS). The timeout must be set in a
	// separate invocation before login.
	if i.loginTimeout > 0 {
		updateArgs := []string{"-m", "node", "-T", iqn, "-p", portal,
			"--op", "update", "-n", "node.conn[0].timeo.login_timeout",
			"-v", strconv.Itoa(i.loginTimeout)}
		if out, err := i.exec.CommandContext(ctx, "iscsiadm", updateArgs...).CombinedOutput(); err != nil {
			return fmt.Errorf("iscsiadm set login_timeout failed (iqn=%s portal=%s): %w, output: %s",
				iqn, portal, err, string(out))
		}
	}

	args := []string{"-m", "node", "-T", iqn, "-p", portal, "--login"}
	klog.V(4).Infof("iscsiadm login: iqn=%s portal=%s", iqn, portal)
	out, err := i.exec.CommandContext(ctx, "iscsiadm", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("iscsiadm login failed (iqn=%s portal=%s): %w, output: %s",
			iqn, portal, err, string(out))
	}
	return nil
}

func (i *iscsiadmInitiator) Logout(ctx context.Context, iqn, portal string) error {
	args := []string{"-m", "node", "-T", iqn, "-p", portal, "--logout"}
	klog.V(4).Infof("iscsiadm logout: iqn=%s portal=%s", iqn, portal)
	out, err := i.exec.CommandContext(ctx, "iscsiadm", args...).CombinedOutput()
	if err != nil {
		// "session not found" is expected during idempotent logout
		if strings.Contains(string(out), "No matching sessions") ||
			strings.Contains(string(out), "session not found") {
			klog.V(3).Infof("iscsiadm logout: no active session for iqn=%s portal=%s (idempotent)", iqn, portal)
			return nil
		}
		return fmt.Errorf("iscsiadm logout failed (iqn=%s portal=%s): %w, output: %s",
			iqn, portal, err, string(out))
	}
	return nil
}

func (i *iscsiadmInitiator) DeleteNode(ctx context.Context, iqn, portal string) error {
	args := []string{"-m", "node", "-T", iqn, "-p", portal, "--op", "delete"}
	klog.V(4).Infof("iscsiadm delete node: iqn=%s portal=%s", iqn, portal)
	out, err := i.exec.CommandContext(ctx, "iscsiadm", args...).CombinedOutput()
	if err != nil {
		// "No records found" is expected during idempotent cleanup
		if strings.Contains(string(out), "No records found") {
			klog.V(3).Infof("iscsiadm delete node: no records for iqn=%s portal=%s (idempotent)", iqn, portal)
			return nil
		}
		return fmt.Errorf("iscsiadm delete node failed (iqn=%s portal=%s): %w, output: %s",
			iqn, portal, err, string(out))
	}
	return nil
}

func (i *iscsiadmInitiator) IsSessionActive(ctx context.Context, iqn, portal string) (bool, error) {
	args := []string{"-m", "session"}
	out, err := i.exec.CommandContext(ctx, "iscsiadm", args...).CombinedOutput()
	if err != nil {
		// Exit code 21 means "no active sessions" — not an error
		if strings.Contains(string(out), "No active sessions") {
			return false, nil
		}
		return false, fmt.Errorf("iscsiadm session query failed: %w, output: %s", err, string(out))
	}
	// Output format: "tcp: [N] portal:port,tpgt iqn.xxx (non-flash)"
	// Parse fields to avoid substring false-positives (e.g. 10.0.0.1:3260
	// matching inside 10.0.0.10:3260 or 110.0.0.1:3260).
	lines := strings.Split(string(out), "\n")
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) >= 4 {
			// fields[2] = "portal:port,tpgt", fields[3] = IQN
			sessionPortal := strings.Split(fields[2], ",")[0] // strip ",tpgt"
			if sessionPortal == portal && fields[3] == iqn {
				return true, nil
			}
		}
	}
	return false, nil
}

func (i *iscsiadmInitiator) CheckIscsiadm(ctx context.Context) error {
	out, err := i.exec.CommandContext(ctx, "iscsiadm", "--version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("iscsiadm --version failed: %w, output: %s", err, string(out))
	}
	klog.V(5).Infof("iscsiadm version: %s", strings.TrimSpace(string(out)))
	return nil
}

// ── Device Path Helpers ──────────────────────────────────────────────────────

// devicePathPrefix is the Linux by-path directory where iSCSI device symlinks appear.
// This is a var (not const) so tests can override it with t.TempDir() to avoid
// writing to the real /dev/disk/by-path/ directory on the host.
var devicePathPrefix = "/dev/disk/by-path"

// BuildDevicePath constructs the expected /dev/disk/by-path/ symlink for an
// iSCSI target. The format is defined by the Linux kernel iSCSI transport:
//
//	ip-<portal>-iscsi-<iqn>-lun-<lun>
func BuildDevicePath(portal, iqn string, lun int) string {
	return fmt.Sprintf("%s/ip-%s-iscsi-%s-lun-%d", devicePathPrefix, portal, iqn, lun)
}

// devicePathRe matches /dev/disk/by-path/ip-<portal>-iscsi-<iqn>-lun-<lun>.
// NOTE: Both capture groups use greedy .+ matching. If an IQN ever contained
// the literal substring "-iscsi-", the first group would greedily consume part
// of the IQN. This is safe because real IQNs (RFC 3720) use the format
// "iqn.yyyy-mm.reverse-domain:identifier" and never contain "-iscsi-".
// This regex uses a hardcoded prefix (not the var) because ParseDevicePath
// operates on stored strings, not the filesystem.
var devicePathRe = regexp.MustCompile(
	`^/dev/disk/by-path/ip-(.+)-iscsi-(.+)-lun-(\d+)$`,
)

// ParseDevicePath is the inverse of BuildDevicePath. It extracts the portal,
// IQN, and LUN from a /dev/disk/by-path/ device path string. Used by
// NodeUnstageVolume to recover the IQN+portal for iscsiadm logout.
func ParseDevicePath(devicePath string) (portal, iqn string, lun int, err error) {
	m := devicePathRe.FindStringSubmatch(devicePath)
	if m == nil {
		return "", "", 0, fmt.Errorf("cannot parse iSCSI device path %q", devicePath)
	}
	lun, err = strconv.Atoi(m[3])
	if err != nil {
		return "", "", 0, fmt.Errorf("invalid LUN in device path %q: %w", devicePath, err)
	}
	return m[1], m[2], lun, nil
}

// WaitForDevice polls until devicePath exists on disk or the timeout is
// reached. The timeout is specified in seconds; if <=0, defaults to 30s.
func WaitForDevice(ctx context.Context, devicePath string, timeoutSeconds int) error {
	// Immediate check — avoid unnecessary 1-second delay when device already exists.
	if _, err := os.Stat(devicePath); err == nil {
		klog.V(4).Infof("WaitForDevice: %s already exists", devicePath)
		return nil
	}

	if timeoutSeconds <= 0 {
		timeoutSeconds = defaultTimeoutSeconds
	}
	deadline := time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := os.Stat(devicePath); err == nil {
				klog.V(4).Infof("WaitForDevice: %s appeared", devicePath)
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("timed out waiting for device %s after %ds", devicePath, timeoutSeconds)
			}
		}
	}
}

// ── Initiator IQN Reader ─────────────────────────────────────────────────────

// defaultInitiatorNamePath is the standard location of the iSCSI initiator name file.
const defaultInitiatorNamePath = "/etc/iscsi/initiatorname.iscsi"

// ReadInitiatorIQN reads the initiator IQN from the given path (typically
// /etc/iscsi/initiatorname.iscsi). The file format is:
//
//	## Generated by /sbin/iscsi-iname or open-iscsi
//	InitiatorName=iqn.1993-08.org.debian:01:aabbccdd
func ReadInitiatorIQN(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("failed to open initiator name file %s: %w", path, err)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		if strings.HasPrefix(line, "InitiatorName=") {
			iqn := strings.TrimPrefix(line, "InitiatorName=")
			iqn = strings.TrimSpace(iqn)
			if iqn == "" {
				return "", fmt.Errorf("empty InitiatorName in %s", path)
			}
			return iqn, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("error reading %s: %w", path, err)
	}
	return "", fmt.Errorf("InitiatorName not found in %s", path)
}

// ── Network IP Helper ────────────────────────────────────────────────────────

// GetInterfaceIP returns the first non-loopback IPv4 address of the named
// network interface. If ifaceName is empty, it returns the first non-loopback
// IPv4 address found on any interface (the "primary" IP).
func GetInterfaceIP(ifaceName string) (string, error) {
	if ifaceName != "" {
		iface, err := net.InterfaceByName(ifaceName)
		if err != nil {
			return "", fmt.Errorf("interface %q not found: %w", ifaceName, err)
		}
		addrs, err := iface.Addrs()
		if err != nil {
			return "", fmt.Errorf("failed to get addresses for interface %q: %w", ifaceName, err)
		}
		for _, addr := range addrs {
			ip := extractIPv4(addr)
			if ip != "" {
				return ip, nil
			}
		}
		return "", fmt.Errorf("no IPv4 address found on interface %q", ifaceName)
	}

	// No specific interface — return the first non-loopback IPv4 address
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return "", fmt.Errorf("failed to get interface addresses: %w", err)
	}
	for _, addr := range addrs {
		ip := extractIPv4(addr)
		if ip != "" {
			return ip, nil
		}
	}
	return "", fmt.Errorf("no non-loopback IPv4 address found")
}

// extractIPv4 returns the IPv4 string from a net.Addr if it is a non-loopback
// IPv4 address, or "" otherwise.
func extractIPv4(addr net.Addr) string {
	var ip net.IP
	switch v := addr.(type) {
	case *net.IPNet:
		ip = v.IP
	case *net.IPAddr:
		ip = v.IP
	}
	if ip == nil || ip.IsLoopback() {
		return ""
	}
	ip = ip.To4()
	if ip == nil {
		return "" // IPv6
	}
	return ip.String()
}
