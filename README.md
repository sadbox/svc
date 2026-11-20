# svc

Code shared by sadbox's Go services (sprobot, sadbox.org, oncall-sync,
towngear, rproxy). Standard library only.

Only what the services genuinely share today goes here: each package
replaces code that is copied, or hand-rolled slightly differently, in
more than one of them.

| Package | What |
| --- | --- |
| [`healthcheck`](healthcheck) | The `-healthcheck` flag (container health client) and the `/healthz` handler |
| [`logging`](logging) | The slog text-format setup, `LOG_LEVEL`, `Fatal` |

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
The service passes the URL; `$HEALTHCHECK_URL`, if set, overrides it. It
uses its own transport (no proxy, no keep-alive), doesn't follow
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
endpoint may be reachable from outside); errors are logged at WARN. Checks
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

The `go` line in `go.mod` is the lowest Go release the code needs (1.22,
for per-iteration loop variables), not the services' build image, so a
service on an older `go` line can still import it. CI tests both that and
the services' current Go.

## License

MIT, see [LICENSE](LICENSE).
