package trace

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

const (
	defaultOtelEndpoint    = "localhost:4317"
	defaultOtelServiceName = "deepseek-cursor-proxy-go"
	otelTimeout            = 5 * time.Second
)

// OTelConfig holds OpenTelemetry configuration.
type OTelConfig struct {
	Endpoint    string
	ServiceName string
}

// OTelTracer wraps the OTel tracer provider and tracer.
type OTelTracer struct {
	provider *sdktrace.TracerProvider
	tracer   trace.Tracer
}

// InitOTel initializes the OTel tracer provider with an OTLP gRPC exporter.
// Returns a cleanup function that should be deferred on shutdown.
func InitOTel(ctx context.Context, cfg OTelConfig) (*OTelTracer, error) {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = defaultOtelEndpoint
	}
	serviceName := cfg.ServiceName
	if serviceName == "" {
		serviceName = defaultOtelServiceName
	}

	// Create gRPC connection to Jaeger OTLP collector
	conn, err := grpc.NewClient(
		endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(10*1024*1024)),
	)
	if err != nil {
		return nil, fmt.Errorf("cannot create gRPC connection to %s: %w", endpoint, err)
	}

	// Create OTLP trace exporter over gRPC
	exporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithGRPCConn(conn),
		otlptracegrpc.WithTimeout(otelTimeout),
	)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("cannot create OTLP trace exporter: %w", err)
	}

	// Read container hostname (container ID in Docker) for trace identification
	hostname, _ := os.Hostname()

	// Build resource with service metadata
	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(serviceName),
			semconv.ServiceVersionKey.String("0.1.0"),
			attribute.String("container.hostname", hostname),
		),
	)
	if err != nil {
		conn.Close()
		exporter.Shutdown(ctx)
		return nil, fmt.Errorf("cannot create OTel resource: %w", err)
	}

	// Create batch span processor with default settings
	bsp := sdktrace.NewBatchSpanProcessor(exporter)

	// Create tracer provider
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithResource(res),
		sdktrace.WithSpanProcessor(bsp),
	)

	// Set global propagator for context propagation
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// Set global tracer provider
	otel.SetTracerProvider(tp)

	tracer := tp.Tracer(serviceName)

	log.Printf("otel: initialized tracer provider, exporting to %s (service=%s)", endpoint, serviceName)

	return &OTelTracer{
		provider: tp,
		tracer:   tracer,
	}, nil
}

// Shutdown gracefully shuts down the tracer provider, flushing any remaining spans.
func (o *OTelTracer) Shutdown(ctx context.Context) error {
	if o.provider == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := o.provider.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("otel shutdown error: %w", err)
	}
	log.Printf("otel: tracer provider shut down")
	return nil
}

// Tracer returns the underlying tracer.
func (o *OTelTracer) Tracer() trace.Tracer {
	if o == nil || o.tracer == nil {
		return noop.NewTracerProvider().Tracer("noop")
	}
	return o.tracer
}

// Provider returns the underlying tracer provider.
func (o *OTelTracer) Provider() *sdktrace.TracerProvider {
	if o == nil {
		return nil
	}
	return o.provider
}
