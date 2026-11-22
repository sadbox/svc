# svc

Code shared by sadbox's Go services (sprobot, sadbox.org, oncall-sync,
towngear, rproxy). The root module is standard library only;
[`telemetry`](telemetry) is a module of its own
(`github.com/sadbox/svc/telemetry`, tagged `telemetry/vX.Y.Z`), so its
OpenTelemetry dependencies reach only the services that import it.

Only what the services genuinely share today goes here: each package
replaces code that is copied, or hand-rolled slightly differently, in
more than one of them.

| Package | What |
| --- | --- |
| [`healthcheck`](healthcheck) | The `-healthcheck` flag (container health client) and the `/healthz` handler |
| [`logging`](logging) | The slog text-format setup, `LOG_LEVEL`, `Fatal` |
| [`telemetry`](telemetry) | OpenTelemetry metrics and traces over OTLP to the host's Alloy: `Start`, `Handler`, `Transport`/`Client`, `Job`, `LogHandler` |

## Telemetry standard

Every service pushes metrics and traces over OTLP/HTTP to Alloy on bonnet
(native, on the host); Alloy forwards them to Grafana Cloud. Nothing is
scraped from the services and no service publishes a port for it.

**Setup.** `shutdown, err := telemetry.Start(ctx, "<service>")` in `main`,
after `healthcheck.Main` (the health client must stay free of it), and
`defer shutdown()`. Configuration is the standard `OTEL_*` environment, set
by the Quadlet unit:

```ini
Environment=OTEL_EXPORTER_OTLP_ENDPOINT=http://<network gateway>:4318
Environment=OTEL_RESOURCE_ATTRIBUTES=service.namespace=sadbox,service.instance.id=bonnet,deployment.environment.name=prod
```

Without an endpoint (tests, local runs) `Start` does nothing and the
helpers record into OpenTelemetry's no-ops.

**What each service instruments.**
- Inbound HTTP: `telemetry.Handler(mux, "<service>")` around the
  ServeMux. Route-labelled (`r.Pattern`), `/healthz` excluded; rproxy's
  `traceparent` continues the edge trace.
- Outbound HTTP: `telemetry.Transport("<peer>", base)` (or `Client`) on
  every client that calls a dependency, `<peer>` a fixed name (`discord`,
  `s3`, `ringcentral`, `graph`, `pushover`, ...). No trace context is sent
  to third parties.
- Background work: one `telemetry.NewJob("<name>")` per loop, task or
  heartbeat condition; `Run` around each pass, or `Succeeded` where a
  condition (not a run) used to ping a heartbeat.
- Logs: `telemetry.LogHandler` around the slog handler, and the `*Context`
  slog calls inside a request or job, so a line carries its `trace_id`.

**What never leaves the process.** Enforced in `telemetry`, not left to
each service: spans lose `url.full`, `url.path`, `url.query`, the user
agent, client and peer addresses, any captured headers, and all error
text (status descriptions, `exception.message` and stack traces; the
error's type stays, and its text is in the log, linked by `trace_id`).
URLs left in any other string attribute are cut to scheme and host. HTTP metrics keep
only method, status, route (server) or `peer.service` (client), and
`error.type`. Don't put IDs (Discord users, guilds, phone numbers, email
addresses), message content or request bodies in span attributes or
metric labels.

**Metric names** (as Prometheus shows them):

| Metric | Labels | From |
| --- | --- | --- |
| `http_server_request_duration_seconds` | `http_request_method`, `http_response_status_code`, `http_route` | `Handler` |
| `http_client_request_duration_seconds` | `http_request_method`, `http_response_status_code`, `peer_service` | `Transport` |
| `sadbox_job_runs_total` | `job_name`, `outcome` (`success`/`failure`) | `Job.Run`, `Job.Succeeded` |
| `sadbox_job_duration_seconds` | `job_name`, `outcome` | `Job.Run` |
| `sadbox_job_last_success_timestamp_seconds` | `job_name` | `Job.Run`, `Job.Succeeded`, `Job.Seed` |
| `go_*` runtime metrics | | `Start` |

`job` and `instance` come from `service.namespace`/`service.name` and
`service.instance.id`.

