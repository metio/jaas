# SPDX-FileCopyrightText: The jaas Authors
# SPDX-License-Identifier: 0BSD

FROM cgr.dev/chainguard/go AS build
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=development
ARG COMMIT=development
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o jaas

FROM cgr.dev/chainguard/static
COPY --from=build /app/jaas /usr/bin/
ENTRYPOINT ["/usr/bin/jaas"]
