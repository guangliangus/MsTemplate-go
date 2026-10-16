// Package telemetry wires OpenTelemetry the way the platform splits its signals: traces are pushed
// over OTLP (Jaeger locally, the cluster collector in production), metrics are scraped from
// /metrics by Prometheus, logs go to stdout. The three never overlap, so nothing is reported twice.
//
// Every other package reaches the providers through the otel globals (otel.Tracer, otel.Meter),
// which Setup installs; nothing needs them passed around.
package telemetry

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	promexporter "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

// Config is bound from the "telemetry" section.
type Config struct {
	// OTLPEndpoint receives traces over OTLP/gRPC, as a URL (http://jaeger:4317). Empty disables the
	// exporter unless OTEL_EXPORTER_OTLP_ENDPOINT is set. Metrics are never pushed here.
	OTLPEndpoint string `koanf:"otlp_endpoint"`
	// TraceSampleRatio is the head-sampling ratio (0..1), applied parent-based so a sampled upstream
	// keeps its children sampled across services. 1.0 records everything: fine locally, a cost hole
	// in production, where the chart lowers it. Tail sampling belongs to the collector.
	TraceSampleRatio float64 `koanf:"trace_sample_ratio" validate:"min=0,max=1"`
	// AttributeValueLengthLimit truncates any span attribute value longer than this many bytes; 0
	// keeps the SDK default of unlimited. A SQL statement is one attribute, and one with a long IN
	// list can be hundreds of kilobytes: a few of them turn an export batch into megabytes.
	AttributeValueLengthLimit int `koanf:"attribute_value_length_limit" validate:"min=0"`
}

// ServiceInfo names the process in the resource attributes.
type ServiceInfo struct {
	Name        string
	Version     string
	Environment string
	Instance    string
}

// Provider owns the tracer and meter providers and the Prometheus registry behind /metrics.
type Provider struct {
	tracer   *sdktrace.TracerProvider
	meter    *sdkmetric.MeterProvider
	registry *prometheus.Registry
}

// Setup installs the global providers and propagators and returns the handle to shut them down.
func Setup(ctx context.Context, cfg Config, svc ServiceInfo, log *slog.Logger) (*Provider, error) {
	res, err := resource.New(ctx,
		resource.WithTelemetrySDK(),
		resource.WithAttributes(
			semconv.ServiceName(svc.Name),
			semconv.ServiceVersion(svc.Version),
			semconv.ServiceInstanceID(svc.Instance),
			semconv.DeploymentEnvironmentName(svc.Environment),
		),
	)
	if err != nil {
		return nil, err
	}

	tracer, err := newTracerProvider(ctx, cfg, res)
	if err != nil {
		return nil, err
	}
	otel.SetTracerProvider(tracer)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	exporter, err := promexporter.New(
		promexporter.WithRegisterer(registry),
		promexporter.WithoutScopeInfo(),
	)
	if err != nil {
		return nil, err
	}
	meter := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter), sdkmetric.WithResource(res))
	otel.SetMeterProvider(meter)

	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Warn("opentelemetry error", slog.Any("error", err))
	}))

	return &Provider{tracer: tracer, meter: meter, registry: registry}, nil
}

func newTracerProvider(ctx context.Context, cfg Config, res *resource.Resource) (*sdktrace.TracerProvider, error) {
	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.TraceSampleRatio))
	limits := sdktrace.NewSpanLimits()
	if cfg.AttributeValueLengthLimit > 0 {
		limits.AttributeValueLengthLimit = cfg.AttributeValueLengthLimit
	}
	opts := []sdktrace.TracerProviderOption{
		sdktrace.WithResource(res), sdktrace.WithSampler(sampler), sdktrace.WithRawSpanLimits(limits),
	}

	endpoint := cfg.OTLPEndpoint
	if endpoint == "" {
		endpoint = os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	}
	if endpoint != "" {
		exporter, err := otlptracegrpc.New(ctx, otlptracegrpc.WithEndpointURL(endpoint))
		if err != nil {
			return nil, err
		}
		opts = append(opts, sdktrace.WithBatcher(exporter))
	}
	return sdktrace.NewTracerProvider(opts...), nil
}

// MetricsHandler serves the Prometheus exposition for /metrics.
func (p *Provider) MetricsHandler() http.Handler {
	return promhttp.HandlerFor(p.registry, promhttp.HandlerOpts{})
}

// Shutdown flushes pending spans and stops the providers.
func (p *Provider) Shutdown(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return errors.Join(p.tracer.Shutdown(ctx), p.meter.Shutdown(ctx))
}
