all: build

.PHONY: all build test lint clean verify verify-generate generate go-deps

build:
	hack/build.sh

test:
	hack/go-test.sh

lint:
	hack/go-lint.sh

generate:
	hack/update-protobuf.sh

verify-generate:
	hack/verify-protobuf.sh

verify: lint test verify-generate

clean:
	rm -rf bin/

go-deps:
	go mod tidy
	go mod vendor
	go mod verify
