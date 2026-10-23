all: build

.PHONY: all build build-kit build-controller test lint clean verify verify-generate generate go-deps image image-dev

build:
	hack/build.sh

build-kit:
	TARGETS=kit hack/build.sh

build-controller:
	TARGETS=kit-controller hack/build.sh

test:
	hack/go-test.sh

lint:
	hack/go-lint.sh

generate:
	hack/update-protobuf.sh
	hack/update-codegen.sh

verify-generate:
	hack/verify-protobuf.sh
	hack/verify-codegen.sh

verify: lint test verify-generate

clean:
	rm -rf bin/

go-deps:
	go mod tidy
	go mod vendor
	go mod verify

image:
	podman build -t localhost/kit-controller:latest .

image-dev: build-controller
	podman build -f Dockerfile.dev -t localhost/kit-controller:latest .
