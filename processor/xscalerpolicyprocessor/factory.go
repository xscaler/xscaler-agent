// Package xscalerpolicyprocessor applies the xScaler collection policy that
// the portal pushes over OpAMP: metric drops, service-level aggregation,
// per-metric cardinality limits and service identity. It reports what it
// changed through its own metrics so the portal can show the savings.
package xscalerpolicyprocessor

import (
	"context"

	"go.opentelemetry.io/collector/component"
	"go.opentelemetry.io/collector/consumer"
	"go.opentelemetry.io/collector/processor"
	"go.opentelemetry.io/collector/processor/processorhelper"
)

const stability = component.StabilityLevelDevelopment

var componentType = component.MustNewType("xscaler_policy")

// NewFactory returns the factory OCB registers for `xscaler_policy`.
func NewFactory() processor.Factory {
	return processor.NewFactory(
		componentType,
		func() component.Config { return createDefaultConfig() },
		processor.WithMetrics(createMetrics, stability),
		processor.WithLogs(createLogs, stability),
		processor.WithTraces(createTraces, stability),
	)
}

// The processor mutates data in place.
var mutates = processorhelper.WithCapabilities(consumer.Capabilities{MutatesData: true})

func createMetrics(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Metrics) (processor.Metrics, error) {
	p, err := newPolicyProcessor(set, cfg.(*Config))
	if err != nil {
		return nil, err
	}
	return processorhelper.NewMetrics(ctx, set, cfg, next, p.processMetrics, mutates)
}

func createLogs(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Logs) (processor.Logs, error) {
	p, err := newPolicyProcessor(set, cfg.(*Config))
	if err != nil {
		return nil, err
	}
	return processorhelper.NewLogs(ctx, set, cfg, next, p.processLogs, mutates)
}

func createTraces(ctx context.Context, set processor.Settings, cfg component.Config, next consumer.Traces) (processor.Traces, error) {
	p, err := newPolicyProcessor(set, cfg.(*Config))
	if err != nil {
		return nil, err
	}
	return processorhelper.NewTraces(ctx, set, cfg, next, p.processTraces, mutates)
}
