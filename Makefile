BINARY    := dra-resctrl
IMAGE     := ghcr.io/fabiendupont/dra-resctrl
TAG       := latest
MODULE    := github.com/fabiendupont/k8s-dra-driver-cache-partition

.PHONY: build test image clean

build:
	CGO_ENABLED=0 go build -o bin/$(BINARY) ./cmd/driver

test:
	go test ./...

image:
	podman build -t $(IMAGE):$(TAG) .

clean:
	rm -rf bin/
