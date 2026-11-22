package telemetry

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// harness installs global providers wired as Start wires them (the
// redacting exporter, the views), with in-memory export.
type harness struct {
	spans  *tracetest.InMemoryExporter
	reader *sdkmetric.ManualReader
	tp     *sdktrace.TracerProvider
}

// shared is the one harness: Job's instruments bind to the global
// MeterProvider once per process, as they do in a service (Start runs
// once), so every test shares one provider; each gets fresh spans.
var shared struct {
	sync.Once
	h *harness
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	shared.Do(func() {
		h := &harness{spans: tracetest.NewInMemoryExporter(), reader: sdkmetric.NewManualReader()}
		h.tp = sdktrace.NewTracerProvider(sdktrace.WithSyncer(redactingExporter{h.spans}))
		mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(h.reader), sdkmetric.WithView(views()...))
		otel.SetTracerProvider(h.tp)
		otel.SetMeterProvider(mp)
		otel.SetTextMapPropagator(propagation.TraceContext{})
		shared.h = h
	})
	shared.h.spans.Reset()
	return shared.h
}

func (h *harness) metrics(t *testing.T) map[string]metricdata.Aggregation {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := h.reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	out := map[string]metricdata.Aggregation{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			out[m.Name] = m.Data
		}
	}
	return out
}

func histPoints(t *testing.T, a metricdata.Aggregation) []metricdata.HistogramDataPoint[float64] {
	t.Helper()
	h, ok := a.(metricdata.Histogram[float64])
	if !ok {
		t.Fatalf("not a float64 histogram: %T", a)
	}
	return h.DataPoints
}

func attrKeys(s attribute.Set) []string {
	var ks []string
	for _, kv := range s.ToSlice() {
		ks = append(ks, string(kv.Key))
	}
	return ks
}

func hasKey(kvs []attribute.KeyValue, k attribute.Key) bool {
	for _, kv := range kvs {
		if kv.Key == k {
			return true
		}
	}
	return false
}

