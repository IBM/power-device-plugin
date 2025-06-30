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

package plugin_test

import (
	"testing"

	api "github.com/ocp-power-demos/power-dev-plugin/api"
	"github.com/ocp-power-demos/power-dev-plugin/pkg/plugin"
	"github.com/stretchr/testify/assert"
)

// mock scanner
type mockScanner struct {
	devices []string
	config  *api.DevicePluginConfig
	err     error
}

func (m mockScanner) GetBlockDevices() ([]string, error) {
	if m.err != nil {
		return nil, m.err
	}
	return m.devices, nil
}

func (m mockScanner) LoadConfig() (*api.DevicePluginConfig, error) {
	return m.config, nil
}

func TestScanDevices_Default(t *testing.T) {
	scanner := mockScanner{
		devices: []string{"/dev/dm-1", "/dev/dm-2"},
		config:  &api.DevicePluginConfig{},
	}

	result, err := plugin.ScanRootForDevicesWithDeps(scanner, false)
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"dm-1", "dm-2"}, result)
}

func TestScanDevices_NxGzip(t *testing.T) {
	scanner := mockScanner{
		devices: []string{"/dev/dm-1"},
		config:  &api.DevicePluginConfig{},
	}

	result, err := plugin.ScanRootForDevicesWithDeps(scanner, true)
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"dm-1", "crypto/nx-gzip"}, result)
}

func TestScanDevices_WithExclude(t *testing.T) {
	scanner := mockScanner{
		devices: []string{"/dev/dm-1", "/dev/dm-2"},
		config: &api.DevicePluginConfig{
			ExcludeDevices: []string{"/dev/dm-1"},
		},
	}

	result, err := plugin.ScanRootForDevicesWithDeps(scanner, false)
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{"dm-2"}, result)
}

func TestScanDevices_WithInclude_NotExist(t *testing.T) {
	scanner := mockScanner{
		devices: []string{"/dev/dm-1", "/dev/dm-2"},
		config: &api.DevicePluginConfig{
			IncludeDevices: []string{"/dev/dm-999"},
		},
	}

	result, err := plugin.ScanRootForDevicesWithDeps(scanner, false)
	assert.NoError(t, err)
	assert.ElementsMatch(t, []string{}, result)
}

func TestMatchesAny(t *testing.T) {
	tests := []struct {
		name     string
		device   string
		patterns []string
		expected bool
	}{
		{
			name:     "Exact match",
			device:   "/dev/sda",
			patterns: []string{"/dev/sda"},
			expected: true,
		},
		{
			name:     "Wildcard match",
			device:   "/dev/sda1",
			patterns: []string{"/dev/sda*"},
			expected: true,
		},
		{
			name:     "No match",
			device:   "/dev/sdb",
			patterns: []string{"/dev/sda"},
			expected: false,
		},
		{
			name:     "Invalid pattern",
			device:   "/dev/sda",
			patterns: []string{"[invalid"},
			expected: false,
		},
		{
			name:     "Empty pattern list",
			device:   "/dev/sda",
			patterns: []string{},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := plugin.MatchesAny(tt.device, tt.patterns)
			if result != tt.expected {
				t.Errorf("matchesAny(%q, %v) = %v; want %v", tt.device, tt.patterns, result, tt.expected)
			}
		})
	}
}

func TestGetValidatedPermission(t *testing.T) {
	tests := []struct {
		name     string
		config   *api.DevicePluginConfig
		expected string
	}{
		{"Nil config", nil, "rwm"},
		{"Valid rw", &api.DevicePluginConfig{Permissions: "rw"}, "rw"},
		{"Uppercase valid", &api.DevicePluginConfig{Permissions: "RWM"}, "rwm"},
		{"Invalid value", &api.DevicePluginConfig{Permissions: "xyz"}, "rw"},
		{"Empty string", &api.DevicePluginConfig{Permissions: ""}, "rw"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := plugin.GetValidatedPermission(tt.config)
			if got != tt.expected {
				t.Errorf("Expected %s, got %s", tt.expected, got)
			}
		})
	}
}

func TestApplyExcludeFilters(t *testing.T) {
	devices := []string{"/dev/sda", "/dev/sdb", "/dev/nvme0n1"}
	excludes := []string{"/dev/sdb", "/dev/nvme0n1"}

	result := plugin.ApplyExcludeFilters(devices, excludes)

	assert.Equal(t, []string{"/dev/sda"}, result)
}

func TestApplyExcludeFilters_NoMatch(t *testing.T) {
	devices := []string{"/dev/sda", "/dev/sdb"}
	excludes := []string{"/dev/sdc"}

	result := plugin.ApplyExcludeFilters(devices, excludes)

	assert.Equal(t, devices, result)
}

func TestApplyIncludeFilters_EmptyIncludes(t *testing.T) {
	devices := []string{"/dev/sda", "/dev/sdb"}
	result := plugin.ApplyIncludeFilters(devices, []string{})

	assert.Equal(t, []string{"sda", "sdb"}, result)
}

func TestApplyIncludeFilters_InvalidPattern(t *testing.T) {
	devices := []string{"/dev/sda"}
	patterns := []string{"["} // invalid pattern

	result := plugin.ApplyIncludeFilters(devices, patterns)

	assert.Empty(t, result)
}
