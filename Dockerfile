# syntax=docker/dockerfile:1.7
#
# xscaler-collector: one OCB-built collector for every role, plus the OpAMP
# supervisor that manages it.
#
# Every build stage runs on the build host's platform and cross-compiles with
# GOOS/GOARCH, so a multi-arch image needs no QEMU and the same file produces
# the Windows and macOS binaries:
#
#   docker buildx build --platform linux/amd64,linux/arm64 .            # image
#   docker buildx build --platform linux/arm64 --target binary -o dist .  # binaries
#   docker buildx build --platform windows/amd64 --target binary -o dist .
#
# Versions default to versions.env; scripts/check-pins.sh keeps them in sync.
ARG GO_VERSION=1.27.3
ARG OBI_GENERATOR_VERSION=0.2.17
ARG ALPINE_VERSION=3.22

# OBI's eBPF objects are not in its published Go module. Run OBI's own
# `make generate` on the release tag; bpf2go emits objects for amd64 and arm64.
FROM --platform=$BUILDPLATFORM ghcr.io/open-telemetry/obi-generator:${OBI_GENERATOR_VERSION} AS obi-gen
ARG OBI_VERSION=0.15.2
RUN git clone --depth 1 --branch "v${OBI_VERSION}" \
      https://github.com/open-telemetry/opentelemetry-ebpf-instrumentation /src/obi
# llvm-strip lives only in the llvm bin dir; append it so bpf2go finds it.
RUN cd /src/obi && PATH="$PATH:$(ls -d /usr/lib/llvm*/bin | tail -1)" make generate

# OCB generates the distribution's Go sources (no compile yet).
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-trixie AS sources
ARG OTEL_VERSION=0.163.0
ARG VERSION=0.0.0-dev
COPY --from=obi-gen /src/obi /src/obi
# Our own components; builder.yaml points at them with `path:`.
COPY processor /src/processor
WORKDIR /build
COPY builder.yaml .
RUN sed -i "s/^  version: .*/  version: ${VERSION}/" builder.yaml
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go run "go.opentelemetry.io/collector/cmd/builder@v${OTEL_VERSION}" \
      --config builder.yaml --skip-compilation \
 && cd _build && go mod download

FROM sources AS collector
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    cd _build \
 && ext=""; [ "${TARGETOS}" = "windows" ] && ext=".exe"; \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
      go build -trimpath -ldflags="-s -w" -o "/out/xscaler-collector${ext}" .

# The supervisor's go.mod has local `replace` lines into the contrib tree, so
# `go install ...@version` refuses it. Build from a sparse checkout of the tag.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-trixie AS supervisor-src
ARG OTEL_VERSION=0.163.0
RUN git clone --depth 1 --filter=blob:none --sparse \
      --branch "cmd/opampsupervisor/v${OTEL_VERSION}" \
      https://github.com/open-telemetry/opentelemetry-collector-contrib /src/contrib \
 && cd /src/contrib \
 && git sparse-checkout set cmd/opampsupervisor \
 && git sparse-checkout add $(grep '=> \.\./\.\./' cmd/opampsupervisor/go.mod \
      | awk '{print $NF}' | sed 's#^\.\./\.\./##' | sort -u)

FROM supervisor-src AS supervisor
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    cd /src/contrib/cmd/opampsupervisor \
 && ext=""; [ "${TARGETOS}" = "windows" ] && ext=".exe"; \
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
      go build -trimpath -ldflags="-s -w" -o "/out/opampsupervisor${ext}" .

# `--target binary -o dist` exports both binaries for the requested platform.
FROM scratch AS binary
COPY --from=collector /out/ /
COPY --from=supervisor /out/ /

# `--target sources-out -o .build` exports the generated trees GoReleaser
# compiles in the release workflow: collector/ (OCB output, local replaces
# made relative), obi/ (the generated OBI tree), processor/ (our components)
# and contrib/ (the supervisor's sparse checkout). Git metadata is dropped; nothing in the build reads it.
FROM sources AS sources-export
COPY --from=supervisor-src /src/contrib /out/contrib
RUN cp -a /build/_build /out/collector \
 && cp -a /src/obi /out/obi \
 && cp -a /src/processor /out/processor \
 && rm -rf /out/obi/.git /out/contrib/.git \
 && cd /out/collector \
 && go mod edit -replace go.opentelemetry.io/obi=../obi \
      -replace github.com/xscaler/xscaler-agent/processor/xscalerpolicyprocessor=../processor/xscalerpolicyprocessor

FROM scratch AS sources-out
COPY --from=sources-export /out/ /

# Default target (LAST stage): the image every Kubernetes role runs.
FROM alpine:${ALPINE_VERSION} AS image
ARG VERSION=0.0.0-dev
ARG OTEL_VERSION=0.163.0
ARG OBI_VERSION=0.15.2
LABEL org.opencontainers.image.title="xscaler-collector" \
      org.opencontainers.image.source="https://github.com/xscaler/xscaler-agent" \
      org.opencontainers.image.licenses="Apache-2.0" \
      org.opencontainers.image.version="${VERSION}" \
      org.opentelemetry.collector.version="${OTEL_VERSION}" \
      io.xscaler.obi.version="${OBI_VERSION}"
# Same user, uid and entrypoint as the upstream opampsupervisor image this
# replaces, so the chart's securityContext and args work unchanged.
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -g 10001 -S opampsupervisor \
 && adduser -u 10001 -S -G opampsupervisor -h /home/opampsupervisor -s /sbin/nologin opampsupervisor
COPY --from=collector --chmod=755 /out/xscaler-collector /usr/local/bin/xscaler-collector
COPY --from=supervisor --chmod=755 /out/opampsupervisor /usr/local/bin/opampsupervisor
# Supervisor configs written for the old images exec otelcol-contrib.
RUN ln -s xscaler-collector /usr/local/bin/otelcol-contrib
USER opampsupervisor
WORKDIR /home/opampsupervisor
ENTRYPOINT ["opampsupervisor"]
