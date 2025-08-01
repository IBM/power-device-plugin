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

package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jaypipes/ghw"
	"github.com/ocp-power-demos/power-dev-plugin/api"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"k8s.io/klog"

	pluginapi "k8s.io/kubelet/pkg/apis/deviceplugin/v1beta1"
)

const (
	socketFile                 = "power-dev.csi.ibm.com-reg.sock"
	socket                     = pluginapi.DevicePluginPath + socketFile
	resource                   = "power-dev-plugin/dev" // TODO: convert to use power-dev.csi.ibm.com/block"
	watchInterval              = 1 * time.Second
	preStartContainerFlag      = false
	getPreferredAllocationFlag = false
	unix                       = "unix"
	configPath                 = "/etc/power-device-plugin/config.json"
)

// DevicePluginServer is a mandatory interface that must be implemented by all plugins.
// For more information see
// https://godoc.org/k8s.io/kubernetes/pkg/kubelet/apis/deviceplugin/v1beta#DevicePluginServer
type PowerPlugin struct {
	devs   []string
	socket string

	stop     chan interface{}
	health   chan *pluginapi.Device
	restart  chan struct{}
	stopOnce sync.Once

	server *grpc.Server

	Config  *api.DevicePluginConfig
	Cache   *DeviceCache
	Scanner DeviceScanner

	DeviceUsage map[string]int
	usageLock   sync.Mutex

	pluginapi.DevicePluginServer
}

type DeviceCache struct {
	Devices      []string
	LastScanTime time.Time
	Mutex        sync.Mutex
}

// Creates a Plugin
func New() (*PowerPlugin, error) {
	// Empty array to start.
	var devs []string = []string{}
	return &PowerPlugin{
		devs:        devs,
		socket:      socket,
		stop:        make(chan interface{}),
		health:      make(chan *pluginapi.Device),
		restart:     make(chan struct{}, 1),
		Cache:       &DeviceCache{},
		DeviceUsage: make(map[string]int),
	}, nil
}

// no-action needed to get options
func (p *PowerPlugin) GetDevicePluginOptions(context.Context, *pluginapi.Empty) (*pluginapi.DevicePluginOptions, error) {
	return &pluginapi.DevicePluginOptions{
		PreStartRequired:                false,
		GetPreferredAllocationAvailable: false,
	}, nil
}

// dial establishes the gRPC communication with the registered device plugin.
func dial() (*grpc.ClientConn, error) {
	c, err := grpc.NewClient(
		unix+":"+pluginapi.KubeletSocket,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		klog.Errorf("%s device plugin unable connect to Kubelet : %v", pluginapi.KubeletSocket, err)
		return nil, err
	}

	return c, nil
}

// Start starts the gRPC server of the device plugin
func (p *PowerPlugin) Start() error {
	config, err := LoadDevicePluginConfig()
	if err != nil {
		klog.Warningf("Failed to load config file: %v. Proceeding without nx-gzip.", err)
	}

	p.Config = config

	devices, err := p.GetDiscoveredDevices()
	if err != nil {
		klog.Errorf("Scan root for devices was unsuccessful during ListAndWatch: %v", err)
		return err
	}

	p.devs = devices
	klog.Infof("Initiatlizing the devices recorded with the plugin to: %v", p.devs)

	errx := p.cleanup()
	if errx != nil {
		return errx
	}

	sock, err := net.Listen("unix", p.socket)
	if err != nil {
		klog.Errorf("failed to listen on socket: %s", err.Error())
		return err
	}

	p.server = grpc.NewServer()
	pluginapi.RegisterDevicePluginServer(p.server, p)

	// start serving from grpcServer
	go func() {
		err := p.server.Serve(sock)
		if err != nil {
			klog.Errorf("serving incoming requests failed: %s", err.Error())
		}
	}()

	// Wait for server to start by launching a blocking connection
	conn, err := dial()
	if err != nil {
		klog.Errorf("unable to dial %v", err)
		return err
	}
	conn.Close()

	// go m.healthcheck()

	return nil
}

