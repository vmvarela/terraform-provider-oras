BINARY     = terraform-provider-orastate
VERSION   ?= 0.1.0
OS_ARCH    = $(shell go env GOOS)_$(shell go env GOARCH)
PLUGIN_DIR = $(HOME)/.terraform.d/plugins/registry.terraform.io/vmvarela/orastate/$(VERSION)/$(OS_ARCH)

.PHONY: build install test test-zot lint clean

build:
	go build -o $(BINARY) .

install: build
	mkdir -p $(PLUGIN_DIR)
	cp $(BINARY) $(PLUGIN_DIR)/$(BINARY)

test:
	go test -race -count=1 ./...

test-zot:
	TF_ORAS_ZOT_TEST=1 go test -race -v -timeout 120s ./internal/oras/... -run Zot

lint:
	golangci-lint run ./...

clean:
	rm -f $(BINARY)
