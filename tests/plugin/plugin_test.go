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
	"errors"
	"testing"
	"time"

	api "github.com/ocp-power-demos/power-dev-plugin/api"
	"github.com/ocp-power-demos/power-dev-plugin/pkg/plugin"
	"github.com/stretchr/testify/assert"
)

// mock scanner with support for FindDevices
type mockScanner struct {
	devices      []string
	config       *api.DevicePluginConfig
	errorOnBlock error
	findResults  map[string][]string
	simulateScanError bool
}

func (m mockScanner) GetBlockDevices() ([]string, error) {
	if m.errorOnBlock != nil {
		return nil, m.errorOnBlock
	} else if m.simulateScanError {
		return nil, errors.New("mock scan failure")
	}
	return m.devices, nil
}

func (m mockScanner) LoadConfig() (*api.DevicePluginConfig, error) {
	return m.config, nil
}

func (m mockScanner) FindDevices(pattern string) ([]string, error) {
	if result, ok := m.findResults[pattern]; ok {
		return result, nil
	}
	return nil, errors.New("no match")
}

func (m mockScanner) StatDevice(path string) error {
	// simulate that all paths returned by FindDevices exist
	for _, paths := range m.findResults {
		for _, p := range paths {
			if p == path {
				return nil
			}
		}
	}
	return errors.New("not found")
}

func TestScanRootForDevicesWithDeps(t *testing.T) {
	tests := []struct {
		name        string
		devices     []string
		findResults map[string][]string
		config      *api.DevicePluginConfig
		nxGzip      bool
		wantResult  []string
	}{
		{
			name:    "Include match and exclude match",
			devices: []string{"/dev/dm-1", "/dev/dm-9", "/dev/sda"},
			findResults: map[string][]string{
				"/dev/dm-1": {"/dev/dm-1"},
			},
			config: &api.DevicePluginConfig{
				IncludeDevices: []string{"/dev/dm-1"},
				ExcludeDevices: []string{"/dev/dm-9"},
			},
			nxGzip:     true,
			wantResult: []string{"dm-1"},
		},
		{
			name:        "Empty include/exclude",
			devices:     []string{"/dev/sda", "/dev/dm-0"},
			findResults: map[string][]string{},
			config:      &api.DevicePluginConfig{},
			nxGzip:      true,
			wantResult:  []string{"/dev/sda", "/dev/dm-0", "/dev/crypto/nx-gzip"},
		},
		{
			name:    "Invalid include pattern",
			devices: []string{"/dev/sda", "/dev/sdb"},
			findResults: map[string][]string{
				"abc": {},
			},
			config: &api.DevicePluginConfig{
				IncludeDevices: []string{"abc", ""},
				ExcludeDevices: []string{"", "sda"},
			},
			nxGzip:     true,
			wantResult: []string{},
		},
		{
			name:    "Include pattern matches nothing",
			devices: []string{"/dev/sda", "/dev/sdb"},
			findResults: map[string][]string{
				"/dev/notexist": {},
			},
			config: &api.DevicePluginConfig{
				IncludeDevices: []string{"/dev/notexist"},
			},
			nxGzip:     false,
			wantResult: []string{},
		},
		{
			name:    "Exclude all",
			devices: []string{"/dev/sda", "/dev/sdb"},
			findResults: map[string][]string{
				"*": {"/dev/sda", "/dev/sdb"},
			},
			config: &api.DevicePluginConfig{
				ExcludeDevices: []string{"/dev/sda", "/dev/sdb"},
			},
			nxGzip:     false,
			wantResult: []string{},
		},
		{
			name:        "Error on block device read",
			devices:     []string{},
			findResults: map[string][]string{},
			config:      &api.DevicePluginConfig{},
			nxGzip:      false,
			wantResult:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scanner := mockScanner{
				devices:     tt.devices,
				config:      tt.config,
				findResults: tt.findResults,
			}
			got, err := plugin.ScanRootForDevicesWithDeps(scanner, tt.nxGzip)
			assert.NoError(t, err)
			assert.ElementsMatch(t, tt.wantResult, got)
		})
	}
}

func TestApplyExcludeFilters(t *testing.T) {
	devices := []string{"/dev/sda", "/dev/sdb", "/dev/nvme0n1"}
	excludes := []string{"/dev/sdb", "/dev/nvme0n1"}
	result := plugin.ApplyExcludeFilters(devices, excludes)
	assert.Equal(t, []string{"/dev/sda"}, result)
}

func TestApplyIncludeFilters_Empty(t *testing.T) {
	scanner := mockScanner{}
	devices := []string{"/dev/sda", "/dev/sdb"}
	result := plugin.ApplyIncludeFilters(scanner, devices, []string{})
	assert.Equal(t, []string{"sda", "sdb"}, result)
}

func TestApplyIncludeFilters_ValidPattern(t *testing.T) {
	scanner := mockScanner{
		findResults: map[string][]string{
			"/dev/sda": {"/dev/sda"},
		},
	}
	devices := []string{"/dev/sda", "/dev/sdb"}
	includes := []string{"/dev/sda"}
	result := plugin.ApplyIncludeFilters(scanner, devices, includes)
	assert.Equal(t, []string{"sda"}, result)
}