**"Has it succeeded lately" keys on the counter**, not the gauge:
`increase(sadbox_job_runs_total{outcome="success"}[<window>]) < 1`. Each
success is an increment Prometheus keeps, so a restart (a counter reset,
which `increase` handles) loses none, and both outcomes read 0 from a
process's first export, so the first success is a visible step. The gauge
lives in the process and is absent after a restart until the next success
(or a `Job.Seed` from the service's own records): it is for dashboards.

## Health check standard

Every service binary is its own container health client: the images are
distroless (no shell, no curl) and Podman ignores Docker's `HEALTHCHECK`
in OCI images, so the Quadlet unit runs the binary itself.

**The command.** `<binary> -healthcheck` (`--healthcheck` works too). It
must be the only argument: a value (`-healthcheck=true`) or anything after
it fails the check rather than falling through to starting the service.
Only `argv[1]` is looked at, so a binary whose `main` hands argv to another
CLI framework (Caddy's) checks it before delegating.

**What it does.** One `GET` of the service's own health URL, on loopback
(an `http://` URL with a literal loopback IP; anything else is refused).
The service passes the URL. It uses its own transport (no proxy, no keep-alive), doesn't follow
redirects, and the whole exchange, body included, is bounded by **3 s**.
Detection comes before anything else in `main`: no flag parsing, config,
secrets, DB or network beyond that one request.

**Exit codes.** `0` if the answer is `2xx`. `1` for everything else
(another status, a redirect, refused, timed out, usage error), with the
reason on stderr, which Podman keeps in the health log. Never `2`: Docker
reserves it.

**The endpoint.** `GET /healthz` (also `HEAD`; other methods get 405)
answers `200 {"status":"ok"}` while healthy and
`503 {"status":"unhealthy","failing":["db"]}` otherwise, JSON, `Cache-Control:
no-store`. The body names failing checks but never shows their errors (the
endpoint may be reachable from outside). The log gets state changes, not
every probe: WARN with the error when a check starts failing, INFO when it
recovers. Checks
run concurrently under a 2 s budget, under the client's 3 s, so a slow
check comes back as a 503 that names it.

**The Quadlet unit.**

```ini
HealthCmd=["/<binary>", "-healthcheck"]
HealthInterval=30s
HealthTimeout=10s
HealthStartPeriod=60s
HealthRetries=3
HealthOnFailure=kill   # stateless web units; `none` for the bots and oncall-sync
Notify=healthy
```

JSON (exec) form, since there is no shell. `HealthTimeout` stays above
3 s so the binary, not Podman, decides and says why.

### Usage

```go
func main() {
	healthcheck.Main(healthcheck.ListenURL(":8080")) // returns unless -healthcheck
	logging.Init()
	// flags, config, secrets, DB...

	mux.Handle(healthcheck.Path, healthcheck.Handler(0,
		healthcheck.Check{Name: "db", Func: db.PingContext},
	))
}
```

- `Main(url)` exits the process if asked for the check, else returns.
  `ListenURL(addr)` turns a listen address (`":8080"`, `"0.0.0.0:9000"`)
  into `http://127.0.0.1:PORT/healthz`.
- `Requested()` and `Probe(url, timeout)` are the two halves of `Main`,
  for a service that needs something else in between.
- `Handler(timeout, checks...)`: `0` means the 2 s default; no checks
  means 200 whenever the server is up.

A binary that delegates to another CLI (rproxy):

```go
func main() {
	healthcheck.Main("http://127.0.0.1:2019/config/") // Caddy's admin API
	caddycmd.Main()
}
```

## Logging

`logging.Init()` makes slog's text format (`time=... level=INFO msg=...
key=value`) the default logger, on stderr, at the level in `$LOG_LEVEL`
(`DEBUG`, `INFO`, `WARN`, `ERROR`; unset means `INFO`; an invalid value
is logged and ignored), and returns it. `logging.New(w, level)` is the
same format on any writer; `logging.Fatal(msg, args...)` logs at ERROR and
exits 1.

## Versioning

Releases are tags `vX.Y.Z` on `main`, following semver (pre-1.0: a minor
bump may break the API). Services pin a release in their `go.mod`, like
any other dependency, and Dependabot (gomod) proposes the bumps.

The `go` line in `go.mod` is 1.26, the lowest every service is on, not the
services' build image (Go 1.27.4), so a service can import a release before
moving its own `go` line. CI tests with the build image's Go.

## License

MIT, see [LICENSE](LICENSE).
