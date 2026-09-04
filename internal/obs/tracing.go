package obs

import (
	"context"
	"fmt"
	"log/slog"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.43.0"
	"go.opentelemetry.io/otel/trace"
)

// Tracing matters more for this kind of service than for an ordinary one. A single turn
// is retrieval, then a model call, then possibly a tool call and a second model call.
// Metrics can tell you a turn took eight seconds; only a trace says which of those it
// was.
//
// Attribute names follow OpenTelemetry's GenAI semantic conventions, so nothing here
// invents a vocabulary.
const (
	AttrGenAISystem        = "gen_ai.system"
	AttrGenAIRequestModel  = "gen_ai.request.model"
	AttrGenAIResponseModel = "gen_ai.response.model"
	AttrGenAIInputTokens   = "gen_ai.usage.input_tokens"
	AttrGenAIOutputTokens  = "gen_ai.usage.output_tokens"
	AttrGenAIFinishReason  = "gen_ai.response.finish_reasons"
)

type TracingOptions struct {
	Enabled     bool
	Endpoint    string
	ServiceName string
	Sampling    float64
}

// StartTracing configures the exporter and returns a shutdown function.
//
// Export is off unless a collector is actually running, so `make run` on its own does
// not fill the log with failed exports.
//
// Sampling defaults to 1.0 rather than a fraction: at a lower rate most conversations
// produce no trace at all, which reads as "tracing is broken" rather than "tracing is
// sampled". Lower it deliberately under real traffic.
func StartTracing(ctx context.Context, opts TracingOptions) (func(context.Context) error, error) {
	if !opts.Enabled {
		return func(context.Context) error { return nil }, nil
	}

	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(opts.Endpoint+"/v1/traces"))
	if err != nil {
		return nil, fmt.Errorf("create OTLP exporter: %w", err)
	}

	// Merged against the default resource rather than replacing it, and with the
	// schema URL left off: resource.Default() carries the SDK's own schema version, and
	// pinning a different one here makes Merge fail at startup with "conflicting Schema
	// URL". Naming a version that happens to match today would break on the next SDK
	// upgrade for no benefit.
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		semconv.ServiceName(opts.ServiceName),
	))
	if err != nil {
		return nil, err
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(opts.Sampling))),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{}, propagation.Baggage{}))

	slog.Info("tracing enabled", "endpoint", opts.Endpoint, "sampling", opts.Sampling)
	return provider.Shutdown, nil
}

// Tracer is the one tracer this service uses.
func Tracer() trace.Tracer { return otel.Tracer("github.com/lai3d/ai-customer-service-go") }

// TraceID is the current trace id, for handing a customer-facing response a way to be
// found in the backend. Empty when nothing is being traced.
func TraceID(ctx context.Context) string {
	span := trace.SpanContextFromContext(ctx)
	if !span.HasTraceID() {
		return ""
	}
	return span.TraceID().String()
}

// RetrievalAttributes describes a vector search without describing the search.
//
// The customer's question is deliberately absent, and that omission is the point. Spring
// AI attached the query text to every vector-store span unconditionally, with no
// property to switch it off -- found by reading a customer's question back out of
// Jaeger, not from documentation. A support question is often the most sensitive thing
// in the request, and traces are retained and read far more widely than a database is.
//
// Everything that makes the span useful is kept: how many passages, what was asked for,
// what came back, how long it took.
func RetrievalAttributes(topK, returned int, threshold float64, dimensions int) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.Int("db.vector.query.top_k", topK),
		attribute.Int("db.vector.query.returned", returned),
		attribute.Float64("db.vector.query.similarity_threshold", threshold),
		attribute.Int("db.vector.query.dimensions", dimensions),
		attribute.String("db.system", "postgresql"),
		attribute.String("db.operation.name", "similarity_search"),
	}
}
