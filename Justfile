# Justfile for pipeline

MODULES := "observers/terminal"

# Default recipe
default: test lint

# Run all tests with race detection
test:
    go test -race ./... $(for mod in {{MODULES}}; do echo "./$mod/..."; done)

# Run linters
lint:
    golangci-lint run ./... $(for mod in {{MODULES}}; do echo "./$mod/..."; done)

# Clean build artifacts
clean:
    go clean

# Run go fmt
fmt:
    go fmt ./...
    for mod in {{MODULES}}; do \
        (cd $mod && go fmt ./...); \
    done

# Run go mod tidy
tidy:
    go mod tidy
    for mod in {{MODULES}}; do \
        (cd $mod && go mod tidy); \
    done

# Check for clean git state after running fmt and tidy
check-clean: fmt tidy
    git diff --exit-code

# Run the demo (formats: terminal, plain, json)
demo format="terminal":
    cd examples/demo && go run . -format {{ format }}

demo-deploy format="terminal":
    cd examples/deploy && go run . -format {{ format }}

# Regenerate the README demo GIF
gif:
    vhs .vhs/demo.tape