# syntax=docker/dockerfile:1
# Build Salcara Hub with checksum-verified modules, without operator secrets.
FROM --platform=$BUILDPLATFORM golang:1.27.1-bookworm AS verify
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY internal/enrollment ./internal/enrollment
COPY internal/hub ./internal/hub
COPY internal/edgepool ./internal/edgepool
COPY internal/hostfleet ./internal/hostfleet
COPY internal/standalone ./internal/standalone
COPY internal/launcher ./internal/launcher
COPY cmd/hub ./cmd/hub
COPY cmd/hub-launcher ./cmd/hub-launcher
COPY cmd/salcara-edge ./cmd/salcara-edge
ENV GOTOOLCHAIN=local GOPROXY=off CGO_ENABLED=0
RUN go test ./... && go vet ./...
RUN CGO_ENABLED=1 go test -race ./...
RUN GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w -buildid=" -o /out/artifacts/salcara-edge-linux-amd64 ./cmd/salcara-edge
RUN GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w -buildid=" -o /out/artifacts/salcara-edge-linux-arm64 ./cmd/salcara-edge
RUN mkdir -p /runtime/data && chmod 0700 /runtime/data && chown 65532:65532 /runtime/data

# Target arguments affect RUN cache keys even when the command does not use them.
# Keep all native checks above this boundary so multiarch builds share one run.
FROM verify AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.5.7-dev
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -buildid= -X salcara/hub/internal/standalone.Version=$VERSION" -o /out/salcara-hub ./cmd/hub
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -buildid= -X salcara/hub/internal/standalone.Version=$VERSION" -o /out/salcara-hub-launcher ./cmd/hub-launcher

FROM scratch AS binary-export
COPY --from=build /out/salcara-hub /salcara-hub
COPY --from=build /out/salcara-hub-launcher /salcara-hub-launcher
COPY --from=build /out/artifacts /artifacts

FROM scratch AS runtime
ARG VERSION=0.5.7-dev
LABEL org.opencontainers.image.title="Salcara Hub standalone" \
      org.opencontainers.image.description="Independent device pairing and remote message relay; no model API key required" \
      org.opencontainers.image.source="https://github.com/dkdjndbfj-wq/salcara-hub-plugin" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.licenses="LGPL-3.0"
COPY --from=build /out/salcara-hub /salcara-hub
COPY --from=build /out/salcara-hub-launcher /salcara-hub-launcher
COPY --from=build --chown=65532:65532 /runtime/data /data
COPY --from=build /out/artifacts /opt/salcara/artifacts
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY LICENSE THIRD_PARTY_NOTICES.md /licenses/
COPY licenses/COPYING.GPL-3.0 licenses/Go-LICENSE licenses/YAML-LICENSE licenses/YAML-NOTICE /licenses/
COPY licenses/X-CRYPTO-LICENSE licenses/X-SYS-LICENSE /licenses/
COPY licenses/MAXMINDDB-LICENSE /licenses/
COPY licenses/durable/ /licenses/durable/
USER 65532:65532
WORKDIR /data
ENV SALCARA_HUB_LISTEN=:8787 SALCARA_HUB_DATA_DIR=/data SALCARA_HUB_ADMIN_ACCOUNT_FILE=/data/admin-account.json GOMEMLIMIT=32MiB
ENV SALCARA_HUB_EDGE_ARTIFACT_DIR=/opt/salcara/artifacts
EXPOSE 8787
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/salcara-hub-launcher", "-healthcheck"]
ENTRYPOINT ["/salcara-hub-launcher"]
