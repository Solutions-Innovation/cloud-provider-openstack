/*
Copyright (c) 2024-2026 Wind River Systems, Inc.
Wind River Migration Framework Team

SPDX-License-Identifier: Apache-2.0
*/

package openstack

// driverMetricsRegistrar is set by the rbd package so InitOpenStackProvider can
// register the driver-specific series without the openstack package importing
// its parent (which would be an import cycle).
var driverMetricsRegistrar func()

// SetDriverMetricsRegistrar records the driver metric registration hook.
func SetDriverMetricsRegistrar(fn func()) { driverMetricsRegistrar = fn }

func registerDriverMetrics() {
	if driverMetricsRegistrar != nil {
		driverMetricsRegistrar()
	}
}
