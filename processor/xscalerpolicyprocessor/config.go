package xscalerpolicyprocessor

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// Config is the policy fragment the portal pushes to a collector over OpAMP.
// Every section is optional; an empty Config passes data through unchanged.
type Config struct {
	// PolicyVersion identifies the fragment that produced this config. It is
	// attached to the processor's own metrics so the portal can tell which
	// policy a collector is running.
	PolicyVersion string `mapstructure:"policy_version"`

	// Drops removes whole metrics by name before anything else runs.
	Drops []DropRule `mapstructure:"drops"`

	// Aggregations merge data points of a metric after removing attributes,
	// e.g. dropping k8s.pod.name to get one series per service.
	Aggregations []AggregationRule `mapstructure:"aggregations"`

	// AggregationState bounds the per-source state kept to merge cumulative
	// counters and histograms correctly.
	AggregationState AggregationStateConfig `mapstructure:"aggregation_state"`

	// Cardinality caps the number of distinct series per metric.
	Cardinality CardinalityConfig `mapstructure:"cardinality"`

	// ServiceIdentity fills service.name from Kubernetes workload attributes
	// when the source left it unset or set one of our placeholders.
	ServiceIdentity ServiceIdentityConfig `mapstructure:"service_identity"`

	// CollectorIdentity is stamped on every aggregated or overflowed data
	// point so two collectors never emit the same series.
	CollectorIdentity CollectorIdentityConfig `mapstructure:"collector_identity"`
}

// DropRule matches metric names. Metric is a glob (`*` and `?`) unless it
// starts with "regex:", in which case the rest is a full-match regexp.
type DropRule struct {
	Metric string `mapstructure:"metric"`
}

// AggregationRule removes attributes from matching metrics and merges the
// data points that become identical. DropAttributes apply to both resource
// and data point attributes.
type AggregationRule struct {
	Metric         string   `mapstructure:"metric"`
	DropAttributes []string `mapstructure:"drop_attributes"`
	// GaugeFunction combines gauge points: avg (default), sum, min, max, last.
	GaugeFunction string `mapstructure:"gauge_function"`
}

// AggregationStateConfig bounds the state behind cumulative merges (both
// aggregation rules and cardinality overflow). Each source series keeps its
// last value so a restart of one source does not look like a reset of the
// merged series.
type AggregationStateConfig struct {
	// TTL forgets a source not seen for this long. Defaults to 10 minutes.
	// It must be at least one minute and at least Warmup: a TTL shorter
	// than the scrape interval would forget every source between scrapes.
	TTL time.Duration `mapstructure:"ttl"`
	// GaugeStaleness stops counting a gauge or non-monotonic sum source in
	// the combination once it has not been seen for this long. Defaults to
	// 5 minutes (the Prometheus lookback); capped at TTL. A staleness
	// marker removes the source at once.
	GaugeStaleness time.Duration `mapstructure:"gauge_staleness"`
	// Warmup is how long after an output is created every joining source
	// counts in full. After a collector restart the sources of one output
	// arrive over several batches; without a warm-up the later ones would
	// only set a baseline. Defaults to 2 minutes; 0 disables it.
	Warmup time.Duration `mapstructure:"warmup"`
	// MaxSources caps tracked source series across all cumulative outputs.
	// Points of new sources past the cap are dropped and counted as
	// overflow. Defaults to 100000.
	MaxSources int `mapstructure:"max_sources"`
}

// CardinalityConfig limits distinct series per metric.
type CardinalityConfig struct {
	// DefaultMaxSeries applies to metrics no limit matches. 0 means no limit.
	DefaultMaxSeries int `mapstructure:"default_max_series"`
	// Window is how often idle series give their slot back. A series keeps
	// its slot while it reports; one not seen during a whole window is
	// released at the end of it. Defaults to one hour.
	Window time.Duration      `mapstructure:"window"`
	Limits []CardinalityLimit `mapstructure:"limits"`
}

// CardinalityLimit is a per-metric series cap. The first matching limit wins.
type CardinalityLimit struct {
	Metric    string `mapstructure:"metric"`
	MaxSeries int    `mapstructure:"max_series"`
}

// ServiceIdentityConfig controls how service.name is derived.
type ServiceIdentityConfig struct {
	Enabled bool `mapstructure:"enabled"`
	// Placeholders are service.name values treated as unset. Values ending in
	// "*" match by prefix.
	Placeholders []string `mapstructure:"placeholders"`
	// Sources are resource attribute keys tried in order; the first non-empty
	// one becomes service.name.
	Sources []string `mapstructure:"sources"`
	// SetNamespace copies k8s.namespace.name into service.namespace when the
	// latter is missing.
	SetNamespace bool `mapstructure:"set_namespace"`
}

// CollectorIdentityConfig names the attribute and value that identify this
// collector on aggregated data.
type CollectorIdentityConfig struct {
	Attribute string `mapstructure:"attribute"`
	// Value defaults to $XSCALER_COLLECTOR_ID, then the host name.
	Value string `mapstructure:"value"`
}

const (
	defaultIdentityAttribute = "xscaler.collector.id"
	overflowAttribute        = "otel_metric_overflow"
	defaultWindow            = time.Hour
	defaultStateTTL          = 10 * time.Minute
	minStateTTL              = time.Minute
	defaultGaugeStaleness    = 5 * time.Minute
	defaultWarmup            = 2 * time.Minute
	defaultMaxSources        = 100000
)

