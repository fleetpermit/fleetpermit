# Copyright The FleetPermit Authors.
# SPDX-License-Identifier: Apache-2.0
#
# Builds one static binary into an empty (scratch) image.
#   CMD=cmd/fleetpermit-controller  -> fleetpermit-controller (default)
#   CMD=demo/tools/mcp-server       -> demo-mcp-tools
#   CMD=demo/tools/probe            -> demo-probe
ARG GO_IMAGE=docker.io/library/golang:1.26.8@sha256:6c2a5538f964f1c82f97ad14988bf05de100d922d159d0e398b54c7b0ca0c6c9
FROM --platform=${BUILDPLATFORM} ${GO_IMAGE} AS build
# Lets `make images` remove this stage's leftover image after a build.
LABEL io.github.fleetpermit.stage=builder
ARG TARGETOS=linux
ARG TARGETARCH
ARG CMD=cmd/fleetpermit-controller
ARG BIN=fleetpermit-controller
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
COPY demo/tools/ demo/tools/
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -trimpath -buildvcs=false \
      -ldflags "-s -w -X main.version=${VERSION}" -o /out/${BIN} ./${CMD}

FROM scratch
ARG BIN=fleetpermit-controller
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/${BIN} /${BIN}
USER 65532:65532
ENTRYPOINT ["/fleetpermit-controller"]
