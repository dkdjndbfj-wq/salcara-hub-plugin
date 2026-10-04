# syntax=docker/dockerfile:1
# Build Salcara Hub without external modules or operator secrets.
FROM --platform=$BUILDPLATFORM golang:1.27.1-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=0.5.0-dev
WORKDIR /src
COPY go.mod ./go.mod
COPY internal/hub ./internal/hub
COPY internal/standalone ./internal/standalone
COPY internal/launcher ./internal/launcher
COPY cmd/hub ./cmd/hub
COPY cmd/hub-launcher ./cmd/hub-launcher
ENV GOTOOLCHAIN=local GOPROXY=off CGO_ENABLED=0
RUN go test ./... && go vet ./...
RUN CGO_ENABLED=1 go test -race ./...
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -buildid= -X salcara/hub/internal/standalone.Version=$VERSION" -o /out/salcara-hub ./cmd/hub
RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -buildid= -X salcara/hub/internal/standalone.Version=$VERSION" -o /out/salcara-hub-launcher ./cmd/hub-launcher
RUN mkdir -p /runtime/data && chmod 0700 /runtime/data && chown 65532:65532 /runtime/data

FROM scratch AS binary-export
COPY --from=build /out/salcara-hub /salcara-hub
COPY --from=build /out/salcara-hub-launcher /salcara-hub-launcher

FROM scratch AS runtime
ARG VERSION=0.5.0-dev
LABEL org.opencontainers.image.title="Salcara Hub standalone" \
      org.opencontainers.image.description="Independent device pairing and remote message relay; no model API key required" \
      org.opencontainers.image.source="https://github.com/dkdjndbfj-wq/salcara-hub-plugin" \
      org.opencontainers.image.version="$VERSION" \
      org.opencontainers.image.licenses="LGPL-3.0"
COPY --from=build /out/salcara-hub /salcara-hub
COPY --from=build /out/salcara-hub-launcher /salcara-hub-launcher
COPY --from=build --chown=65532:65532 /runtime/data /data
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY LICENSE THIRD_PARTY_NOTICES.md /licenses/
COPY licenses/COPYING.GPL-3.0 licenses/Go-LICENSE /licenses/
USER 65532:65532
WORKDIR /data
ENV SALCARA_HUB_LISTEN=:8787 SALCARA_HUB_DATA_DIR=/data SALCARA_HUB_ADMIN_ACCOUNT_FILE=/data/admin-account.json GOMEMLIMIT=32MiB
EXPOSE 8787
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["/salcara-hub-launcher", "-healthcheck"]
ENTRYPOINT ["/salcara-hub-launcher"]