// Stop stops the gRPC server
func (p *PowerPlugin) Stop() error {
	if p.server == nil {
		return nil
	}
	p.server.Stop()
	p.server = nil
	close(p.stop)

	return p.cleanup()
}

// Registers the device plugin for the given resourceName with Kubelet.
func (p *PowerPlugin) Register(kubeletEndpoint, resourceName string) error {
	conn, err := dial()
	//defer conn.Close()
	if err != nil {
		return err
	}
	klog.Infof("Dial kubelet endpoint %s", conn.Target())

	client := pluginapi.NewRegistrationClient(conn)
	request := &pluginapi.RegisterRequest{
		Version:      pluginapi.Version,
		Endpoint:     socketFile,
		ResourceName: resourceName,
	}

	_, err = client.Register(context.Background(), request)
	if err != nil {
		return err
	}

	return nil
}

// Lists devices and update that list according to the health status
func (p *PowerPlugin) ListAndWatch(e *pluginapi.Empty, stream pluginapi.DevicePlugin_ListAndWatchServer) error {
	klog.Infof("Listing devices: %v", p.devs)

	go p.MonitorSocketHealth()

	// Initial scan if devices list is empty
	if len(p.devs) == 0 {
		devices, err := p.GetDiscoveredDevices()
		if err != nil {
			klog.Errorf("Scan root for devices was unsuccessful during ListAndWatch: %v", err)
			return err
		}
		p.devs = devices
		klog.Infof("Updating the devices to %d total devices", len(p.devs))
	}

	// Always send device list at the beginning
	if err := stream.Send(&pluginapi.ListAndWatchResponse{Devices: convertDeviceToPluginDevices(p.devs)}); err != nil {
		klog.Errorf("Failed to send initial device list: %v", err)
		return err
	}

	for {
		select {
		case <-p.stop:
			klog.Infoln("Told to Stop...")
			return nil

		case <-p.restart:
			klog.Infoln("Told to restart...")
			p.Stop()
			return nil

		case d := <-p.health:
			//ignoring unhealthy state.
			klog.Infoln("Checking the health")
			klog.Infof("Device health update received for %s", d.ID)
			d.Health = pluginapi.Healthy

			if err := stream.Send(&pluginapi.ListAndWatchResponse{Devices: convertDeviceToPluginDevices(p.devs)}); err != nil {
				klog.Errorf("Failed to send updated device health to kubelet: %v", err)
				return err
			}
		}
	}
}

// Allocate returns list of devices for the container request.
func (p *PowerPlugin) Allocate(ctx context.Context, reqs *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	klog.Infof("Allocate request: %v", reqs)

	devices, err := p.GetDiscoveredDevices()
	if err != nil {
		klog.Errorf("Scan root for devices was unsuccessful: %v", err)
		return nil, err
	}

	config, err := LoadDevicePluginConfig()
	if err != nil {
		klog.Warningf("Failed to load config: %v", err)
	}

	upperLimit := config.UpperLimitPerDevice
	if upperLimit <= 0 {
		upperLimit = 1 // default fallback
	}
	klog.Infof("Using upper-limit per device: %d", upperLimit)

	responses := pluginapi.AllocateResponse{}

	for i, req := range reqs.ContainerRequests {
		klog.Infof("Handling container request %d: %+v", i, req)

		ds := []*pluginapi.DeviceSpec{}
		allocated := 0
		skippedDueToLimit := 0
		totalDevices := len(devices)

		p.usageLock.Lock()
		klog.Infof("Current device usage: %+v", p.DeviceUsage)
		for _, dev := range devices {
			devPath := dev
			if !strings.HasPrefix(dev, "/dev/") {
				devPath = "/dev/" + dev
			}
			count := p.DeviceUsage[devPath]
			klog.Infof("Evaluating device %s: current usage=%d, limit=%d", dev, count, upperLimit)

			if count < upperLimit {
				p.DeviceUsage[devPath]++
				klog.Infof("Allocating device %s to container. New usage: %d", dev, p.DeviceUsage[dev])

				ds = append(ds, &pluginapi.DeviceSpec{
					HostPath:      devPath,
					ContainerPath: devPath,
					// Per DeviceSpec:
					// Cgroups permissions of the device, candidates are one or more of
					// * r - allows container to read from the specified device.
					// * w - allows container to write to the specified device.
					// * m - allows container to create device files that do not yet exist.
					// We don't need `m`
					Permissions: GetValidatedPermission(config),
				})
				allocated++
				break // Allocate 1 device per container
			} else {
				klog.Infof("Device %s reached upper-limit; marking skipped", dev)
				skippedDueToLimit++
			}
		}
		p.usageLock.Unlock()

		if allocated == 0 {
			if skippedDueToLimit == totalDevices {
				klog.Errorf("All devices reached upper-limit; cannot allocate to container %d", i)
				return nil, fmt.Errorf("upper limit per device reached for all devices for container %d", i)
			}
			klog.Errorf("Insufficient devices: requested=1, allocated=0 for container %d", i)
			return nil, fmt.Errorf("not enough available devices to satisfy request for container %d", i)
		}

		response := pluginapi.ContainerAllocateResponse{
			Devices: ds,
		}
		klog.Infof("Allocate response for container %d: %+v", i, response)
		responses.ContainerResponses = append(responses.ContainerResponses, &response)
	}

	klog.Infof("Final Allocate response for all containers: %+v", responses)
	return &responses, nil
}

