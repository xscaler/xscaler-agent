# xscaler-agent

The xScaler telemetry agent: **`xscaler-collector`**, xScaler's own
[OpenTelemetry Collector](https://opentelemetry.io/docs/collector/)
distribution, run under the OpenTelemetry **opampsupervisor**, which enrolls
into xScaler over [OpAMP](https://opentelemetry.io/docs/specs/opamp/) and
applies the remote configuration xScaler assigns to the agent's labels.

Everything here is **Apache-2.0**: the collector (core + contrib), the
opampsupervisor, and OBI. See [`LICENSE`](LICENSE) and [`NOTICE`](NOTICE).

## xscaler-collector

One binary and one image for every role: node DaemonSet, cluster Deployment,
Linux host and Windows host. It is built with the [OpenTelemetry Collector
Builder](https://opentelemetry.io/docs/collector/custom-collector/) (OCB) from
[`builder.yaml`](builder.yaml): upstream modules, all at one pinned upstream
version, plus our own
[`xscaler_policy`](processor/xscalerpolicyprocessor/README.md) processor, built
from [`processor/`](processor) in this repository.
[OBI](https://opentelemetry.io/docs/zero-code/obi/) eBPF auto-instrumentation
is compiled into every build and only runs where a config enables the `obi`
receiver and the process holds the BPF capabilities.

A component missing from `builder.yaml` fails any config that names it: add it
there, and to `scripts/expected-components.txt`, before a template starts using
it.

There is no contrib fork. If an upstream component ever needs a patch, it gets a
`replaces:` line in `builder.yaml` pointing that one module at
`github.com/xscaler/opentelemetry-collector-contrib`, with a comment linking the
upstream PR. There are none today.

### The pin

[`versions.env`](versions.env) is the only place versions are set:

| Variable | Value | Covers |
| --- | --- | --- |
| `OTEL_VERSION` | `0.163.0` | every contrib module, core beta modules, OCB, the supervisor |
| `OTEL_STABLE_VERSION` | `1.69.0` | core stable modules (confmap providers) |
| `CONTRIB_STABLE_VERSION` | `1.2.0` | contrib's stable-base set (k8sattributes) |
| `OBI_VERSION` | `0.15.2` | the `obi` receiver and its eBPF sources |
| `OBI_GENERATOR_VERSION` | `0.2.17` | the image OBI's `make generate` runs in |

0.163.0 is the newest release where core, contrib and OBI line up: OBI
v0.15.2's `collector/go.mod` requires core `v0.163.0` / pdata `v1.69.0`, which
is exactly core v0.163.0 (v0.162.0 ships pdata v1.68.0). OBI v1.0.0-rc.2 builds
against the same core but is a release candidate; move to 1.0.0 once it is GA.

[`scripts/check-pins.sh`](scripts/check-pins.sh) fails CI when any module in
`builder.yaml` or any `ARG` default in the `Dockerfile` differs from
`versions.env`, or when a `replaces:` line points anywhere other than the OBI
generate tree or our fork.

### Bumping the pin

1. Pick the new core release. Its `versions.yaml` gives the beta
   (`OTEL_VERSION`) and stable (`OTEL_STABLE_VERSION`) pair; contrib's
   `versions.yaml` at the same tag gives `CONTRIB_STABLE_VERSION` and the
   modules in its stable-base set.
2. Find the OBI release whose `collector/go.mod` requires that core version.
   If none does yet, stay on the current pin: OBI sets the ceiling.
3. Set `OBI_GENERATOR_VERSION` to `GEN_IMG` in OBI's Makefile at that tag.
4. Update `versions.env`, then every version in `builder.yaml` and the `ARG`
   defaults in the `Dockerfile`. Run `scripts/check-pins.sh`.
5. Build and run the checks below. Read the contrib changelog for renamed or
   removed config fields in the components we ship.

### Components

[`scripts/expected-components.txt`](scripts/expected-components.txt) is the
contract: `scripts/check-components.sh` fails when the binary is missing one of
these, or reports one that is not listed.

| Kind | Components |
| --- | --- |
| Receivers | `otlp`, `hostmetrics`, `filelog` (with the `container` operator), `journald`, `windowseventlog`, `windows_perf_counters`, `prometheus`, `kubeletstats`, `k8s_cluster`, `k8sobjects`, `k8s_events` (kept for configs pushed to 0.4.x agents), `receiver_creator`, `docker_stats`, `syslog`, `tcp_log`, `udp_log`, `postgresql`, `mysql`, `mongodb`, `redis`, `sqlserver`, `oracledb`, `sqlquery`, `elasticsearch`, `nginx`, `apache`, `rabbitmq`, `kafka_metrics`, `http_check`, `statsd`, `snmp`, `netflow`, `awscloudwatch`, `azuremonitor`, `googlecloudmonitoring`, `obi`, `nop` |
| Processors | `memory_limiter`, `k8sattributes`, `resourcedetection`, `resource`, `attributes`, `transform`, `filter`, `metrics_transform`, `groupbyattrs`, `cumulative_to_delta`, `delta_to_cumulative`, `interval`, `redaction`, `probabilistic_sampler`, `tail_sampling`, `batch`, `xscaler_policy` |
| Connectors | `forward`, `span_metrics`, `service_graph`, `routing`, `count` |
| Exporters | `otlp`, `otlphttp`, `debug`, `nop` |
| Extensions | `opamp`, `health_check`, `file_storage`, `k8s_leader_elector`, `pprof`, `basicauth`, `bearertokenauth`, `k8s_observer`, `host_observer`, `zpages` |
| Providers | `env`, `file`, `http`, `https`, `yaml` |

The `components` command, and the list the supervisor reports to agent-api,
include the deprecated pre-rename names (`filelog` next to `file_log`); configs
written with the old names still load and log a deprecation warning.

`xscaler_policy` is ours: the policy fragment the portal pushes over OpAMP
(drops, service-level aggregation, cardinality guard). `builder.yaml` builds it
from `processor/xscalerpolicyprocessor` with `path:`, and
[`processor-ci.yml`](.github/workflows/processor-ci.yml) runs its tests.

`journald` runs `journalctl`, so it works on package installs where the host has
it. The image is Alpine-based and does not ship `journalctl`.

### Building

Every build stage runs on the build host's platform and cross-compiles, so
multi-arch images need no QEMU and the same Dockerfile produces the host
binaries:

```sh
# Binaries (xscaler-collector + opampsupervisor) for one platform
docker buildx build --platform linux/arm64 --target binary -o dist/linux-arm64 .
docker buildx build --platform windows/amd64 --target binary -o dist/windows-amd64 .

# The image (default target), both arches
docker buildx build --platform linux/amd64,linux/arm64 -t xscaler-collector:dev .
```

Pass `--build-arg VERSION=<x.y.z>` to stamp the distribution version into
`xscaler-collector --version` and the image labels. The image records the
bundled upstream versions as `org.opentelemetry.collector.version` and
`io.xscaler.obi.version`.

The build has four parts:

1. **`obi-gen`** clones OBI at `OBI_VERSION` and runs `make generate` in OBI's
   own generator image. The published `go.opentelemetry.io/obi` module omits
   the generated CO-RE BPF objects, which is why `builder.yaml` replaces it
   with that tree. Unmodified upstream source.
2. **`sources`** runs OCB with `--skip-compilation`.
3. **`collector`** and **`supervisor`** compile for `TARGETOS`/`TARGETARCH`
   with `CGO_ENABLED=0`. The supervisor's `go.mod` carries local `replace`
   lines into the contrib tree, so it is built from a sparse checkout of the
   `cmd/opampsupervisor/v<OTEL_VERSION>` tag rather than `go install`.
4. **`image`** is Alpine with the same user (uid 10001), home directory and
   `opampsupervisor` entrypoint as the upstream supervisor image it replaces.
   `/usr/local/bin/otelcol-contrib` is a symlink to `xscaler-collector`, so
   supervisor configs written for the old images keep working.

### Checks

```sh
scripts/check-pins.sh
scripts/check-components.sh dist/linux-arm64/xscaler-collector
scripts/validate-examples.sh dist/linux-arm64/xscaler-collector
scripts/service-identity-test.sh dist/linux-arm64/xscaler-collector
scripts/smoke-test.sh dist/linux-arm64/xscaler-collector
```

The smoke test sends OTLP logs while the exporter's endpoint is down, stops the
collector, brings up a sink, restarts the collector on the same `file_storage`
directory and passes only when every record queued before the restart reaches
the sink. [`collector-ci.yml`](.github/workflows/collector-ci.yml) runs all five
on native amd64 and arm64 runners, cross-compiles Windows, macOS and FreeBSD,
and builds the multi-arch image.

### Service identity

The xScaler service model is built from what this collector emits, so every
config stamps the same identity on logs, metrics and traces:

| Attribute | Source |
| --- | --- |
| `service.name` | the SDK's value, else the pod's `app.kubernetes.io/name`, else the owning deployment, statefulset, daemonset, cronjob or job. Never a fixed name and never the pod name. The SDK default `unknown_service:<process>` is replaced when one of the others exists. |
| `service.namespace` | the Kubernetes namespace |
| `service.version` | the SDK's value, else `app.kubernetes.io/version` |
| `k8s.*` | cluster, namespace, deployment/statefulset/daemonset/cronjob/job, pod and node, from `k8s_attributes` on every workload pipeline |
| `xscaler.agent.id` | the agent-api id of the collector that saw it, so a collector can be linked to the services it sees |
| `host.name` | the node (inserted only; an SDK's own `host.name` stays) |

`k8s_attributes` does not derive `service.*` itself: upstream ranks
`app.kubernetes.io/instance` (usually the Helm release) above
`app.kubernetes.io/name` and falls back to the pod name.
`transform/xscaler_service` applies the order above instead, and
[`scripts/service-identity-test.sh`](scripts/service-identity-test.sh) checks it
case by case.

`${xscaler:agent_id}` in a config is a placeholder agent-api fills in per agent
when it pushes the config. The collector has no `xscaler` provider, so a config
loaded without agent-api must have it replaced first (the test scripts do).

Node infrastructure (host metrics, kubelet and cAdvisor scrapes) is not a
service and carries no `service.name`. On hosts, OTLP apps name themselves,
journal entries take their systemd unit (`nginx.service` becomes `nginx`) and
Windows event log records their provider.

The cluster agent keeps every Warning event and every event about a Deployment
or ReplicaSet (`ScalingReplicaSet`, `SuccessfulCreate`, ...), so each rollout is
recorded with `k8s.deployment.name`, the key that joins it to the workload's
services. Database receivers carry `db.system.name`, `db.system`,
`server.address` and `server.port`, the keys a client span uses for the same
database, so database nodes in the service graph join to receiver metrics.

### Example configs

[`examples/`](examples) has one collector config per role. All of them run the
memory limiter below the container limit, put every exporter's sending queue on
`file_storage`, and keep retrying instead of dropping:

| File | Role |
| --- | --- |
| `node-daemonset.yaml` | kubelet, container logs (container parser) and OTLP with the service identity above; host metrics in a separate `metrics/infra` pipeline |
| `node-daemonset-ebpf.yaml` | overlay that adds `obi`; load it as a second `--config` |
| `cluster-deployment.yaml` | `k8s_cluster`, Warnings and Deployment/ReplicaSet change events, one active replica via `k8s_leader_elector` |
| `linux-host.yaml` | host metrics, `/var/log`, the journal (service from the systemd unit), local OTLP |
| `windows-host.yaml` | host metrics, Application and System event logs, local OTLP |
| `freebsd-host.yaml` | host metrics, `/var/log`, local OTLP; no journal |
| `databases.yaml` | every database receiver with `db.system.name`, `db.system`, `server.address`, `server.port`; merge with a role config |

In containers the limiter uses `limit_percentage: 80` and
`spike_limit_percentage: 20`, which the collector reads against the cgroup
limit: a 512Mi pod gets a 409Mi hard and 307Mi soft limit. The portal's
generated configs set `limit_mib` 800 and 1500 against that same 512Mi pod
limit, so the limiter never fired before the kernel OOM-killed the pod.

### Chart

Setting `image.repository=ghcr.io/xscaler/xscaler-collector` switches the chart
to this image. The chart then uses one tag for every role (OBI is in every
build) and leaves `reports_remote_config` out of the supervisor config: the
0.163 supervisor refuses to start with it, while the 0.151 supervisor in the
legacy images needs it to report config status. Both report
`reports_available_components` and `k8s.cluster.name`, which agent-api records
in its collector inventory together with role, version and host.

The xscaler-collector image also reports `xscaler.distribution:
xscaler-collector`, which agent-api uses to offer the collector policy
(the `xscaler_policy` processor). That needs xscaler-collector 0.6.0 or newer.

### Images

```
ghcr.io/xscaler/xscaler-collector:<version>   # every role
ghcr.io/xscaler/xscaler-collector:latest
```

`<version>` is the xScaler distribution version (`Chart.yaml`'s `version`, see
[Releasing](#releasing)), independent of the upstream pin.

## Legacy images

Until every install path runs `xscaler-collector`, the release workflow still
publishes the old images from [`legacy/Dockerfile`](legacy/Dockerfile) at
collector 0.151.0 / OBI v0.9.0. Both tags carry the same OCB build from
[`legacy/builder.yaml`](legacy/builder.yaml), with `obi` and `xscaler_policy`:

```
ghcr.io/xscaler/xscaler-agent:<version>        # supervisor + OCB build
ghcr.io/xscaler/xscaler-agent:<version>-ebpf   # same binary; the chart's eBPF tag
```

The legacy build copies `processor/`, so it runs from the repository root:
`docker build -f legacy/Dockerfile .`.

The monorepo's `deploy/agents` is an older copy of the same build and is due
for removal once local docker-compose uses `xscaler-collector`.

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
  ghcr.io/xscaler/xscaler-collector:latest \
  --config /etc/otelcol/supervisor.yaml
```

The image's entrypoint is `opampsupervisor`; it launches and manages
`/usr/local/bin/xscaler-collector` (`otelcol-contrib` is a symlink to it, which
is what the `agent.executable` above points at).

### Stable instance UID on Kubernetes

The supervisor keys each fleet agent on its OpAMP `instance_uid`, which it
persists in `<storage.directory>/persistent_state.yaml`. In the Helm chart that
directory is an `emptyDir`, so a pod restart would wipe it, the supervisor would
mint a fresh random UID, and the fleet view would show a **new online agent plus
the old one going offline** — one ghost per restart.

To prevent that, the chart derives the UID deterministically from stable cluster
identity instead of relying on persisted state. A `derive-instance-uid` init
container hashes a cluster-qualified identity (SHA-256 → a name-based UUID) and
writes it to `persistent_state.yaml` before the supervisor starts:

- **Node agent (DaemonSet)** — identity is `"<clusterName>/<nodeName>"`
  (`spec.nodeName` via the Downward API), 1:1 with the DaemonSet pod.
- **Cluster agent (Deployment)** — identity is `"<clusterName>/<role>"`.

Same identity → same UID → the existing fleet row flips offline→online on
restart, and no ghost is created. There's no namespace/salt to configure:
`clusterName` makes the identity globally unique (agent-api partitions rows by
`(org, instance_uid)`), so **set `clusterName` distinctly per cluster** —
otherwise two clusters left at the default would derive colliding UIDs. See
`charts/xscaler-agent/values.yaml`.

### Hosts: packages and MSI

Each release publishes `xscaler-collector` as deb and rpm packages, Windows
MSIs and tar.gz/zip archives for linux, windows, darwin and freebsd on amd64 and
arm64,
built from the same sources as the image
([`collector-release.yml`](.github/workflows/collector-release.yml)).

Debian and Ubuntu:

```sh
curl -fsSL https://packages.xscalerlabs.com/xscaler-archive-keyring.gpg \
  | sudo tee /usr/share/keyrings/xscaler-archive-keyring.gpg >/dev/null
echo "deb [signed-by=/usr/share/keyrings/xscaler-archive-keyring.gpg] https://packages.xscalerlabs.com/apt stable main" \
  | sudo tee /etc/apt/sources.list.d/xscaler.list
sudo apt-get update && sudo apt-get install xscaler-collector
```

RHEL, Rocky, Amazon Linux and other dnf/yum hosts:

```sh
sudo curl -fsSL https://packages.xscalerlabs.com/rpm/xscaler.repo -o /etc/yum.repos.d/xscaler.repo
sudo dnf install xscaler-collector
```

Release candidates go to the `testing` channel (`.../apt testing main`, or
`dnf --enablerepo xscaler-testing`). The apt index lists the newest version
per channel; the yum index lists every version.

The package installs:

| Path | |
| --- | --- |
| `/usr/bin/xscaler-collector`, `/usr/bin/xscaler-supervisor` | the collector and the OpAMP supervisor |
| `xscaler-collector.service` | runs the supervisor as user `xscaler-collector`, in groups `adm` (`/var/log`) and `systemd-journal` |
| `/etc/xscaler-collector/xscaler-collector.env` | `XSCALER_ENROLLMENT_TOKEN`, `XSCALER_ENDPOINT`, `XSCALER_AGENT_NAME` |
| `/etc/xscaler-collector/supervisor.yaml` | supervisor config; add labels here |
| `/var/lib/xscaler-collector` | supervisor and collector state |
| `/usr/share/xscaler-collector/ebpf.conf` | systemd drop-in granting the eBPF capabilities, off by default |

Both config files are kept on upgrade. The service is enabled on install and
starts once the env file has a token:

```sh
sudo sed -i 's/^XSCALER_ENROLLMENT_TOKEN=.*/XSCALER_ENROLLMENT_TOKEN=xse_.../' \
  /etc/xscaler-collector/xscaler-collector.env
sudo systemctl restart xscaler-collector
```

Installing the package stops and disables `otelcol-contrib.service` and the
`opampsupervisor.service` written by `get-xscaler/install.sh`, so a host never
runs two agents. eBPF instrumentation: link the drop-in as described at the
top of `ebpf.conf`.

#### FreeBSD

FreeBSD on amd64 and arm64 gets the tar.gz archive only: no
package, no rc.d script. It is the same manifest as every other platform,
built with `CGO_ENABLED=0`, so `xscaler-collector components` lists the same 78
components as on Linux (`scripts/check-components.sh` applies to it unchanged).
Four of them are in the binary but refuse to start on FreeBSD; a config that
uses one fails at `validate` with an "only supported on ..." error:

| Component | Why it does nothing on FreeBSD |
| --- | --- |
| `journald` | no systemd journal |
| `windowseventlog`, `windows_perf_counters` | Windows only |
| `obi` | Linux eBPF (amd64 and arm64) only |

The Kubernetes receivers and processors (`k8s_cluster`, `k8s_objects`,
`k8s_events`, `kubeletstats`, `k8sattributes`, the leader elector and observer)
compile, but need a cluster to talk to.

`host_metrics` works with the `cpu`, `memory`, `load`, `filesystem`, `network`,
`disk`, `paging` and `processes` scrapers. `system.network.connections` runs
`lsof`, which the base system lacks, so the scraper logs an error every
interval; turn the metric off or `pkg install lsof`. See
[`examples/freebsd-host.yaml`](examples/freebsd-host.yaml).

```sh
fetch https://github.com/xscaler/xscaler-agent/releases/download/v<version>/xscaler-collector_<version>_freebsd_amd64.tar.gz
tar xzf xscaler-collector_<version>_freebsd_amd64.tar.gz
cd xscaler-collector_<version>_freebsd_amd64
mkdir -p state
```

Run it under the supervisor from that directory. `config/supervisor.yaml`
already points at `./xscaler-collector` and `./state`:

```sh
export XSCALER_ENROLLMENT_TOKEN=xse_...
daemon -f -p /var/run/xscaler-supervisor.pid -o /var/log/xscaler-supervisor.log \
  ./xscaler-supervisor --config config/supervisor.yaml
```

The arm64 build was run by hand on FreeBSD 14.5 under QEMU: `components`,
`validate`, a 60 second run with host metrics and `/var/log/messages` records
reaching the debug exporter, and the supervisor enrolling with agent-api and
starting the collector. CI cross-compiles both architectures and does not run
them.

Windows, silent install (service `xscaler-collector`, binaries in
`C:\Program Files\xScaler Collector`):

```powershell
msiexec /i xscaler-collector_<version>_windows_amd64.msi /qn ENROLLMENT_TOKEN=xse_... [ENDPOINT=wss://...]
```

The config is written to `%ProgramData%\xScaler Collector\supervisor.yaml`
(SYSTEM and Administrators only). Upgrades keep it, and passing
`ENROLLMENT_TOKEN` or `ENDPOINT` again changes only those values. Uninstall
leaves it in place.

#### Verifying a download

Each release has `xscaler-collector_<version>_checksums.txt`, a cosign keyless
signature bundle for it, an SPDX SBOM per artifact (`<artifact>.spdx.json`)
and GitHub build provenance:

```sh
cosign verify-blob \
  --bundle xscaler-collector_<version>_checksums.txt.sigstore.json \
  --certificate-identity-regexp '^https://github.com/xscaler/xscaler-agent/\.github/workflows/collector-release\.yml@' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  xscaler-collector_<version>_checksums.txt
sha256sum --check --ignore-missing xscaler-collector_<version>_checksums.txt
gh attestation verify xscaler-collector_<version>_amd64.deb --repo xscaler/xscaler-agent
```

## kube-state-metrics (optional subchart)

The chart can install
[kube-state-metrics](https://github.com/kubernetes/kube-state-metrics) alongside
the agents, as a Helm dependency gated on a condition. It's **off by default**:
many clusters already run one (kube-prometheus-stack bundles it), and a second
copy only doubles the API-server watch load. Check first:

```sh
kubectl get deploy -A -l app.kubernetes.io/name=kube-state-metrics
```

Then enable it:

```sh
helm install xscaler charts/xscaler-agent \
  --set enrollmentToken=xse_... \
  --set kube-state-metrics.enabled=true
```

KSM exposes cluster-object state (deployment replicas, pod phase, job/cronjob
status, PVC binding) as Prometheus metrics — signals the `k8s_cluster` and
`kubeletstats` receivers don't produce.

**The chart installs the target, not the scrape.** Like every other pipeline
here, the `prometheus` receiver that reads KSM arrives as a pushed OpAMP config.
So a pushed config can find it, the **cluster** agent reports two extra
non-identifying attributes whenever the subchart is enabled:

| Attribute | Value |
| --- | --- |
| `kube_state_metrics` | `"true"` — a capability flag, same idea as `node_ip`; select on it in an assignment |
| `kube_state_metrics_endpoint` | e.g. `xscaler-kube-state-metrics.observability.svc.cluster.local:8080` |

Both are chart-emitted and can't be overridden from `labels`, so they always
describe what is actually deployed. Scrape from the cluster agent, not the node
DaemonSet — one exporter wants one scraper, not one per node.

Everything under the `kube-state-metrics:` block in `values.yaml` belongs to
[the upstream chart](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-state-metrics);
the chart sets only `replicas`, CPU/memory **requests** (no memory limit — KSM's
footprint tracks cluster object count, and a limit sized for a small cluster
OOMKills it on a big one) and disables the ServiceMonitor.

### Installing the published chart

Nothing extra to do — the release workflow vendors the subchart into the
packaged archive, so it ships **inside** the chart. Installing from the registry
needs no `helm repo add` and no network to prometheus-community, whether or not
you enable it:

```sh
helm install xscaler oci://ghcr.io/xscaler/charts/xscaler-agent --version 0.4.0 \
  --set enrollmentToken=xse_... \
  --set kube-state-metrics.enabled=true
```

### Installing from a clone

A source checkout has no `charts/` directory — the vendored archive is
gitignored — so fetch dependencies once before the first `helm install` or
`helm template`, which fail without it:

> Error: found in Chart.yaml, but missing in charts/ directory: kube-state-metrics

`helm lint` is the exception: it downgrades this to a warning and still exits 0,
so a green lint doesn't mean the chart would install.

```sh
helm dependency build charts/xscaler-agent
```

That's the whole setup — no `helm repo add`. The dependency is declared as an
`oci://` ref (prometheus-community's registry mirror), which helm resolves
without the repo being in your local list. The version is pinned in `Chart.yaml`
and locked in `Chart.lock` — `dependency build` honours the lock, while
`dependency update` re-resolves it and is what you want when bumping the pin.

## eBPF (OBI)

OBI instruments processes on its **own node** and emits both traces and
metrics, so it belongs on the node DaemonSet. Check it is in a build with
`xscaler-collector components | grep obi`.

### Runtime requirements

- Privileges: prefer scoped capabilities over privileged — `BPF`, `PERFMON`,
  `SYS_ADMIN`, `SYS_PTRACE`, `NET_RAW`, `DAC_READ_SEARCH`, `CHECKPOINT_RESTORE`;
  plus `hostPID` and `/sys` + `/host/proc` mounts.
- `hostNetwork` + `dnsPolicy: ClusterFirstWithHostNet` only when OBI's `network`
  feature is enabled.
- Kernel: needs CO-RE / BTF. Reliable on real Linux nodes (GKE/EKS/AKS); **kind
  on macOS may fail** BTF and is not a supported eBPF target.

## Releasing

Releases are driven by the **distro version** — the `version` field in
[`charts/xscaler-agent/Chart.yaml`](charts/xscaler-agent/Chart.yaml). This is
xScaler's own release number and is **not** the bundled collector's
`OTEL_VERSION` (see [The pin](#the-pin)).

To cut a release, bump `version` in `Chart.yaml` in a PR and merge to `main`.
[`.github/workflows/release.yml`](.github/workflows/release.yml) then:

1. tags `v<version>` if that tag doesn't exist yet (nothing happens if the
   version is unchanged);
2. builds `xscaler-collector` and the two legacy images for `linux/amd64` +
   `linux/arm64` and pushes them as `xscaler-collector:<version>` and
   `:latest`, and `xscaler-agent:<version>`, `:<version>-ebpf` and `:latest`;
3. packages the Helm chart with `version` and `appVersion` set to `<version>`
   and pushes it to `oci://ghcr.io/xscaler/charts`;
4. runs [`collector-release.yml`](.github/workflows/collector-release.yml) for
   the tag: binaries, archives, deb/rpm, MSIs, install tests on every target
   distro and on windows-2022, checksums, cosign signature, SBOMs and
   provenance on the GitHub release, then the apt/yum repository.

Pushing a `v*` tag by hand runs step 4 alone; a prerelease tag
(`v0.6.0-rc.1`) publishes to the `testing` channel. Every pull request that
touches packaging runs step 4 as a snapshot, without publishing.

Step 4 needs the `PACKAGES_GPG_PRIVATE_KEY` and `PACKAGES_GPG_PASSPHRASE`
secrets (the repository and rpm signing key), and the `PACKAGES_BUCKET`,
`PACKAGES_CLOUDFRONT_ID` and `PACKAGES_AWS_ROLE_ARN` variables from
terraform-aws `envs/ops/packages`. The repository job runs in the `packages`
environment, the only one the role trusts.

So one number in `Chart.yaml` drives the git tag, both image tags, and the chart
version/appVersion in lockstep, and is stamped into `xscaler-collector
--version`. The first publish of each package (images and
chart) creates it as private — set it public once in the GitHub package
settings.

## License

Apache License 2.0. Copyright 2026 XSCALER LTD. See [LICENSE](LICENSE) and
[NOTICE](NOTICE).
