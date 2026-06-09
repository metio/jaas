# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD

# The builder is pinned to the native build platform so multi-arch image builds
# cross-compile through Go's GOOS/GOARCH (CGO is disabled) instead of emulating
# each target under QEMU. buildx supplies TARGETOS/TARGETARCH/TARGETVARIANT per
# target; the runtime stage below takes $TARGETPLATFORM and pulls the matching
# distroless base.
FROM --platform=$BUILDPLATFORM cgr.dev/chainguard/go AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=development
ARG COMMIT=development
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o jaas

# distroless/static publishes amd64, arm64, arm/v7, ppc64le, riscv64, and s390x
# — the same arch set every metio image ships, so the operator and the JOI
# library images it consumes are co-schedulable on any node architecture. Like
# chainguard's static image it carries no shell (the drain delay is implemented
# in the binary, not a preStop sleep).
FROM gcr.io/distroless/static:nonroot
COPY --from=build /app/jaas /usr/bin/
ENTRYPOINT ["/usr/bin/jaas"]
