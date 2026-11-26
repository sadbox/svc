package telemetry

import (
	"context"
	"errors"
	"sync/atomic"
	"syscall"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// refusedRetry tries an export again while the collector refuses the
// connection: Alloy restarting, its receiver closed for some seconds. The
// upstream exporters retry only what a collector answers (429, 502-504);
// a refused connection fails at once, and the batch processor then drops
// the batch, so every restart of the collector lost each service's spans
// of that time. Nothing was sent when a connection is refused, so nothing
// can arrive twice (which is why a reset or a timeout isn't retried).
//
// Only while on: [Start] turns it on after its first export and off before
// the last, so neither a start nor a stop waits for a collector that's
// down any longer than it did.
type refusedRetry struct {
	on atomic.Bool
	// How long one export keeps trying, under the SDK's own 30s export
	// timeouts (OTEL_BSP_EXPORT_TIMEOUT, OTEL_METRIC_EXPORT_TIMEOUT), and
	// the pause between tries. Spans ended meanwhile wait in the batch
	// processor's queue (2048 by default; past that they are dropped).
	limit, wait time.Duration
}

func newRefusedRetry() *refusedRetry {
	return &refusedRetry{limit: 20 * time.Second, wait: time.Second}
}

// do calls export until it returns anything but a refused connection, the
// limit is reached or ctx is done, and returns export's last error.
func (r *refusedRetry) do(ctx context.Context, export func(context.Context) error) error {
	start := time.Now()
	for {
		err := export(ctx)
		if err == nil || !errors.Is(err, syscall.ECONNREFUSED) || !r.on.Load() || time.Since(start)+r.wait > r.limit {
			return err
		}
		t := time.NewTimer(r.wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return err
		case <-t.C:
		}
	}
}

// retryingSpanExporter is a SpanExporter whose exports go through retry.
type retryingSpanExporter struct {
	sdktrace.SpanExporter
	retry *refusedRetry
}

func (e retryingSpanExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	return e.retry.do(ctx, func(ctx context.Context) error { return e.SpanExporter.ExportSpans(ctx, spans) })
}

// retryingMetricExporter is a metric Exporter whose exports go through
// retry. A metric export that fails loses nothing (the counters are
// cumulative: the next one carries them), but it leaves a gap in the
// series and a warning in the log.
type retryingMetricExporter struct {
	sdkmetric.Exporter
	retry *refusedRetry
}

func (e retryingMetricExporter) Export(ctx context.Context, rm *metricdata.ResourceMetrics) error {
	return e.retry.do(ctx, func(ctx context.Context) error { return e.Exporter.Export(ctx, rm) })
}
