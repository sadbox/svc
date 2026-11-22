package telemetry

import (
	"context"
	"regexp"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// droppedSpanKeys never leave the process. otelhttp records all of them by
// default:
//   - url.full: outbound URLs carry secrets in the path or query (Discord
//     webhook and interaction tokens, S3 presigned signatures);
//   - url.path, url.query: inbound paths carry user IDs (sprobot-web's
//     profiles) and login links (towngear); http.route says which handler;
//   - user_agent.original, client.address, network.peer.*: visitors;
//   - exception.message, exception.stacktrace: an error's text can hold
//     anything (oncall-sync's name people, numbers and addresses);
//     exception.type stays, and the text is in the service's log, linked by
//     trace_id (LogHandler).
var droppedSpanKeys = map[attribute.Key]bool{
	"exception.message":    true,
	"exception.stacktrace": true,
	"url.full":             true,
	"url.path":             true,
	"url.query":            true,
	"url.fragment":         true,
	"user_agent.original":  true,
	"client.address":       true,
	"client.port":          true,
	"network.peer.address": true,
	"network.peer.port":    true,
}

// droppedSpanPrefixes: captured request/response headers (cookies,
// authorization), should anything ever turn header capture on.
var droppedSpanPrefixes = []string{"http.request.header.", "http.response.header."}

func keepSpanAttr(kv attribute.KeyValue) bool {
	if droppedSpanKeys[kv.Key] {
		return false
	}
	for _, p := range droppedSpanPrefixes {
		if strings.HasPrefix(string(kv.Key), p) {
			return false
		}
	}
	return true
}

// urlRE matches an http(s) URL in free text (an error's message: *url.Error
// quotes the whole URL), any case, up to whitespace or a quote: group 1 is
// the scheme, group 2 the authority as a URL parser reads it (up to the
// first '/', '?' or '#'), group 3 the rest.
var urlRE = regexp.MustCompile(`(?i)(https?://)([^/\s"'?#]*)([^\s"']*)`)

// hostRE is what a host may look like once scrubbed: a name or IPv4
// address, or a bracketed IPv6 address, with an optional numeric port.
var hostRE = regexp.MustCompile(`^(?:[A-Za-z0-9.-]+|\[[0-9A-Fa-f:.]+\])(?::[0-9]+)?$`)

// redactedHost stands in for a host that can't be told apart from
// credentials.
const redactedHost = "[redacted]"

// scrubURLs cuts every URL in s down to its scheme and host: credentials,
// path and query are where the secrets are. It errs toward redacting:
//   - the host is what follows the authority's last '@', so no part of a
//     userinfo survives however many '@' it holds;
//   - an '@' after the authority (an unencoded '/' in a password, or an
//     email address in the path) makes the authority ambiguous: the host
//     is redacted;
//   - so is anything left that isn't host[:port].
func scrubURLs(s string) string {
	return urlRE.ReplaceAllStringFunc(s, func(u string) string {
		m := urlRE.FindStringSubmatch(u)
		scheme, authority, rest := m[1], m[2], m[3]
		host := authority[strings.LastIndex(authority, "@")+1:]
		if strings.Contains(rest, "@") || !hostRE.MatchString(host) {
			host = redactedHost
		}
		return scheme + host + "/…"
	})
}

// redactingExporter applies the rules above to every span on its way out:
// attributes (span, event and link; URLs scrubbed from the string values
// kept), and the status description (dropped).
type redactingExporter struct {
	sdktrace.SpanExporter
}

func (e redactingExporter) ExportSpans(ctx context.Context, spans []sdktrace.ReadOnlySpan) error {
	out := make([]sdktrace.ReadOnlySpan, len(spans))
	for i, s := range spans {
		out[i] = redactedSpan{s}
	}
	return e.SpanExporter.ExportSpans(ctx, out)
}

// redactedSpan overrides the accessors that can carry sensitive data.
// Embedding ReadOnlySpan keeps its unexported method, so this stays a valid
// ReadOnlySpan as the SDK adds methods.
type redactedSpan struct {
	sdktrace.ReadOnlySpan
}

func (s redactedSpan) Attributes() []attribute.KeyValue {
	return redactAttrs(s.ReadOnlySpan.Attributes())
}

// Status keeps the code and drops the description: like exception.message,
// it's an error's free text.
func (s redactedSpan) Status() sdktrace.Status {
	st := s.ReadOnlySpan.Status()
	st.Description = ""
	return st
}

func (s redactedSpan) Events() []sdktrace.Event {
	evs := s.ReadOnlySpan.Events()
	out := make([]sdktrace.Event, len(evs))
	for i, ev := range evs {
		ev.Name = scrubURLs(ev.Name)
		ev.Attributes = redactAttrs(ev.Attributes)
		out[i] = ev
	}
	return out
}

func (s redactedSpan) Links() []sdktrace.Link {
	ls := s.ReadOnlySpan.Links()
	out := make([]sdktrace.Link, len(ls))
	for i, l := range ls {
		l.Attributes = redactAttrs(l.Attributes)
		out[i] = l
	}
	return out
}

// redactAttrs drops the keys above and scrubs URLs out of the string
// values that remain.
func redactAttrs(in []attribute.KeyValue) []attribute.KeyValue {
	out := make([]attribute.KeyValue, 0, len(in))
	for _, kv := range in {
		if !keepSpanAttr(kv) {
			continue
		}
		if kv.Value.Type() == attribute.STRING {
			kv.Value = attribute.StringValue(scrubURLs(kv.Value.AsString()))
		}
		out = append(out, kv)
	}
	return out
}

// HTTP metric attributes kept; everything else otelhttp adds is dropped.
// server.address on the client side is whatever host a request went to
// (sprobot fetches user-supplied URLs), so the client side is keyed by
// peer.service instead.
var (
	serverMetricKeys = []attribute.Key{
		"http.request.method", "http.response.status_code", "http.route",
		"error.type",
	}
	clientMetricKeys = []attribute.Key{
		"http.request.method", "http.response.status_code", "error.type",
		PeerService,
	}
)

// views bounds the HTTP metrics' attributes to the allowlists above.
func views() []sdkmetric.View {
	return []sdkmetric.View{
		allowView("http.server.*", serverMetricKeys),
		allowView("http.client.*", clientMetricKeys),
	}
}

func allowView(name string, keys []attribute.Key) sdkmetric.View {
	return sdkmetric.NewView(
		sdkmetric.Instrument{Name: name},
		sdkmetric.Stream{AttributeFilter: attribute.NewAllowKeysFilter(keys...)},
	)
}