func convertDeviceToPluginDevices(devS []string) []*pluginapi.Device {
	klog.Infof("Converting Devices to Plugin Devices - %d", len(devS))
	devs := []*pluginapi.Device{}
	for idx := range devS {
		devs = append(devs, &pluginapi.Device{
			ID:     strconv.Itoa(idx),
			Health: pluginapi.Healthy,
		})
	}
	klog.Infoln("Conversion completed")
	return devs
}

func (p *PowerPlugin) unhealthy(dev *pluginapi.Device) {
	p.health <- dev
}

// no-action needed to configure/load et cetra
func (p *PowerPlugin) PreStartContainer(context.Context, *pluginapi.PreStartContainerRequest) (*pluginapi.PreStartContainerResponse, error) {
	return &pluginapi.PreStartContainerResponse{}, nil
}

// It's restarted, and we need to cleanup... conditionally...
func (p *PowerPlugin) cleanup() error {
	if err := os.Remove(p.socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Serve starts the gRPC server and register the device plugin to Kubelet

// Serve starts the gRPC server and register the device plugin to Kubelet
func (p *PowerPlugin) Serve() error {
	err := p.Start()
	if err != nil {
		klog.Errorf("Could not start device plugin: %v", err)
		return err
	}
	klog.Infof("Starting to serve on %s", p.socket)

	err = p.Register(pluginapi.KubeletSocket, resource)
	if err != nil {
		klog.Errorf("Could not register device plugin: %v", err)
		p.Stop()
		return err
	}
	klog.Infof("Registered device plugin with Kubelet")
	return nil
}

// Captures the Signal to shutdown the container and dispatches to the Application
func SystemShutdown() {
	// Get notified about syscall
	klog.V(1).Infof("Listening for term signals")
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	// Catch termination signals
	sig := <-sigCh
	klog.Infof("Received signal \"%v\", shutting down.", sig)
	if err := AppShutdown(); err != nil {
		klog.Errorf("stopping servers produced error: %s", err.Error())
	}
}

// Shutdown the Application
func AppShutdown() error {
	return nil
}

type DeviceScanner interface {
	GetBlockDevices() ([]string, error)
	LoadConfig() (*api.DevicePluginConfig, error)
	FindDevices(pattern string) ([]string, error)
	StatDevice(path string) error
}

type realDeviceScanner struct{}

func (r realDeviceScanner) GetBlockDevices() ([]string, error) {
	return getBlockDevices()
}

func (r realDeviceScanner) LoadConfig() (*api.DevicePluginConfig, error) {
	return LoadDevicePluginConfig()
}

func (r *realDeviceScanner) FindDevices(pattern string) ([]string, error) {
	return filepath.Glob(pattern)
}

func (r *realDeviceScanner) StatDevice(path string) error {
	_, err := os.Stat(path)
	return err
}

// scans the local disk using ghw to find the blockdevices
func ScanRootForDevicesWithDeps(scanner DeviceScanner, nxGzipEnabled bool) ([]string, error) {
	// relies on GHW_CHROOT=/host/dev
	// lsblk -f --json --paths -s | jq -r '.blockdevices[] | select(.fstype != "xfs")' | grep mpath | grep -v fstype | sort -u | wc -l
	// This may be the best way to get the devices.
	config, err := scanner.LoadConfig()
	if err != nil {
		klog.Warningf("ScanRootForDevices: failed to load config, proceeding with default behavior: %v", err)
	}
	if config == nil {
		klog.Warning("ScanRootForDevices: config is nil, using empty config")
		config = &api.DevicePluginConfig{
			NxGzip:            false,
			DiscoveryStrategy: "default",
			Permissions:       "rw",
		}
	}

	// The logic to discover, include and exclude disks dynamically. Steps are indicated with numbers
	// 1) discover: List all block devices/block disks
	devices, err := scanner.GetBlockDevices()
	if err != nil {
		return nil, err
	}

	if nxGzipEnabled {
		devices = append(devices, "/dev/crypto/nx-gzip")
		klog.Infof("nx-gzip enabled: appended /dev/crypto/nx-gzip to devices")
	}

	// 2) exclude: using configmap exclude devices
	filtered := ApplyExcludeFilters(devices, config.ExcludeDevices)

	// 3) include: Only include devices that match the include patterns and exist on the host.
	finalDevices := ApplyIncludeFilters(scanner, filtered, config.IncludeDevices)

	klog.Infof("Final filtered device list: %v", finalDevices)
	return finalDevices, nil
}

func getBlockDevices() ([]string, error) {
	block, err := ghw.Block()
	if err != nil {
		fmt.Printf("Error getting block storage info: %v", err)
		return nil, err
	}

	devices := []string{}
	fmt.Printf("DEVICE: %v\n", block)
	for _, disk := range block.Disks {
		fmt.Printf("    - DISK: %v\n", disk.Name)
		for _, part := range disk.Partitions {
			fmt.Printf("        - PART: %v\n", part.Disk.Name)
			devices = append(devices, "/dev/"+part.Name)
		}
		devices = append(devices, "/dev/"+disk.Name)
	}
	return devices, nil
}

func ApplyExcludeFilters(devices []string, excludes []string) []string {
	if excludes == nil {
		return devices
	}
	filtered := []string{}
	for _, dev := range devices {
		if MatchesAny(dev, excludes) {
			klog.V(4).Infof("Excluding device: %s", dev)
			continue
		}
		filtered = append(filtered, dev)
	}
	return filtered
}

func ApplyIncludeFilters(scanner DeviceScanner, devices []string, includes []string) []string {
	if includes == nil {
		return devices
	}
	cleaned := []string{}
	for _, item := range includes {
		p := strings.TrimSpace(item)
		if p == "" {
			klog.Warningf("Include-devices contains an empty string. Dropping entry.")
			continue
		}
		cleaned = append(cleaned, p)
	}

	if len(cleaned) == 0 {
		final := []string{}
		for _, dev := range devices {
			final = append(final, strings.TrimPrefix(dev, "/dev/"))
		}
		return final
	}

	klog.Infof("Include-devices specified, overriding with: %v", cleaned)
	final := []string{}
	for _, pattern := range cleaned {
		matches, err := scanner.FindDevices(pattern)
		if err != nil {
			klog.Warningf("Invalid include pattern: %s, skipping. Error: %v", pattern, err)
			continue
		}
		for _, dev := range matches {
			if err := scanner.StatDevice(dev); err == nil {
				final = append(final, strings.TrimPrefix(dev, "/dev/"))
				klog.V(4).Infof("Included device: %s", dev)
			} else {
				klog.Warningf("Device does not exist or is inaccessible: %s", dev)
			}
		}
	}
	return final
}

func (m *PowerPlugin) GetAllocateFunc() func(r *pluginapi.AllocateRequest, devs map[string]pluginapi.Device) (*pluginapi.AllocateResponse, error) {
	return func(r *pluginapi.AllocateRequest, devs map[string]pluginapi.Device) (*pluginapi.AllocateResponse, error) {
		devices, err := m.GetDiscoveredDevices()
		if err != nil {
			klog.Errorf("Scan root for devices was unsuccessful: %v", err)
			return nil, err
		}

		config, err := LoadDevicePluginConfig()
		if err != nil {
			klog.Warningf("Failed to load config: %v, err")
		}

		var responses pluginapi.AllocateResponse
		for _, req := range r.ContainerRequests {

			klog.V(5).Infof("Container Request: %s", req)
			response := &pluginapi.ContainerAllocateResponse{}

			// Dev: DevicesIDs and health are ignore. We are granting access to all devices needed.

			// Originally req.DeviceIds
			for i := range devices {
				response.Devices = append(response.Devices, &pluginapi.DeviceSpec{
					HostPath:      "/dev/" + devices[i],
					ContainerPath: "/dev/" + devices[i],
					// Per DeviceSpec:
					// Cgroups permissions of the device, candidates are one or more of
					// * r - allows container to read from the specified device.
					// * w - allows container to write to the specified device.
					// * m - allows container to create device files that do not yet exist.
					// We don't need `m`
					Permissions: GetValidatedPermission(config),
				})
			}

			responses.ContainerResponses = append(responses.ContainerResponses, response)
		}

		klog.Infof("Get Allocate response: %v", responses)
		return &responses, nil
	}
}

// monitoring socket health function
func (p *PowerPlugin) MonitorSocketHealth() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		for _, path := range []string{pluginapi.KubeletSocket, p.socket} {
			if _, err := os.Stat(path); os.IsNotExist(err) {
				klog.Warningf("Healthcheck: socket deleted (%s), triggering plugin restart", path)
				p.restart <- struct{}{}
				return
			} else if err != nil {
				klog.Errorf("Healthcheck: error checking %s: %v", path, err)
			}
		}
	}
}

