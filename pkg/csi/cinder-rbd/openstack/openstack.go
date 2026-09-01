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

// Package openstack provides the Cinder API layer for the Cinder RBD CSI
// driver. It deliberately creates only a Cinder (block-storage v3) service
// client: the driver has no Nova dependency and reserves volumes with
// self-service attachment records (microversion >= 3.27).
//
// Design reference:
//
//	docs/cinder-csi-plugin/migration/rbd-cinder-csi-implementation-design.md
package openstack

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gophercloud/gophercloud/v2"
	gos "github.com/gophercloud/gophercloud/v2/openstack"
	"github.com/gophercloud/gophercloud/v2/openstack/blockstorage/v3/volumes"
	"github.com/spf13/pflag"
	gcfg "gopkg.in/gcfg.v1"
	"k8s.io/cloud-provider-openstack/pkg/client"
	"k8s.io/cloud-provider-openstack/pkg/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
	"k8s.io/klog/v2"
)

// IOpenStackRBD is the Cinder interface used by the RBD CSI driver.
//
// It mirrors IOpenStackISCSI with three deliberate differences (design §6.1):
//   - UpdateAttachmentConnector returns *RBDConnectionInfo.
//   - ExpandVolume and the snapshot methods are absent: expansion, snapshots
//     and clones are non-goals and are not advertised as CSI capabilities.
//   - ListAttachmentsByVolume is added. Two recovery boundaries in the design
//     (reservation created but metadata write failed; connector update
//     succeeded but the RPC response was lost) cannot be resolved without it.
type IOpenStackRBD interface {
	// ── Volume operations (Cinder) ──
	CreateVolume(ctx context.Context, opts *volumes.CreateOpts,
		schedulerHints volumes.SchedulerHintOptsBuilder) (*volumes.Volume, error)
	DeleteVolume(ctx context.Context, volumeID string) error
	GetVolume(ctx context.Context, volumeID string) (*volumes.Volume, error)
	GetVolumesByName(ctx context.Context, name string) ([]volumes.Volume, error)
	WaitVolumeTargetStatus(ctx context.Context, volumeID string, tStatus []string, timeoutSeconds int) error
	SetVolumeMetadata(ctx context.Context, volumeID string, metadata map[string]string) error
	DeleteVolumeMetadata(ctx context.Context, volumeID string, keys []string) error

	// ── Cinder v3 self-service attachment records (no Nova) ──
	CreateAttachment(ctx context.Context, volumeID string) (string, error)
	UpdateAttachmentConnector(ctx context.Context, attachmentID string,
		connector *AttachmentConnector) (*RBDConnectionInfo, error)
	CompleteAttachment(ctx context.Context, attachmentID string) error
	GetAttachment(ctx context.Context, attachmentID string) (*Attachment, error)
	ListAttachmentsByVolume(ctx context.Context, volumeID string) ([]Attachment, error)
	DeleteAttachment(ctx context.Context, attachmentID string) error

	// ── Discovery & configuration ──
	DiscoverCinderCapabilities(ctx context.Context) (*CinderCapabilities, error)
	GetRBDOpts() RBDOpts
	GetVolumeOpts() VolumeOpts
	GetCinderCapabilities() *CinderCapabilities
}

// ── Constants ────────────────────────────────────────────────────────────────

const (
	// DriverVolumeTypeRBD is the only connection type this driver accepts.
	DriverVolumeTypeRBD = "rbd"

	// MounterKRBD is the only supported node mapping method.
	MounterKRBD = "krbd"

	// DefaultCephUser is the identity Cinder returns as auth_username on the
	// validated WRCP 24.09 deployment. It is only a default: the node always
	// uses the value Cinder actually returns.
	DefaultCephUser = "cinder"
)

// Cinder microversions used by this driver.
const (
	// MvSelfServiceAttach is mandatory: self-service attachment records.
	MvSelfServiceAttach = "3.27"
	// MvAttachComplete is optional: POST os-complete.
	MvAttachComplete = "3.44"
)

