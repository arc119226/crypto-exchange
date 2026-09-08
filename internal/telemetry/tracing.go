package telemetry

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Tracing is the minimal OpenTelemetry setup of docs/plan-v1.0.md §15: one
// span per HTTP request, per pgx query, per NATS request/reply and per
// consumed event, with the W3C traceparent carried through the outbox and
// NATS headers so a trace follows an order from the api role through the
// engine's transaction to every consumer. Everything here is a no-op until
// SetupTracing installs a provider, so a process without
// OTEL_EXPORTER_OTLP_ENDPOINT pays only a context lookup per call.
//
// Only this package and internal/platform/pg import the OTel modules; the
// rest of the code base sees StartSpan / InjectTrace / ExtractTrace.

const instrumentationName = "github.com/arc119226/crypto-exchange"

// TraceIDKey is the log attribute carrying the current trace id, so a log
// line and the trace it belongs to can be matched in either direction.
const TraceIDKey = "trace_id"

// TracingConfig configures SetupTracing.
type TracingConfig struct {
	// Endpoint is the OTLP/HTTP collector base URL (OTEL_EXPORTER_OTLP_ENDPOINT,
	// e.g. http://jaeger:4318). Empty disables tracing.
	Endpoint    string
	ServiceName string
	Version     string
}

// SetupTracing installs the global tracer provider and the W3C propagator
// when cfg.Endpoint is set. Sampling is left to the SDK's standard
// OTEL_TRACES_SAMPLER / OTEL_TRACES_SAMPLER_ARG variables (default: sample
// everything the parent sampled). Exporter errors go to the debug log, so
// a collector that is simply not running (the observability profile is
// off) does not fill the log. The returned function flushes and stops the
// provider.
func SetupTracing(ctx context.Context, cfg TracingConfig, log *slog.Logger) (func(context.Context) error, error) {
	if cfg.Endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	if log == nil {
		log = slog.Default()
	}
	endpoint, err := tracesURL(cfg.Endpoint)
	if err != nil {
		return nil, err
	}
	exp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint))
	if err != nil {
		return nil, fmt.Errorf("telemetry: otlp exporter: %w", err)
	}
	res, err := resource.Merge(resource.Default(), resource.NewSchemaless(
		attribute.String("service.name", cfg.ServiceName),
		attribute.String("service.version", cfg.Version),
	))
	if err != nil {
		return nil, fmt.Errorf("telemetry: resource: %w", err)
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		log.Debug("otel exporter", slog.String("err", err.Error()))
	}))
	log.Info("tracing enabled", slog.String("otlp_endpoint", cfg.Endpoint), slog.String("service", cfg.ServiceName))
	return tp.Shutdown, nil
}

// tracesURL turns the base URL OTEL_EXPORTER_OTLP_ENDPOINT denotes into the
// traces URL the exporter posts to: the spec appends /v1/traces to a base
// endpoint, while WithEndpointURL takes its argument as the full URL, so a
// bare http://jaeger:4318 would otherwise post to "/". A URL that already
// carries a path is used as it is.
func tracesURL(endpoint string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("telemetry: OTEL_EXPORTER_OTLP_ENDPOINT %q is not a URL", endpoint)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/v1/traces"
	}
	return u.String(), nil
}

func tracer() trace.Tracer { return otel.GetTracerProvider().Tracer(instrumentationName) }

// WithSpanOf returns dst carrying src's current span, so work that crosses
// a queue (the engine's per-market runner) can continue the caller's trace
// without inheriting the caller's cancellation.
func WithSpanOf(dst, src context.Context) context.Context {
	return trace.ContextWithSpan(dst, trace.SpanFromContext(src))
}

// SpanKind says which side of a call a span is; it maps onto the OTel kinds.
type SpanKind int

// The kinds this code base uses.
const (
	SpanInternal SpanKind = iota
	SpanServer
	SpanClient
	SpanProducer
	SpanConsumer
)

func (k SpanKind) otel() trace.SpanKind {
	switch k {
	case SpanServer:
		return trace.SpanKindServer
	case SpanClient:
		return trace.SpanKindClient
	case SpanProducer:
		return trace.SpanKindProducer
	case SpanConsumer:
		return trace.SpanKindConsumer
	default:
		return trace.SpanKindInternal
	}
}

// StartSpan begins a span under whatever span ctx carries and returns the
// context to pass on plus the function that ends it. A non-nil err passed
// to end is recorded and marks the span failed. Attributes are plain
// strings so callers need no OTel import.
func StartSpan(ctx context.Context, name string, kind SpanKind, attrs map[string]string) (context.Context, func(err error)) {
	kvs := make([]attribute.KeyValue, 0, len(attrs))
	for k, v := range attrs {
		kvs = append(kvs, attribute.String(k, v))
	}
	ctx, span := tracer().Start(ctx, name, trace.WithSpanKind(kind.otel()), trace.WithAttributes(kvs...))
	return ctx, func(err error) {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}

// TraceID returns the hex trace id of the span in ctx, or "" when there is
// no sampled span (which is always the case without a provider).
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() || !sc.IsSampled() {
		return ""
	}
	return sc.TraceID().String()
}

// InjectTrace writes the W3C trace headers for ctx's span into carrier
// (nothing when there is no span or no propagator). The keys are lower
// case: "traceparent" and, when present, "tracestate".
func InjectTrace(ctx context.Context, carrier map[string]string) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.MapCarrier(carrier))
}

// ExtractTrace returns ctx with the remote span context found in carrier
// as the parent for the next StartSpan; ctx unchanged when there is none.
func ExtractTrace(ctx context.Context, carrier map[string]string) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier(carrier))
}

// TracingMiddleware opens a server span per request, parented on the
// incoming traceparent when there is one. routeOf is read after the
// handler ran, so a router can report the matched pattern ("GET
// /v1/orders/{id}") as the span name instead of the raw path; the span
// starts as the bare method. When the span is sampled the request logger
// gains trace_id. Mount it after CorrelationMiddleware.
func TracingMiddleware(routeOf func(*http.Request) string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
			ctx, span := tracer().Start(ctx, r.Method, trace.WithSpanKind(trace.SpanKindServer), trace.WithAttributes(
				attribute.String("http.request.method", r.Method),
				attribute.String("url.path", r.URL.Path),
			))
			defer span.End()
			if span.IsRecording() {
				if cid := CorrelationID(ctx); cid != "" {
					span.SetAttributes(attribute.String("exchange.correlation_id", cid))
				}
				if tid := TraceID(ctx); tid != "" {
					ctx = WithLogger(ctx, Logger(ctx).With(slog.String(TraceIDKey, tid)))
				}
			}
			sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(sw, r.WithContext(ctx))
			if route := routeOf(r); route != "" {
				span.SetName(r.Method + " " + route)
				span.SetAttributes(attribute.String("http.route", route))
			}
			span.SetAttributes(attribute.Int("http.response.status_code", sw.status))
			if sw.status >= http.StatusInternalServerError {
				span.SetStatus(codes.Error, strconv.Itoa(sw.status))
			}
		})
	}
}

// TracingTransport wraps an HTTP client transport so outbound requests
// carry traceparent and get a client span (the webhook dispatcher: the
// customer's endpoint receives the trace id of the delivery).
func TracingTransport(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return otelhttp.NewTransport(base)
}
