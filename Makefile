PLUGIN_ID := cpa-subscription-value
# Version from the nearest tag (v0.1.0 -> 0.1.0); falls back to 0.1.0 with no tags.
VERSION ?= $(shell git describe --tags --always 2>/dev/null | sed -e 's/^v//')
ifeq ($(strip $(VERSION)),)
VERSION := 0.1.0
endif
GOOS ?= $(shell go env GOOS)
GOARCH ?= $(shell go env GOARCH)
EXT := so
ifeq ($(GOOS),darwin)
EXT := dylib
endif
ifeq ($(GOOS),windows)
EXT := dll
endif
DIST := dist
OUT := $(DIST)/$(PLUGIN_ID).$(EXT)
ZIP := $(DIST)/$(PLUGIN_ID)_$(VERSION)_$(GOOS)_$(GOARCH).zip
LDFLAGS := -s -w -X main.pluginVersion=$(VERSION)
# ARTIFACT is the shared library to zip; `package` sets it to OUT, CI points it at a Docker output.
ARTIFACT ?= $(OUT)

.PHONY: build test vet build-linux-amd64 build-linux-arm64 package zip checksums clean

build:
	@mkdir -p $(DIST)
	CGO_ENABLED=1 GOOS=$(GOOS) GOARCH=$(GOARCH) go build -trimpath -buildvcs=false -buildmode=c-shared \
		-ldflags "$(LDFLAGS)" -o $(OUT) ./cmd/plugin
	rm -f $(DIST)/*.h

test:
	go test ./...

vet:
	go vet ./...

# glibc-compatible .so for the CPA image (debian bookworm), built in Docker.
build-linux-amd64:
	docker build -f Dockerfile.build --platform linux/amd64 --build-arg VERSION=$(VERSION) \
		--output $(DIST)/linux/amd64 .

build-linux-arm64:
	docker build -f Dockerfile.build --platform linux/arm64 --build-arg VERSION=$(VERSION) \
		--output $(DIST)/linux/arm64 .

package: build
	$(MAKE) zip ARTIFACT=$(OUT) GOOS=$(GOOS) GOARCH=$(GOARCH) VERSION=$(VERSION)

# Zip ARTIFACT as <id>.<ext> at the archive root, then refresh checksums.txt.
zip:
	@test -f "$(ARTIFACT)" || { echo "ARTIFACT $(ARTIFACT) not found" >&2; exit 1; }
	@mkdir -p $(DIST)
	@rm -f $(ZIP)
	@tmp=$$(mktemp -d) && cp "$(ARTIFACT)" "$$tmp/$(PLUGIN_ID).$(EXT)" && \
		(cd "$$tmp" && zip -9 -q "$(abspath $(ZIP))" "$(PLUGIN_ID).$(EXT)") && rm -rf "$$tmp"
	@echo "Created $(ZIP)"
	$(MAKE) checksums

checksums:
	@cd $(DIST) && { command -v sha256sum >/dev/null 2>&1 && sha256sum *.zip || shasum -a 256 *.zip; } > checksums.txt
	@cat $(DIST)/checksums.txt

clean:
	rm -rf $(DIST)
