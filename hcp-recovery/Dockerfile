ARG PLATFORM

FROM --platform=${PLATFORM} mcr.microsoft.com/oss/go/microsoft/golang:1.26-azurelinux3.0 AS builder
ENV GOTOOLCHAIN=local GOWORK=/app/go.work GOFLAGS=-mod=vendor GOPROXY=off
WORKDIR /app
COPY . .
ARG ARO_HCP_REVISION
ENV CGO_ENABLED=1 GOFIPS140=latest
RUN --mount=type=cache,target=/go/cache \
    GOCACHE=/go/cache \
    make --directory hcp-recovery build ARO_HCP_REVISION=${ARO_HCP_REVISION}

FROM --platform=${PLATFORM} mcr.microsoft.com/azurelinux/distroless/base:3.0
USER 65532:65532
WORKDIR /
COPY --from=builder /app/hcp-recovery/bin/hcp-recovery .
ARG ARO_HCP_REVISION
ARG ARO_HCP_COMMIT_TIME
LABEL vcs-ref="${ARO_HCP_REVISION}"
LABEL vcs-ref-time="${ARO_HCP_COMMIT_TIME}"
LABEL vcs-url="https://github.com/Azure/ARO-HCP"
ENTRYPOINT ["/hcp-recovery", "controller"]