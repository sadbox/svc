package telemetry

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// connectProxy is a CONNECT-only proxy, as egress is: it records each
// CONNECT's headers and the client's address, then tunnels.
type connectProxy struct {
	mu       sync.Mutex
	connects []connectSeen
}

type connectSeen struct {
	header http.Header
	remote string
}

func (p *connectProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "CONNECT only", http.StatusMethodNotAllowed)
		return
	}
	p.mu.Lock()
	p.connects = append(p.connects, connectSeen{header: r.Header.Clone(), remote: r.RemoteAddr})
	p.mu.Unlock()
	up, err := net.Dial("tcp", r.Host)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.WriteHeader(http.StatusOK)
	c, buf, err := http.NewResponseController(w).Hijack()
	if err != nil {
		up.Close()
		return
	}
	go func() { _, _ = io.Copy(up, buf); up.Close() }()
	go func() { _, _ = io.Copy(c, up); c.Close() }()
}

func (p *connectProxy) seen() []connectSeen {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]connectSeen(nil), p.connects...)
}

func TestTransportProxyTraceparent(t *testing.T) {
	h := newHarness(t)
	var upMu sync.Mutex
	var upTraceparents []string
	up := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upMu.Lock()
		upTraceparents = append(upTraceparents, r.Header.Get("Traceparent"))
		upMu.Unlock()
	}))
	defer up.Close()
	proxy := &connectProxy{}
	ps := httptest.NewServer(proxy)
	defer ps.Close()
	pu, _ := url.Parse(ps.URL)

	base := &http.Transport{
		Proxy:              http.ProxyURL(pu),
		TLSClientConfig:    &tls.Config{RootCAs: up.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs},
		ProxyConnectHeader: http.Header{"X-Egress-Purpose": {"fetch"}},
	}
	c := &http.Client{Transport: Transport("proxied", base), Timeout: 5 * time.Second}

	ctx, parent := otel.Tracer("t").Start(context.Background(), "parent")
	for range 2 {
		req, _ := http.NewRequestWithContext(ctx, "GET", up.URL+"/x", nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	parent.End()

	if base.GetProxyConnectHeader != nil {
		t.Error("Transport changed the caller's *http.Transport")
	}
	for _, tp := range upTraceparents {
		if tp != "" {
			t.Errorf("traceparent sent through the tunnel to the API: %q", tp)
		}
	}
	connects := proxy.seen()
	if len(connects) != 1 {
		t.Fatalf("%d CONNECTs, want 1 (the second request reuses the tunnel)", len(connects))
	}
	cn := connects[0]
	if got := cn.header.Get("X-Egress-Purpose"); got != "fetch" {
		t.Errorf("CONNECT lost the transport's own proxy header: X-Egress-Purpose %q", got)
	}

	var clients []sdktrace.ReadOnlySpan
	for _, s := range h.spans.GetSpans().Snapshots() {
		if s.Name() == "proxied GET" {
			clients = append(clients, s)
		}
	}
	if len(clients) != 2 {
		t.Fatalf("%d client spans, want 2", len(clients))
	}
	first, second := clients[0], clients[1]
	if first.StartTime().After(second.StartTime()) {
		first, second = second, first
	}
	sc := trace.SpanContextFromContext(propagation.TraceContext{}.Extract(context.Background(), propagation.HeaderCarrier(cn.header)))
	if sc.TraceID() != parent.SpanContext().TraceID() || sc.SpanID() != first.SpanContext().SpanID() {
		t.Errorf("CONNECT traceparent %s/%s, want the first client span's %s/%s",
			sc.TraceID(), sc.SpanID(), first.SpanContext().TraceID(), first.SpanContext().SpanID())
	}

	_, proxySidePort, _ := net.SplitHostPort(cn.remote)
	for i, s := range []sdktrace.ReadOnlySpan{first, second} {
		a := map[attribute.Key]attribute.Value{}
		for _, kv := range s.Attributes() {
			a[kv.Key] = kv.Value
		}
		if want := i == 1; a[ConnReused].Type() != attribute.BOOL || a[ConnReused].AsBool() != want {
			t.Errorf("request %d: %s = %v, want %v", i+1, ConnReused, a[ConnReused].Emit(), want)
		}
		if got := strconv.FormatInt(a["network.local.port"].AsInt64(), 10); got != proxySidePort {
			t.Errorf("request %d: network.local.port %s, want %s (the proxy's view of the client)", i+1, got, proxySidePort)
		}
		if a["network.local.address"].AsString() != "127.0.0.1" {
			t.Errorf("request %d: network.local.address %q", i+1, a["network.local.address"].AsString())
		}
	}
}

// A base that isn't an *http.Transport is used as it is: nothing to copy.
func TestTransportOtherBase(t *testing.T) {
	newHarness(t)
	var called bool
	rt := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: http.StatusNoContent, Body: http.NoBody, Request: r}, nil
	})
	req, _ := http.NewRequest("GET", "http://example.invalid/", nil)
	resp, err := Transport("x", rt).RoundTrip(req)
	if err != nil || !called {
		t.Fatalf("got %v, called %v", err, called)
	}
	resp.Body.Close()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
