# SPDX-License-Identifier: AGPL-3.0-only
# Minimal xb-voice image. Pure Go (CGO off), static binary, no shell, non-root (uid 10001).
# Private module access: github.com/openpak/xb-common is a private repo, so the build
# needs a netrc build secret (CI writes one from GITHUB_TOKEN; locally use your own):
#   podman build --secret id=netrc,src=$HOME/.netrc -t ghcr.io/openpak/xb-voice:dev .
ARG GO_IMAGE=docker.io/library/golang:1.26-bookworm
FROM --platform=$BUILDPLATFORM ${GO_IMAGE} AS build
ARG TARGETOS TARGETARCH
ENV GOPRIVATE=github.com/openpak/* GONOSUMDB=github.com/openpak/* GOFLAGS=-buildvcs=false
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=secret,id=netrc,target=/root/.netrc go mod download
COPY . .
RUN --mount=type=secret,id=netrc,target=/root/.netrc \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath -ldflags="-s -w" -o /out/xb-voice ./cmd/xb-voice

FROM scratch
COPY --from=build /out/xb-voice /xb-voice
USER 10001:10001
ENTRYPOINT ["/xb-voice"]
