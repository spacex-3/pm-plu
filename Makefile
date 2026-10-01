PLUGIN := prism-plugin
VERSION ?= 0.1.0

UNAME_S := $(shell uname -s)
ifeq ($(OS),Windows_NT)
EXT := dll
else ifeq ($(UNAME_S),Darwin)
EXT := dylib
else
EXT := so
endif

.PHONY: fmt test build package clean

fmt:
	gofmt -w .

test:
	go test ./...
	go vet ./...

build:
	mkdir -p dist
	CGO_ENABLED=1 go build -trimpath -buildvcs=false -buildmode=c-shared \
		-ldflags "-s -w -X main.pluginVersion=$(VERSION)" \
		-o dist/$(PLUGIN).$(EXT) .
	rm -f dist/$(PLUGIN).h

package: build
	python3 scripts/package.py --version $(VERSION)

clean:
	rm -rf dist
