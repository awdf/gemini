# Makefile for the Go AI Voice/Text Assistant

# Go parameters
GO_CMD=go
GO_BUILD=$(GO_CMD) build
GO_TEST=$(GO_CMD) test

# Tools
LINTER=golangci-lint

# Build variables, reserved project name gemini
BINARY_NAME=gemini

# PlantUML parameters
PLANTUML_JAR=plantuml.jar
PLANTUML_VERSION=1.2024.4
PLANTUML_URL=https://github.com/plantuml/plantuml/releases/download/v$(PLANTUML_VERSION)/plantuml-$(PLANTUML_VERSION).jar
UML_DIR=UML

# Default target executed when you just run `make`
.PHONY: all build launch test coverage lint diagrams clean help
all: test lint build

# Build the application binary. This matches the build step in your CI.
build:
	@echo "Building $(BINARY_NAME)..."
	$(GO_BUILD) -v -o $(BINARY_NAME) .

# Launch the application. This target depends on `build` to ensure the binary is up-to-date.
launch: build
	@echo "Launching $(BINARY_NAME)..."
	./$(BINARY_NAME) $(ARGS)

# Run tests with race detector and coverage. This matches the test step in your CI.
test:
	@echo "Running tests and generating coverage report..."
	$(GO_TEST) -v -race -coverprofile=coverage.out -covermode=atomic ./...
	@echo "Generating HTML coverage report..."
	$(GO_CMD) tool cover -html=coverage.out -o coverage.html

# View the HTML coverage report in the default browser. Depends on `test` to ensure the report is fresh.
coverage: test
	@echo "Opening coverage report..."
	@xdg-open coverage.html 2>/dev/null || open coverage.html 2>/dev/null || echo "Please open coverage.html in your browser."

# Run the linter using your existing .golangci.yml configuration.
lint:
	@echo "Running linter..."
	$(LINTER) run -v

# Generate UML diagrams from PlantUML source files.
# It will download the PlantUML jar if it doesn't exist.
diagrams: $(PLANTUML_JAR)
	@echo "Generating UML diagrams from $(UML_DIR)/*.puml..."
	@if ! command -v java >/dev/null 2>&1; then \
		echo "Error: 'java' command not found. Please install Java to run PlantUML."; \
		exit 1; \
	fi
	java -jar $(PLANTUML_JAR) -tpng $(UML_DIR)/
	@echo "Diagrams generated in $(UML_DIR)/ directory."

# Rule to download plantuml.jar if it's missing
$(PLANTUML_JAR):
	@echo "Downloading PlantUML v$(PLANTUML_VERSION)..."
	@if ! command -v wget >/dev/null 2>&1; then \
		echo "Error: 'wget' command not found. Please install wget to download PlantUML."; \
		exit 1; \
	fi
	wget -q --show-progress -O $(PLANTUML_JAR) $(PLANTUML_URL)

# Clean up build artifacts and coverage reports.
clean:
	@echo "Cleaning up..."
	rm -f $(BINARY_NAME)
	rm -f coverage.out coverage.html
	rm -f $(PLANTUML_JAR)
	rm -f $(UML_DIR)/*.png
	rm -f build.log

# Display help message with available commands.
help:
	@echo "Available commands:"
	@echo "  all      - Run tests, lint, and build the application"
	@echo "  launch   - Build and launch the application. Use ARGS to pass flags (e.g., make launch ARGS=\"--voice\")"
	@echo "  build    - Build the application binary"
	@echo "  test     - Run tests and generate coverage.out and coverage.html reports"
	@echo "  coverage - Generate and open the HTML coverage report"
	@echo "  lint     - Run the golangci-lint linter"
	@echo "  diagrams - Generate UML diagrams from .puml files in the UML/ directory."
	@echo "  clean    - Remove build artifacts, diagrams, and downloaded files"
	@echo "  help     - Display this help message"