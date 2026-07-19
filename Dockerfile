# xscaler-agent: the OpAMP supervisor bundled with the OTel collector it manages.
#
# The published opampsupervisor image ships the supervisor only (no collector
# binary), so we graft a collector in. Two targets:
#
#   default (LAST stage) — supervisor + prebuilt otel/opentelemetry-collector-contrib.
#                          Cheap/fast build. `docker build .` selects this.
#   ebpf                 — supervisor + an OCB-built collector that adds the OBI
#                          (OpenTelemetry eBPF Instrumentation) receiver for
#                          zero-code auto-instrumentation. Heavy build (clones
#                          OBI, runs the Collector Builder). `--target ebpf`.
#
# Both flavours ride the same OTEL_VERSION and differ only by the `obi` receiver.
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
COPY builder.yaml /build/builder.yaml
RUN go run "go.opentelemetry.io/collector/cmd/builder@v${OTEL_VERSION}" \
      --config /build/builder.yaml \
    && test -x /build/otelcol-contrib

FROM otel/opentelemetry-collector-contrib:${OTEL_VERSION} AS contrib

# ebpf target: supervisor base + the OCB-built collector (has `obi`).
FROM ghcr.io/open-telemetry/opentelemetry-collector-releases/opentelemetry-collector-opampsupervisor:${OTEL_VERSION} AS ebpf
COPY --from=ocb-build /build/otelcol-contrib /usr/local/bin/otelcol-contrib

# default target (LAST stage): supervisor base + prebuilt contrib collector.
FROM ghcr.io/open-telemetry/opentelemetry-collector-releases/opentelemetry-collector-opampsupervisor:${OTEL_VERSION} AS default
COPY --from=contrib /otelcol-contrib /usr/local/bin/otelcol-contrib