func TestApplyIncludeFilters_InvalidPattern(t *testing.T) {
	scanner := mockScanner{
		findResults: map[string][]string{},
	}
	devices := []string{"/dev/sda"}
	patterns := []string{"["} // invalid
	result := plugin.ApplyIncludeFilters(scanner, devices, patterns)
	assert.Empty(t, result)
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
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestMatchesAny(t *testing.T) {
	tests := []struct {
		name     string
		device   string
		patterns []string
		expected bool
	}{
		{"Exact match", "/dev/sda", []string{"/dev/sda"}, true},
		{"Wildcard match", "/dev/sda1", []string{"/dev/sda*"}, true},
		{"No match", "/dev/sdb", []string{"/dev/sda"}, false},
		{"Invalid pattern", "/dev/sda", []string{"[invalid"}, false},
		{"Empty pattern list", "/dev/sda", []string{}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := plugin.MatchesAny(tt.device, tt.patterns)
			assert.Equal(t, tt.expected, result)
		})
	}
}

func TestGetDiscoveredDevices_TimeStrategy(t *testing.T) {
	tests := []struct {
		name              string
		lastScanTime      time.Time
		cachedDevices     []string
		config            *api.DevicePluginConfig
		expectCached      bool
		expectError       bool
		simulateScanError bool
		devices           []string
	}{
		{
			name:          "Fresh scan due to no previous scan",
			lastScanTime:  time.Time{},
			cachedDevices: []string{},
			config: &api.DevicePluginConfig{
				DiscoveryStrategy: "time",
				ScanInterval:      "1m",
			},
			devices: []string{"/dev/dm-0", "/dev/dm-1"},
		},
		{
			name:          "Skip scan due to valid cache",
			lastScanTime:  time.Now().Add(-30 * time.Second),
			cachedDevices: []string{"/dev/dm-0", "/dev/dm-1"},
			config: &api.DevicePluginConfig{
				DiscoveryStrategy: "time",
				ScanInterval:      "1m",
			},
			expectCached: true,
		},
		{
			name:          "Trigger scan because interval passed",
			lastScanTime:  time.Now().Add(-2 * time.Minute),
			cachedDevices: []string{"/dev/dm-0"},
			config: &api.DevicePluginConfig{
				DiscoveryStrategy: "time",
				ScanInterval:      "1m",
			},
			devices: []string{"/dev/dm-0", "/dev/dm-1"},
		},
		{
			name:          "Fallback to cached on scan failure",
			lastScanTime:  time.Now().Add(-2 * time.Minute),
			cachedDevices: []string{"/dev/dm-0", "/dev/dm-1"},
			config: &api.DevicePluginConfig{
				DiscoveryStrategy: "time",
				ScanInterval:      "1m",
			},
			simulateScanError: true,
			expectCached: true,
		},
		{
			name:          "No scan-interval provided in config",
			lastScanTime:  time.Time{},
			cachedDevices: []string{},
			config: &api.DevicePluginConfig{
				DiscoveryStrategy: "time",
			},
			devices: []string{"/dev/dm-0", "/dev/dm-1"},
		},
		{
			name: "Invalid scan-interval format",
			lastScanTime: time.Time{},
			cachedDevices: []string{},
			config: &api.DevicePluginConfig{
				DiscoveryStrategy: "time",
				ScanInterval: "bad-format",
			},
			devices: []string{"/dev/dm-0", "/dev/dm-1"},
		},
		{
			name: "Non-time strategy (default)",
			lastScanTime: time.Now().Add(-10 * time.Second),
			cachedDevices: []string{"/dev/dm-0"},
			config: &api.DevicePluginConfig{
				DiscoveryStrategy: "default",
			},
			devices: []string{"/dev/dm-0", "/dev/dm-1"},
		},
		{
			name:          "Return error on scan failure and no cache",
			lastScanTime:  time.Now().Add(-2 * time.Minute),
			cachedDevices: []string{},
			config: &api.DevicePluginConfig{
				DiscoveryStrategy: "time",
				ScanInterval:      "1m",
			},
			simulateScanError: true,
			expectError:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &plugin.PowerPlugin{
				Config: tt.config,
				Cache: &plugin.DeviceCache{
					Devices:      tt.cachedDevices,
					LastScanTime: tt.lastScanTime,
				},
				Scanner: mockScanner{
					simulateScanError: tt.simulateScanError,
					devices:           tt.devices,
					config:            tt.config,
				},
			}

			devs, err := p.GetDiscoveredDevices()
			if tt.expectError {
				assert.Error(t, err)
				assert.Nil(t, devs)
				return
			}

			assert.NoError(t, err)

			expectedDevices := tt.cachedDevices
			if !tt.expectCached {
				expectedDevices = tt.devices
			}

			assert.Equal(t, expectedDevices, devs)
		})
	}
}
