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
	"context"
	"fmt"
	"net/http"
	"os"

	"github.com/gophercloud/gophercloud/v2"
	gos "github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/snapshots"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/spf13/pflag"
	gcfg "gopkg.in/gcfg.v1"

	"k8s.io/cloud-provider-openstack/pkg/client"
	"k8s.io/cloud-provider-openstack/pkg/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"
)

// userAgentData is used to add extra information to the gophercloud user-agent
var userAgentData []string

// AddExtraFlags is called by the main package to add component specific command line flags
func AddExtraFlags(fs *pflag.FlagSet) {
	fs.StringArrayVar(&userAgentData, "user-agent", nil, "Extra data to add to gophercloud user-agent. Use multiple times to add more than one component.")
}

// ── IOpenStackISCSI Interface ────────────────────────────────────────────────

// IOpenStackISCSI defines the OpenStack operations required by the iSCSI-Cinder
// CSI driver. Key difference from the NFS driver: no Nova methods — all
// operations use the Cinder v3 attachment API.
type IOpenStackISCSI interface {
	// ── Volume Operations (Cinder) ──────────────────────────────────────
	CreateVolume(ctx context.Context, opts *volumes.CreateOpts,
		schedulerHints volumes.SchedulerHintOptsBuilder) (*volumes.Volume, error)
	DeleteVolume(ctx context.Context, volumeID string) error
	GetVolume(ctx context.Context, volumeID string) (*volumes.Volume, error)
	GetVolumesByName(ctx context.Context, name string) ([]volumes.Volume, error)
	ExpandVolume(ctx context.Context, volumeID string, status string, newSize int) error
	WaitVolumeTargetStatus(ctx context.Context, volumeID string, tStatus []string) error
	SetVolumeMetadata(ctx context.Context, volumeID string, metadata map[string]string) error
	DeleteVolumeMetadata(ctx context.Context, volumeID string, keys []string) error

	// ── Cinder v3 Attachment Operations (REPLACES Shadow VM) ────────────
	CreateAttachment(ctx context.Context, volumeID string) (string, error)
	UpdateAttachmentConnector(ctx context.Context, attachmentID string,
		connector *AttachmentConnector) (*ISCSIConnectionInfo, error)
	CompleteAttachment(ctx context.Context, attachmentID string) error
	GetAttachment(ctx context.Context, attachmentID string) (*Attachment, error)
	DeleteAttachment(ctx context.Context, attachmentID string) error

	// ── Snapshot Operations (Cinder) ────────────────────────────────────
	CreateSnapshot(ctx context.Context, name, volID string, tags map[string]string) (*snapshots.Snapshot, error)
	DeleteSnapshot(ctx context.Context, snapID string) error
	GetSnapshotByID(ctx context.Context, snapshotID string) (*snapshots.Snapshot, error)

	// ── Discovery & Configuration ───────────────────────────────────────
	DiscoverCinderCapabilities(ctx context.Context) (*CinderCapabilities, error)
	GetISCSIOpts() ISCSIOpts
	GetVolumeOpts() VolumeOpts
	GetCinderCapabilities() *CinderCapabilities
}

// ── iSCSI Data Types ─────────────────────────────────────────────────────────

// ISCSIConnectionInfo from Cinder v3 attachment connection_info
type ISCSIConnectionInfo struct {
	DriverVolumeType string `json:"driver_volume_type"` // "iscsi"
	TargetPortal     string `json:"target_portal"`      // "69.167.149.97:3260"
	TargetIQN        string `json:"target_iqn"`         // "iqn.2010-10.org.openstack:volume-xxx"
	TargetLUN        int    `json:"target_lun"`         // 0
	AuthMethod       string `json:"auth_method"`        // "CHAP" or ""
	AuthUsername     string `json:"auth_username"`      // CHAP username
	AuthPassword     string `json:"auth_password"`      // CHAP password
	VolumeID         string `json:"volume_id"`          // Cinder volume UUID
	Encrypted        bool   `json:"encrypted"`          // false
	TargetDiscovered bool   `json:"target_discovered"`  // false
	AccessMode       string `json:"access_mode"`        // "rw"
	AttachmentID     string `json:"attachment_id"`      // Cinder attachment UUID

	// Multipath fields (Pure Storage — arrays of targets)
	TargetPortals    []string `json:"target_portals,omitempty"`
	TargetIQNs       []string `json:"target_iqns,omitempty"`
	TargetLUNs       []int    `json:"target_luns,omitempty"`
	Discard          bool     `json:"discard,omitempty"`
	EnforceMultipath bool     `json:"enforce_multipath"`
}

