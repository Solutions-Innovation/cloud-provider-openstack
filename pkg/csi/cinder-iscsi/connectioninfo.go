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
	"fmt"
	"strconv"
	"strings"

	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-iscsi/openstack"
	"k8s.io/klog/v2"
)

// ── Publish Context Keys ─────────────────────────────────────────────────────
// These keys are used in the PublishContext map returned by ControllerPublishVolume
// and consumed by NodeStageVolume.

const (
	PublishContextTargetPortal     = "target_portal"
	PublishContextTargetIQN        = "target_iqn"
	PublishContextTargetLUN        = "target_lun"
	PublishContextAuthMethod       = "auth_method"
	PublishContextAuthUsername     = "auth_username"
	PublishContextAuthPassword     = "auth_password"
	PublishContextDriverVolumeType = "driver_volume_type"
)

// ── Node Identity Parsing ────────────────────────────────────────────────────
// Node ID format: "hostname;iqn;ip"
// Example: "worker-3;iqn.1993-08.org.debian:01:abc123;10.0.0.103"

const nodeIDSeparator = ";"

// ParseNodeID extracts host, IQN, and IP from the composite node identity
// string used by this driver.
func ParseNodeID(nodeID string) (host, iqn, ip string, err error) {
	parts := strings.SplitN(nodeID, nodeIDSeparator, 3)
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("invalid node ID %q: expected format 'hostname;iqn;ip'", nodeID)
	}
	host = parts[0]
	iqn = parts[1]
	ip = parts[2]

	if host == "" || iqn == "" || ip == "" {
		return "", "", "", fmt.Errorf("invalid node ID %q: all parts (host, iqn, ip) must be non-empty", nodeID)
	}

	return host, iqn, ip, nil
}

// BuildPublishContext converts ISCSIConnectionInfo into a publish context map
// suitable for the ControllerPublishVolumeResponse.
func BuildPublishContext(connInfo *openstack.ISCSIConnectionInfo) map[string]string {
	ctx := map[string]string{
		PublishContextTargetPortal:     connInfo.TargetPortal,
		PublishContextTargetIQN:        connInfo.TargetIQN,
		PublishContextTargetLUN:        strconv.Itoa(connInfo.TargetLUN),
		PublishContextDriverVolumeType: connInfo.DriverVolumeType,
	}

	// Include CHAP credentials if present
	if connInfo.AuthMethod != "" {
		ctx[PublishContextAuthMethod] = connInfo.AuthMethod
	}
	if connInfo.AuthUsername != "" {
		ctx[PublishContextAuthUsername] = connInfo.AuthUsername
	}
	if connInfo.AuthPassword != "" {
		ctx[PublishContextAuthPassword] = connInfo.AuthPassword
	}

	klog.V(5).Infof("BuildPublishContext: portal=%s iqn=%s lun=%d",
		connInfo.TargetPortal, connInfo.TargetIQN, connInfo.TargetLUN)

	return ctx
}

// ValidateISCSIConnectionInfo checks that the essential iSCSI fields are
// present in the connection info returned by Cinder.
func ValidateISCSIConnectionInfo(connInfo *openstack.ISCSIConnectionInfo) error {
	if connInfo.DriverVolumeType != openstack.DriverVolumeTypeISCSI {
		return fmt.Errorf("unexpected driver_volume_type %q, expected %q",
			connInfo.DriverVolumeType, openstack.DriverVolumeTypeISCSI)
	}
	if connInfo.TargetPortal == "" {
		return fmt.Errorf("connection_info missing target_portal")
	}
	if connInfo.TargetIQN == "" {
		return fmt.Errorf("connection_info missing target_iqn")
	}
	return nil
}