func TestEnabled(t *testing.T) {
	for _, tc := range []struct {
		env  map[string]string
		want bool
	}{
		{map[string]string{}, false},
		{map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://10.89.2.1:4318"}, true},
		{map[string]string{"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://x:4318/v1/traces"}, true},
		{map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://x:4318", "OTEL_SDK_DISABLED": "TRUE"}, false},
		{map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://x:4318", "OTEL_SDK_DISABLED": "false"}, true},
	} {
		for _, k := range []string{"OTEL_SDK_DISABLED", "OTEL_EXPORTER_OTLP_ENDPOINT",
			"OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "OTEL_EXPORTER_OTLP_METRICS_ENDPOINT"} {
			t.Setenv(k, tc.env[k])
		}
		if got := Enabled(); got != tc.want {
			t.Errorf("Enabled() with %v = %v, want %v", tc.env, got, tc.want)
		}
	}
}

func TestStartDisabledIsNoop(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")
	before := otel.GetTracerProvider()
	shutdown, err := Start(context.Background(), "x")
	if err != nil {
		t.Fatal(err)
	}
	shutdown()
	if otel.GetTracerProvider() != before {
		t.Error("Start installed a TracerProvider with export disabled")
	}
}

func TestScrubURLs(t *testing.T) {
	for in, want := range map[string]string{
		`Post "https://discord.com/api/v10/webhooks/123/SECRET?wait=true": EOF`: `Post "https://discord.com/…": EOF`,
		`get http://10.0.0.1:9000/a?X-Amz-Signature=abc failed`:                 `get http://10.0.0.1:9000/… failed`,
		`no url here`: `no url here`,
		`dial https://user:pass@db.example/x failed`: `dial https://db.example/… failed`,
		`"https://tok@hooks.example/a?b=c"`:          `"https://hooks.example/…"`,
		`HTTPS://API.EXAMPLE/v1/secret?k=v`:          `HTTPS://API.EXAMPLE/…`,
		`Http://u@h:8080`:                            `Http://h:8080/…`,
	} {
		if got := scrubURLs(in); got != want {
			t.Errorf("scrubURLs(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRedactingExporter(t *testing.T) {
	h := newHarness(t)
	_, span := otel.Tracer("t").Start(context.Background(), "s")
	span.SetAttributes(
		attribute.String("url.full", "https://x/secret"),
		attribute.String("url.path", "/profiles/1/2/3"),
		attribute.String("user_agent.original", "ua"),
		attribute.String("client.address", "203.0.113.9"),
		attribute.String("http.request.header.cookie", "c"),
		attribute.String("http.route", "/profiles/{g}/{t}/{u}"),
		attribute.String("note", "see https://example.com/a/b?tok=1"),
	)
	span.RecordError(errors.New(`Get "https://api.example/x?token=t": timeout`))
	span.SetStatus(codes.Error, `Get "https://api.example/x?token=t": timeout`)
	span.End()

	got := h.spans.GetSpans()
	if len(got) != 1 {
		t.Fatalf("got %d spans", len(got))
	}
	s := got[0]
	for _, k := range []attribute.Key{"url.full", "url.path", "user_agent.original", "client.address", "http.request.header.cookie"} {
		if hasKey(s.Attributes, k) {
			t.Errorf("%s exported", k)
		}
	}
	if !hasKey(s.Attributes, "http.route") {
		t.Error("http.route dropped")
	}
	if s.Status.Description != "" {
		t.Errorf("status description exported: %q", s.Status.Description)
	}
	for _, ev := range s.Events {
		if hasKey(ev.Attributes, "exception.message") {
			t.Error("exception.message exported")
		}
		if !hasKey(ev.Attributes, "exception.type") {
			t.Error("exception.type dropped")
		}
	}
	all := s.Status.Description
	for _, kv := range s.Attributes {
		all += " " + kv.Value.String()
	}
	for _, ev := range s.Events {
		for _, kv := range ev.Attributes {
			all += " " + kv.Value.String()
		}
	}
	for _, leak := range []string{"token=t", "tok=1", "/x?", "/a/b"} {
		if strings.Contains(all, leak) {
			t.Errorf("exported span contains %q: %s", leak, all)
		}
	}
}

func TestHandler(t *testing.T) {
	h := newHarness(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /profiles/{u}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {})
	srv := Handler(mux, "sprobot-web")

	for _, p := range []string{"/profiles/12345", "/healthz"} {
		r := httptest.NewRequest("GET", p+"?secret=1", nil)
		r.Header.Set("User-Agent", "visitor")
		r.Header.Set("X-Forwarded-For", "203.0.113.9")
		r.Host = "attacker-chosen.example"
		srv.ServeHTTP(httptest.NewRecorder(), r)
	}

	spans := h.spans.GetSpans()
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1 (healthz untraced)", len(spans))
	}
	if spans[0].Name != "GET /profiles/{u}" {
		t.Errorf("span name %q", spans[0].Name)
	}
	for _, kv := range spans[0].Attributes {
		if v := kv.Value.String(); strings.Contains(v, "12345") || strings.Contains(v, "secret") ||
			strings.Contains(v, "visitor") || strings.Contains(v, "203.0.113.9") || strings.Contains(v, "attacker") {
			t.Errorf("span attribute %s=%s leaks request data", kv.Key, v)
		}
	}

	pts := histPoints(t, h.metrics(t)["http.server.request.duration"])
	if len(pts) != 1 {
		t.Fatalf("got %d server duration points, want 1", len(pts))
	}
	attrs := pts[0].Attributes
	if v, _ := attrs.Value("http.route"); v.AsString() != "/profiles/{u}" {
		t.Errorf("route = %q", v.AsString())
	}
	if v, _ := attrs.Value("http.response.status_code"); v.AsInt64() != 404 {
		t.Errorf("status = %d", v.AsInt64())
	}
	for _, k := range attrKeys(attrs) {
		if !hasKey(allow(serverMetricKeys), attribute.Key(k)) {
			t.Errorf("server metric has attribute %s", k)
		}
	}
}

func allow(keys []attribute.Key) []attribute.KeyValue {
	out := make([]attribute.KeyValue, len(keys))
	for i, k := range keys {
		out[i] = k.String("")
	}
	return out
}

func TestTransport(t *testing.T) {
	h := newHarness(t)
	var gotTraceparent string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTraceparent = r.Header.Get("Traceparent")
		w.WriteHeader(http.StatusTeapot)
	}))
	defer up.Close()

	c := Client("discord", &http.Client{Timeout: 5 * time.Second})
	ctx, parent := otel.Tracer("t").Start(context.Background(), "parent")
	req, _ := http.NewRequestWithContext(ctx, "POST", up.URL+"/webhooks/1/SECRET?wait=true", nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	parent.End()

	if gotTraceparent != "" {
		t.Errorf("traceparent sent to a third party: %q", gotTraceparent)
	}
	var client sdktrace.ReadOnlySpan
	for _, s := range h.spans.GetSpans().Snapshots() {
		if s.Name() == "discord POST" {
			client = s
		}
	}
	if client == nil {
		t.Fatalf("no client span; got %v", h.spans.GetSpans())
	}
	if client.Parent().SpanID() != parent.SpanContext().SpanID() {
		t.Error("client span isn't a child of the caller's span")
	}
	for _, kv := range client.Attributes() {
		if strings.Contains(kv.Value.String(), "SECRET") {
			t.Errorf("client span attribute %s leaks the URL", kv.Key)
		}
	}
	if !hasKey(client.Attributes(), PeerService) {
		t.Error("client span lacks peer.service")
	}

	pts := histPoints(t, h.metrics(t)["http.client.request.duration"])
	if len(pts) != 1 {
		t.Fatalf("got %d client duration points", len(pts))
	}
	if v, _ := pts[0].Attributes.Value(PeerService); v.AsString() != "discord" {
		t.Errorf("peer.service = %q", v.AsString())
	}
	for _, k := range attrKeys(pts[0].Attributes) {
		if !hasKey(allow(clientMetricKeys), attribute.Key(k)) {
			t.Errorf("client metric has attribute %s", k)
		}
	}
}

func TestJob(t *testing.T) {
	h := newHarness(t)
	j := NewJob("test_job")
	if NewJob("test_job") != j {
		t.Error("NewJob not idempotent by name")
	}
	if !j.LastSuccess().IsZero() {
		t.Fatal("last success set before any run")
	}
	if g := lastSuccessPoints(t, h, "test_job"); len(g) != 0 {
		t.Fatalf("gauge observed before first success: %v", g)
	}
	if r := runPoints(t, h, "test_job"); len(r) != 2 || r["success"] != 0 || r["failure"] != 0 {
		t.Fatalf("runs before any run = %v, want both outcomes at 0", r)
	}

	boom := errors.New("boom")
	if err := j.Run(context.Background(), func(context.Context) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("Run returned %v", err)
	}
	if !j.LastSuccess().IsZero() {
		t.Error("failure moved last success")
	}
	before := time.Now()
	if err := j.Run(context.Background(), func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if j.LastSuccess().Before(before) {
		t.Error("success didn't move last success")
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic swallowed")
			}
		}()
		_ = j.Run(context.Background(), func(context.Context) error { panic("p") })
	}()

	runs := runPoints(t, h, "test_job")
	if runs["success"] != 1 || runs["failure"] != 2 {
		t.Errorf("runs counter = %v, want success 1, failure 2", runs)
	}

	counts := map[string]uint64{}
	for _, p := range histPoints(t, h.metrics(t)[jobDurationName]) {
		if v, _ := p.Attributes.Value(JobName); v.AsString() != "test_job" {
			continue
		}
		o, _ := p.Attributes.Value("outcome")
		counts[o.AsString()] += p.Count
	}
	if counts["success"] != 1 || counts["failure"] != 2 {
		t.Errorf("outcome counts = %v, want success 1, failure 2", counts)
	}

	g := lastSuccessPoints(t, h, "test_job")
	if len(g) != 1 || g[0] < float64(before.Unix()) {
		t.Errorf("last success gauge = %v", g)
	}

	var failed int
	for _, s := range h.spans.GetSpans() {
		if s.Name == "job test_job" && s.Status.Code == codes.Error {
			failed++
		}
	}
	if failed != 2 {
		t.Errorf("%d failed job spans, want 2", failed)
	}
}

func TestJobSucceeded(t *testing.T) {
	h := newHarness(t)
	j := NewJob("gateway_ready")
	j.Succeeded()
	if g := lastSuccessPoints(t, h, "gateway_ready"); len(g) != 1 {
		t.Errorf("gauge points = %v", g)
	}
}

func lastSuccessPoints(t *testing.T, h *harness, name string) []float64 {
	t.Helper()
	a, ok := h.metrics(t)[jobLastSuccessName]
	if !ok {
		return nil
	}
	var out []float64
	for _, p := range a.(metricdata.Gauge[float64]).DataPoints {
		if v, _ := p.Attributes.Value(JobName); v.AsString() == name {
			out = append(out, p.Value)
		}
	}
	return out
}

func TestLogHandler(t *testing.T) {
	newHarness(t)
	var buf bytes.Buffer
	log := slog.New(LogHandler(slog.NewTextHandler(&buf, nil)))

	log.Info("outside")
	ctx, span := otel.Tracer("t").Start(context.Background(), "s")
	log.InfoContext(ctx, "inside")
	span.End()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("lines: %q", lines)
	}
	if strings.Contains(lines[0], "trace_id=") {
		t.Errorf("trace_id outside a span: %s", lines[0])
	}
	want := "trace_id=" + span.SpanContext().TraceID().String()
	if !strings.Contains(lines[1], want) {
		t.Errorf("line %q lacks %s", lines[1], want)
	}
}

// TestStartExports runs Start against a fake OTLP/HTTP collector: a span
// and a job's metrics reach /v1/traces and /v1/metrics by shutdown.
func TestStartExports(t *testing.T) {
	newHarness(t) // the shared providers, restored below
	var mu sync.Mutex
	got := map[string]int{}
	col := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got[r.URL.Path]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/x-protobuf")
	}))
	defer col.Close()
	t.Setenv("OTEL_SDK_DISABLED", "")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", col.URL)
	t.Setenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "")

	tp, mp, prop := otel.GetTracerProvider(), otel.GetMeterProvider(), otel.GetTextMapPropagator()
	t.Cleanup(func() {
		otel.SetTracerProvider(tp)
		otel.SetMeterProvider(mp)
		otel.SetTextMapPropagator(prop)
	})

	shutdown, err := Start(context.Background(), "telemetry-test")
	if err != nil {
		t.Fatal(err)
	}
	_, span := otel.Tracer("t").Start(context.Background(), "s")
	span.End()
	shutdown()

	mu.Lock()
	defer mu.Unlock()
	if got["/v1/traces"] == 0 || got["/v1/metrics"] == 0 {
		t.Errorf("collector got %v, want /v1/traces and /v1/metrics", got)
	}
}

