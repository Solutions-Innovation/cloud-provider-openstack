/*
Copyright 2024 The Kubernetes Authors.

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

package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	iscsi "k8s.io/cloud-provider-openstack/pkg/csi/cinder-iscsi"
	"k8s.io/cloud-provider-openstack/pkg/csi/cinder-iscsi/openstack"
	"k8s.io/cloud-provider-openstack/pkg/version"
	"k8s.io/component-base/cli"
	"k8s.io/klog/v2"
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
		Use:   "cinder-iscsi-csi-plugin",
		Short: "CSI driver for iSCSI-backed Cinder volumes",
		Run: func(cmd *cobra.Command, args []string) {
			handle()
		},
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if !provideControllerService {
				return nil
			}
			configs, err := cmd.Flags().GetStringSlice("cloud-config")
			if err != nil {
				return err
			}
			if len(configs) == 0 {
				return fmt.Errorf("--cloud-config is required when providing controller service")
			}
			return nil
		},
		Version: version.Version,
	}

	cmd.PersistentFlags().StringVar(&endpoint, "endpoint", "", "CSI endpoint")
	if err := cmd.MarkPersistentFlagRequired("endpoint"); err != nil {
		klog.Fatalf("Unable to mark flag endpoint to be required: %v", err)
	}

	cmd.Flags().StringSliceVar(&cloudConfig, "cloud-config", nil, "CSI driver cloud config. This option can be given multiple times")
	cmd.PersistentFlags().StringVar(&cloudName, "cloud-name", "", "Cloud name to instruct CSI driver to read OpenStack cloud credentials from the configuration subsection.")
	cmd.PersistentFlags().StringVar(&cluster, "cluster", "", "The identifier of the cluster that the plugin is running in.")
	cmd.PersistentFlags().StringVar(&httpEndpoint, "http-endpoint", "", "The TCP network address where the HTTP server for providing metrics for diagnostics will listen (example: `:8080`).")
	cmd.PersistentFlags().BoolVar(&provideControllerService, "provide-controller-service", true, "If set to true then the CSI driver provides the controller service (default: true)")
	cmd.PersistentFlags().BoolVar(&provideNodeService, "provide-node-service", true, "If set to true then the CSI driver provides the node service (default: true)")

	openstack.AddExtraFlags(pflag.CommandLine)

	code := cli.Run(cmd)
	os.Exit(code)
}

func handle() {
	d := iscsi.NewDriver(&iscsi.DriverOpts{
		Endpoint:  endpoint,
		ClusterID: cluster,
	})

	openstack.InitOpenStackProvider(cloudConfig, httpEndpoint)

	if provideControllerService {
		cloud, err := openstack.GetOpenStackProvider(cloudName)
		if err != nil {
			klog.Fatalf("Failed to get OpenStack provider %q: %v", cloudName, err)
			return
		}
		d.SetupControllerService(cloud)
	}

	if provideNodeService {
		d.SetupNodeService()
	}

	d.Run()
}
