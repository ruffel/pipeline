# Justfile for pipeline

# Default recipe
default: test lint

# Run all tests with race detection
test:
    go test -race ./...

# Run linters
lint:
    golangci-lint run ./...

# Clean build artifacts
clean:
    go clean

# Run go fmt
fmt:
    go fmt ./...

# Run go mod tidy
tidy:
    go mod tidy

# Check for clean git state after running fmt and tidy
check-clean: fmt tidy
    git diff --exit-code
