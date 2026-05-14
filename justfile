# forge — async coding agent

# Go 1.26.1 toolchain auto-download needs the sum database enabled
export GOSUMDB := "sum.golang.org"

# List available recipes
default:
  @just --list

# ── Build ────────────────────────────────────────────────────

# Build web frontend (requires Node.js)
build-web:
  cd web && npm ci && npm run build

# Build unified forge binary
build:
  go build -o forge ./cmd/forge

# Build everything (web + go)
build-all: build-web build

# Install unified forge binary to GOBIN (defaults to ~/go/bin)
install:
  go install ./cmd/forge

# ── Dev ──────────────────────────────────────────────────────

# Run interactive CLI (unified binary)
dev: build
  ./forge

# Build and run gateway (unified binary)
dev-gateway: build
  ./forge gateway

# Run web frontend dev server (Vite with HMR, proxies /api to localhost:3000)
dev-web:
  cd web && npm run dev

# Build and run gateway in daemon mode (unified binary)
dev-gateway-daemon: build
  ./forge gateway -daemon

# Stop daemon gateway
stop-gateway:
  @if [ -f /tmp/forge/sessions/forge.pid ]; then \
    kill $(cat /tmp/forge/sessions/forge.pid) && echo "Gateway stopped"; \
  else \
    echo "No PID file found at /tmp/forge/sessions/forge.pid"; \
  fi

# Tail gateway logs (daemon mode)
tail-gateway:
  tail -f /tmp/forge/sessions/forge.log

# Build and run CLI
dev-cli: build
  ./forge

# ── Test ──────────────────────────────────────────────────────

# Run all tests
test:
  go test ./...

# Run tests with verbose output
test-v:
  go test -v ./...

# ── Lint ──────────────────────────────────────────────────────

# Run go vet
vet:
  go vet ./...

# ── Cleanup ──────────────────────────────────────────────────

# Remove build artifacts
clean:
  rm -f forge
