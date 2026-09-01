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

// Command cinder-rbd-csi-plugin runs the Cinder RBD CSI driver
// (cinder-rbd.csi.windriver.com) in controller mode, node mode, or both.
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"k8s.io/component-base/cli"
	"k8s.io/klog/v2"

	rbd "k8s.io/cloud-provider-openstack/pkg/csi/cinder-rbd"
	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-rbd/openstack"
	"k8s.io/cloud-provider-openstack/pkg/version"
)

var (
	endpoint                 string
	cloudConfig              []string
	cloudName                string
	cluster                  string
	httpEndpoint             string
	provideControllerService bool
	provideNodeService       bool
)

func main() {
	cmd := &cobra.Command{
		Use:   "cinder-rbd-csi-plugin",
		Short: "Cinder RBD CSI plugin for Ceph RBD-backed Cinder volumes",
		PersistentPreRunE: func(_ *cobra.Command, _ []string) error {
			// Controller mode needs cloud.conf for Cinder authentication.
			// Node mode does not: it only reads driver.conf.
			if provideControllerService && len(cloudConfig) == 0 {
				return fmt.Errorf("unable to mark flag \"cloud-config\" as required: " +
					"controller mode requires at least one --cloud-config")
			}
			return nil
		},
		Run: func(_ *cobra.Command, _ []string) {
			handle()
		},
		Version: version.Version,
	}

	cmd.PersistentFlags().StringVar(&endpoint, "endpoint", "",
		"CSI endpoint, e.g. unix:///csi/csi.sock")
	if err := cmd.MarkPersistentFlagRequired("endpoint"); err != nil {
		klog.Fatalf("Unable to mark flag endpoint to be required: %v", err)
	}

	cmd.Flags().StringSliceVar(&cloudConfig, "cloud-config", nil,
		"CSI driver cloud config. This option can be given multiple times "+
			"(cloud.conf for Cinder auth, driver.conf for [RBD]/[Volume] options)")

	cmd.PersistentFlags().StringVar(&cloudName, "cloud-name", "",
		"Cloud name to instruct CSI driver to read from the right section of the cloud config")
	cmd.PersistentFlags().StringVar(&cluster, "cluster", "",
		"The identifier of the cluster that the plugin is running in")
	cmd.PersistentFlags().StringVar(&httpEndpoint, "http-endpoint", "",
		"The TCP network address where the HTTP server for diagnostics, "+
			"including metrics, will listen (example: :8080)")
	cmd.PersistentFlags().BoolVar(&provideControllerService, "provide-controller-service", true,
		"If set to true then the CSI driver does provide the controller service")
	cmd.PersistentFlags().BoolVar(&provideNodeService, "provide-node-service", true,
		"If set to true then the CSI driver does provide the node service")

	openstack.AddExtraFlags(pflag.CommandLine)

	code := cli.Run(cmd)
	os.Exit(code)
}

func handle() {
	d := rbd.NewDriver(&rbd.DriverOpts{Endpoint: endpoint, ClusterID: cluster})

	// Registered through a hook so the openstack package need not import its
	// parent, which would be an import cycle.
	openstack.SetDriverMetricsRegistrar(rbd.RegisterDriverMetrics)
	openstack.InitOpenStackProvider(cloudConfig, httpEndpoint)

	if provideControllerService {
		cloud, err := openstack.GetOpenStackProvider(cloudName)
		if err != nil {
			// Microversion 3.27 is mandatory, so an unusable Cinder is fatal
			// here rather than a per-RPC failure later.
			klog.Fatalf("Failed to GetOpenStackProvider: %v", err)
		}
		d.SetupControllerService(cloud)
	}

	if provideNodeService {
		cfg, err := openstack.GetConfigFromFiles(cloudConfig)
		if err != nil {
			// Node mode must not silently fall back to defaults: expected-fsid
			// and the credential path come from driver.conf, and running without
			// them would disable an identity check rather than fail loudly.
			klog.Fatalf("Failed to load driver configuration for node service: %v", err)
		}
		d.SetupNodeService(cfg.RBD, cfg.Volume)
	}

	d.Run()
}
