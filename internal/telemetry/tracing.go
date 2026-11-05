// Package telemetry sets up the gateway's OpenTelemetry tracing.
package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.39.0"

	"github.com/solidDoWant/toolgate/internal/version"
)

// ServiceName is the `service.name` resource attribute of the gateway's spans.
const ServiceName = "toolgate"

// NewTracerProvider returns a tracer provider that exports spans over
// OTLP/HTTP to endpoint (a base URL; spans go to its `/v1/traces`). With an
// empty endpoint, spans are created, so trace context still propagates, but
// not exported. Call Shutdown on the result to flush the spans.
func NewTracerProvider(ctx context.Context, endpoint string) (*sdktrace.TracerProvider, error) {
	resource, err := sdkresource.Merge(sdkresource.Default(), sdkresource.NewSchemaless(
		semconv.ServiceName(ServiceName),
		semconv.ServiceVersion(version.Get()),
	))
	if err != nil {
		return nil, fmt.Errorf("tracing resource: %w", err)
	}

	options := []sdktrace.TracerProviderOption{sdktrace.WithResource(resource)}

	if endpoint != "" {
		exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint+"/v1/traces"))
		if err != nil {
			return nil, fmt.Errorf("OTLP trace exporter: %w", err)
		}

		options = append(options, sdktrace.WithBatcher(exporter))
	}

	return sdktrace.NewTracerProvider(options...), nil
}

// Propagator is the W3C trace-context propagator, which reads and writes the
// `traceparent` and `tracestate` headers.
func Propagator() propagation.TextMapPropagator {
	return propagation.TraceContext{}
}

// SetGlobalPropagator installs the W3C trace-context propagator as the
// process-wide OpenTelemetry default.
func SetGlobalPropagator() {
	otel.SetTextMapPropagator(Propagator())
}
