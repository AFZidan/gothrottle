# GoThrottle Makefile
# Common development commands for the GoThrottle rate limiting library

.PHONY: help build test test-race test-cover test-bench clean fmt lint vet security deps verify install cross-build docker-test coverage-html coverage-check mod-tidy

# Default target
.DEFAULT_GOAL := help

# Variables
GO_VERSION := $(shell go version | cut -d ' ' -f 3)
GOPATH := $(shell go env GOPATH)
GOOS := $(shell go env GOOS)
GOARCH := $(shell go env GOARCH)
COVERAGE_OUT := coverage.out
COVERAGE_HTML := coverage.html
MIN_COVERAGE := 60

# Colors for output
RED := \033[0;31m
GREEN := \033[0;32m
YELLOW := \033[0;33m
BLUE := \033[0;34m
PURPLE := \033[0;35m
CYAN := \033[0;36m
WHITE := \033[0;37m
NC := \033[0m # No Color

help: ## Show this help message
	@echo "$(CYAN)GoThrottle Development Commands$(NC)"
	@echo "$(YELLOW)Go Version: $(GO_VERSION)$(NC)"
	@echo ""
	@awk 'BEGIN {FS = ":.*?## "} /^[a-zA-Z_-]+:.*?## / {printf "$(GREEN)%-20s$(NC) %s\n", $$1, $$2}' $(MAKEFILE_LIST)

# Build commands
build: ## Build the project
	@echo "$(BLUE)Building GoThrottle...$(NC)"
	go build -v ./...

build-race: ## Build with race detector
	@echo "$(BLUE)Building with race detector...$(NC)"
	go build -race -v ./...

cross-build: ## Build for multiple platforms
	@echo "$(BLUE)Cross-compiling for multiple platforms...$(NC)"
	@echo "$(YELLOW)Building for Linux AMD64...$(NC)"
	GOOS=linux GOARCH=amd64 go build ./...
	@echo "$(YELLOW)Building for Linux ARM64...$(NC)"
	GOOS=linux GOARCH=arm64 go build ./...
	@echo "$(YELLOW)Building for macOS AMD64...$(NC)"
	GOOS=darwin GOARCH=amd64 go build ./...
	@echo "$(YELLOW)Building for macOS ARM64...$(NC)"
	GOOS=darwin GOARCH=arm64 go build ./...
	@echo "$(YELLOW)Building for Windows AMD64...$(NC)"
	GOOS=windows GOARCH=amd64 go build ./...
	@echo "$(GREEN)Cross-compilation completed!$(NC)"

# Testing commands
test: ## Run tests
	@echo "$(BLUE)Running tests...$(NC)"
	go test -v ./tests/...

test-race: ## Run tests with race detector
	@echo "$(BLUE)Running tests with race detector...$(NC)"
	go test -v -race ./tests/...

test-cover: ## Run tests with coverage
	@echo "$(BLUE)Running tests with coverage...$(NC)"
	go test -v -race -coverprofile=$(COVERAGE_OUT) -coverpkg=./... ./tests/...
	@echo "$(GREEN)Coverage report generated: $(COVERAGE_OUT)$(NC)"

test-bench: ## Run benchmarks
	@echo "$(BLUE)Running benchmarks...$(NC)"
	go test -bench=. -benchmem ./tests/...

test-all: test-race test-cover test-bench ## Run all tests (race, coverage, benchmarks)

coverage-html: test-cover ## Generate HTML coverage report
	@echo "$(BLUE)Generating HTML coverage report...$(NC)"
	go tool cover -html=$(COVERAGE_OUT) -o $(COVERAGE_HTML)
	@echo "$(GREEN)HTML coverage report generated: $(COVERAGE_HTML)$(NC)"

coverage-check: test-cover ## Check if coverage meets minimum threshold
	@echo "$(BLUE)Checking coverage threshold...$(NC)"
	@coverage=$$(go tool cover -func=$(COVERAGE_OUT) | grep total | awk '{print $$3}' | sed 's/%//'); \
	if [ $$(echo "$$coverage >= $(MIN_COVERAGE)" | bc -l) -eq 1 ]; then \
		echo "$(GREEN)✓ Coverage $$coverage% meets minimum threshold of $(MIN_COVERAGE)%$(NC)"; \
	else \
		echo "$(RED)✗ Coverage $$coverage% is below minimum threshold of $(MIN_COVERAGE)%$(NC)"; \
		exit 1; \
	fi

# Code quality commands
fmt: ## Format code
	@echo "$(BLUE)Formatting code...$(NC)"
	gofmt -s -w .
	@echo "$(GREEN)Code formatted!$(NC)"

fmt-check: ## Check if code is formatted
	@echo "$(BLUE)Checking code formatting...$(NC)"
	@unformatted=$$(gofmt -s -l .); \
	if [ -n "$$unformatted" ]; then \
		echo "$(RED)The following files are not formatted properly:$(NC)"; \
		echo "$$unformatted"; \
		exit 1; \
	else \
		echo "$(GREEN)✓ All files are properly formatted$(NC)"; \
	fi

vet: ## Run go vet
	@echo "$(BLUE)Running go vet...$(NC)"
	go vet ./...
	@echo "$(GREEN)✓ go vet passed$(NC)"

lint: ## Run golangci-lint
	@echo "$(BLUE)Running golangci-lint...$(NC)"
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run --timeout=5m; \
	elif [ -f "$(GOPATH)/bin/golangci-lint" ]; then \
		$(GOPATH)/bin/golangci-lint run --timeout=5m; \
	else \
		echo "$(RED)golangci-lint not installed. Run: make install-tools$(NC)"; \
		exit 1; \
	fi
	@echo "$(GREEN)✓ Linting passed$(NC)"

security: ## Run security scan with gosec
	@echo "$(BLUE)Running security scan...$(NC)"
	@if command -v gosec >/dev/null 2>&1; then \
		gosec ./...; \
	elif [ -f "$(GOPATH)/bin/gosec" ]; then \
		$(GOPATH)/bin/gosec ./...; \
	else \
		echo "$(RED)gosec not installed. Run: make install-tools$(NC)"; \
		exit 1; \
	fi
	@echo "$(GREEN)✓ Security scan completed$(NC)"

# Quality gate - run all checks
quality: fmt-check vet lint security ## Run all quality checks (format, vet, lint, security)
	@echo "$(GREEN)✓ All quality checks passed!$(NC)"

# Dependency management
deps: ## Download dependencies
	@echo "$(BLUE)Downloading dependencies...$(NC)"
	go mod download
	@echo "$(GREEN)✓ Dependencies downloaded$(NC)"

verify: ## Verify dependencies
	@echo "$(BLUE)Verifying dependencies...$(NC)"
	go mod verify
	@echo "$(GREEN)✓ Dependencies verified$(NC)"

mod-tidy: ## Tidy up go.mod and go.sum
	@echo "$(BLUE)Tidying up modules...$(NC)"
	go mod tidy
	@echo "$(GREEN)✓ Modules tidied$(NC)"

mod-update: ## Update dependencies to latest versions
	@echo "$(BLUE)Updating dependencies...$(NC)"
	go get -u ./...
	go mod tidy
	@echo "$(GREEN)✓ Dependencies updated$(NC)"

# Tool installation
install-tools: ## Install development tools
	@echo "$(BLUE)Installing development tools...$(NC)"
	@echo "$(YELLOW)Installing golangci-lint...$(NC)"
	go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest
	@echo "$(YELLOW)Installing gosec...$(NC)"
	go install github.com/securego/gosec/v2/cmd/gosec@latest
	@echo "$(GREEN)✓ Development tools installed$(NC)"

# Docker commands
docker-test: ## Run tests in Docker with Redis
	@echo "$(BLUE)Running tests in Docker with Redis...$(NC)"
	docker-compose -f docker-compose.test.yml up --build --abort-on-container-exit --exit-code-from test
	docker-compose -f docker-compose.test.yml down

# CI simulation
ci: deps verify quality test-all cross-build ## Simulate CI pipeline locally
	@echo "$(GREEN)✓ CI simulation completed successfully!$(NC)"

# Development workflow
dev: fmt vet test ## Quick development workflow (format, vet, test)
	@echo "$(GREEN)✓ Development workflow completed!$(NC)"

# Clean up
clean: ## Clean build artifacts and coverage files
	@echo "$(BLUE)Cleaning up...$(NC)"
	rm -f $(COVERAGE_OUT) $(COVERAGE_HTML)
	go clean -cache -testcache -modcache
	@echo "$(GREEN)✓ Cleanup completed$(NC)"

# Release preparation
release-check: ci coverage-check ## Full release readiness check
	@echo "$(GREEN)✓ Release readiness check completed!$(NC)"
	@echo "$(CYAN)Project is ready for release!$(NC)"

# Quick commands for daily use
quick-test: fmt vet test ## Quick test cycle
	@echo "$(GREEN)✓ Quick test cycle completed!$(NC)"

quick-build: fmt vet build ## Quick build cycle
	@echo "$(GREEN)✓ Quick build cycle completed!$(NC)"

# Information commands
info: ## Show project information
	@echo "$(CYAN)GoThrottle Project Information$(NC)"
	@echo "$(YELLOW)Go Version:$(NC) $(GO_VERSION)"
	@echo "$(YELLOW)GOOS:$(NC) $(GOOS)"
	@echo "$(YELLOW)GOARCH:$(NC) $(GOARCH)"
	@echo "$(YELLOW)GOPATH:$(NC) $(GOPATH)"
	@echo "$(YELLOW)Project Path:$(NC) $(PWD)"
	@echo ""
	@echo "$(BLUE)Available targets:$(NC)"
	@$(MAKE) help

# Watch mode (requires entr or similar tool)
watch-test: ## Watch files and run tests on changes (requires 'entr')
	@echo "$(BLUE)Watching for changes... (Press Ctrl+C to stop)$(NC)"
	@if command -v entr >/dev/null 2>&1; then \
		find . -name "*.go" | entr -c make quick-test; \
	else \
		echo "$(RED)entr not installed. Install with: brew install entr (macOS) or apt-get install entr (Ubuntu)$(NC)"; \
	fi
