package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	"go.opentelemetry.io/otel/sdk/resource"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// errRefused is a refused connection as an exporter returns it.
func errRefused() error {
	return fmt.Errorf("traces export: %w", &url.Error{Op: "Post", URL: "http://10.89.2.1:4318/v1/traces", Err: &net.OpError{
		Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED),
	}})
}

func TestRefusedRetry(t *testing.T) {
	// export returns errs in turn, then nil.
	run := func(r *refusedRetry, ctx context.Context, errs ...error) (calls int, err error) {
		err = r.do(ctx, func(context.Context) error {
			calls++
			if calls <= len(errs) {
				return errs[calls-1]
			}
			return nil
		})
		return calls, err
	}
	quick := func(on bool) *refusedRetry {
		r := &refusedRetry{limit: time.Second, wait: time.Millisecond}
		r.on.Store(on)
		return r
	}
	ctx := t.Context()

	if calls, err := run(quick(true), ctx, errRefused(), errRefused()); calls != 3 || err != nil {
		t.Errorf("refused twice, then taken: %d calls, err %v; want 3, nil", calls, err)
	}
	other := errors.New("traces export: 500 Internal Server Error")
	if calls, err := run(quick(true), ctx, other, nil); calls != 1 || !errors.Is(err, other) {
		t.Errorf("another failure: %d calls, err %v; want 1 and that error", calls, err)
	}
	reset := fmt.Errorf("traces export: %w", syscall.ECONNRESET)
	if calls, err := run(quick(true), ctx, reset, nil); calls != 1 || !errors.Is(err, syscall.ECONNRESET) {
		t.Errorf("a reset (the request may have arrived): %d calls, err %v; want 1 and that error", calls, err)
	}
	if calls, err := run(quick(false), ctx, errRefused(), nil); calls != 1 || !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("off: %d calls, err %v; want 1 and the refusal", calls, err)
	}

	// Refused for good: gives up at the limit, with the refusal.
	always := make([]error, 10000)
	for i := range always {
		always[i] = errRefused()
	}
	r := &refusedRetry{limit: 50 * time.Millisecond, wait: 5 * time.Millisecond}
	r.on.Store(true)
	began := time.Now()
	calls, err := run(r, ctx, always...)
	if took := time.Since(began); calls < 2 || !errors.Is(err, syscall.ECONNREFUSED) || took > time.Second {
		t.Errorf("refused for good: %d calls in %v, err %v; want several, within the limit, and the refusal", calls, took, err)
	}

	// The caller's deadline ends the wait (the SDK's export timeout, a stop).
	r = &refusedRetry{limit: time.Minute, wait: time.Minute}
	r.on.Store(true)
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	began = time.Now()
	calls, err = run(r, short, always...)
	if took := time.Since(began); calls != 1 || !errors.Is(err, syscall.ECONNREFUSED) || took > 5*time.Second {
		t.Errorf("context done: %d calls in %v, err %v; want 1, at once, and the refusal", calls, took, err)
	}
}

// lateCollector reserves an address nothing listens on, and starts an OTLP
// collector there after delay. posts are the paths it was sent.
func lateCollector(t *testing.T, delay time.Duration) (addr string, posts func() []string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr = l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatal(err)
	}
	var (
		mu  sync.Mutex
		got []string
	)
	srv := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.URL.Path)
		mu.Unlock()
	})}
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		select {
		case <-stop: // the test ended first: never listens
			return
		case <-time.After(delay):
		}
		l, err := net.Listen("tcp", addr)
		if err != nil {
			t.Errorf("the collector couldn't take %s back: %v", addr, err)
			return
		}
		_ = srv.Serve(l)
	}()
	t.Cleanup(func() {
		close(stop)
		_ = srv.Close()
		<-done
	})
	return addr, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
}

// The upstream exporters, as Start wraps them, against a collector that
// isn't listening yet (Alloy restarting): the export waits for it and is
// delivered once, where the bare exporter fails with a refusal that
// errors.Is must recognise through the exporter's own wrapping.
func TestExportersRetryARefusedConnection(t *testing.T) {
	for _, k := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "OTEL_EXPORTER_OTLP_HEADERS"} {
		t.Setenv(k, "")
	}
	ctx := t.Context()
	retry := &refusedRetry{limit: 10 * time.Second, wait: 20 * time.Millisecond}
	spans := tracetest.SpanStubs{{Name: "work"}}.Snapshots()
	metrics := &metricdata.ResourceMetrics{Resource: resource.Empty(), ScopeMetrics: []metricdata.ScopeMetrics{{Metrics: []metricdata.Metrics{{
		Name: "runs", Data: metricdata.Sum[int64]{Temporality: metricdata.CumulativeTemporality, IsMonotonic: true, DataPoints: []metricdata.DataPoint[int64]{{Value: 1}}},
	}}}}}

	// Off (a start, a stop): the refusal comes straight back.
	addr, posts := lateCollector(t, time.Hour)
	spanExp, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpoint(addr), otlptracehttp.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	if err := (retryingSpanExporter{spanExp, retry}).ExportSpans(ctx, spans); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("spans, nothing listening, retry off: err %v; want a refused connection", err)
	}
	metricExp, err := otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpoint(addr), otlpmetrichttp.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	if err := (retryingMetricExporter{metricExp, retry}).Export(ctx, metrics); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("metrics, nothing listening, retry off: err %v; want a refused connection", err)
	}
	if got := posts(); len(got) != 0 {
		t.Fatalf("nothing listened, yet it was sent %v", got)
	}

	// On: each export outlasts the collector's absence.
	retry.on.Store(true)
	addr, posts = lateCollector(t, 300*time.Millisecond)
	spanExp, err = otlptracehttp.New(ctx, otlptracehttp.WithEndpoint(addr), otlptracehttp.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	if err := (retryingSpanExporter{spanExp, retry}).ExportSpans(ctx, spans); err != nil {
		t.Fatalf("spans, the collector 300ms late: %v", err)
	}
	if got := posts(); len(got) != 1 || got[0] != "/v1/traces" {
		t.Errorf("spans: the collector was sent %v; want /v1/traces once", got)
	}
	addr, posts = lateCollector(t, 300*time.Millisecond)
	metricExp, err = otlpmetrichttp.New(ctx, otlpmetrichttp.WithEndpoint(addr), otlpmetrichttp.WithInsecure())
	if err != nil {
		t.Fatal(err)
	}
	if err := (retryingMetricExporter{metricExp, retry}).Export(ctx, metrics); err != nil {
		t.Fatalf("metrics, the collector 300ms late: %v", err)
	}
	if got := posts(); len(got) != 1 || got[0] != "/v1/metrics" {
		t.Errorf("metrics: the collector was sent %v; want /v1/metrics once", got)
	}
}
