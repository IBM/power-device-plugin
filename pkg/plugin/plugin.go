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
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/jaypipes/ghw"
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
)

// DevicePluginServer is a mandatory interface that must be implemented by all plugins.
// For more information see
// https://godoc.org/k8s.io/kubernetes/pkg/kubelet/apis/deviceplugin/v1beta#DevicePluginServer
type PowerPlugin struct {
	devs   []string
	socket string

	stop   chan interface{}
	health chan *pluginapi.Device

	server *grpc.Server

	pluginapi.DevicePluginServer
}

// Creates a Plugin
func New() (*PowerPlugin, error) {
	// Empty array to start.
	var devs []string = []string{}
	return &PowerPlugin{
		devs:   devs,
		socket: socket,
		stop:   make(chan interface{}),
		health: make(chan *pluginapi.Device),
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
	devices, err := ScanRootForDevices()
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
	klog.Infof("ListAndWatch: Listing devices: %v", p.devs)

	// Initial scan if devices list is empty
	if len(p.devs) == 0 {
		devices, err := ScanRootForDevices()
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
	klog.Infof("ListAndWatch: for loop %+v\n: ", p)

	for {
		klog.Infof("ListAndWatch: In for loop")
		select {
		case <-p.stop:
			klog.Infoln("Told to Stop...")
			klog.Infof("ListAndWatch: Exit")
			return nil

		case d := <-p.health:
			//ignoring unhealthy state.
			klog.Infoln("Checking the health")
			klog.Infof("Device health update received for %s", d.ID)
			d.Health = pluginapi.Healthy

			if err := stream.Send(&pluginapi.ListAndWatchResponse{Devices: convertDeviceToPluginDevices(p.devs)}); err != nil {
				klog.Errorf("Failed to send updated device health to kubelet: %v", err)
				klog.Infof("ListAndWatch: Exit")
				return err // Let kubelet re-initiate plugin
			}
		}
	}
}

// Allocate which return list of devices.
func (p *PowerPlugin) Allocate(ctx context.Context, reqs *pluginapi.AllocateRequest) (*pluginapi.AllocateResponse, error) {
	klog.Infof("Allocate request: %v", reqs)

	devices, err := ScanRootForDevices()
	if err != nil {
		klog.Errorf("Scan root for devices was unsuccessful: %v", err)
		return nil, err
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
				Permissions: "rw",
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
func ScanRootForDevices() ([]string, error) {
	// relies on GHW_CHROOT=/host/dev
	// lsblk -f --json --paths -s | jq -r '.blockdevices[] | select(.fstype != "xfs")' | grep mpath | grep -v fstype | sort -u | wc -l
	// This may be the best way to get the devices.
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
			devices = append(devices, part.Name)
		}
		devices = append(devices, disk.Name)
	}
	return devices, nil
}

func (m *PowerPlugin) GetAllocateFunc() func(r *pluginapi.AllocateRequest, devs map[string]pluginapi.Device) (*pluginapi.AllocateResponse, error) {
	return func(r *pluginapi.AllocateRequest, devs map[string]pluginapi.Device) (*pluginapi.AllocateResponse, error) {
		devices, err := ScanRootForDevices()
		if err != nil {
			klog.Errorf("Scan root for devices was unsuccessful: %v", err)
			return nil, err
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
					Permissions: "rw",
				})
			}

			responses.ContainerResponses = append(responses.ContainerResponses, response)
		}
		return &responses, nil
	}
}