// Volume status values.
const (
	VolumeAvailableStatus = "available"
	VolumeInUseStatus     = "in-use"
	VolumeReservedStatus  = "reserved"
	// VolumeAttachingStatus is a real intermediate state, observed on the
	// validated deployment after the connector update and before
	// CompleteAttachment. No publish path may wait on "in-use", because
	// completion is optional (design §6.10).
	VolumeAttachingStatus = "attaching"
)

// Delete-volume modes. Retain is the default: the migration Blueprint needs
// the Cinder volume to survive PVC deletion.
const (
	DeleteVolumeModeRetain = "retain"
	DeleteVolumeModeDelete = "delete"
)

// ── Data types ───────────────────────────────────────────────────────────────

// MonAddr is one Ceph monitor endpoint, formed by pairing hosts[n] with
// ports[n] from the Cinder connection_info.
type MonAddr struct {
	Host string
	Port string
}

// String renders the monitor as host:port, IPv6-safe.
func (m MonAddr) String() string {
	if m.Port == "" {
		return m.Host
	}
	if strings.Contains(m.Host, ":") && !strings.HasPrefix(m.Host, "[") {
		return "[" + m.Host + "]:" + m.Port
	}
	return m.Host + ":" + m.Port
}

// RBDConnectionInfo is the normalized, validated form of a Cinder RBD
// connection_info. The wire form is decoded in rbdconnection.go; only this
// type is handed to the rest of the driver.
//
// It never carries credential material. secret_uuid is recorded as
// ClusterFSID because that is what it is: a cluster identifier, not a key.
type RBDConnectionInfo struct {
	DriverVolumeType string
	ClusterName      string
	ClusterFSID      string
	Pool             string
	Image            string
	AuthEnabled      bool
	AuthUsername     string
	Monitors         []MonAddr
	VolumeID         string
	AttachmentID     string
	AccessMode       string
	Discard          bool
}

// MonitorList renders the monitors as a comma-separated host:port list, the
// form used in publish_context and in a generated ceph.conf mon_host.
func (ci *RBDConnectionInfo) MonitorList() string {
	parts := make([]string, 0, len(ci.Monitors))
	for _, m := range ci.Monitors {
		parts = append(parts, m.String())
	}
	return strings.Join(parts, ",")
}

// ImageSpec returns "<pool>/<image>", the exact form accepted by the rbd CLI.
func (ci *RBDConnectionInfo) ImageSpec() string {
	return ci.Pool + "/" + ci.Image
}

// AttachmentConnector is the body sent to PUT /v3/attachments/{id}.
//
// Frozen by lab measurement, not by analogy with iSCSI (design §6.3):
// the validated Cinder RBD backend rejects an empty connector ("does not have
// enough properties") and accepts {"host": "<hostname>"} alone, returning a
// complete flat connection_info. No initiator, ip, platform, os_type,
// multipath or do_local_attach is required, so none is sent.
type AttachmentConnector struct {
	Host string `json:"host"`
}

// Attachment wraps a Cinder v3 attachment record.
type Attachment struct {
	ID             string             `json:"id"`
	VolumeID       string             `json:"volume_id"`
	Status         string             `json:"status"`
	Instance       *string            `json:"instance"`
	ConnectionInfo *RBDConnectionInfo `json:"-"`
}

// CinderCapabilities records the microversion probe result.
type CinderCapabilities struct {
	SupportsV327 bool
	SupportsV344 bool
}

// ── Configuration ────────────────────────────────────────────────────────────

// RBDCinderConfig is the parsed driver configuration.
//
// Global comes from cloud.conf (a Secret); RBD and Volume come from
// driver.conf (a ConfigMap). Both are supplied through repeatable
// --cloud-config flags.
type RBDCinderConfig struct {
	Global map[string]*client.AuthOpts
	RBD    RBDOpts
	Volume VolumeOpts
}

