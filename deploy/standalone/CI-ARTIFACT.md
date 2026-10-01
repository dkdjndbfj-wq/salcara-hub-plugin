# Development Docker build — not a signed update release

This CI artifact contains the exact unsigned Hub/launcher binaries from the
tested source for linux/amd64 and cross-built linux/arm64, the tested amd64 Docker
image archive, and the arm64 OCI archive. It contains no operator data volume,
admin token, device secret, publisher private key, signed feed, or mobile APK.

The amd64 archive can be imported with:

```sh
docker load -i salcara-hub-linux-amd64.docker.tar
```

Its image name is
`salcara-hub-standalone:ci`, not a production registry address. The arm64 OCI
archive is a build/export result, not evidence of an arm64 runtime test; use an
OCI-aware import tool. Prefer the documented Compose source build for an actual
deployment, and separately validate HTTPS, pairing and desktop capabilities.

These unsigned files must not be placed into `/data/updates` or supplied to the
web application updater. That updater only accepts a published, Ed25519-signed
version feed whose binary sizes and SHA-256 hashes match the actual release.
The launcher, operating system image and container configuration still require
a Docker image replacement to update. See docs/STANDALONE-DOCKER.zh.md.
