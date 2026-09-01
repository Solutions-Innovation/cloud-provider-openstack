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
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors from connection-info normalization. Callers use errors.Is to
// distinguish a malformed backend response from a transport failure.
var (
	// ErrWrongDriverVolumeType means the attachment is not RBD-backed.
	ErrWrongDriverVolumeType = errors.New("openstack: connection_info driver_volume_type is not rbd")

	// ErrConnectionInfoConflict means top-level and nested `data` disagree on a
	// field. Preferring either one could silently map the wrong image, so the
	// response is rejected.
	ErrConnectionInfoConflict = errors.New("openstack: connection_info top-level and nested data conflict")

	// ErrConnectionInfoIncomplete means a required identity field is missing.
	ErrConnectionInfoIncomplete = errors.New("openstack: connection_info is missing required fields")

	// ErrConnectionInfoHasSecret means the backend returned credential
	// material. Consuming it would repeat the exposure fixed by CVE-2020-10755,
	// so the driver refuses the response.
	ErrConnectionInfoHasSecret = errors.New("openstack: connection_info unexpectedly contains credential material")
)

// secretFieldNames are fields that must never appear in an RBD connection_info.
// The Ceph key is supplied out-of-band by the operator; a backend that returns
// one is misconfigured or regressed.
var secretFieldNames = []string{"keyring", "key", "secret", "secret_key", "password", "userkey", "user_key"}

// requiredMonitorPorts is the field pairing rule: hosts[n] belongs with ports[n].
// A length mismatch is rejected rather than truncated, because a partially
// reachable monitor list produces intermittent map failures that are very hard
// to diagnose.

// parseRBDConnectionInfo normalizes a Cinder RBD connection_info payload.
//
// The validated backend returns a flat structure. A nested `data` object is
// accepted only as a backward-compatible alternate source for fields that are
// absent at the top level; it may never override a top-level value, and a
// disagreement is an error.
//
// Order of operations matters and is asserted by tests:
//
//  1. driver_volume_type is read from the top level and must be "rbd".
//  2. Nested `data` fills only absent fields; conflicts are rejected.
//  3. `name` splits on the FIRST "/" into pool and image, verbatim.
//  4. hosts and ports are paired positionally and must be equal length.
//  5. secret_uuid becomes ClusterFSID: an identifier, never a key.
//  6. auth_username is required when auth_enabled.
//  7. cluster_name, when present, is preserved for comparison by the caller.
//  8. secret_type is diagnostic only and triggers no lookup.
//  9. Any credential-bearing field rejects the whole response.
func parseRBDConnectionInfo(raw map[string]any) (*RBDConnectionInfo, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("%w: response is empty", ErrConnectionInfoIncomplete)
	}

	// Rule 1: driver_volume_type comes from the top level only.
	dvt, _ := stringField(raw, "driver_volume_type")
	if dvt != DriverVolumeTypeRBD {
		return nil, fmt.Errorf("%w: got %q, want %q", ErrWrongDriverVolumeType, dvt, DriverVolumeTypeRBD)
	}

	// Rule 2: merge, with the top level authoritative.
	merged, err := mergeWithNestedData(raw)
	if err != nil {
		return nil, err
	}

	// Rule 9: refuse credential material anywhere in the payload. Only the
	// field NAME is reported; the value is never logged or echoed.
	for _, f := range secretFieldNames {
		if _, present := merged[f]; present {
			return nil, fmt.Errorf("%w: field %q", ErrConnectionInfoHasSecret, f)
		}
	}

	ci := &RBDConnectionInfo{DriverVolumeType: DriverVolumeTypeRBD}

	// Rule 3: `name` is authoritative. Split on the first "/" only; never add a
	// "volume-" prefix and never assume the pool.
	name, ok := stringField(merged, "name")
	if !ok || name == "" {
		return nil, fmt.Errorf("%w: missing name", ErrConnectionInfoIncomplete)
	}
	pool, image, found := strings.Cut(name, "/")
	if !found {
		return nil, fmt.Errorf("%w: name %q is not in <pool>/<image> form",
			ErrConnectionInfoIncomplete, name)
	}
	if pool == "" || image == "" {
		return nil, fmt.Errorf("%w: name %q has an empty pool or image",
			ErrConnectionInfoIncomplete, name)
	}
	ci.Pool = pool
	ci.Image = image

	// Rule 4: pair hosts with ports positionally.
	hosts, err := stringSliceField(merged, "hosts")
	if err != nil {
		return nil, err
	}
	ports, err := stringSliceField(merged, "ports")
	if err != nil {
		return nil, err
	}
	if len(hosts) == 0 {
		return nil, fmt.Errorf("%w: hosts is empty", ErrConnectionInfoIncomplete)
	}
	if len(hosts) != len(ports) {
		return nil, fmt.Errorf("%w: hosts has %d entries but ports has %d",
			ErrConnectionInfoIncomplete, len(hosts), len(ports))
	}
	ci.Monitors = make([]MonAddr, 0, len(hosts))
	for i := range hosts {
		if hosts[i] == "" || ports[i] == "" {
			return nil, fmt.Errorf("%w: monitor %d has an empty host or port",
				ErrConnectionInfoIncomplete, i)
		}
		ci.Monitors = append(ci.Monitors, MonAddr{Host: hosts[i], Port: ports[i]})
	}

	// Rule 5: secret_uuid identifies the Ceph cluster.
	ci.ClusterFSID, _ = stringField(merged, "secret_uuid")
	if ci.ClusterFSID == "" {
		return nil, fmt.Errorf("%w: missing secret_uuid (Ceph cluster FSID)", ErrConnectionInfoIncomplete)
	}

	// Rule 6: auth_username is required when authentication is enabled.
	ci.AuthEnabled = boolField(merged, "auth_enabled")
	ci.AuthUsername, _ = stringField(merged, "auth_username")
	if ci.AuthEnabled && ci.AuthUsername == "" {
		return nil, fmt.Errorf("%w: auth_enabled is true but auth_username is empty",
			ErrConnectionInfoIncomplete)
	}

	// Rule 7 and 8: preserved for the caller; secret_type is not acted upon.
	ci.ClusterName, _ = stringField(merged, "cluster_name")
	ci.VolumeID, _ = stringField(merged, "volume_id")
	ci.AttachmentID, _ = stringField(merged, "attachment_id")
	ci.AccessMode, _ = stringField(merged, "access_mode")
	ci.Discard = boolField(merged, "discard")

	return ci, nil
}

