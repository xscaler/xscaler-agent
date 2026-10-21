package xscalerpolicyprocessor

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/confmap"
	"go.opentelemetry.io/collector/confmap/provider/envprovider"
	"go.opentelemetry.io/collector/confmap/xconfmap"
)

// decodeCase is one entry of testdata/policy_bodies.yaml.
type decodeCase struct {
	Name   string `mapstructure:"name"`
	Body   string `mapstructure:"body"`
	Accept bool   `mapstructure:"accept"`
	// Error is a substring the collector's error must contain.
	Error string `mapstructure:"error"`
	// Expect maps a config path (e.g. policy_version, drops.0.metric) to
	// the value the collector ends up with, as a string.
	Expect map[string]string `mapstructure:"expect"`
	// Portal is "reject" when the portal must be stricter than the
	// collector for this body. The collector test ignores it.
	Portal string `mapstructure:"portal"`
	Note   string `mapstructure:"note"`
}

// envValue is what ${env:XSCALER_POLICY_TEST_ENV} expands to in the cases.
const envValue = "from-collector-env"

// decodeLikeCollector loads body the way the collector loads the policy
// layer agent-api writes: the body goes under processors::xscaler_policy
// in a YAML file, the confmap resolver parses it and expands ${...}
// references (env is the default scheme), otelcol unmarshals the
// processors map and then the component's section into its default
// config, and xconfmap.Validate runs Config.Validate.
func decodeLikeCollector(t *testing.T, body string) (*Config, error) {
	t.Helper()
	var b strings.Builder
	b.WriteString("processors:\n  xscaler_policy:\n")
	for _, line := range strings.Split(body, "\n") {
		b.WriteString("    " + line + "\n")
	}
	layer := b.String()
	policy := confmap.NewProviderFactory(func(confmap.ProviderSettings) confmap.Provider {
		return layerProvider(layer)
	})
	res, err := confmap.NewResolver(confmap.ResolverSettings{
		URIs:              []string{"policy:layer"},
		ProviderFactories: []confmap.ProviderFactory{policy, envprovider.NewFactory()},
		DefaultScheme:     "env",
	})
	require.NoError(t, err)
	conf, err := res.Resolve(context.Background())
	if err != nil {
		return nil, err
	}
	procs, err := conf.Sub("processors")
	if err != nil {
		return nil, err
	}
	raw := map[component.ID]map[string]any{}
	if err = procs.Unmarshal(&raw); err != nil {
		return nil, err
	}
	sub, err := procs.Sub(componentType.String())
	if err != nil {
		return nil, err
	}
	cfg := NewFactory().CreateDefaultConfig()
	if err = sub.Unmarshal(&cfg); err != nil {
		return nil, err
	}
	if err = xconfmap.Validate(cfg); err != nil {
		return nil, err
	}
	return cfg.(*Config), nil
}

type layerProvider string

func (l layerProvider) Retrieve(context.Context, string, confmap.WatcherFunc) (*confmap.Retrieved, error) {
	return confmap.NewRetrievedFromYAML([]byte(l))
}
func (layerProvider) Scheme() string                 { return "policy" }
func (layerProvider) Shutdown(context.Context) error { return nil }

func loadDecodeCases(t *testing.T) []decodeCase {
	t.Helper()
	data, err := os.ReadFile("testdata/policy_bodies.yaml")
	require.NoError(t, err)
	r, err := confmap.NewRetrievedFromYAML(data)
	require.NoError(t, err)
	conf, err := r.AsConf()
	require.NoError(t, err)
	var file struct {
		Cases []decodeCase `mapstructure:"cases"`
	}
	require.NoError(t, conf.Unmarshal(&file))
	require.NotEmpty(t, file.Cases)
	return file.Cases
}

// TestPolicyBodyDecodeContract pins which policy bodies the collector
// accepts. The portal's validation mirrors testdata/policy_bodies.yaml:
// it must reject every body marked accept: false (or the push fails on
// the collector), and may only be stricter where portal: reject says so.
func TestPolicyBodyDecodeContract(t *testing.T) {
	t.Setenv("XSCALER_POLICY_TEST_ENV", envValue)
	seen := map[string]bool{}
	for _, tc := range loadDecodeCases(t) {
		require.False(t, seen[tc.Name], "duplicate case %q", tc.Name)
		seen[tc.Name] = true
		require.Contains(t, []string{"", "reject"}, tc.Portal, tc.Name)
		t.Run(tc.Name, func(t *testing.T) {
			cfg, err := decodeLikeCollector(t, tc.Body)
			if !tc.Accept {
				require.Error(t, err, "collector accepted a body marked accept: false")
				if tc.Error != "" {
					assert.Contains(t, err.Error(), tc.Error)
				}
				return
			}
			require.NoError(t, err)
			for path, want := range tc.Expect {
				assert.Equal(t, want, configValue(t, cfg, path), path)
			}
		})
	}
}

// configValue reads the few fields the cases pin, as strings.
func configValue(t *testing.T, cfg *Config, path string) string {
	t.Helper()
	switch path {
	case "policy_version":
		return cfg.PolicyVersion
	case "drops.0.metric":
		return cfg.Drops[0].Metric
	case "aggregations.0.drop_attributes.0":
		return cfg.Aggregations[0].DropAttributes[0]
	case "service_identity.enabled":
		return fmt.Sprint(cfg.ServiceIdentity.Enabled)
	case "collector_identity.value":
		return cfg.CollectorIdentity.Value
	case "aggregation_state.ttl":
		return cfg.AggregationState.TTL.String()
	case "cardinality.limits.0.max_series":
		return fmt.Sprint(cfg.Cardinality.Limits[0].MaxSeries)
	}
	t.Fatalf("unknown expect path %q", path)
	return ""
}
