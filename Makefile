GOLANG_VERSION := 1.25.5

APP_NAME := pdf-reader
APP_VERSION := 1.1.0

.DEFAULT_GOAL := help
.PHONY: build build-mac build-linux

help:
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n\nTargets:\n"} /^[a-zA-Z_-]+:.*?##/ \
	{ printf "  \033[36m%-15s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)

build: ## build binary for current platform
	CGO_ENABLED=0 go build -a \
	-ldflags "-s -w -X 'main.Version=$(APP_VERSION)'" \
	-o $(APP_NAME)

build-mac: ## build binary for macOS (arm64)
	CGO_ENABLED=0 go build -a \
	-ldflags "-s -w -X 'main.Version=$(APP_VERSION)'" \
	-o $(APP_NAME)_macos_arm64
	sha256sum $(APP_NAME)_macos_arm64 > $(APP_NAME)_macos_arm64.sha256

build-linux: ## build binary for Linux (amd64)
	GOOS=linux GOARCH=amd64 go build -a \
	-ldflags "-s -w -X 'main.Version=$(APP_VERSION)'" \
	-o $(APP_NAME)_linux_amd64
	sha256sum $(APP_NAME)_linux_amd64 > $(APP_NAME)_linux_amd64.sha256
