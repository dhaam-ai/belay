# Multi-stage build for belay
# This Dockerfile produces a minimal image with belay and essential runtime dependencies.

# Build stage: compile belay from source
FROM golang:1.26.3-alpine AS builder

WORKDIR /build

# Copy source code
COPY go.mod go.sum ./
COPY cmd cmd
COPY internal internal
COPY pkg pkg

# Build belay with static linking
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w" \
    -o belay ./cmd/belay

# Runtime stage: minimal image with belay and essential tools
# Note: Not a scratch image because belay shells out to external tools
FROM debian:bookworm-slim

LABEL org.opencontainers.image.title="belay"
LABEL org.opencontainers.image.description="Durable, resumable orchestrator for autonomous coding agents"
LABEL org.opencontainers.image.source="https://github.com/belay-dev/belay"

# Install runtime dependencies for shell command execution
# and common build tools that belay may invoke
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    curl \
    git \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Copy belay binary from builder
COPY --from=builder /build/belay /usr/local/bin/belay

# Create a non-root user for running belay
RUN groupadd -r belay && useradd -r -g belay belay

# Set the working directory
WORKDIR /home/belay

# Drop privileges
USER belay

# Default command shows help
ENTRYPOINT ["belay"]
CMD ["--help"]
