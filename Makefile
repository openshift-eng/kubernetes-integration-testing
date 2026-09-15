all: build

.PHONY: all build test lint clean verify go-deps

build:
	hack/build.sh

test:
	hack/go-test.sh

lint:
	hack/go-lint.sh

verify: lint test

clean:
	rm -rf bin/

go-deps:
	go mod tidy
	go mod vendor
	go mod verify
