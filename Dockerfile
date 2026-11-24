# Copyright (C) 2026 Intel Corporation
# SPDX-License-Identifier: Apache-2.0

# Build the manager binary
FROM golang:1.26 AS builder

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the go source
COPY cmd/ cmd/
COPY api/ api/
COPY internal/ internal/

# Build with hardening flags (Intel standard compiler flags and settings).
# -trimpath: strip filesystem paths for reproducibility and security.
# -ldflags "-s -w": strip debug info (-s: symbol table, -w: DWARF).
# -a: force rebuild of packages.
# -buildvcs rejected for container builds: requires .git in build context (security
# risk; bloats layer cache). VCS stamping works in Makefile builds where .git is present.
#
# Note: -buildmode=pie omitted to maintain minimal runtime image (distroless/static).
# PIE requires ELF loader, forcing distroless/base with glibc and Debian packages.
# For CGO-free Go binaries, ASLR's marginal benefit does not justify the CVE surface.
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -a -trimpath -ldflags="-s -w" -o manager ./cmd

# License texts the binary's redistribution requires: every linked module's license
# file and upstream NOTICE, and the Go standard library's license, next to the
# project's own LICENSE and NOTICE. The script fails on a module with no license
# file; the loop then checks the set against the modules the binary itself records
# (a replaced module is recorded under its replacement's path, as the script does).
COPY LICENSE NOTICE licenses/
COPY hack/list-third-party-licenses.sh hack/
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    ./hack/list-third-party-licenses.sh copy licenses/third_party && \
    missing="$(go version -m manager \
      | awk '$1 == "dep" { if (m) print m; m = $2 } $1 == "=>" { m = $2 } END { if (m) print m }' \
      | while read -r m; do [ -d "licenses/third_party/$m" ] || echo "$m"; done)" && \
    if [ -n "$missing" ]; then echo "no license text copied for: $missing" >&2; exit 1; fi

# Use distroless static nonroot for minimal attack surface.
# No libc, no package dependencies — minimal runtime image compared to base.
FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/manager .
COPY --from=builder /workspace/licenses /licenses
USER 65532:65532

HEALTHCHECK --interval=30s --timeout=3s --start-period=15s --retries=3 \
  CMD ["/manager", "healthcheck"]

ENTRYPOINT ["/manager"]
