/*
Copyright (c) 2024-2026 Wind River Systems, Inc.
Wind River Migration Framework Team

SPDX-License-Identifier: Apache-2.0
*/

package openstack

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gophercloud/gophercloud/v2"
	"github.com/stretchr/testify/require"
)

func TestDeleteVolumeMetadata_DeletesEachKeyFromMetadataSubresource(t *testing.T) {
	const volumeID = "volume-1"

	var deleted []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodDelete, r.Method)
		deleted = append(deleted, r.URL.Path)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	provider := &gophercloud.ProviderClient{TokenID: "token"}
	client := &gophercloud.ServiceClient{
		ProviderClient: provider,
		Endpoint:       server.URL + "/",
	}
	openstack := &OpenStackRBD{blockstorage: client}

	err := openstack.DeleteVolumeMetadata(
		context.Background(),
		volumeID,
		[]string{"csi.rbd.attachment_id", "csi.rbd.cleanupVolume"},
	)

	require.NoError(t, err)
	require.Equal(t, []string{
		"/volumes/volume-1/metadata/csi.rbd.attachment_id",
		"/volumes/volume-1/metadata/csi.rbd.cleanupVolume",
	}, deleted)
}