// Defaults for the [RBD] section.
const (
	defaultExpectedClusterName    = "ceph"
	defaultCephClientVersionMajor = 18
	defaultCredentialPath         = "/etc/cinder-rbd-csi/ceph"
	defaultRuntimeDir             = "/run/cinder-rbd-csi"
	defaultStateDir               = "/var/lib/cinder-rbd-csi"
	defaultMapTimeout             = 120 * time.Second
	defaultUnmapTimeout           = 120 * time.Second
	defaultDeviceWaitTimeout      = 60 * time.Second
)

// Defaults for the [Volume] section.
const (
	defaultCreateTimeoutSeconds = 300
	defaultDetachTimeoutSeconds = 120
	// DefaultMetadataPrefix yields csi.rbd.attachment_id and
	// csi.rbd.cleanupVolume. A single prefix governs every driver-owned
	// metadata key so the sibling drivers cannot collide on one volume.
	DefaultMetadataPrefix = "csi.rbd"
)

// RBDOpts is the [RBD] section of driver.conf.
//
// Every field here has a reader in the driver. Timeout fields are declared as
// strings because gcfg cannot parse time.Duration; ApplyDefaults resolves them
// into the unexported duration fields exposed by the *Duration accessors.
type RBDOpts struct {
	// Mounter selects the node mapping method. Only "krbd" is accepted.
	Mounter string `gcfg:"mounter"`
	// Exclusive must be true. A writable non-exclusive map is not a
	// supported configuration, so "false" is rejected by Validate.
	Exclusive *bool `gcfg:"exclusive"`
	// ExpectedClusterName is compared against connection_info.cluster_name.
	ExpectedClusterName string `gcfg:"expected-cluster-name"`
	// ExpectedFSID is compared against connection_info.secret_uuid and
	// against the live sysfs cluster_fsid. Environment-specific identifier,
	// not a credential. Required in production.
	ExpectedFSID string `gcfg:"expected-fsid"`
	// CephClientVersionMajor is checked against the bundled `rbd --version`.
	CephClientVersionMajor int `gcfg:"ceph-client-version-major"`
	// CredentialPath is the directory where the operator-managed Ceph
	// credential Secret is projected (files: userID, userKey).
	CredentialPath string `gcfg:"credential-path"`
	// RuntimeDir holds generated ceph.conf and keyring files. Should be
	// memory-backed so keys never reach disk.
	RuntimeDir string `gcfg:"runtime-dir"`
	// StateDir holds the durable node-scoped staging index.
	StateDir string `gcfg:"state-dir"`

	MapTimeout        string `gcfg:"map-timeout"`
	UnmapTimeout      string `gcfg:"unmap-timeout"`
	DeviceWaitTimeout string `gcfg:"device-wait-timeout"`

	// MaxVolumesPerNode is reported by NodeGetInfo. 0 means unlimited,
	// pending measurement of the krbd mapping limit.
	MaxVolumesPerNode int64 `gcfg:"max-volumes-per-node"`

	// resolved values, populated by ApplyDefaults
	mapTimeout        time.Duration
	unmapTimeout      time.Duration
	deviceWaitTimeout time.Duration
}

// VolumeOpts is the [Volume] section of driver.conf.
type VolumeOpts struct {
	// CreateTimeout bounds the wait for a new volume to become available.
	CreateTimeout int `gcfg:"create-timeout"`
	// DetachTimeout bounds the wait for a volume to leave in-use/reserved
	// during unpublish and delete.
	DetachTimeout int `gcfg:"detach-timeout"`
	// DefaultVolumeType is used when the StorageClass omits "type".
	// Never hard-coded in Go.
	DefaultVolumeType string `gcfg:"default-volume-type"`
	// MetadataPrefix prefixes every driver-owned Cinder metadata key.
	MetadataPrefix string `gcfg:"metadata-prefix"`
	// DeleteVolumeMode is "retain" (default) or "delete".
	DeleteVolumeMode string `gcfg:"delete-volume-mode"`
}

