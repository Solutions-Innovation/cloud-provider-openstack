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
	"fmt"
	"net"
	"strconv"
	"strings"

	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-rbd/openstack"
)

// publish_context keys.
//
// publish_context travels through the Kubernetes VolumeAttachment object, which
// is readable by anyone with RBAC on that resource. It therefore carries
// identifiers only. No Ceph key, keyring, or Secret content ever appears here.
const (
	PublishContextDriverVolumeType = "driver_volume_type"
	PublishContextClusterName      = "cluster_name"
	PublishContextClusterFSID      = "cluster_fsid"
	PublishContextPool             = "pool"
	PublishContextImage            = "image"
	PublishContextMonitors         = "monitors"
	PublishContextAuthEnabled      = "auth_enabled"
	PublishContextAuthUsername     = "auth_username"
	PublishContextVolumeID         = "volume_id"
	PublishContextAccessMode       = "access_mode"
	// PublishContextAttachmentID is included for log correlation only. The
	// node must not treat it as authoritative for anything.
	PublishContextAttachmentID = "attachment_id"
)

// nodeIDSeparator separates the optional IP suffix in a node ID.
const nodeIDSeparator = ";"

// BuildNodeID renders a node ID.
//
// The emitted form is the bare hostname: the validated Cinder RBD backend
// accepts a connector containing only "host", so there is nothing else to
// carry. The two-field form exists solely for forward compatibility.
func BuildNodeID(host, ip string) string {
	if ip == "" {
		return host
	}
	return host + nodeIDSeparator + ip
}

// ParseNodeID parses a node ID produced by BuildNodeID.
//
// Both the one-field and two-field forms are accepted from the outset. If a
// future backend ever requires an IP, nodes already registered with the
// one-field ID keep working across the upgrade instead of failing to publish.
func ParseNodeID(nodeID string) (host, ip string, err error) {
	if nodeID == "" {
		return "", "", fmt.Errorf("node ID is empty")
	}
	parts := strings.Split(nodeID, nodeIDSeparator)
	switch len(parts) {
	case 1:
		if parts[0] == "" {
			return "", "", fmt.Errorf("invalid node ID %q: empty host", nodeID)
		}
		return parts[0], "", nil
	case 2:
		if parts[0] == "" || parts[1] == "" {
			return "", "", fmt.Errorf("invalid node ID %q: host and ip must both be non-empty", nodeID)
		}
		return parts[0], parts[1], nil
	default:
		return "", "", fmt.Errorf("invalid node ID %q: expected %q or %q",
			nodeID, "<host>", "<host>;<ip>")
	}
}

// BuildPublishContext renders the connection information for the node.
//
// Only identifiers are emitted. publish_context is stored in the Kubernetes
// VolumeAttachment object, which is readable by anyone with RBAC on that
// resource, so no Ceph key, keyring, or Secret content may ever appear here.
// The node obtains its credential out-of-band from a projected Secret.
func BuildPublishContext(ci *openstack.RBDConnectionInfo, attachmentID string) map[string]string {
	ctx := map[string]string{
		PublishContextDriverVolumeType: ci.DriverVolumeType,
		PublishContextClusterFSID:      ci.ClusterFSID,
		PublishContextPool:             ci.Pool,
		PublishContextImage:            ci.Image,
		PublishContextMonitors:         ci.MonitorList(),
		PublishContextAuthEnabled:      strconv.FormatBool(ci.AuthEnabled),
	}

	// Optional fields are omitted rather than emitted empty, so the node can
	// distinguish "absent" from "empty" when validating.
	if ci.ClusterName != "" {
		ctx[PublishContextClusterName] = ci.ClusterName
	}
	if ci.AuthUsername != "" {
		ctx[PublishContextAuthUsername] = ci.AuthUsername
	}
	if ci.VolumeID != "" {
		ctx[PublishContextVolumeID] = ci.VolumeID
	}
	if ci.AccessMode != "" {
		ctx[PublishContextAccessMode] = ci.AccessMode
	}
	if attachmentID != "" {
		ctx[PublishContextAttachmentID] = attachmentID
	}
	return ctx
}

