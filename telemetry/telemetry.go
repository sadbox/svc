// Package telemetry sets up OpenTelemetry metrics and traces for sadbox's
// services, exported over OTLP/HTTP to the host's Alloy, and gives them the
// few helpers they share: an instrumented HTTP handler and transport, and
// [Job] for background work.
//
// Configuration is the standard OTEL_* environment, read by the upstream
// SDK and exporters; nothing here adds a variable of its own:
//
//	OTEL_EXPORTER_OTLP_ENDPOINT=http://10.89.2.1:4318
//	OTEL_RESOURCE_ATTRIBUTES=service.namespace=sadbox,service.instance.id=bonnet,deployment.environment.name=prod
//
// With no OTLP endpoint set (tests, local runs) or OTEL_SDK_DISABLED=true,
// [Start] installs nothing: the global providers stay OpenTelemetry's
// no-ops and every helper here costs next to nothing.
//
// What leaves the process is bounded and redacted in one place, so a
// service can't leak by forgetting: spans lose raw URLs, paths, queries,
// user agents and client addresses ([redactingExporter]); HTTP metrics keep
// only method, status, route and dependency name ([views]).
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// shutdownTimeout bounds the final flush in the function [Start] returns,
// so a stop never waits on an unreachable collector past the unit's
// StopTimeout.
const shutdownTimeout = 5 * time.Second

// Enabled reports whether the environment asks for export: an OTLP
// endpoint is set and the SDK isn't disabled.
func Enabled() bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_SDK_DISABLED")), "true") {
		return false
	}
	for _, k := range []string{
		"OTEL_EXPORTER_OTLP_ENDPOINT",
		"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"OTEL_EXPORTER_OTLP_METRICS_ENDPOINT",
	} {
		if os.Getenv(k) != "" {
			return true
		}
	}
	return false
}

// Start installs the global MeterProvider, TracerProvider and W3C trace
// context propagator, and starts the Go runtime metrics. service is the
// default service.name; OTEL_SERVICE_NAME or OTEL_RESOURCE_ATTRIBUTES
// override it.
//
// The returned function flushes and stops both providers (bounded by a few
// seconds); call it on the way out. When export isn't enabled ([Enabled])
// Start installs nothing and returns a no-op.
//
// Export failures (the collector down) never fail the service: the SDK
// drops what it can't send and reports through [otel.SetErrorHandler],
// which Start points at slog.
func Start(ctx context.Context, service string) (shutdown func(), err error) {
	if !Enabled() {
		return func() {}, nil
	}

	res, err := newResource(ctx, service)
	if err != nil {
		return nil, err
	}

	spanExp, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("telemetry: trace exporter: %w", err)
	}
	metricExp, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("telemetry: metric exporter: %w", err)
	}

	// The sampler is the SDK's default (parent-based, always on), unless
	// OTEL_TRACES_SAMPLER says otherwise: the SDK reads it.
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithResource(res),
		sdktrace.WithBatcher(redactingExporter{spanExp}),
	)
	// The export interval is OTEL_METRIC_EXPORT_INTERVAL (default 60s),
	// read by the reader.
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExp)),
		sdkmetric.WithView(views()...),
	)

	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		slog.Warn("telemetry: export", "err", scrubURLs(err.Error()))
	}))
	otel.SetTracerProvider(tp)
	otel.SetMeterProvider(mp)
	otel.SetTextMapPropagator(propagation.TraceContext{})

	if err := runtime.Start(runtime.WithMeterProvider(mp)); err != nil {
		return nil, fmt.Errorf("telemetry: runtime metrics: %w", err)
	}

	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := errors.Join(tp.Shutdown(ctx), mp.Shutdown(ctx)); err != nil {
			slog.Warn("telemetry: shutdown", "err", scrubURLs(err.Error()))
		}
	}, nil
}

// newResource describes the process: service.name (the default given,
// overridable from the environment), service.version from the build's VCS
// revision, plus whatever OTEL_RESOURCE_ATTRIBUTES adds. No host detector:
// a container's hostname is its ID, which changes on every restart and
// would start new series each time; service.instance.id comes from the
// unit's environment instead.
func newResource(ctx context.Context, service string) (*resource.Resource, error) {
	attrs := resource.WithAttributes(semconv.ServiceName(service))
	opts := []resource.Option{attrs, resource.WithFromEnv(), resource.WithTelemetrySDK()}
	if v := buildRevision(); v != "" {
		opts = append(opts, resource.WithAttributes(semconv.ServiceVersion(v)))
	}
	// WithFromEnv comes after the default name so OTEL_SERVICE_NAME and
	// OTEL_RESOURCE_ATTRIBUTES win.
	res, err := resource.New(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("telemetry: resource: %w", err)
	}
	return res, nil
}

// buildRevision is the short VCS revision the binary was built from, or "".
func buildRevision() string {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 7 {
			return s.Value[:7]
		}
	}
	return ""
}
