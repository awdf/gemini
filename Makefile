# Makefile for the Go AI Voice/Text Assistant

# Go parameters
GO_CMD=go
GO_BUILD=$(GO_CMD) build
GO_TEST=$(GO_CMD) test

# Tools
LINTER=golangci-lint

# Build variables, reserved project name gemini
BINARY_NAME=gemini

# Default target executed when you just run `make`
.PHONY: all
all: test lint build

# Build the application binary. This matches the build step in your CI.
.PHONY: build
build:
	@echo "Building $(BINARY_NAME)..."
	$(GO_BUILD) -v -o $(BINARY_NAME) .

# Launch the application. This target depends on `build` to ensure the binary is up-to-date.
# Renamed from 'run' to 'launch' for better IDE/plugin integration.
# You can pass arguments to the application like this: make launch ARGS="--voice --no-ai"
.PHONY: launch
launch: build
	@echo "Launching $(BINARY_NAME)..."
	./$(BINARY_NAME) $(ARGS)

# Run tests with race detector and coverage. This matches the test step in your CI.
.PHONY: test
test:
	@echo "Running tests and generating coverage report..."
	$(GO_TEST) -v -race -coverprofile=coverage.out -covermode=atomic ./...
	@echo "Generating HTML coverage report..."
	$(GO_CMD) tool cover -html=coverage.out -o coverage.html

# View the HTML coverage report in the default browser. Depends on `test` to ensure the report is fresh.
.PHONY: coverage
coverage: test
	@echo "Opening coverage report..."
	@xdg-open coverage.html 2>/dev/null || open coverage.html 2>/dev/null || echo "Please open coverage.html in your browser."

# Run the linter using your existing .golangci.yml configuration.
.PHONY: lint
lint:
	@echo "Running linter..."
	$(LINTER) run -v

# Clean up build artifacts and coverage reports.
.PHONY: clean
clean:
	@echo "Cleaning up..."
	rm -f $(BINARY_NAME)
	rm -f coverage.out coverage.html

# Display help message with available commands.
.PHONY: help
help:
	@echo "Available commands:"
	@echo "  all      - Run tests, lint, and build the application"
	@echo "  launch   - Build and launch the application. Use ARGS to pass flags (e.g., make launch ARGS=\"--voice\")"
	@echo "  build    - Build the application binary"
	@echo "  test     - Run tests and generate coverage.out and coverage.html reports"
	@echo "  coverage - Generate and open the HTML coverage report"
	@echo "  lint     - Run the golangci-lint linter"
	@echo "  clean    - Remove build artifacts"
	@echo "  help     - Display this help message"