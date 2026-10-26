# xscaler_policy processor

Applies the collection policy the xScaler portal pushes over OpAMP. agent-api
merges it into every matched config as its own layer, so customer pipelines
stay untouched. Every section is optional; an empty config passes data
through.

Order on metrics: service identity, drops, aggregations, cardinality limits.
Logs and traces only get service identity.

```yaml
processors:
  xscaler_policy:
    policy_version: "2026-10-20T10:00:00Z"
    drops:
      - metric: "k8s_pod_filesystem_*"        # glob
      - metric: "regex:go_gc_.*|go_memstats_.*"
    aggregations:
      # One series per service instead of per pod.
      - metric: "http_server_request_duration*"
        drop_attributes: [k8s.pod.name, k8s.pod.uid]
      - metric: "k8s_pod_memory_working_set"
        drop_attributes: [k8s.pod.name, k8s.pod.uid]
        gauge_function: sum                     # avg (default), sum, min, max, last
    aggregation_state:
      ttl: 10m                                  # forget sources unseen this long; >= max(warmup, 1m)
      warmup: 2m                                # new outputs count every joining source in full
      gauge_staleness: 5m                       # gauge sources stop counting after this
      max_sources: 100000                       # cap on tracked source series
    cardinality:
      default_max_series: 0                     # 0 = unlimited
      window: 1h
      limits:
        - metric: "regex:db_query_.*"
          max_series: 2000
    service_identity:
      enabled: true
      # defaults: placeholders [k8s-node, k8s-cluster, unknown_service*],
      # sources [app.kubernetes.io/name, k8s.deployment.name,
      # k8s.statefulset.name, k8s.daemonset.name, k8s.cronjob.name, k8s.job.name]
    collector_identity:
      attribute: xscaler.collector.id           # default
      # value defaults to $XSCALER_COLLECTOR_ID, then the host name
```

## Behaviour worth knowing

- **Aggregation keeps a collector identity.** A node agent only sees its own
  node's pods. Every aggregated point gets `xscaler.collector.id`, so two
  nodes never send the same series, which Mimir would reject as duplicates.
  Sum across collectors in the query. To get one series per service with no
  collector label, aggregate in the cluster agent or server side.
- **Merges remember sources across batches.** Receivers deliver sources in
  separate batches (the prometheus receiver and OTLP pushes send one pod
  per batch), so every merge keeps per-source state in one collector,
  bounded by `aggregation_state`: sources unseen for `ttl` (default 10m) are
  forgotten, and at most `max_sources` (default 100000) are tracked. Points
  of new sources past the cap are dropped and counted in
  `xscaler_policy_overflow_data_points`. Output timestamps always move
  forward, even when a source's clock lags, because Mimir rejects samples
  that go back in time or repeat a timestamp. They are also capped at 1m
  past the collector's clock: a source whose clock runs ahead would
  otherwise pull the output into the future, where Mimir rejects it and
  every later point. Capped points are counted in
  `xscaler_policy_future_timestamps_clamped`.
- **Gauges and non-monotonic sums** (levels, not totals) combine the latest
  point of every live source, so a batch with one pod reports the
  combination over all pods instead of flapping between per-pod values.
  Gauges use `gauge_function`; non-monotonic sums add. A source stops
  counting once it has not been seen for `gauge_staleness` (default 5m,
  the Prometheus lookback; capped at `ttl`).
- **Staleness markers.** The prometheus receiver sends points flagged
  `NoRecordedValue` when a scrape fails or a target disappears (histogram
  buckets zeroed). They carry no value and are never read as 0: a
  cumulative source keeps its last value (so the next real point adds only
  the real increase) and expires through `ttl` if it does not come back; a
  gauge or non-monotonic sum source is removed from the combination at
  once, and when the last source of an output goes stale the output emits
  a staleness marker too.
