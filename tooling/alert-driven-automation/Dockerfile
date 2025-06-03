ARG PLATFORM

# Build stage (FIPS-compliant Go base)
FROM --platform=${PLATFORM} mcr.microsoft.com/oss/go/microsoft/golang:1.24-fips-cbl-mariner2.0 AS builder

# Install tooling (optional, like `jq` for debugging)
RUN yum install --assumeyes jq

ARG CURRENT_COMMIT

# Set working directory inside container
WORKDIR /app

# Copy go.mod and go.sum separately to leverage Docker layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the app code
COPY . ./

# Build the binary with FIPS mode enabled
ENV CGO_ENABLED=1 GOFLAGS='-tags=requirefips' CURRENT_COMMIT=${CURRENT_COMMIT}
RUN go build -o /ada main.go

# Runtime stage (distroless, non-root, FIPS-compliant)
FROM --platform=${PLATFORM} mcr.microsoft.com/cbl-mariner/distroless/base:2.0-nonroot

WORKDIR /
COPY --from=builder /ada .

# Run as non-root user by default
ENTRYPOINT ["./ada"]
