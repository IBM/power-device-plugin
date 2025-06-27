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

	nxGzip bool

	pluginapi.DevicePluginServer
}

// Creates a Plugin
func New() (*PowerPlugin, error) {
	// Empty array to start.
	var devs []string = []string{}
	return &PowerPlugin{
		devs:    devs,
		socket:  socket,
		stop:    make(chan interface{}),
		health:  make(chan *pluginapi.Device),
		restart: make(chan struct{}, 1),
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
	config, err := loadDevicePluginConfig()
	if err != nil {
		klog.Warningf("Failed to load config file: %v. Proceeding without nx-gzip.", err)
	}

	p.nxGzip = config != nil && config.NxGzip
	klog.Infof("nxGzip enabled: %v", p.nxGzip)

	devices, err := ScanRootForDevices(p.nxGzip)
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

	go p.monitorSocketHealth()

	// Initial scan if devices list is empty
	if len(p.devs) == 0 {
		devices, err := ScanRootForDevices(p.nxGzip)
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

// Allocate which return list of devices.
func (p *PowerPlugin) Allocate(ctx context.Context, reqs *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	klog.Infof("Allocate request: %v", reqs)

	devices, err := ScanRootForDevices(p.nxGzip)
	if err != nil {
		klog.Errorf("Scan root for devices was unsuccessful: %v", err)
		return nil, err
	}

	config, err := loadDevicePluginConfig()
	if err != nil {
		klog.Warningf("Failed to load config: %v")
	}

	responses := pluginapi.AllocateResponse{}
	for _, req := range reqs.ContainerRequests {
		klog.Infoln("Container requests device: ", req)
		ds := make([]*pluginapi.DeviceSpec, len(devices))

		response := pluginapi.ContainerAllocateResponse{
			Devices: ds,
		}

		// Originally req.DeviceIds
		for i := range devices {
			ds[i] = &pluginapi.DeviceSpec{
				HostPath:      "/dev/" + devices[i],
				ContainerPath: "/dev/" + devices[i],
				// Per DeviceSpec:
				// Cgroups permissions of the device, candidates are one or more of
				// * r - allows container to read from the specified device.
				// * w - allows container to write to the specified device.
				// * m - allows container to create device files that do not yet exist.
				// We don't need `m`
				Permissions: getValidatedPermission(config),
			}
		}
		responses.ContainerResponses = append(responses.ContainerResponses, &response)
	}
	klog.Infof("Allocate response: %v", responses)
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

// func (p *PowerPlugin) Run() int {
// 	klog.V(0).Info("Start Run")
// 	stopCh := make(chan struct{})
// 	defer close(stopCh)

// 	exitCh := make(chan error)
// 	defer close(exitCh)

// 	for {
// 		select {
// 		case <-stopCh:
// 			klog.V(0).Info("Run(): stopping plugin")
// 			return 0
// 		case err := <-exitCh:
// 			klog.Error(err, "got an error", err)
// 			return 99
// 		}
// 	}
// }

// Kublet may restart, and we'll need to restart.
// func monitorPluginRegistration() error {
// 	return nil
// }

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

// scans the local disk using ghw to find the blockdevices
func ScanRootForDevices(nxGzipEnabled bool) ([]string, error) {
	// relies on GHW_CHROOT=/host/dev
	// lsblk -f --json --paths -s | jq -r '.blockdevices[] | select(.fstype != "xfs")' | grep mpath | grep -v fstype | sort -u | wc -l
	// This may be the best way to get the devices.
	config, err := loadDevicePluginConfig()
	if err != nil {
		klog.Warningf("ScanRootForDevices: failed to load config, proceeding with default behavior: %v", err)
		config = &api.DevicePluginConfig{}
	}

	// The logic to discover, include and exclude disks dynamically. Steps are indicated with numbers
	// 1) discover: List all block devices/block disks
	devices, err := getBlockDevices()
	if err != nil {
		return nil, err
	}

	if nxGzipEnabled {
		devices = append(devices, "/dev/crypto/nx-gzip")
		klog.Infof("nx-gzip enabled: appended /dev/crypto/nx-gzip to devices")
	}

	// 2) exclude: using configmap exclude devices
	filtered := applyExcludeFilters(devices, config.ExcludeDevices)

	// 3) include: Only include devices that match the include patterns and exist on the host.
	finalDevices := applyIncludeFilters(filtered, config.IncludeDevices)

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

func applyExcludeFilters(devices []string, excludes []string) []string {
	filtered := []string{}
	for _, dev := range devices {
		if matchesAny(dev, excludes) {
			klog.V(4).Infof("Excluding device: %s", dev)
			continue
		}
		filtered = append(filtered, dev)
	}
	return filtered
}

func applyIncludeFilters(devices []string, includes []string) []string {
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
		// No include logic, return everything (minus excludes)
		final := []string{}
		for _, dev := range devices {
			final = append(final, strings.TrimPrefix(dev, "/dev/"))
		}
		return final
	}

	klog.Infof("Include-devices specified, overriding with: %v", cleaned)
	final := []string{}
	for _, pattern := range cleaned {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			klog.Warningf("Invalid include pattern: %s, skipping. Error: %v", pattern, err)
			continue
		}
		for _, dev := range matches {
			if _, err := os.Stat(dev); err == nil {
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
		devices, err := ScanRootForDevices(m.nxGzip)
		if err != nil {
			klog.Errorf("Scan root for devices was unsuccessful: %v", err)
			return nil, err
		}

		config, err := loadDevicePluginConfig()
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
					Permissions: getValidatedPermission(config),
				})
			}

			responses.ContainerResponses = append(responses.ContainerResponses, response)
		}

		klog.Infof("Get Allocate response: %v", responses)
		return &responses, nil
	}
}

// monitoring socket health function
func (p *PowerPlugin) monitorSocketHealth() {
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
func loadDevicePluginConfig() (*api.DevicePluginConfig, error) {
	klog.Infof("Attempting to read config file from: %s", configPath)

	info, err := os.Stat(filepath.Clean(configPath))
	if err != nil {
		if os.IsNotExist(err) {
			klog.Warningf("Config file not found at %s. Proceeding with default configuration.", configPath)
			return &api.DevicePluginConfig{}, nil
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

func getValidatedPermission(config *api.DevicePluginConfig) string {
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

func matchesAny(dev string, patterns []string) bool {
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
