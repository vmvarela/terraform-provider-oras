BINARY     = terraform-provider-oras
VERSION   ?= 0.1.0
OS_ARCH    = $(shell go env GOOS)_$(shell go env GOARCH)
PLUGIN_DIR = $(HOME)/.terraform.d/plugins/registry.terraform.io/vmvarela/oras/$(VERSION)/$(OS_ARCH)

.PHONY: build install dev-override test test-zot lint clean

build:
	go build -o $(BINARY) .

install: build
	mkdir -p $(PLUGIN_DIR)
	cp $(BINARY) $(PLUGIN_DIR)/$(BINARY)

# ponytail: generated, not committed — dev_overrides needs an absolute path
dev-override:
	@printf 'provider_installation {\n  dev_overrides {\n    "vmvarela/oras" = "%s"\n  }\n  direct {}\n}\n' '$(CURDIR)' > .terraformrc.dev
	@echo 'wrote .terraformrc.dev -> $(CURDIR)'
	@echo 'use it: export TF_CLI_CONFIG_FILE=$(CURDIR)/.terraformrc.dev'

test:
	go test -race -count=1 ./...

test-zot:
	TF_ORAS_ZOT_TEST=1 go test -race -v -timeout 120s ./internal/oras/... -run Zot

lint:
	golangci-lint run ./...

clean:
	rm -f $(BINARY)
