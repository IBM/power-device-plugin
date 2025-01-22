# Power Device Plugin

Power Device Plugin to add protected devices into a non-privileged container. The Power Device Plugin uses the [Kubernetes Device Plugin](https://kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/device-plugins/) in order to add specific devices to the given Pod.

## Steps

### Installation
The device plugin only installs on the workers.

1. To deploy the device plugin using [kustomize](https://kustomize.io/): 

``` shell
# kustomize build manifests/ | oc apply -f -
```

-or-

1. Create the resources directly:

``` shell
oc apply -f manifests/00-project.yaml
oc apply -f manifests/01-sa.yaml
oc apply -f manifests/02-rbac.yaml
oc apply -f manifests/03-daemonset.yaml
```

These resources need to be created by a user with ClusterAdmin privileges.

### Uninstallation

The device plugin only uninstalls from the workers.

1. To undeploy the device plugin using [kustomize](https://kustomize.io/): 

``` shell
# kustomize build manifests/ | oc delete -f -
```

-or-

1. Create the resources directly:

``` shell
oc delete -f manifests/00-project.yaml
oc delete -f manifests/01-sa.yaml
oc delete -f manifests/02-rbac.yaml
oc delete -f manifests/03-daemonset.yaml
```

These resources need to be created by a user with ClusterAdmin privileges.

## Debug
#### Debug DaemonSet
To debug the running plugin, you can use: 

```
export GRPC_GO_LOG_VERBOSITY_LEVEL=99
export GRPC_GO_LOG_SEVERITY_LEVEL=info
```

Thse are commented out in the DaemonSet.

#### Debug Kubelet

You can check the kubelet behavior using:

```
# journalctl -u kubelet
...
 7446 handler.go:95] "Registered client" name="power-dev-plugin/dev"
wrapper[7446]: I1219 04:32:20.722778    7446 manager.go:230] "Device plugin connected" resourceName="power-dev-plugin/dev"
wrapper[7446]: I1219 04:32:20.723559    7446 client.go:93] "State pushed for device plugin" resource="power-dev-plugin/dev" re>
wrapper[7446]: I1219 04:32:20.726284    7446 manager.go:279] "Processed device updates for resource" resourceName="power-dev-p>
wrapper[7446]: I1219 04:32:27.293908    7446 setters.go:333] "Updated capacity for device plugin" plugin="power-dev-plugin/dev>
```

### Debug Socket

To debug the socket connection:
1. Connect to the worker
``` shell
ssh core@worker-0
```

2. Change to Root
``` shell
sudo -s
```

3. Check the socket is live
``` shell
nc -U /var/lib/kubelet/device-plugins/power-dev.csi.ibm.com-reg.sock
```

#### Resource Usage

In practice, the plugin only used around 1m core, and 18mi memory as shown below:

```
[root@bastion ~]# oc adm top pods -n power-device-plugin
NAME                               CPU(cores)   MEMORY(bytes)
power-dev-mutate-c7f8b4689-rfhhf   1m           18Mi
power-device-plugin-2fsdv          1m           20Mi
power-device-plugin-jwqmm          1m           20Mi
power-device-plugin-mkpbv          1m           21Mi
power-device-plugin-pp4vw          1m           19Mi
power-device-plugin-x9szk          1m           21Mi
power-device-plugin-zkxkz          1m           20Mi
```

### Sample

1. To deploy the sample: `kustomize build examples | oc apply -f -`

## Build

The build includes multiple architectures: `linux/amd64`, `linux/ppc64le`, `linux/s390x`.
The build uses the [ubi9/ubi:9.4](https://catalog.redhat.com/software/containers/ubi9/ubi/615bcf606feffc5384e8452e?architecture=ppc64le&image=676258d7607921b4d7fcb8c8&gti-tabs=unauthenticated) image.

## Bugs

### kubelet issues
```
Jan 01 16:25:32 worker-0 kubenswrapper[36781]: E0117 16:25:32.788348   36781 client.go:90] "ListAndWatch ended unexpectedly for device plugin" err="rpc error: code = Unavailable desc = error reading from server: EOF" resource="power-dev-plugin/dev"
```
indicates a problem with the socket and you'll want to enable logging per [Editing kubelet log level verbosity and gathering logs](https://docs.openshift.com/container-platform/4.8/rest_api/editing-kubelet-log-level-verbosity.html)

Command is... 
```
echo -e "[Service]\nEnvironment=\"KUBELET_LOG_LEVEL=8\"" > /etc/systemd/system/kubelet.service.d/30-logging.conf
```

You should see why it's failing there.

### Checking Allocations

```
# oc describe nodes | grep power-dev-pl
  power-dev-plugin/dev:         742
  power-dev-plugin/dev:         742
  power-dev-plugin/dev         0               0
  power-dev-plugin/dev:         742
  power-dev-plugin/dev:         742
  power-dev-plugin/dev         1            1
  power-dev-plugin/dev:         928
  power-dev-plugin/dev:         928
  power-dev-plugin/dev         0               0
  power-dev-plugin/dev:         928
  power-dev-plugin/dev:         928
  power-dev-plugin/dev         0               0
  power-dev-plugin/dev:         928
  power-dev-plugin/dev:         928
  power-dev-plugin/dev         0               0
  power-dev-plugin/dev:         0
  power-dev-plugin/dev:         0
  power-dev-plugin/dev         0               0
  power-dev-plugin/dev:         928
  power-dev-plugin/dev:         928
  power-dev-plugin/dev         0               0
```

## Sources

1. https://github.com/intel/intel-device-plugins-for-kubernetes/blob/main/pkg/deviceplugin/manager.go#L96
2. https://github.com/kairen/simple-device-plugin/tree/master
3. https://github.com/kubernetes/kubelet/tree/master/pkg/apis/deviceplugin/v1beta1
4. https://kubernetes.io/docs/concepts/extend-kubernetes/compute-storage-net/device-plugins/#examples
5. https://github.com/k8stopologyawareschedwg/sample-device-plugin/blob/main/pkg/deviceplugin/deviceplugin.go