// MapTimeoutDuration returns the resolved map timeout.
func (o RBDOpts) MapTimeoutDuration() time.Duration { return o.mapTimeout }

// UnmapTimeoutDuration returns the resolved unmap timeout.
func (o RBDOpts) UnmapTimeoutDuration() time.Duration { return o.unmapTimeout }

// DeviceWaitTimeoutDuration returns the resolved device-appearance timeout.
func (o RBDOpts) DeviceWaitTimeoutDuration() time.Duration { return o.deviceWaitTimeout }

// IsExclusive reports whether exclusive mapping is enabled. Unset means true.
func (o RBDOpts) IsExclusive() bool { return o.Exclusive == nil || *o.Exclusive }

// parseDurationOr returns the parsed duration, or def when s is empty.
// A malformed value is an error rather than a silent fallback: quietly
// substituting a default for "12x" would hide a misconfiguration.
func parseDurationOr(s string, def time.Duration, field string) (time.Duration, error) {
	if strings.TrimSpace(s) == "" {
		return def, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("[RBD] %s: invalid duration %q: %w", field, s, err)
	}
	if d <= 0 {
		return 0, fmt.Errorf("[RBD] %s: must be positive, got %q", field, s)
	}
	return d, nil
}

// ApplyDefaults fills unset fields and resolves duration strings.
// It is the single place [RBD] defaults are defined.
func (o *RBDOpts) ApplyDefaults() error {
	if o.Mounter == "" {
		o.Mounter = MounterKRBD
	}
	if o.ExpectedClusterName == "" {
		o.ExpectedClusterName = defaultExpectedClusterName
	}
	if o.CephClientVersionMajor == 0 {
		o.CephClientVersionMajor = defaultCephClientVersionMajor
	}
	if o.CredentialPath == "" {
		o.CredentialPath = defaultCredentialPath
	}
	if o.RuntimeDir == "" {
		o.RuntimeDir = defaultRuntimeDir
	}
	if o.StateDir == "" {
		o.StateDir = defaultStateDir
	}

	var err error
	if o.mapTimeout, err = parseDurationOr(o.MapTimeout, defaultMapTimeout, "map-timeout"); err != nil {
		return err
	}
	if o.unmapTimeout, err = parseDurationOr(o.UnmapTimeout, defaultUnmapTimeout, "unmap-timeout"); err != nil {
		return err
	}
	if o.deviceWaitTimeout, err = parseDurationOr(o.DeviceWaitTimeout, defaultDeviceWaitTimeout, "device-wait-timeout"); err != nil {
		return err
	}
	if o.MaxVolumesPerNode < 0 {
		return fmt.Errorf("[RBD] max-volumes-per-node: must be >= 0, got %d", o.MaxVolumesPerNode)
	}
	return nil
}

// Validate rejects configurations the driver refuses to run with.
//
// Both rejections are deliberate: making an unsafe value unrepresentable is
// better than documenting that it is unsafe.
func (o RBDOpts) Validate() error {
	if o.Mounter != MounterKRBD {
		return fmt.Errorf("[RBD] mounter: only %q is supported in this release, got %q", MounterKRBD, o.Mounter)
	}
	if !o.IsExclusive() {
		return fmt.Errorf("[RBD] exclusive: must be true; a writable non-exclusive kernel RBD mapping is not a supported configuration")
	}
	if o.CephClientVersionMajor <= 0 {
		return fmt.Errorf("[RBD] ceph-client-version-major: must be positive, got %d", o.CephClientVersionMajor)
	}
	return nil
}