// Placeholders the starter configs write (agent-starter-configs.ts) plus the
// SDK default.
var defaultPlaceholders = []string{"k8s-node", "k8s-cluster", "unknown_service*"}

// Workload attributes in the order they best name a service. The first key
// is the recommended Kubernetes label when k8sattributes extracts it.
var defaultSources = []string{
	"app.kubernetes.io/name",
	"k8s.deployment.name",
	"k8s.statefulset.name",
	"k8s.daemonset.name",
	"k8s.cronjob.name",
	"k8s.job.name",
}

func createDefaultConfig() *Config {
	return &Config{
		AggregationState: AggregationStateConfig{TTL: defaultStateTTL, Warmup: defaultWarmup, MaxSources: defaultMaxSources},
		Cardinality:      CardinalityConfig{Window: defaultWindow},
		ServiceIdentity: ServiceIdentityConfig{
			Placeholders: append([]string(nil), defaultPlaceholders...),
			Sources:      append([]string(nil), defaultSources...),
			SetNamespace: true,
		},
		CollectorIdentity: CollectorIdentityConfig{Attribute: defaultIdentityAttribute},
	}
}

// Validate checks the fragment so a bad push is rejected by the collector's
// config validation instead of failing at runtime.
func (c *Config) Validate() error {
	var errs []error
	for i, d := range c.Drops {
		if _, err := compileMatcher(d.Metric); err != nil {
			errs = append(errs, fmt.Errorf("drops[%d]: %w", i, err))
		}
	}
	identity := c.CollectorIdentity.Attribute
	if identity == "" {
		identity = defaultIdentityAttribute
	}
	for i, a := range c.Aggregations {
		if _, err := compileMatcher(a.Metric); err != nil {
			errs = append(errs, fmt.Errorf("aggregations[%d]: %w", i, err))
		}
		if len(a.DropAttributes) == 0 {
			errs = append(errs, fmt.Errorf("aggregations[%d]: drop_attributes must not be empty", i))
		}
		for _, k := range a.DropAttributes {
			if k == identity {
				errs = append(errs, fmt.Errorf("aggregations[%d]: cannot drop the collector identity attribute %q", i, k))
			}
		}
		switch a.GaugeFunction {
		case "", "avg", "sum", "min", "max", "last":
		default:
			errs = append(errs, fmt.Errorf("aggregations[%d]: unknown gauge_function %q", i, a.GaugeFunction))
		}
	}
	as := c.AggregationState
	switch ttl := as.effectiveTTL(); {
	case as.TTL < 0:
		errs = append(errs, errors.New("aggregation_state.ttl must not be negative"))
	case ttl < minStateTTL:
		errs = append(errs, fmt.Errorf("aggregation_state.ttl must be at least %s", minStateTTL))
	case ttl < as.Warmup:
		errs = append(errs, fmt.Errorf("aggregation_state.ttl (%s) must be at least aggregation_state.warmup (%s)", ttl, as.Warmup))
	}
	if as.Warmup < 0 {
		errs = append(errs, errors.New("aggregation_state.warmup must not be negative"))
	}
	if as.GaugeStaleness < 0 {
		errs = append(errs, errors.New("aggregation_state.gauge_staleness must not be negative"))
	} else if as.GaugeStaleness > 0 && as.GaugeStaleness < minStateTTL {
		errs = append(errs, fmt.Errorf("aggregation_state.gauge_staleness must be 0 or at least %s", minStateTTL))
	}
	if c.AggregationState.MaxSources < 0 {
		errs = append(errs, errors.New("aggregation_state.max_sources must not be negative"))
	}
	if c.Cardinality.DefaultMaxSeries < 0 {
		errs = append(errs, errors.New("cardinality.default_max_series must not be negative"))
	}
	if c.Cardinality.Window < 0 {
		errs = append(errs, errors.New("cardinality.window must not be negative"))
	}
	for i, l := range c.Cardinality.Limits {
		if _, err := compileMatcher(l.Metric); err != nil {
			errs = append(errs, fmt.Errorf("cardinality.limits[%d]: %w", i, err))
		}
		if l.MaxSeries < 0 {
			errs = append(errs, fmt.Errorf("cardinality.limits[%d]: max_series must not be negative", i))
		}
	}
	return errors.Join(errs...)
}

// effectiveTTL is the TTL in force: 0 means the default.
func (a AggregationStateConfig) effectiveTTL() time.Duration {
	if a.TTL == 0 {
		return defaultStateTTL
	}
	return a.TTL
}

func (c *Config) identity() (string, string) {
	attr := c.CollectorIdentity.Attribute
	if attr == "" {
		attr = defaultIdentityAttribute
	}
	val := c.CollectorIdentity.Value
	if val == "" {
		val = os.Getenv("XSCALER_COLLECTOR_ID")
	}
	if val == "" {
		val, _ = os.Hostname()
	}
	if val == "" {
		val = "unknown"
	}
	return attr, val
}

// compileMatcher turns a glob or "regex:" pattern into an anchored regexp.
func compileMatcher(pattern string) (*regexp.Regexp, error) {
	if pattern == "" {
		return nil, errors.New("metric pattern must not be empty")
	}
	if rest, ok := strings.CutPrefix(pattern, "regex:"); ok {
		re, err := regexp.Compile("^(?:" + rest + ")$")
		if err != nil {
			return nil, fmt.Errorf("invalid regex %q: %w", rest, err)
		}
		return re, nil
	}
	var b strings.Builder
	b.WriteString("^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")
	return regexp.Compile(b.String())
}