- **Cumulative counters and histograms merge by per-source deltas.** Each
  source's last value and start time is kept. Every new point becomes a
  delta against it; a lower value, a lower bucket count or a changed start
  time is a reset, so the delta is the new value. Deltas add into a running
  total with a stable start time, so one pod restarting never makes the
  merged series drop and `rate()` never sees a false reset. Details:
  - **Warm-up.** A newly created cumulative output emits nothing for
    `aggregation_state.warmup` (default 2m) after it first appears. During
    that time every joining source counts in full; the first point after
    the warm-up carries the whole accumulated value. After a collector
    restart the pods of one output arrive over several batches: emitting
    the first pod's value alone would drop the counter below what the
    previous process reported, and `rate()`/`increase()` would count the
    recovery as a phantom increase. A short gap is harmless. The hold-back
    counts from the output's first creation and runs once per output (see
    the next point). `0` disables the warm-up (outputs
    emit at once and later old pods only set a baseline). Gauges and
    non-monotonic sums are not held back. `ttl` must be at least
    `max(warmup, 1m)`: a TTL shorter than the scrape interval would forget
    every source between scrapes.
  - **Outputs resume.** When every source of a cumulative output has gone
    unseen for `ttl`, the output is parked, not forgotten: its running
    total, start time and last timestamp are kept for 6 TTLs after its last
    point. If a source comes back in that time the output continues the
    same series (same start time, never lower, no second warm-up), and
    each source follows the rules below: a returning source with the same
    start adds only its increase since its last counted value, one with a
    new start (it restarted) counts from its start, and a source this
    output never saw counts in full only if it started after the output.
    Pods coming back one per batch therefore never emit a partial total.
    After 6 TTLs the output is gone and the next point starts a new series.
  - **Start time.** Outputs created during the processor's start-up warm-up
    start at the processor's start; outputs created later (or after a
    parked output expired) start at their creation. Either way a new series says so with a later start time
    instead of reusing an older one. The start time is kept strictly before
    the output's first point.
  - After the warm-up, a joining source counts in full if it started after
    the output was created, and only sets a baseline if it started earlier
    (this collector just began seeing it), so its history does not land as
    a spike.
  - Duplicate and out-of-order points (timestamp not newer than the
    source's last) add nothing.
  - A forgotten source's start time and last value are kept for 6 more
    TTLs (bounded by `max_sources`): a source returning with the same start
    sets a baseline at its last counted value, so it adds only its increase
    since then (nothing for a repeated point); one returning with a new
    start, or a lower value, counts in full.
  - Histogram min and max cannot be kept across cumulative merges and are
    omitted.
  - **Exponential histograms** merge too: points are downscaled to the
    coarsest scale among them, and the zero bucket widened to the largest
    zero threshold (buckets below it move into the zero count). A merged
    side that would exceed 160 buckets drops to a coarser scale. Per-source
    deltas work as for explicit histograms; a lower count, zero count or
    bucket, a finer scale or a changed start time is a reset.
  - **Restart limits.** The new process starts from the sum of the pods'
    current values. That equals or exceeds the old process's total unless
    a pod restarted, or was only baselined, during the old process's
    lifetime; then the new series can begin lower. Its later start time
    marks it as a new series, which backends that honour start (created)
    timestamps handle as a reset.
- **Delta inputs** add every point. Sources of one output arrive in
  separate batches with their own windows, so each output remembers where
  its last emitted window ended: the next window starts there and ends at
  the newest point (or 1ms later if that is not newer). Windows are
  contiguous, never overlap and move forward; totals stay exact and a late
  point is reported in the next window. This state takes one slot of
  `max_sources` per output and expires after `ttl`.
- **Summaries cannot be aggregated** (quantiles of different sources do
  not combine). An aggregation rule leaves them as they are, attributes
  included, and counts them in `xscaler_policy_unaggregated_data_points`
  with `reason="summary"`. Histogram points whose bucket layout differs
  from the one their output uses are counted there with
  `reason="bucket_layout"`.
- **Cardinality overflow keeps totals.** Points of series past the limit lose
  their attributes and fold into one series per metric, resource and scope
  with `otel_metric_overflow="true"`. Cumulative overflow series use the
  same per-source delta accumulation, under the same state bounds.
  Summaries past the limit are dropped (and counted as overflow).
  Staleness markers never claim a free slot.
- **Admissions are sticky.** A series keeps its slot as long as it keeps
  reporting. At the end of each `window` the series not seen during that
  whole window give their slots back, so a slot is released between one
  and two windows after a series' last point. Overflow is sticky too: a
  series that is active in the overflow does not claim a slot freed by
  another series (its history is already in the overflow total) until it
  has been absent for a whole window. An active series therefore never
  moves between its own series and the overflow. A released series is
  remembered for 6 windows: if it comes back in that time while the limit
  is full it joins the overflow with a baseline only, since its history
  was already reported on its own series. Both records hold at most the
  metric's series limit per metric; a series past that bound (or back
  after 6 windows) follows the usual rules of the overflow output, which
  baseline a source that started before that output.