// ApplyDefaults fills unset [Volume] fields.
func (o *VolumeOpts) ApplyDefaults() error {
	if o.CreateTimeout <= 0 {
		o.CreateTimeout = defaultCreateTimeoutSeconds
	}
	if o.DetachTimeout <= 0 {
		o.DetachTimeout = defaultDetachTimeoutSeconds
	}
	if o.MetadataPrefix == "" {
		o.MetadataPrefix = DefaultMetadataPrefix
	}
	if o.DeleteVolumeMode == "" {
		o.DeleteVolumeMode = DeleteVolumeModeRetain
	}
	return nil
}

// Validate rejects an unknown delete-volume-mode. The baseline treats any
// non-"delete" value as retain, which silently accepts typos such as
// "delet" — here they are rejected instead.
func (o VolumeOpts) Validate() error {
	switch o.DeleteVolumeMode {
	case DeleteVolumeModeRetain, DeleteVolumeModeDelete:
	default:
		return fmt.Errorf("[Volume] delete-volume-mode: must be %q or %q, got %q",
			DeleteVolumeModeRetain, DeleteVolumeModeDelete, o.DeleteVolumeMode)
	}
	return nil
}

// ShouldDeleteVolume reports whether DeleteVolume destroys the Cinder volume.
// Precedence: per-volume metadata override, then driver configuration.
func (o VolumeOpts) ShouldDeleteVolume(perVolumeCleanup string) bool {
	if strings.EqualFold(strings.TrimSpace(perVolumeCleanup), "true") {
		return true
	}
	return o.DeleteVolumeMode == DeleteVolumeModeDelete
}

// ── Provider ─────────────────────────────────────────────────────────────────

// OpenStackRBD is the Cinder-only implementation of IOpenStackRBD.
type OpenStackRBD struct {
	blockstorage *gophercloud.ServiceClient
	epOpts       gophercloud.EndpointOpts
	rbdOpts      RBDOpts
	volumeOpts   VolumeOpts
	cinderCaps   *CinderCapabilities
}

// blockStorageClient returns a copy of the block-storage ServiceClient with
// the given microversion pinned. Gophercloud's Microversion is a mutable
// field, so concurrent RPCs needing different microversions must not share
// one client.
func (os *OpenStackRBD) blockStorageClient(microversion string) (*gophercloud.ServiceClient, error) {
	c, err := gos.NewBlockStorageV3(os.blockstorage.ProviderClient, os.epOpts)
	if err != nil {
		return nil, fmt.Errorf("failed to create blockstorage client: %w", err)
	}
	c.Microversion = microversion
	return c, nil
}

var (
	osInstance    IOpenStackRBD
	configFiles   = []string{"/etc/cloud.conf"}
	userAgentData []string
)

// AddExtraFlags registers driver-specific flags.
func AddExtraFlags(fs *pflag.FlagSet) {
	fs.StringArrayVar(&userAgentData, "user-agent", nil,
		"Extra data to add to gophercloud user-agent. Use multiple times to add more than one component.")
}

// InitOpenStackProvider records config paths and starts metrics serving.
func InitOpenStackProvider(cfgFiles []string, httpEndpoint string) {
	metrics.RegisterMetrics("cinder-rbd-csi")
	if httpEndpoint != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", legacyregistry.HandlerWithReset())
		go func() {
			if err := http.ListenAndServe(httpEndpoint, mux); err != nil {
				klog.Fatalf("failed to listen & serve metrics from %q: %v", httpEndpoint, err)
			}
		}()
		klog.Infof("metrics available at %q", httpEndpoint)
	}
	configFiles = cfgFiles
	klog.V(2).Infof("InitOpenStackProvider configFiles: %s", configFiles)
}

