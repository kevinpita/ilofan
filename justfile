set positional-arguments
set shell := ["bash", "-eu", "-o", "pipefail", "-c"]

# List available recipes.
default:
    @just --list

# Format/fix and lint, then run tests, race tests, and build.
check: lint test race build

# Apply formatter and linter fixes and write .golangci-lint-report.json.
lint:
    golangci-lint run

# Format Go source through golangci-lint.
format:
    golangci-lint fmt

# Format the Nix development configuration.
format-nix:
    nixfmt devenv.nix

# Run all Go tests.
test:
    go test ./...

# Run the race detector. CGO and the C compiler are supplied by devenv.
race:
    go test -race ./...

# Run tests with coverage and show the result.
coverage:
    go test -coverprofile=coverage.out ./...
    go tool cover -func=coverage.out

# Build the application.
build:
    mkdir -p bin
    go build -o bin/ilofan ./cmd/ilofan

# Run the application with optional arguments.
run *args:
    go run ./cmd/ilofan "$@"

# Update go.mod and go.sum for the current imports.
tidy:
    go mod tidy

# Remove build output and generated reports.
clean:
    rm -rf bin coverage.out .golangci-lint-report.json
