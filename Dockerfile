# xscaler-agent: the OpAMP supervisor bundled with the OTel collector it manages.
#
# The published opampsupervisor image ships the supervisor only (no collector
# binary), so we graft in our own OCB-built collector (builder.yaml): the
# contrib subset our templates use, the OBI eBPF receiver and the
# xscaler_policy processor. One distribution, two targets:
#
#   default (LAST stage) — `docker build .` selects this.
#   ebpf                 — the same binary, kept as a target so the `-ebpf`
#                          image tag the Helm chart selects keeps resolving.
#
# Both targets carry identical binaries; OBI stays idle unless a config names
# the `obi` receiver.
ARG OTEL_VERSION=0.151.0
ARG OBI_VERSION=v0.9.0

FROM ghcr.io/open-telemetry/obi-generator:0.2.12 AS obi-gen
ARG OBI_VERSION
RUN git clone --depth 1 --branch "${OBI_VERSION}" \
      https://github.com/open-telemetry/opentelemetry-ebpf-instrumentation /src/obi
# llvm-strip lives only in llvm20/bin here; append it so bpf2go finds it. No
# `|| true` — a generate failure must fail the build.
RUN cd /src/obi && PATH="$PATH:/usr/lib/llvm20/bin" make generate

FROM golang:1.25-bookworm AS ocb-build
ARG OTEL_VERSION
COPY --from=obi-gen /src/obi /src/obi
COPY processor /src/processor
COPY builder.yaml /build/builder.yaml
RUN go run "go.opentelemetry.io/collector/cmd/builder@v${OTEL_VERSION}" \
      --config /build/builder.yaml \
    && test -x /build/otelcol-contrib

# ebpf target: supervisor base + the OCB-built collector.
FROM ghcr.io/open-telemetry/opentelemetry-collector-releases/opentelemetry-collector-opampsupervisor:${OTEL_VERSION} AS ebpf
ARG OTEL_VERSION
ARG OBI_VERSION
# The image is tagged with xscaler's own distro version; these labels record the
# bundled upstream versions so `docker inspect` reveals what's inside.
LABEL org.opentelemetry.collector.version="${OTEL_VERSION}" \
      io.xscaler.obi.version="${OBI_VERSION}"
COPY --from=ocb-build /build/otelcol-contrib /usr/local/bin/otelcol-contrib

# default target (LAST stage): supervisor base + the same OCB-built collector.
FROM ghcr.io/open-telemetry/opentelemetry-collector-releases/opentelemetry-collector-opampsupervisor:${OTEL_VERSION} AS default
ARG OTEL_VERSION
ARG OBI_VERSION
LABEL org.opentelemetry.collector.version="${OTEL_VERSION}" \
      io.xscaler.obi.version="${OBI_VERSION}"
COPY --from=ocb-build /build/otelcol-contrib /usr/local/bin/otelcol-contrib
