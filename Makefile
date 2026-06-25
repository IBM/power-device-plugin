#Arches can be: amd64 s390x arm64 ppc64le
ARCH ?= ppc64le

REGISTRY ?= quay.io/powercloud
REPOSITORY ?= power-dev-plugin
TAG ?= main

CONTAINER_RUNTIME ?= $(shell command -v podman 2> /dev/null || echo docker)

########################################################################
# Go Targets

.PHONY: build
build: fmt vet
	GOOS=linux GOARCH=$(ARCH) go build -o bin/power-dev-plugin-$(ARCH) cmd/power-dev-plugin/main.go

# darwin build is only for development purposes
.PHONY: build-plugin
build-plugin: fmt vet
	GOOS=linux GOARCH=amd64 go build -o bin/power-dev-plugin-x86_64 cmd/power-dev-plugin/main.go
	GOOS=linux GOARCH=ppc64le go build -o bin/power-dev-plugin-ppc64le cmd/power-dev-plugin/main.go
	GOOS=linux GOARCH=s390x go build -o bin/power-dev-plugin-s390x cmd/power-dev-plugin/main.go
	GOOS=darwin GOARCH=arm64 go build -o bin/power-dev-plugin-arm64 cmd/power-dev-plugin/main.go

.PHONY: build-scanner
build-scanner: fmt vet
	GOOS=linux GOARCH=$(ARCH) go build -o bin/devices-scanner cmd/scanner/main.go

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: clean
clean:
	rm -f ./bin/power-dev-plugin
	rm -rf vendor

########################################################################
# Container Targets

.PHONY: image
image:
	$(CONTAINER_RUNTIME) buildx build \
		-t $(REGISTRY)/$(REPOSITORY):$(TAG) \
		--platform linux/$(ARCH) -f build/Containerfile .

.PHONY: image-ci
image-ci:
	$(CONTAINER_RUNTIME) buildx build \
		-t $(REGISTRY)/$(REPOSITORY):$(TAG) \
		--platform linux/$(ARCH) -f build/Containerfile-build .

.PHONY: push
push:
	$(info push Container image...)
	$(CONTAINER_RUNTIME) push $(REGISTRY)/$(REPOSITORY):$(TAG)

.PHONY: push-ci
push-ci:
	$(info push ci Container image...)
	$(CONTAINER_RUNTIME) push $(REGISTRY)/$(REPOSITORY):$(TAG)

# target builds, creates an image and pushes
.PHONY: all-ci
all-ci: build-plugin image-ci push-ci

########################################################################
# Deployment Targets

.PHONY: dep-plugin
dep-plugin:
	kustomize build manifests | oc apply -f -

.PHONY: dep-examples
dep-examples:
	kustomize build examples | oc apply -f -