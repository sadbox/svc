package telemetry

import (
	"context"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// PeerService is the attribute naming the dependency an outbound call goes
// to ("discord", "s3", "ringcentral"): a small fixed set, chosen by the
// caller of [Transport], never derived from the URL.
const PeerService = attribute.Key("peer.service")

// Handler wraps a service's inbound handler: a server span per request
// (continuing a trace from rproxy's traceparent) and the
// http.server.request.duration histogram, by method, status and route.
//
// server is the service's name, used as server.address instead of the
// request's Host header (which a client chooses). Wrap the ServeMux, or
// anything that passes the same *http.Request on to it: the route is the
// ServeMux pattern the request matched (r.Pattern), so a handler that
// substitutes a copy of the request (r.WithContext) before the mux runs
// hides it, and the request is recorded with no route.
//
// /healthz is never traced or counted: the container health check polls it
// every 30 seconds.
func Handler(h http.Handler, server string) http.Handler {
	return otelhttp.NewHandler(h, server,
		otelhttp.WithServerName(server),
		otelhttp.WithFilter(func(r *http.Request) bool {
			return r.URL.Path != "/healthz"
		}),
	)
}

// Transport wraps base (nil: http.DefaultTransport) for calls to one
// dependency: a client span per request, named "<peer> <METHOD>", and the
// http.client.request.duration histogram, by method, status and peer.
//
// No trace context is sent to the API: these are third parties (Discord,
// RingCentral, Microsoft Graph, S3), which have no use for our trace IDs.
// The outbound proxy gets it instead: when base is an *http.Transport,
// Transport uses a copy whose CONNECTs carry the client span's
// traceparent (GetProxyConnectHeader, which net/http sends to the proxy
// only, never through the tunnel), after the copy's own proxy headers
// (ProxyConnectHeader, or its GetProxyConnectHeader's). egress makes its
// span for the CONNECT that span's child. A copy has a connection pool
// of its own: one per Transport call, as a client is built once.
//
// Each client span also says which connection it used: whether it was
// reused (ConnReused) and its local address (network.local.address,
// network.local.port). Through egress, that port is the egress span's
// egress.source.port, so a request on a reused tunnel finds the CONNECT
// that opened it.
func Transport(peer string, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	base = connTransport{proxyTraceparent(base)}
	kv := PeerService.String(peer)
	inner := otelhttp.NewTransport(base,
		otelhttp.WithPropagators(propagation.NewCompositeTextMapPropagator()),
		otelhttp.WithSpanOptions(trace.WithAttributes(kv)),
		otelhttp.WithSpanNameFormatter(func(_ string, r *http.Request) string {
			return peer + " " + r.Method
		}),
	)
	return peerTransport{inner: inner, kv: kv}
}

// peerTransport puts the peer.service attribute on the client metrics,
// through otelhttp's labeler in the request's context. Always a fresh
// labeler: adding to one found in the context would append again on every
// request that reuses that context.
type peerTransport struct {
	inner http.RoundTripper
	kv    attribute.KeyValue
}

func (t peerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	l := &otelhttp.Labeler{}
	l.Add(t.kv)
	// A shallow copy with the new context, as RoundTripper allows; otelhttp
	// clones it again before sending.
	return t.inner.RoundTrip(r.WithContext(otelhttp.ContextWithClientLabeler(r.Context(), l)))
}

// ConnReused: whether a client request went out on a connection an earlier
// request had used (no new dial, no CONNECT to the proxy).
const ConnReused = attribute.Key("http.connection.reused")

// proxyTraceparent: base, or for an *http.Transport a copy that sends the
// span's traceparent on CONNECT (Transport).
func proxyTraceparent(base http.RoundTripper) http.RoundTripper {
	t, ok := base.(*http.Transport)
	if !ok {
		return base
	}
	c := t.Clone()
	TraceConnects(c)
	return c
}

// TraceConnects makes t's CONNECTs to its proxy carry the traceparent of the
// request's span, after t's own proxy headers (ProxyConnectHeader, or its
// GetProxyConnectHeader's), as [Transport] does for an *http.Transport base.
// It is for a client whose *http.Transport sits under a wrapper [Transport]
// can't see through, such as the AWS SDK's (through
// awshttp.BuildableClient.WithTransportOptions); the client's spans still
// come from [Transport] or [Client] around the whole client. It changes t
// in place: call it before t's first request.
func TraceConnects(t *http.Transport) {
	static, dynamic := t.ProxyConnectHeader, t.GetProxyConnectHeader
	t.GetProxyConnectHeader = func(ctx context.Context, proxy *url.URL, target string) (http.Header, error) {
		h := static
		if dynamic != nil {
			var err error
			if h, err = dynamic(ctx, proxy, target); err != nil {
				return nil, err
			}
		}
		h = h.Clone()
		if h == nil {
			h = http.Header{}
		}
		// The dial's context is the request's (values kept): its client
		// span.
		propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(h))
		return h, nil
	}
}

// connTransport records the connection a request got on its client span.
type connTransport struct {
	base http.RoundTripper
}

func (t connTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	span := trace.SpanFromContext(r.Context())
	if !span.IsRecording() {
		return t.base.RoundTrip(r)
	}
	ct := &httptrace.ClientTrace{GotConn: func(i httptrace.GotConnInfo) {
		kv := []attribute.KeyValue{ConnReused.Bool(i.Reused)}
		if i.Conn != nil {
			if a, ok := i.Conn.LocalAddr().(*net.TCPAddr); ok {
				kv = append(kv, attribute.String("network.local.address", a.IP.String()), attribute.Int("network.local.port", a.Port))
			}
		}
		span.SetAttributes(kv...)
	}}
	return t.base.RoundTrip(r.WithContext(httptrace.WithClientTrace(r.Context(), ct)))
}

// Client is an *http.Client whose transport is [Transport](peer, base),
// for the many places that build their own client.
func Client(peer string, base *http.Client) *http.Client {
	c := &http.Client{}
	if base != nil {
		*c = *base
	}
	c.Transport = Transport(peer, c.Transport)
	return c
}