// Read config map file
func LoadDevicePluginConfig() (*api.DevicePluginConfig, error) {
	klog.Infof("Attempting to read config file from: %s", configPath)

	info, err := os.Stat(filepath.Clean(configPath))
	if err != nil {
		if os.IsNotExist(err) {
			klog.Warningf("Config file not found at %s. Proceeding with default configuration.", configPath)
			return &api.DevicePluginConfig{}, err
		}
		klog.Warningf("Unable to stat config file: %v", err)
		return nil, err
	}

	if info.IsDir() {
		klog.Warningf("Config path %s is a directory, not a file. Proceeding with default configuration.", configPath)
		return &api.DevicePluginConfig{}, nil
	}

	data, err := os.ReadFile(filepath.Clean(configPath))
	if err != nil {
		klog.Warningf("Unable to read config file: %v", err)
		return nil, err
	}

	var config api.DevicePluginConfig
	if err := json.Unmarshal(data, &config); err != nil {
		klog.Errorf("Failed to unmarshal config file: %v", err)
		return nil, err
	}

	klog.Infof("Config loaded successfully")
	return &config, nil
}

func GetValidatedPermission(config *api.DevicePluginConfig) string {
	if config == nil {
		klog.Infof("No config provided, using default device permission: 'rwm'")
		return "rwm"
	}

	perm := strings.ToLower(config.Permissions)
	valid := map[string]bool{
		"r": true, "w": true, "m": true,
		"rw": true, "rm": true, "wm": true, "rwm": true,
	}

	if valid[perm] {
		klog.Infof("Using validated device permission: '%s'", perm)
		return perm
	}

	if perm != "" {
		klog.Warningf("Invalid device permission '%s' in config, using default 'rw'", perm)
	} else {
		klog.Infof("No permission set in config, using default 'rw'")
	}
	return "rw"
}