- **Concurrency.** Pipelines may call the processor at the same time.
  State is split into 64 shards, each with its own lock: aggregation and
  overflow state by output series, cardinality admissions by metric name.
  Only the `max_sources` budget is shared (atomically). Expiry is
  incremental: each batch sweeps the shards that are due (last swept at
  least `ttl/10` ago) until it has visited about 4096 entries, so a batch
  never walks all state, and expiry spreads over the following batches.
  Gauge sources are also checked against `gauge_staleness` whenever their
  output is updated.
- **Service identity only fills gaps.** A real `service.name` is never
  replaced. Disabled unless `service_identity.enabled` is true.

## Config reference and validation rules

The portal validates policy bodies against a mirror of this config before
pushing them. Keep the mirror in step with this table. The collector
rejects unknown keys at any level, so a body the portal accepts must use
only these fields.

| Field | Type | Default | Rule |
|---|---|---|---|
| `policy_version` | string | `""` | free text |
| `drops[].metric` | pattern | | required, non-empty |
| `aggregations[].metric` | pattern | | required, non-empty |
| `aggregations[].drop_attributes` | list of strings | | required, non-empty; must not contain the collector identity attribute |
| `aggregations[].gauge_function` | string | `avg` | one of `""`, `avg`, `sum`, `min`, `max`, `last` |
| `aggregation_state.ttl` | duration | `10m` | `>= 0`; `0` means the default; the effective value must be `>= 1m` and `>= warmup` |
| `aggregation_state.warmup` | duration | `2m` | `>= 0`; `0` disables the warm-up (new cumulative outputs emit at once) |
| `aggregation_state.gauge_staleness` | duration | `5m` | `0` (the default) or `>= 1m`; capped at `ttl` |
| `aggregation_state.max_sources` | int | `100000` | `>= 0`; `0` means the default |
| `cardinality.default_max_series` | int | `0` | `>= 0`; `0` means unlimited |
| `cardinality.window` | duration | `1h` | `>= 0`; `0` means the default; idle series are released at each window boundary |
| `cardinality.limits[].metric` | pattern | | required, non-empty |
| `cardinality.limits[].max_series` | int | | `>= 0`; `0` means unlimited |
| `service_identity.enabled` | bool | `false` | |
| `service_identity.placeholders` | list of strings | `[k8s-node, k8s-cluster, unknown_service*]` | trailing `*` matches by prefix |
| `service_identity.sources` | list of strings | see above | resource attribute keys, tried in order |
| `service_identity.set_namespace` | bool | `true` | |
| `collector_identity.attribute` | string | `xscaler.collector.id` | |
| `collector_identity.value` | string | `$XSCALER_COLLECTOR_ID`, then host name | |

- **Patterns** are globs (`*` any run, `?` one character, everything else
  literal, matched against the whole name) unless prefixed with `regex:`,
  in which case the rest must compile as an RE2 regexp and is anchored to
  the whole name.
- **Durations** are Go duration strings (`90s`, `10m`, `1h`). A bare
  integer decodes as nanoseconds.
- **Types are strict.** The collector does no weak conversion: unquoted
  `1.0`, `7`, dates and timestamps are not strings, `yes`/`no`/`"true"`
  are not booleans, unknown keys fail. `testdata/policy_bodies.yaml` lists
  accepted and rejected bodies; `TestPolicyBodyDecodeContract` decodes
  them exactly as the collector does, and the portal mirror uses the same
  cases.
- The identity check on `drop_attributes` uses
  `collector_identity.attribute` when set, otherwise `xscaler.collector.id`.

## Own metrics

| Metric | Meaning |
|---|---|
| `xscaler_policy_dropped_data_points` | Points removed by drop rules |
| `xscaler_policy_aggregated_data_points` | Points folded away by aggregation |
| `xscaler_policy_overflow_data_points` | Points moved to the overflow series, or dropped past `max_sources` or the series limit |
| `xscaler_policy_future_timestamps_clamped` | Merged data points timestamped more than 1m ahead of the collector's clock; the output timestamp was capped |
| `xscaler_policy_unaggregated_data_points` | Points an aggregation rule matched but could not merge (`reason`: `summary`, `bucket_layout`) |
| `xscaler_policy_service_identity_set` | Resources given a derived service.name |

All carry `policy_version`.

## Tests

```bash
cd processor/xscalerpolicyprocessor && go test -race ./...
```
