# NOTE: This Dockerfile builds cmd/timestampwriter (the long-running AKS workload).
# The setup wizard (cmd/setup) has its own separate Dockerfile: Dockerfile.setup
# Reason: different binary, different image tag, different deployment lifecycle
# (setup is ephemeral; timestampwriter is persistent). Build and version them independently.
# =============================================================================

# =============================================================================
# Stage 1 — builder
# Purpose: Compile the Go binary in a full Go toolchain environment.
#          This stage is discarded after build; nothing here reaches prod.
# =============================================================================
FROM golang:1.22-alpine AS builder

WORKDIR /src

# Copy dependency manifests first so Docker cache is reused when only source changes.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source tree.
COPY . .

# Build a statically-linked binary (CGO_ENABLED=0) so it runs in the
# distroless final image, which has no libc or shell.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /timestampwriter ./cmd/timestampwriter

# =============================================================================
# Stage 2 — runtime
# Purpose: Minimal, non-root, read-only image for production.
#
# Choice: gcr.io/distroless/static-debian12:nonroot
#   - No shell, no package manager, no libc — drastically reduces attack surface.
#   - The :nonroot tag sets UID/GID 65532, satisfying runAsNonRoot in K8s.
#   - "static-debian12" is appropriate for CGO_ENABLED=0 Go binaries with no
#     system library dependencies.
#   - If you ever need shell access for debugging, swap to alpine:3.20 and add
#     RUN addgroup -S app && adduser -S app -G app  +  USER app.
# =============================================================================
FROM gcr.io/distroless/static-debian12:nonroot

# Copy only the compiled binary from the builder stage.
COPY --from=builder /timestampwriter /timestampwriter

# No secrets. No credentials. All identity is injected at runtime by the
# AKS Workload Identity webhook via projected service account token.

ENTRYPOINT ["/timestampwriter"]