func MatchesAny(dev string, patterns []string) bool {
	for _, pattern := range patterns {
		matched, err := filepath.Match(pattern, dev)
		if err != nil {
			klog.Warningf("Invalid pattern: %s. Skipping...", pattern)
			continue
		}
		if matched {
			return true
		}
	}
	return false
}

func (p *PowerPlugin) GetDiscoveredDevices() ([]string, error) {
	klog.Info("GetDiscoveredDevices: starting device discovery")

	// Determine strategy
	strategy := "default"
	if p.Config != nil && p.Config.DiscoveryStrategy != "" {
		strategy = p.Config.DiscoveryStrategy
		klog.Infof("Discovery strategy set to: %s", strategy)
	} else {
		klog.Info("No discovery strategy specified, using default")
	}

	nxGzip := false
	if p.Config != nil {
		nxGzip = p.Config.NxGzip
	}
	klog.Infof("nxGzip enabled: %v", nxGzip)

	scanner := p.Scanner
	if scanner == nil {
		scanner = &realDeviceScanner{}
	}

	if strategy == "time" {
		p.Cache.Mutex.Lock()
		defer p.Cache.Mutex.Unlock()

		now := time.Now().UTC()
		klog.Infof("Current time: %v", now)

		interval := 60 * time.Minute // fallback default
		klog.Infof("Default interval is: %v", interval)

		if p.Config != nil && p.Config.ScanInterval != "" {
			parsedInterval, err := time.ParseDuration(p.Config.ScanInterval)
			if err != nil {
				klog.Warningf("Invalid scan-interval '%s': %v. Using default interval: %v", p.Config.ScanInterval, err, interval)
			} else {
				interval = parsedInterval
				klog.Infof("Parsed scan-interval successfully: %v", interval)
			}
		} else {
			klog.Warning("No scan-interval provided in config. Using default: 60m")
		}

		var timeSinceLastScan time.Duration
		if !p.Cache.LastScanTime.IsZero() {
			timeSinceLastScan = now.Sub(p.Cache.LastScanTime)
			klog.Infof("Time since last scan: %v (Last scan at: %v UTC)", timeSinceLastScan, p.Cache.LastScanTime.UTC())
		} else {
			klog.Infof("No previous scan found. Starting first device scan.")
		}

		klog.Infof("Cached devices count: %d", len(p.Cache.Devices))
		klog.Infof("Configured scan interval: %v", interval)

		if len(p.Cache.Devices) > 0 && timeSinceLastScan < interval {
			klog.Infof("Skipping rescan. Using cached devices. Next scan after: %v", p.Cache.LastScanTime.Add(interval))
			return p.Cache.Devices, nil
		}

		klog.Infof("Triggering fresh scan now (reason: interval passed or cache empty).")
		klog.Infof("scanner: %v", scanner)
		devices, err := ScanRootForDevicesWithDeps(scanner, nxGzip)
		if err != nil {
			klog.Errorf("Scan failed: %v", err)
			if len(p.Cache.Devices) > 0 {
				klog.Warning("Falling back to cached devices due to scan failure.")
				return p.Cache.Devices, nil
			}
			klog.Error("No cached devices available, returning error.")
			return nil, err
		}

		klog.Infof("Scan successful. Found %d devices.", len(devices))
		p.Cache.Devices = devices
		p.Cache.LastScanTime = now
		klog.Infof("Devices cached. Next scan will occur after: %v", now.Add(interval))
		return devices, nil
	}

	klog.Infof("Discovery strategy is '%s'. Performing fresh scan every call.", strategy)
	devices, err := ScanRootForDevicesWithDeps(scanner, nxGzip)
	if err != nil {
		klog.Errorf("Scan failed during default strategy: %v", err)
		return nil, err
	}
	klog.Infof("Scan completed with %d devices found.", len(devices))
	return devices, nil
}
