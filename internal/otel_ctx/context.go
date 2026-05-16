package otel_ctx

import (
	"context"

	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

type tracerKey struct{}

// WithTracer stores a tracer in ctx (e.g. from echo-opentelemetry middleware).
func WithTracer(ctx context.Context, t trace.Tracer) context.Context {
	if t == nil {
		return ctx
	}
	return context.WithValue(ctx, tracerKey{}, t)
}

var noopTracer = noop.NewTracerProvider().Tracer("")

// Tracer returns the tracer from ctx, or a noop tracer when none was injected.
func Tracer(ctx context.Context) trace.Tracer {
	if t, ok := ctx.Value(tracerKey{}).(trace.Tracer); ok && t != nil {
		return t
	}
	return noopTracer
}
