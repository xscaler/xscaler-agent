# xscaler-agent

The xscaler telemetry agent — an [OpenTelemetry
Collector](https://opentelemetry.io/docs/collector/) distribution that enrolls
into xscaler over [OpAMP](https://opentelemetry.io/docs/specs/opamp/) and is
centrally managed, with optional [eBPF
auto-instrumentation](https://opentelemetry.io/docs/zero-code/obi/) for shipping
metrics, logs, and traces to xscaler.

The image bundles the OpenTelemetry **opampsupervisor** with the
**otelcol-contrib** collector it manages. The published supervisor image ships
the supervisor only (no collector binary), so this distribution grafts a
collector in. The supervisor connects to xscaler, presents an enrollment token,
and applies the remote configuration xscaler assigns to the agent's labels.

Everything here is **Apache-2.0**: the collector (core + contrib), the
opampsupervisor, and OBI. See [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).

## Build targets

The [`Dockerfile`](Dockerfile) has two flavours that ride the same
`OTEL_VERSION` and differ **only** by the `obi` eBPF receiver:

| Flavour | Build | Collector | `obi` receiver? |
| --- | --- | --- | --- |
| **default** | `docker build .` | prebuilt `otel/opentelemetry-collector-contrib`, grafted in | no |
| **ebpf** | `docker build --target ebpf .` | custom OCB build incl. `obi` | yes |

The `default` flavour is fast — the OCB compile lives only in the `ebpf` target's
dependency graph, so a plain `docker build` never pays for it. The `ebpf`
flavour needs a custom [OpenTelemetry Collector
Builder](https://opentelemetry.io/docs/collector/custom-collector/) build
because the prebuilt contrib binary doesn't ship `obi`; that build clones OBI
and runs a Go + BPF toolchain, so it is slow.

### Version pins

Both flavours are pinned via build args in the `Dockerfile`:

- `OTEL_VERSION` (`0.151.0`) — fleet-wide collector/supervisor release.
- `OBI_VERSION` (`v0.9.0`) — OBI module tag **and** source-checkout tag (`ebpf` only).

`OTEL_VERSION` is pinned to the release OBI `v0.9.0` is built against (contrib
`v0.151.0` / core `v1.57.0`): OBI can't be mixed into an older core, so it sets
the floor for both flavours. When bumping, move `OBI_VERSION`, every pin in
[`builder.yaml`](builder.yaml), and `OTEL_VERSION` together.

## Images

Published to GitHub Container Registry (public, multi-arch `linux/amd64` +
`linux/arm64`):

```
ghcr.io/xscaler/xscaler-agent:<version>          # default flavour
ghcr.io/xscaler/xscaler-agent:<version>-ebpf     # eBPF flavour
ghcr.io/xscaler/xscaler-agent:latest             # latest default
```

`<version>` matches the `OTEL_VERSION` pin (e.g. `0.151.0`) or the release tag.
The xscaler `k8s-collector` Helm chart points `image.repository` at
`ghcr.io/xscaler/xscaler-agent`; the per-node DaemonSet uses the `-ebpf` tag.

## Run

Every agent needs a supervisor config with your xscaler OpAMP endpoint and an
enrollment token (starts `xse_`, obtained from the xscaler UI). A minimal
`supervisor.yaml`:

```yaml
server:
  endpoint: wss://<your-xscaler-opamp-endpoint>/v1/opamp
  headers:
    Authorization: "Bearer xse_REPLACE_ME"   # your enrollment token — never commit a real one

capabilities:
  accepts_remote_config: true
  reports_effective_config: true
  reports_remote_config: true
  reports_health: true
  # Required since OTEL 0.151: heartbeat is opt-in. Without it the supervisor
  # connects once and goes silent, so the server's stale sweep marks it offline.
  reports_heartbeat: true

agent:
  executable: /usr/local/bin/otelcol-contrib
  description:
    identifying_attributes:
      service.name: io.opentelemetry.collector
    non_identifying_attributes:
      # Free-form labels — xscaler matches config assignments to agents by
      # label selector (e.g. environment, region, role).
      environment: prod

storage:
  directory: /tmp/supervisor
```

### Container

```sh
docker run --rm \
  -v "$PWD/supervisor.yaml:/etc/otelcol/supervisor.yaml:ro" \
  ghcr.io/xscaler/xscaler-agent:latest \
  --config /etc/otelcol/supervisor.yaml
```

The image's entrypoint is `opampsupervisor` (inherited from the base image); it
launches and manages `/usr/local/bin/otelcol-contrib`.

### Host binary via systemd

For host/VM fleets, run the supervisor as a service. Extract the two binaries
from the image (or install them however you distribute host binaries), drop a
supervisor config at `/etc/xscaler/supervisor.yaml`, and run:

```ini
# /etc/systemd/system/xscaler-agent.service
[Unit]
Description=xscaler telemetry agent (OpAMP supervisor)
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/opampsupervisor --config /etc/xscaler/supervisor.yaml
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now xscaler-agent
```

## eBPF (OBI) flavour

The `ebpf` image adds the
[OBI](https://opentelemetry.io/docs/zero-code/obi/configure/collector-receiver/)
eBPF receiver (donated from Grafana Beyla) for zero-code auto-instrumentation.
OBI instruments processes on its **own node** and emits both traces and metrics,
so it's intended for a per-node DaemonSet (the xscaler `k8s-collector` node
agent).

### Why the build is heavy

The published `go.opentelemetry.io/obi` module omits the generated CO-RE BPF
objects, so [`builder.yaml`](builder.yaml) has `replaces:
go.opentelemetry.io/obi => /src/obi` and the build generates them in two stages:

1. **`obi-gen`** clones OBI at `OBI_VERSION` and runs `make generate` inside
   OBI's own pinned generator image (`ghcr.io/open-telemetry/obi-generator`).
   Required, not a convenience: OBI's BPF C uses C23 attributes that need
   clang ≥ 18, which Debian bookworm's apt `clang` (14) can't compile. Keep the
   generator image tag in sync with OBI's Makefile `GEN_IMG` when bumping.
2. **`ocb-build`** copies that generated source and runs OCB on
   `golang:1.25-bookworm` (glibc, matching the supervisor runtime).

### Verify `obi` is present

```sh
docker run --rm --entrypoint /usr/local/bin/otelcol-contrib \
  ghcr.io/xscaler/xscaler-agent:<version>-ebpf components | grep -A40 receivers
# expect `obi` in the receivers list
```

### Runtime requirements

- Privileges: prefer scoped capabilities over privileged — `BPF`, `PERFMON`,
  `SYS_ADMIN`, `SYS_PTRACE`, `NET_RAW`, `DAC_READ_SEARCH`, `CHECKPOINT_RESTORE`;
  plus `hostPID` and `/sys` + `/host/proc` mounts.
- `hostNetwork` + `dnsPolicy: ClusterFirstWithHostNet` only when OBI's `network`
  feature is enabled.
- Kernel: needs CO-RE / BTF. Reliable on real Linux nodes (GKE/EKS/AKS); **kind
  on macOS may fail** BTF and is not a supported eBPF target.

## Releasing

Pushing a git tag (e.g. `v0.151.0`) triggers
[`.github/workflows/release.yml`](.github/workflows/release.yml), which builds
both targets for `linux/amd64` + `linux/arm64` and pushes them to
`ghcr.io/xscaler/xscaler-agent`. The first publish creates the package as
private — set it public once in the GitHub package settings.
