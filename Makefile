BINARY     = terraform-provider-oras
# Last tagged release (v-prefix stripped), falling back to 0.0.0-dev when the
# checkout has no tags (shallow clone, tarball). Override with `make install VERSION=0.1.5`
# if you need a specific mirror directory.
GIT_TAG    ?= $(shell git describe --tags --abbrev=0 2>/dev/null)
VERSION    ?= $(if $(GIT_TAG),$(patsubst v%,%,$(GIT_TAG)),0.0.0-dev)
OS_ARCH    = $(shell go env GOOS)_$(shell go env GOARCH)
PLUGIN_DIR = $(HOME)/.terraform.d/plugins/registry.terraform.io/vmvarela/oras/$(VERSION)/$(OS_ARCH)

# Minimum total coverage percentage enforced by the coverage target.
COVERAGE_THRESHOLD ?= 80

.PHONY: build install dev-override test test-zot coverage lint clean

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

# Coverage gate: validate the threshold, profile every package, fail below it.
coverage:
	@# Validate before running any tests: must be a decimal number in [0, 100].
	@if [ -z "$(COVERAGE_THRESHOLD)" ] || ! awk -v t="$(COVERAGE_THRESHOLD)" 'BEGIN { exit (t ~ /^[0-9]+(\.[0-9]+)?$$/ && t+0 >= 0 && t+0 <= 100) ? 0 : 1 }'; then \
		echo "::error::COVERAGE_THRESHOLD '$(COVERAGE_THRESHOLD)' is not a valid number between 0 and 100 (e.g. 80 or 83.9)"; \
		exit 1; \
	fi
	go test -race -count=1 -coverpkg=./... -coverprofile=coverage.out ./...
	@total=$$(go tool cover -func=coverage.out | awk '/^total:/ {gsub(/%/,"",$$3); print $$3}'); \
	echo "total coverage: $$total% (threshold: $(COVERAGE_THRESHOLD)%)"; \
	if [ $$(awk -v t="$(COVERAGE_THRESHOLD)" -v c="$$total" 'BEGIN {print (c+0 < t+0) ? 1 : 0}') -eq 1 ]; then \
		echo "::error::coverage $$total% is below the $(COVERAGE_THRESHOLD)% threshold"; \
		exit 1; \
	fi

test-zot:
	TF_ORAS_ZOT_TEST=1 go test -race -v -timeout 120s ./internal/oras/... -run Zot

lint:
	golangci-lint run ./...

clean:
	rm -f $(BINARY) coverage.out
