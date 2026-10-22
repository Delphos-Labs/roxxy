# Makefile for roxxy
# Automatically sets build time when building

# Detect OS and set executable extension
ifeq ($(OS),Windows_NT)
    EXE_EXT := .exe
    INSTALL_DIR := $(LOCALAPPDATA)/roxxy
else
    EXE_EXT :=
    INSTALL_DIR := $(HOME)/bin
endif

# Get current timestamp in UTC (works with Git Bash on Windows too)
BUILD_TIME := $(shell date -u +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || echo "unknown")

# Default output location
OUTPUT ?= roxxy$(EXE_EXT)
ROAR_OUTPUT ?= roar$(EXE_EXT)

# Build flags
LDFLAGS := -X main.BuildTime=$(BUILD_TIME)
RELEASE_LDFLAGS := $(LDFLAGS) -s -w
RELEASE_FLAGS := -trimpath

.PHONY: generate
generate:
	@go generate ./...

.PHONY: validate
validate:
	@echo "Validating HTML templates and JavaScript..."
	@go test ./internal/ui/static -run TestValidate -v
	@echo "✓ Templates and JavaScript validated"

.PHONY: build
build: generate validate
	@echo "Building roxxy with build time: $(BUILD_TIME)"
	@go build -ldflags "$(LDFLAGS)" -o $(OUTPUT) .
	@echo "Building roar (roxxy archiver)..."
	@go build -ldflags "$(LDFLAGS)" -o $(ROAR_OUTPUT) ./cmd/roar
	@echo ""
	@echo "✓ Build complete:"
	@echo "  - roxxy: $(OUTPUT)"
	@echo "  - roar:  $(ROAR_OUTPUT)"

.PHONY: install
install: generate validate
ifeq ($(OS),Windows_NT)
	@echo ""
	@echo "On Windows, run the PowerShell installer:"
	@echo ""
	@echo "  powershell -ExecutionPolicy Bypass -File scripts/install.ps1"
	@echo ""
else
	@echo "Installing to $(INSTALL_DIR)..."
	@mkdir -p $(INSTALL_DIR)
	@go build -ldflags "$(LDFLAGS)" -o $(INSTALL_DIR)/roxxy .
	@go build -ldflags "$(LDFLAGS)" -o $(INSTALL_DIR)/roar ./cmd/roar
	@echo "Installed to $(INSTALL_DIR)"
endif

.PHONY: release
release: generate validate
	@echo "Building roxxy release with build time: $(BUILD_TIME)"
	@go build -ldflags "$(RELEASE_LDFLAGS)" $(RELEASE_FLAGS) -o $(OUTPUT) .
	@go build -ldflags "$(RELEASE_LDFLAGS)" $(RELEASE_FLAGS) -o $(ROAR_OUTPUT) ./cmd/roar
	@echo ""
	@echo "✓ Release build complete:"
	@echo "  - roxxy: $(OUTPUT)"
	@echo "  - roar:  $(ROAR_OUTPUT)"

.PHONY: linux-amd64
linux-amd64: generate validate
	@echo "Cross-compiling for linux/amd64 (build time: $(BUILD_TIME))"
	@GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -ldflags "$(RELEASE_LDFLAGS)" $(RELEASE_FLAGS) -o roxxy-linux-amd64 .
	@GOOS=linux GOARCH=amd64 CGO_ENABLED=0 \
		go build -ldflags "$(RELEASE_LDFLAGS)" $(RELEASE_FLAGS) -o roar-linux-amd64 ./cmd/roar
	@echo ""
	@echo "✓ linux/amd64 build complete:"
	@echo "  - roxxy-linux-amd64"
	@echo "  - roar-linux-amd64"

.PHONY: linux-arm64
linux-arm64: generate validate
	@echo "Cross-compiling for linux/arm64 (build time: $(BUILD_TIME))"
	@GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -ldflags "$(RELEASE_LDFLAGS)" $(RELEASE_FLAGS) -o roxxy-linux-arm64 .
	@GOOS=linux GOARCH=arm64 CGO_ENABLED=0 \
		go build -ldflags "$(RELEASE_LDFLAGS)" $(RELEASE_FLAGS) -o roar-linux-arm64 ./cmd/roar
	@echo ""
	@echo "✓ linux/arm64 build complete:"
	@echo "  - roxxy-linux-arm64"
	@echo "  - roar-linux-arm64"

.PHONY: linux
linux: linux-amd64 linux-arm64

.PHONY: clean
clean:
	rm -f roxxy roar roxxy.exe roar.exe \
	      roxxy-linux-amd64 roar-linux-amd64 \
	      roxxy-linux-arm64 roar-linux-arm64

.PHONY: test
test:
	go test ./...

.DEFAULT_GOAL := build