// AttachmentConnector sent to Cinder PUT /v3/attachments/{id}
type AttachmentConnector struct {
	Initiator string `json:"initiator"` // IQN from /etc/iscsi/initiatorname.iscsi
	IP        string `json:"ip"`        // Storage network IP of WRCP host
	Host      string `json:"host"`      // WRCP hostname
	Multipath bool   `json:"multipath"` // false (single-path initially)
	Platform  string `json:"platform"`  // "x86_64"
	OSType    string `json:"os_type"`   // "linux2"
}

// Attachment wraps Cinder v3 attachment response
type Attachment struct {
	ID             string               `json:"id"`
	VolumeID       string               `json:"volume_id"`
	Status         string               `json:"status"`
	Instance       *string              `json:"instance"`
	ConnectionInfo *ISCSIConnectionInfo `json:"connection_info"`
}

// CinderCapabilities caches Cinder API version discovery results
type CinderCapabilities struct {
	MaxMicroversion string // e.g. "3.70"
	SupportsV327    bool   // Self-service attachments
	SupportsV344    bool   // os-complete action
}

// ── iSCSI Config Structs ─────────────────────────────────────────────────────

// ISCSICinderConfig for the iSCSI-Cinder CSI driver.
// Global comes from cloud.conf (Secret); ISCSI and Volume from driver.conf (ConfigMap).
type ISCSICinderConfig struct {
	Global map[string]*client.AuthOpts // from cloud.conf
	ISCSI  ISCSIOpts                   // from driver.conf [ISCSI]
	Volume VolumeOpts                  // from driver.conf [Volume]
}

// ISCSIOpts controls iSCSI initiator behavior in NodeStageVolume/NodeUnstageVolume.
type ISCSIOpts struct {
	EnableMultipath   bool   `gcfg:"enable-multipath"`    // Default: false
	CHAPAuthEnabled   bool   `gcfg:"chap-auth-enabled"`   // Default: true
	LoginTimeout      int    `gcfg:"login-timeout"`       // Default: 30 (seconds)
	DeviceWaitTimeout int    `gcfg:"device-wait-timeout"` // Default: 30 (seconds)
	ISCSIInterface    string `gcfg:"iscsi-interface"`     // Default: "default"
	StorageInterface  string `gcfg:"storage-interface"`   // Default: "" (primary IP)
}

// DeleteVolumeMode constants control the driver-level default for DeleteVolume.
const (
	// DeleteVolumeModeRetain leaves the Cinder volume in "available" state
	// after PVC deletion so Blueprint can create the target VM. This is the
	// default for migration workloads.
	DeleteVolumeModeRetain = "retain"

	// DeleteVolumeModeDelete deletes the Cinder volume entirely on PVC
	// deletion, like a standard CSI driver.
	DeleteVolumeModeDelete = "delete"
)

// VolumeOpts controls Cinder volume lifecycle.
type VolumeOpts struct {
	CreateTimeout     int    `gcfg:"create-timeout"`      // Default: 300 (seconds)
	DetachTimeout     int    `gcfg:"detach-timeout"`      // Default: 120 (seconds)
	DefaultVolumeType string `gcfg:"default-volume-type"` // Optional
	MetadataPrefix    string `gcfg:"metadata-prefix"`     // Default: "csi"
	DeleteVolumeMode  string `gcfg:"delete-volume-mode"`  // Default: "retain"
}

