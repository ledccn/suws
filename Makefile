# Su WebSocket Makefile

# Binary name
BINARY_NAME=suws

# Go parameters
GOCMD=go
GOBUILD=$(GOCMD) build
GOCLEAN=$(GOCMD) clean
GOTEST=$(GOCMD) test
GOGET=$(GOCMD) get
GOMOD=$(GOCMD) mod
GOFMT=$(GOCMD) fmt
GOVET=$(GOCMD) vet

# Build variables
VERSION?=dev
COMMIT=$(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME=$(shell TZ=Asia/Shanghai date '+%Y-%m-%d_%H:%M:%S')
LDFLAGS=-ldflags "-X main.Version=$(VERSION) -X main.Commit=$(COMMIT) -X main.BuildTime=$(BUILD_TIME)"

# Default target
.PHONY: all
all: build

# Build the binary
.PHONY: build
build:
	$(GOBUILD) $(LDFLAGS) -o $(BINARY_NAME) -v

# Build for multiple platforms
.PHONY: build-all
build-all: build-linux build-windows build-darwin

.PHONY: build-linux
build-linux:
	GOOS=linux GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BINARY_NAME)-linux-amd64 -v
	GOOS=linux GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(BINARY_NAME)-linux-arm64 -v

.PHONY: build-windows
build-windows:
	GOOS=windows GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BINARY_NAME)-windows-amd64.exe -v

.PHONY: build-darwin
build-darwin:
	GOOS=darwin GOARCH=amd64 $(GOBUILD) $(LDFLAGS) -o $(BINARY_NAME)-darwin-amd64 -v
	GOOS=darwin GOARCH=arm64 $(GOBUILD) $(LDFLAGS) -o $(BINARY_NAME)-darwin-arm64 -v

# Run the application
.PHONY: run
run:
	$(GOBUILD) -o $(BINARY_NAME) -v
	./$(BINARY_NAME)

# Run with custom config
.PHONY: run-config
run-config:
	$(GOBUILD) -o $(BINARY_NAME) -v
	./$(BINARY_NAME) -config $(CONFIG)


# Clean build artifacts
.PHONY: clean
clean:
	$(GOCLEAN)
	rm -f $(BINARY_NAME)
	rm -f $(BINARY_NAME)-*
	rm -f coverage.out coverage.html

# Format code
.PHONY: fmt
fmt:
	$(GOFMT) ./...

# Vet code
.PHONY: vet
vet:
	$(GOVET) ./...

# Download dependencies
.PHONY: deps
deps:
	$(GOMOD) download


# Tidy dependencies
.PHONY: tidy
tidy:
	$(GOMOD) tidy

# Verify dependencies
.PHONY: verify
verify:
	$(GOMOD) verify

# Install the binary to $GOPATH/bin
.PHONY: install
install:
	$(GOCMD) install $(LDFLAGS)

# Uninstall the binary from $GOPATH/bin
.PHONY: uninstall
uninstall:
	rm -f $(GOPATH)/bin/$(BINARY_NAME)


# Development build with race detector
.PHONY: dev
dev:
	$(GOBUILD) -race $(LDFLAGS) -o $(BINARY_NAME) -v
	./$(BINARY_NAME)

# Check code quality (fmt, vet)
.PHONY: check
check: fmt vet