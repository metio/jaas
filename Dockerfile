# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD

# The builder is pinned to the native build platform so multi-arch image builds
# cross-compile through Go's GOOS/GOARCH (CGO is disabled) instead of emulating
# each target under QEMU. buildx supplies TARGETOS/TARGETARCH/TARGETVARIANT per
# target; the runtime stage below takes $TARGETPLATFORM and pulls the matching
# distroless base. The builder is Docker Hub's official golang image, pinned in
# lockstep with dev/Containerfile so prod and dev builds agree — cgr.dev
# throttles anonymous pulls, making its large Go builder layer very slow to
# fetch in CI.
FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.4@sha256:32c0e6e5c4f6707717051091b4d0b077464a679eaab563e11474efc5328e2aa5 AS build
# Recent golang base images default GOTOOLCHAIN=local, which blocks auto-download
# of a higher toolchain directive in go.mod. `auto` lets go.mod pin a newer
# toolchain than this base image without a Dockerfile change.
ENV GOTOOLCHAIN=auto
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

# Generate the CRDs from the Go API types into /crds so the image carries them
# freshly derived from the source — the authoritative copy the Helm chart vendors
# (and a future build can stop committing them entirely). controller-gen runs on
# $BUILDPLATFORM, so its output is arch-independent yaml regardless of the target.
# The version is the go.mod `tool` directive's — one source of truth, bumped by
# Renovate's gomod manager, no pinned version to keep in sync here.
RUN go tool controller-gen crd paths=./api/... output:crd:dir=/crds

# distroless/static publishes amd64, arm64, arm/v7, ppc64le, riscv64, and s390x
# — the same arch set every metio image ships, so the operator and the JOI
# library images it consumes are co-schedulable on any node architecture. It
# carries no shell (the drain delay is implemented in the binary, not a preStop
# sleep).
FROM gcr.io/distroless/static:nonroot@sha256:963fa6c544fe5ce420f1f54fb88b6fb01479f054c8056d0f74cc2c6000df5240
COPY --from=build /app/jaas /usr/bin/
# The generated CRDs ride along so downstream tooling (the Helm chart's
# vendoring step) can extract them straight from the released image.
COPY --from=build /crds /crds
ENTRYPOINT ["/usr/bin/jaas"]
