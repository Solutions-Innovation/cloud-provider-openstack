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
	"sync"
	"time"

	"k8s.io/component-base/metrics"
	"k8s.io/component-base/metrics/legacyregistry"
)

// Driver-specific metric series.
//
// Every label carries a non-secret identifier only: a result string, a reason,
// or a state name. Pool, image, FSID and device path are deliberately NOT labels
// — they are unbounded in cardinality and belong in logs, not in a time series.
var (
	mapDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Name:    "cinder_rbd_csi_map_duration_seconds",
			Help:    "Time taken to map a Ceph RBD image with kernel RBD.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
		}, []string{"result"})

	unmapDuration = metrics.NewHistogramVec(
		&metrics.HistogramOpts{
			Name:    "cinder_rbd_csi_unmap_duration_seconds",
			Help:    "Time taken to unmap a Ceph RBD image.",
			Buckets: []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120},
		}, []string{"result"})

	exclusiveLockFailures = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Name: "cinder_rbd_csi_exclusive_lock_failures_total",
			Help: "Number of times an exclusive kernel RBD mapping was denied. " +
				"A non-zero value means another writable client held the Ceph exclusive lock.",
		}, []string{})

	attachmentRecordsCreated = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Name: "cinder_rbd_csi_attachment_records_created_total",
			Help: "Number of Cinder attachment records created, by reason.",
		}, []string{"reason"})

	attachmentRecordsDeleted = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Name: "cinder_rbd_csi_attachment_records_deleted_total",
			Help: "Number of Cinder attachment records deleted.",
		}, []string{})

	duplicateAttachmentRecords = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Name: "cinder_rbd_csi_duplicate_attachment_records_total",
			Help: "Number of times a volume was found with more than one attachment record. " +
				"Requires operator resolution; the driver refuses to guess.",
		}, []string{})

	isolatedMappings = metrics.NewGauge(
		&metrics.GaugeOpts{
			Name: "cinder_rbd_csi_isolated_mappings",
			Help: "Number of live kernel RBD mappings that failed identity verification. " +
				"These are neither adopted nor unmapped and need operator resolution.",
		})

	orphanedStagingRecords = metrics.NewGauge(
		&metrics.GaugeOpts{
			Name: "cinder_rbd_csi_orphaned_staging_records",
			Help: "Number of staging records with no corresponding kernel mapping, " +
				"discarded at startup reconciliation.",
		})

	unrecognizedMappings = metrics.NewGauge(
		&metrics.GaugeOpts{
			Name: "cinder_rbd_csi_unrecognized_mappings",
			Help: "Number of live kernel RBD mappings in a driver-owned pool with no staging record. " +
				"Reported only; never unmapped, since they may belong to another client.",
		})

	stagedVolumes = metrics.NewGauge(
		&metrics.GaugeOpts{
			Name: "cinder_rbd_csi_staged_volumes",
			Help: "Number of volumes currently staged by this node plugin.",
		})

	volumesRetained = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Name: "cinder_rbd_csi_volumes_retained_total",
			Help: "Number of Cinder volumes retained on PVC deletion for migration handoff.",
		}, []string{})

	volumesDeleted = metrics.NewCounterVec(
		&metrics.CounterOpts{
			Name: "cinder_rbd_csi_volumes_deleted_total",
			Help: "Number of Cinder volumes deleted on PVC deletion under an explicit cleanup policy.",
		}, []string{})
)

// Reasons for attachment record creation, used as a metric label. The set is
// closed so the series cardinality stays bounded.
const (
	attachReasonCreateVolume = "create_volume"
	attachReasonOnDemand     = "on_demand"
	attachReasonReplacement  = "replacement"
	attachReasonAdopted      = "adopted"
)

// Results used as the map/unmap histogram label.
const (
	resultSuccess    = "success"
	resultLockDenied = "lock_denied"
	resultError      = "error"
)

var registerRBDMetrics sync.Once

// RegisterDriverMetrics registers the driver-specific series.
//
// Called from InitOpenStackProvider's component registration path. It is
// idempotent because both the controller and node services may run in one
// process, and double registration would panic.
func RegisterDriverMetrics() {
	registerRBDMetrics.Do(func() {
		legacyregistry.MustRegister(
			mapDuration,
			unmapDuration,
			exclusiveLockFailures,
			attachmentRecordsCreated,
			attachmentRecordsDeleted,
			duplicateAttachmentRecords,
			isolatedMappings,
			orphanedStagingRecords,
			unrecognizedMappings,
			stagedVolumes,
			volumesRetained,
			volumesDeleted,
		)
	})
}

// observeMap records the outcome and latency of a map attempt.
func observeMap(start time.Time, result string) {
	mapDuration.WithLabelValues(result).Observe(time.Since(start).Seconds())
	if result == resultLockDenied {
		exclusiveLockFailures.WithLabelValues().Inc()
	}
}

// observeUnmap records the outcome and latency of an unmap attempt.
func observeUnmap(start time.Time, result string) {
	unmapDuration.WithLabelValues(result).Observe(time.Since(start).Seconds())
}
