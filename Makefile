PLUGIN_ID     := cpa-credential-budget
GOOS          ?= $(shell go env GOOS)
GOARCH        ?= $(shell go env GOARCH)
VERSION       := $(shell sed -n 's/.*PluginVersion = "\(.*\)".*/\1/p' internal/plugin/plugin.go)
EXT           := $(if $(filter darwin,$(GOOS)),dylib,$(if $(filter windows,$(GOOS)),dll,so))
DIST          := dist
TARGET        := $(DIST)/$(PLUGIN_ID).$(EXT)
PKG           := $(PLUGIN_ID)_$(VERSION)_$(GOOS)_$(GOARCH).zip

.PHONY: all build test vet fmt package clean

all: test build

build:
	CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -tags cshared -buildmode=c-shared \
		-o $(TARGET) ./cmd/$(PLUGIN_ID)
	@echo "built $(TARGET)"

test:
	CGO_ENABLED=1 go test -race ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

package: build
	@mkdir -p $(DIST)
	@rm -f $(DIST)/$(PKG)
	@if command -v zip >/dev/null 2>&1; then \
		cd $(DIST) && zip -j $(PKG) $(PLUGIN_ID).$(EXT); \
	else \
		python3 -c "import zipfile; zipfile.ZipFile(r'$(DIST)/$(PKG)', 'w', zipfile.ZIP_DEFLATED).write(r'$(TARGET)', '$(PLUGIN_ID).$(EXT)')"; \
	fi
	@echo "packaged $(DIST)/$(PKG)"

clean:
	rm -rf $(DIST)
