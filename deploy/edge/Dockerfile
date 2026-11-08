# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.27.1-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/edgepool ./internal/edgepool
COPY cmd/salcara-edge ./cmd/salcara-edge
ENV GOTOOLCHAIN=local GOPROXY=off CGO_ENABLED=0
RUN go test ./... && go vet ./...
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -buildid=" -o /out/salcara-edge ./cmd/salcara-edge
RUN mkdir -p /runtime/data && chmod 0700 /runtime/data && chown 65532:65532 /runtime/data

FROM scratch
COPY --from=build /out/salcara-edge /salcara-edge
COPY --from=build --chown=65532:65532 /runtime/data /data
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY LICENSE THIRD_PARTY_NOTICES.md /licenses/
COPY licenses/COPYING.GPL-3.0 licenses/Go-LICENSE licenses/YAML-LICENSE licenses/YAML-NOTICE licenses/MAXMINDDB-LICENSE licenses/X-SYS-LICENSE /licenses/
USER 65532:65532
WORKDIR /data
ENV SALCARA_EDGE_DATA_DIR=/data GOMEMLIMIT=96MiB
ENTRYPOINT ["/salcara-edge"]
