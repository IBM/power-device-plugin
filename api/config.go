/*
Copyright 2025.

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

package api

// DevicePluginConfig holds the configuration parsed from the ConfigMap
type DevicePluginConfig struct {
	NxGzip              bool     `json:"nx-gzip"`
	Permissions         string   `json:"permissions"`               // Accepts: R, RW, RWM, RM, W, WM, M
	IncludeDevices      []string `json:"include-devices,omitempty"` // e.g., "/dev/dm-0", "/dev/dm-*"
	ExcludeDevices      []string `json:"exclude-devices,omitempty"` // e.g., "/dev/dm-3", "/dev/dm-*"
	DiscoveryStrategy   string   `json:"discovery-strategy"`        // "default" or "time"
	ScanInterval        string   `json:"scan-interval"`             // e.g., "60m", min 1m
	UpperLimitPerDevice int      `json:"upper-limit,omitempty"`
}
