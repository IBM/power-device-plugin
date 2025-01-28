### Example
0. Create namespace `oc new-project ex-device-plugin`
1. Install `kustomize build examples/ | oc apply -f -`
2. Uninstall `kustomize build examples/ | oc delete -f -`

To remove stuck namespace:

```
oc get namespace "ex-device-plugin" -o json \
  | tr -d "\n" | sed "s/\"finalizers\": \[[^]]\+\]/\"finalizers\": []/" \
  | oc replace --raw /api/v1/namespaces/ex-device-plugin/finalize -f -
```

### Storage Class Tests
You will need to change `storageclass-tbd` in 03-pvc.yaml to work with your storageclass.