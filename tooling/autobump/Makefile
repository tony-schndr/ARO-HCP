.PHONY: build update autobump clean test test-verbose test-coverage help

BINARY_NAME := autobump
CONFIG_FILE := config.yaml
BUMPER_CONFIG_FILE := autobump-config.yaml
COMPONENTS ?=
EXCLUDE_COMPONENTS ?=
COMPONENT_FLAGS := $(if $(COMPONENTS),--components $(COMPONENTS))
EXCLUDE_FLAGS := $(if $(EXCLUDE_COMPONENTS),--exclude-components $(EXCLUDE_COMPONENTS))

.DEFAULT_GOAL := help

build:
	@go build -o $(BINARY_NAME) .

autobump: build
	@./$(BINARY_NAME) bump --config $(CONFIG_FILE) --bumper-config $(BUMPER_CONFIG_FILE)

test:
	@go test ./...

test-verbose:
	@go test ./... -v

test-coverage:
	@go test ./... -cover

clean:
	rm -f $(BINARY_NAME)

help:
	@echo "Available targets:"
	@echo "  build              - Build the autobump binary"
	@echo "  update             - Update images manually (use COMPONENTS= or EXCLUDE_COMPONENTS= to filter)"
	@echo "  autobump           - Update images and create PR automatically"
	@echo "  test               - Run tests"
	@echo "  test-verbose       - Run tests with verbose output"
	@echo "  test-coverage      - Run tests with coverage report"
	@echo "  clean              - Remove build artifacts"