// ParsePublishContext reconstructs the connection information on the node.
//
// The result is re-validated by the caller: the node never trusts a partially
// populated context, because a missing FSID or pool would disable an identity
// check rather than fail loudly.
func ParsePublishContext(pc map[string]string) (*openstack.RBDConnectionInfo, string, error) {
	if len(pc) == 0 {
		return nil, "", fmt.Errorf("publish context is empty")
	}

	ci := &openstack.RBDConnectionInfo{
		DriverVolumeType: pc[PublishContextDriverVolumeType],
		ClusterName:      pc[PublishContextClusterName],
		ClusterFSID:      pc[PublishContextClusterFSID],
		Pool:             pc[PublishContextPool],
		Image:            pc[PublishContextImage],
		AuthUsername:     pc[PublishContextAuthUsername],
		VolumeID:         pc[PublishContextVolumeID],
		AccessMode:       pc[PublishContextAccessMode],
	}

	if raw, ok := pc[PublishContextAuthEnabled]; ok && raw != "" {
		enabled, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, "", fmt.Errorf("publish context %s=%q is not a boolean: %w",
				PublishContextAuthEnabled, raw, err)
		}
		ci.AuthEnabled = enabled
	}

	monitors, err := parseMonitorList(pc[PublishContextMonitors])
	if err != nil {
		return nil, "", err
	}
	ci.Monitors = monitors

	return ci, pc[PublishContextAttachmentID], nil
}

// parseMonitorList splits a comma-separated host:port list, preserving
// bracketed IPv6 literals.
func parseMonitorList(list string) ([]openstack.MonAddr, error) {
	if strings.TrimSpace(list) == "" {
		return nil, fmt.Errorf("publish context %s is empty", PublishContextMonitors)
	}

	parts := strings.Split(list, ",")
	out := make([]openstack.MonAddr, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		host, port, err := net.SplitHostPort(p)
		if err != nil {
			// A bare address without a port is not usable for a monitor: fail
			// rather than guessing 6789, which could point at the wrong cluster.
			return nil, fmt.Errorf("publish context %s: monitor %q is not host:port: %w",
				PublishContextMonitors, p, err)
		}
		if host == "" || port == "" {
			return nil, fmt.Errorf("publish context %s: monitor %q has an empty host or port",
				PublishContextMonitors, p)
		}
		out = append(out, openstack.MonAddr{Host: host, Port: port})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("publish context %s yielded no monitors", PublishContextMonitors)
	}
	return out, nil
}

// ValidateRBDConnectionInfo enforces the invariants required before the node
// may act on connection information.
//
// The expected-cluster checks are applied only when configured, so a deployment
// that has not set them still works — but §7.4 identity check 1 then has
// nothing to compare against, which is why the chart refuses to render an empty
// expected-fsid.
func ValidateRBDConnectionInfo(ci *openstack.RBDConnectionInfo, opts openstack.RBDOpts) error {
	if ci == nil {
		return fmt.Errorf("connection info is nil")
	}
	if ci.DriverVolumeType != openstack.DriverVolumeTypeRBD {
		return fmt.Errorf("driver_volume_type is %q, want %q",
			ci.DriverVolumeType, openstack.DriverVolumeTypeRBD)
	}
	if ci.ClusterFSID == "" {
		return fmt.Errorf("cluster FSID is empty")
	}
	if ci.Pool == "" || ci.Image == "" {
		return fmt.Errorf("pool or image is empty (pool=%q image=%q)", ci.Pool, ci.Image)
	}
	if len(ci.Monitors) == 0 {
		return fmt.Errorf("no Ceph monitors present")
	}
	for i, m := range ci.Monitors {
		if m.Host == "" || m.Port == "" {
			return fmt.Errorf("monitor %d has an empty host or port", i)
		}
	}
	if ci.AuthEnabled && ci.AuthUsername == "" {
		return fmt.Errorf("auth is enabled but auth_username is empty")
	}
	if opts.ExpectedFSID != "" && ci.ClusterFSID != opts.ExpectedFSID {
		return fmt.Errorf("cluster FSID %q does not match configured expected-fsid %q",
			ci.ClusterFSID, opts.ExpectedFSID)
	}
	// cluster_name is optional in the payload; compare only when both sides
	// have a value, since a missing alias is not a mismatch.
	if opts.ExpectedClusterName != "" && ci.ClusterName != "" &&
		ci.ClusterName != opts.ExpectedClusterName {
		return fmt.Errorf("cluster name %q does not match configured expected-cluster-name %q",
			ci.ClusterName, opts.ExpectedClusterName)
	}
	return nil
}
