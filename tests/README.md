This document outlines the test cases:

## *Deployment*

### 1. *Case 1*: Install
Install using README.md

Is the power-device-plugin started?
Does the kubelet recognize the plugin on the workers?

### 2. *Case 2*: Uninstall
Uninstall using README.md

Is the power-device-plugin cleaned up?

### 3. *Case 3*: Upgrade
Upgrade using README.md

Does the Pod report changes, does it restart

## *Use*
The following assume the Device Plugin is installed and registered with `kubelet`.

### 1. *Case 1* Kubelet Restart
1. deploy ex-device-plugin
2. delete ex-device-plugin
3. restart kubelet while delete/create Pod is happening

Kubelet should report correct availability.
Plugin shouldn't report unrecoverable errors.
Pod should deploy.

### 2. *Case 2* Kubelet Restart
1. deploy ex-device-plugin
2. restart kubelet while Pod is running

Kubelet should report correct availability.
Plugin shouldn't report unrecoverable errors.
Pod should deploy.

### 3. *Case 3* Pod Restart
1. deploy ex-device-plugin
2. Loop over Pod deletion/creation at least to the size of the power-dev/dev resource type

Kubelet should report correct availability.
Plugin shouldn't report unrecoverable errors.
Pod should deploy.

This test is to ensure no resource exhaustion.

### 4. *Case 4*: Simple Scale - 2 per available node
1. deploy ex-device-plugin
2. Scale the number of Pods to per Node `oc scale deployment/ex-device-plugin -n ex-device-plugin --replicas=4` 4 if 2 node, 2 if 1 node.

Kubelet should report correct availability.
Plugin shouldn't report unrecoverable errors.
Node should account for resources correctly `oc describe nodes | grep power-dev`
Pods should deploy.

### 5. *Case 5*: Complex Scale - More than available
1. deploy ex-device-plugin
2. Scale the number of Pods over the number of power-dev/dev resource type limit `oc scale deployment/ex-device-plugin -n ex-device-plugin --replicas=50`

Kubelet should report correct availability.
Plugin shouldn't report unrecoverable errors.
Node should account for resources correctly `oc describe nodes | grep power-dev`
Pods should deploy.

## *Security*
These tests cases are to be determined and worked on.