// ── OpenStackISCSI Provider ──────────────────────────────────────────────────

// ── Cinder Microversion Constants ────────────────────────────────────────────
// Each constant documents *why* the microversion is required so the minimum
// API contract is visible in one place.
const (
	// MvSelfServiceAttach is the minimum Cinder microversion for the
	// self-service (no-Nova) v3 attachment API (create/update/delete).
	MvSelfServiceAttach = "3.27"

	// MvServerSideNameFilter enables server-side volume name filtering
	// in GET /v3/volumes?name=...
	MvServerSideNameFilter = "3.34"

	// MvOnlineResize enables os-extend on in-use volumes.
	MvOnlineResize = "3.42"

	// MvAttachComplete enables the POST os-complete action that
	// transitions an attachment from "attaching" → "attached".
	MvAttachComplete = "3.44"
)

// OpenStackISCSI is the concrete implementation of IOpenStackISCSI.
// Phase 1: struct + provider init only. Method bodies in Phase 2+.
type OpenStackISCSI struct {
	blockstorage *gophercloud.ServiceClient
	epOpts       gophercloud.EndpointOpts
	iscsiOpts    ISCSIOpts
	volumeOpts   VolumeOpts
	cinderCaps   *CinderCapabilities
}

// blockStorageClient returns a thread-safe copy of the block-storage
// ServiceClient with the given microversion pinned. Each Gophercloud
// ServiceClient is mutable (Microversion is a plain string field), so
// concurrent RPCs that need different microversions must each get their
// own copy via NewBlockStorageV3.
func (os *OpenStackISCSI) blockStorageClient(microversion string) (*gophercloud.ServiceClient, error) {
	c, err := gos.NewBlockStorageV3(os.blockstorage.ProviderClient, os.epOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to create blockstorage client: %w", err)
	}
	c.Microversion = microversion
	return c, nil
}

// ── Provider Initialization ──────────────────────────────────────────────────

var (
	osInstance  IOpenStackISCSI
	configFiles = []string{"/etc/cloud.conf"}
)

// InitOpenStackProvider initializes the global config file paths and metrics.
func InitOpenStackProvider(cfgFiles []string, httpEndpoint string) {
	metrics.RegisterMetrics("cinder-iscsi-csi")
	if httpEndpoint != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", legacyregistry.HandlerWithReset())
		go func() {
			err := http.ListenAndServe(httpEndpoint, mux)
			if err != nil {
				klog.Fatalf("failed to listen & serve metrics from %q: %v", httpEndpoint, err)
			}
			klog.Infof("metrics available in %q", httpEndpoint)
		}()
	}
	configFiles = cfgFiles
	klog.V(2).Infof("InitOpenStackProvider configFiles: %s", configFiles)
}

// GetConfigFromFiles retrieves the iSCSI-Cinder CSI config from files.
func GetConfigFromFiles(configFilePaths []string) (ISCSICinderConfig, error) {
	var cfg ISCSICinderConfig
	for _, configFilePath := range configFilePaths {
		config, err := os.Open(configFilePath)
		if err != nil {
			klog.Errorf("Failed to open configuration file: %v", err)
			return cfg, err
		}
		defer config.Close()

		err = gcfg.FatalOnly(gcfg.ReadInto(&cfg, config))
		if err != nil {
			klog.Errorf("Failed to read configuration file: %v", err)
			return cfg, err
		}
	}

	for _, global := range cfg.Global {
		if global.UseClouds {
			if global.CloudsFile != "" {
				os.Setenv("OS_CLIENT_CONFIG_FILE", global.CloudsFile)
			}
			err := client.ReadClouds(global)
			if err != nil {
				return cfg, err
			}
			klog.V(5).Infof("Credentials are loaded from %s:", global.CloudsFile)
		}
	}

	return cfg, nil
}

