# =============================================================================
# llama-swap Slim Dockerfile
# =============================================================================
# Ships ONLY the llama-swap binary with Docker CLI and nvidia-smi for sidecar
# orchestration and GPU monitoring. No AI inference engines included.
#
# Build:
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -f docker/slim/Dockerfile -t llama-swap:slim .
#
# Run:
#   docker run --gpus all \
#     -v /var/run/docker.sock:/var/run/docker.sock \
#     -v /path/to/models:/models \
#     llama-swap:slim
# =============================================================================

# ---------------------------------------------------------------------------
# Stage 1: Build the Go binary (multi-arch aware)
# ---------------------------------------------------------------------------
FROM golang:1.26.1-alpine AS go-builder

# Install build dependencies: npm for Svelte UI, git for module resolution,
# make for the build system, musl-dev for static linking.
RUN apk add --no-cache \
    npm \
    git \
    make \
    musl-dev \
    && rm -rf /var/cache/apk/*

WORKDIR /app

# Copy module files first for better layer caching.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source.
COPY . .

# Build the binary with embedded Svelte UI.
# TARGETARCH is automatically set by Docker BuildKit per platform
# (e.g., amd64, arm64). Falls back to amd64 for non-BuildKit builds.
ARG TARGETARCH=amd64
RUN make linux-${TARGETARCH}

# ---------------------------------------------------------------------------
# Stage 2: Minimal runtime image
# ---------------------------------------------------------------------------
FROM alpine:3.21

# Install runtime dependencies:
#   docker-cli          - manage sidecar containers
#   gcompat             - glibc compatibility layer; required to run the host's
#                         nvidia-smi (a glibc binary) inside this musl-based image
#   curl / jq           - HTTP calls and JSON parsing
#   tini                - proper init process (PID 1)
#   ca-certificates     - TLS verification
#   Note: nvidia-smi is mounted by the NVIDIA Container Toolkit at runtime
#   when the container is launched with --gpus all (NVIDIA_DRIVER_CAPABILITIES
#   defaults to compute,utility which includes nvidia-smi). gcompat is needed
#   because Alpine uses musl libc and nvidia-smi is a glibc binary.
RUN apk add --no-cache \
    ca-certificates \
    docker-cli \
    gcompat \
    curl \
    jq \
    && rm -rf /var/cache/apk/*

# Create config directory.
RUN mkdir -p /etc/llama-swap/config

# Copy the built binary from the builder stage.
ARG TARGETARCH=amd64
COPY --from=go-builder /app/build/llama-swap-linux-${TARGETARCH} /usr/local/bin/llama-swap

# Copy example config.
COPY config.example.yaml /etc/llama-swap/config/config.yaml

# Ensure the binary is executable.
RUN chmod +x /usr/local/bin/llama-swap

# Expose the default listen port.
EXPOSE 8080

ENTRYPOINT ["llama-swap"]
CMD ["llama-swap", "-config", "/etc/llama-swap/config/config.yaml", "-listen", "0.0.0.0:8080"]
