package telemetry

import (
	"net/http"

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
// No trace context is sent: these are third-party APIs (Discord,
// RingCentral, Microsoft Graph, S3), which have no use for our trace IDs.
func Transport(peer string, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
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