// GetConfigFromFiles parses cloud.conf and/or driver.conf.
//
// gcfg.FatalOnly tolerates unknown sections, which is what lets a node-mode
// process start with only driver.conf. Defaults and validation are applied
// here so every caller sees a usable configuration.
func GetConfigFromFiles(configFilePaths []string) (RBDCinderConfig, error) {
	var cfg RBDCinderConfig
	for _, p := range configFilePaths {
		f, err := os.Open(p)
		if err != nil {
			klog.Errorf("Failed to open configuration file %q: %v", p, err)
			return cfg, err
		}
		err = gcfg.FatalOnly(gcfg.ReadInto(&cfg, f))
		_ = f.Close()
		if err != nil {
			klog.Errorf("Failed to read configuration file %q: %v", p, err)
			return cfg, err
		}
	}

	if err := cfg.RBD.ApplyDefaults(); err != nil {
		return cfg, err
	}
	if err := cfg.RBD.Validate(); err != nil {
		return cfg, err
	}
	if err := cfg.Volume.ApplyDefaults(); err != nil {
		return cfg, err
	}
	if err := cfg.Volume.Validate(); err != nil {
		return cfg, err
	}

	for _, global := range cfg.Global {
		if global.UseClouds {
			if global.CloudsFile != "" {
				if err := os.Setenv("OS_CLIENT_CONFIG_FILE", global.CloudsFile); err != nil {
					return cfg, err
				}
			}
			if err := client.ReadClouds(global); err != nil {
				return cfg, err
			}
			klog.V(5).Infof("Credentials loaded from %s", global.CloudsFile)
		}
	}
	return cfg, nil
}

// CreateOpenStackProvider builds a Cinder-only provider and probes
// microversions. Microversion 3.27 is mandatory, so an unsupported backend
// fails startup rather than failing later at publish time.
func CreateOpenStackProvider(cloudName string) (IOpenStackRBD, error) {
	cfg, err := GetConfigFromFiles(configFiles)
	if err != nil {
		klog.Errorf("GetConfigFromFiles %s failed: %v", configFiles, err)
		return nil, err
	}

	global := cfg.Global[cloudName]
	if global == nil {
		return nil, fmt.Errorf("cloud name %q not found in configuration files: %s", cloudName, configFiles)
	}

	provider, err := client.NewOpenStackClient(global, "cinder-rbd-csi-plugin", userAgentData...)
	if err != nil {
		return nil, err
	}

	epOpts := gophercloud.EndpointOpts{
		Region:       global.Region,
		Availability: global.EndpointType,
	}

	// Cinder only. No Nova client is created.
	blockstorageClient, err := gos.NewBlockStorageV3(provider, epOpts)
	if err != nil {
		return nil, err
	}

	instance := &OpenStackRBD{
		blockstorage: blockstorageClient,
		epOpts:       epOpts,
		rbdOpts:      cfg.RBD,
		volumeOpts:   cfg.Volume,
	}

	caps, err := instance.DiscoverCinderCapabilities(context.Background())
	if err != nil {
		return nil, fmt.Errorf("cinder microversion probe failed: %w", err)
	}
	klog.Infof("Cinder capabilities: v%s=%v v%s=%v",
		MvSelfServiceAttach, caps.SupportsV327, MvAttachComplete, caps.SupportsV344)

	osInstance = instance
	return instance, nil
}

// GetOpenStackProvider returns the cached provider or creates one.
func GetOpenStackProvider(cloudName string) (IOpenStackRBD, error) {
	if osInstance != nil {
		return osInstance, nil
	}
	return CreateOpenStackProvider(cloudName)
}

// ── Config accessors ─────────────────────────────────────────────────────────

// GetRBDOpts returns the [RBD] options.
func (os *OpenStackRBD) GetRBDOpts() RBDOpts { return os.rbdOpts }

// GetVolumeOpts returns the [Volume] options.
func (os *OpenStackRBD) GetVolumeOpts() VolumeOpts { return os.volumeOpts }

// GetCinderCapabilities returns the cached microversion probe result.
func (os *OpenStackRBD) GetCinderCapabilities() *CinderCapabilities { return os.cinderCaps }
