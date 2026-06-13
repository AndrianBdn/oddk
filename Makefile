.PHONY: all build build-linux-amd64 build-linux-arm64 clean run-daemon install test test-e2e test-e2e-cleanup test-all lint fmt fmt-check

all: build

build:
	go build -buildvcs=true -o bin/oddk ./cmd/oddk

build-linux-amd64:
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -buildvcs=true -o build/oddk-linux-amd64 ./cmd/oddk

build-linux-arm64:
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -buildvcs=true -o build/oddk-linux-arm64 ./cmd/oddk

clean:
	rm -rf bin/ build/*

run-daemon: build
	./bin/oddk daemon

install: build
	sudo cp bin/oddk /usr/local/bin/

test:
	go test ./internal/...

# The e2e suite builds with the oddk_debug tag so the debug-only test endpoints
# (e.g. backup time-shift) and the DebugSetRawKV harness helper are compiled in.
# Production binaries (make build) omit the tag, so those never ship.
test-e2e: build
	@echo "🧪 Running end-to-end tests..."
	cd e2e && go run -tags oddk_debug .

test-e2e-cleanup:
	@echo "🧹 Cleaning up test containers..."
	cd e2e && go run -tags oddk_debug . --cleanup

test-all: test test-e2e

lint:
	go tool golangci-lint run

# Apply formatters (gofumpt + goimports) configured in .golangci.yml.
# Note: `make lint` does NOT format — formatters are a separate v2 subsystem.
fmt:
	go tool golangci-lint fmt

# Fail if anything is unformatted (CI / pre-commit). Prints the diff.
fmt-check:
	go tool golangci-lint fmt --diff