// mergeWithNestedData returns the effective field map.
//
// The result is a fresh map: the caller's input is never mutated, so a retry or
// a second parse of the same payload behaves identically.
func mergeWithNestedData(raw map[string]any) (map[string]any, error) {
	merged := make(map[string]any, len(raw))
	for k, v := range raw {
		if k == "data" {
			continue
		}
		merged[k] = v
	}

	nested, ok := raw["data"].(map[string]any)
	if !ok {
		return merged, nil
	}

	for k, v := range nested {
		existing, present := merged[k]
		if !present {
			merged[k] = v
			continue
		}
		if !sameScalar(existing, v) {
			// Report only the field name and never the values: one of them
			// could in principle be sensitive on a regressed backend.
			return nil, fmt.Errorf("%w: field %q", ErrConnectionInfoConflict, k)
		}
	}
	return merged, nil
}

// sameScalar compares two decoded JSON values for equivalence, tolerating the
// string/number representation differences seen across backends (ports in
// particular appear as both "6789" and 6789).
func sameScalar(a, b any) bool {
	if fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b) {
		return true
	}
	as, aok := a.([]any)
	bs, bok := b.([]any)
	if aok && bok {
		if len(as) != len(bs) {
			return false
		}
		for i := range as {
			if !sameScalar(as[i], bs[i]) {
				return false
			}
		}
		return true
	}
	return false
}

// stringField reads a string, accepting a JSON number so a backend that emits
// ports or ids unquoted still parses.
func stringField(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case float64:
		// Ports and ids are integral; %v on a float64 would render 6789 as 6789.
		return fmt.Sprintf("%d", int64(t)), true
	case int:
		return fmt.Sprintf("%d", t), true
	case int64:
		return fmt.Sprintf("%d", t), true
	default:
		return "", false
	}
}

// boolField reads a bool, accepting the string spellings some backends emit.
func boolField(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok || v == nil {
		return false
	}
	switch t := v.(type) {
	case bool:
		return t
	case string:
		return strings.EqualFold(t, "true")
	default:
		return false
	}
}

// stringSliceField reads a list of strings, tolerating numeric elements.
func stringSliceField(m map[string]any, key string) ([]string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, fmt.Errorf("%w: missing %s", ErrConnectionInfoIncomplete, key)
	}
	items, ok := v.([]any)
	if !ok {
		// A single scalar is accepted as a one-element list.
		if s, isStr := stringField(m, key); isStr {
			return []string{s}, nil
		}
		return nil, fmt.Errorf("%w: %s is not a list", ErrConnectionInfoIncomplete, key)
	}
	out := make([]string, 0, len(items))
	for i, it := range items {
		switch t := it.(type) {
		case string:
			out = append(out, t)
		case float64:
			out = append(out, fmt.Sprintf("%d", int64(t)))
		default:
			return nil, fmt.Errorf("%w: %s[%d] is not a string", ErrConnectionInfoIncomplete, key, i)
		}
	}
	return out, nil
}