// CreateOpenStackProvider creates an OpenStackISCSI instance for the given cloud name.
// Unlike the existing Cinder CSI driver, this does NOT create a Nova client —
// the iSCSI driver only needs the Cinder block-storage service client.
func CreateOpenStackProvider(cloudName string) (IOpenStackISCSI, error) {
	cfg, err := GetConfigFromFiles(configFiles)
	if err != nil {
		klog.Errorf("GetConfigFromFiles %s failed with error: %v", configFiles, err)
		return nil, err
	}

	global := cfg.Global[cloudName]
	if global == nil {
		return nil, fmt.Errorf("cloud name %q not found in configuration files: %s", cloudName, configFiles)
	}

	provider, err := client.NewOpenStackClient(global, "cinder-iscsi-csi-plugin", userAgentData...)
	if err != nil {
		return nil, err
	}

	epOpts := gophercloud.EndpointOpts{
		Region:       global.Region,
		Availability: global.EndpointType,
	}

	// Init Cinder ServiceClient only — no Nova dependency
	blockstorageclient, err := gos.NewBlockStorageV3(provider, epOpts)
	if err != nil {
		return nil, err
	}

	instance := &OpenStackISCSI{
		blockstorage: blockstorageclient,
		epOpts:       epOpts,
		iscsiOpts:    cfg.ISCSI,
		volumeOpts:   cfg.Volume,
	}

	// Probe Cinder microversions at startup. If the minimum required
	// microversion (3.27 — self-service attachments) is not supported the
	// driver cannot function, so we surface the error immediately.
	// The cached CinderCapabilities are later read by Probe() to report
	// readiness to the livenessprobe sidecar.
	ctx := context.Background()
	caps, err := instance.DiscoverCinderCapabilities(ctx)
	if err != nil {
		return nil, fmt.Errorf("Cinder microversion probe failed: %w", err)
	}
	klog.Infof("Cinder capabilities: v3.27=%v v3.44=%v", caps.SupportsV327, caps.SupportsV344)

	osInstance = instance
	return instance, nil
}

// GetOpenStackProvider returns the cached provider or creates a new one.
func GetOpenStackProvider(cloudName string) (IOpenStackISCSI, error) {
	if osInstance != nil {
		return osInstance, nil
	}
	return CreateOpenStackProvider(cloudName)
}

// ── Volume and Attachment Operations ──────────────────────────────────────────
// Implemented in openstack_volumes.go and openstack_attachments.go

// ── Snapshot Operation Stubs (Phase 2) ───────────────────────────────────────

func (os *OpenStackISCSI) CreateSnapshot(ctx context.Context, name, volID string, tags map[string]string) (*snapshots.Snapshot, error) {
	return nil, fmt.Errorf("CreateSnapshot not implemented (Phase 2)")
}

func (os *OpenStackISCSI) DeleteSnapshot(ctx context.Context, snapID string) error {
	return fmt.Errorf("DeleteSnapshot not implemented (Phase 2)")
}

func (os *OpenStackISCSI) GetSnapshotByID(ctx context.Context, snapshotID string) (*snapshots.Snapshot, error) {
	return nil, fmt.Errorf("GetSnapshotByID not implemented (Phase 2)")
}

// ── Config Accessors ─────────────────────────────────────────────────────────

// GetISCSIOpts returns the iSCSI initiator options.
func (os *OpenStackISCSI) GetISCSIOpts() ISCSIOpts {
	return os.iscsiOpts
}

// GetVolumeOpts returns the volume lifecycle options.
func (os *OpenStackISCSI) GetVolumeOpts() VolumeOpts {
	return os.volumeOpts
}

// GetCinderCapabilities returns the cached Cinder API version capabilities.
func (os *OpenStackISCSI) GetCinderCapabilities() *CinderCapabilities {
	return os.cinderCaps
}