func runPoints(t *testing.T, h *harness, name string) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	a, ok := h.metrics(t)[jobRunsName]
	if !ok {
		return out
	}
	sum := a.(metricdata.Sum[int64])
	if !sum.IsMonotonic || sum.Temporality != metricdata.CumulativeTemporality {
		t.Errorf("runs: monotonic %v, temporality %v", sum.IsMonotonic, sum.Temporality)
	}
	for _, p := range sum.DataPoints {
		if v, _ := p.Attributes.Value(JobName); v.AsString() == name {
			o, _ := p.Attributes.Value("outcome")
			out[o.AsString()] = p.Value
		}
	}
	return out
}

func TestJobSeed(t *testing.T) {
	h := newHarness(t)
	j := NewJob("seeded")
	old := time.Unix(1_700_000_000, 0)
	j.Seed(old)
	j.Seed(old.Add(-time.Hour)) // older: ignored
	if !j.LastSuccess().Equal(old) {
		t.Errorf("LastSuccess = %v, want %v", j.LastSuccess(), old)
	}
	if g := lastSuccessPoints(t, h, "seeded"); len(g) != 1 || g[0] != float64(old.Unix()) {
		t.Errorf("gauge = %v", g)
	}
	if r := runPoints(t, h, "seeded"); r["success"] != 0 {
		t.Errorf("Seed counted a run: %v", r)
	}
	j.Succeeded()
	if !j.LastSuccess().After(old) {
		t.Error("Succeeded didn't move past the seed")
	}
}
