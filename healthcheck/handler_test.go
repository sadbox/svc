package healthcheck

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func serve(t *testing.T, h http.Handler, method string) (*httptest.ResponseRecorder, response) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, Path, nil))
	var body response
	if method == http.MethodGet && rec.Code != http.StatusMethodNotAllowed {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body %q: %v", rec.Body.String(), err)
		}
	}
	return rec, body
}

// captureLog points slog.Default at a buffer for the test.
func captureLog(t *testing.T) *bytes.Buffer {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return &buf
}

func pass(context.Context) error { return nil }

func TestHandlerNoChecks(t *testing.T) {
	rec, body := serve(t, Handler(0), http.MethodGet)
	if rec.Code != http.StatusOK || body.Status != "ok" || body.Failing != nil {
		t.Fatalf("%d %+v, want 200 ok", rec.Code, body)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control %q", got)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"status":"ok"}` {
		t.Errorf("body %s", got)
	}
}

func TestHandlerFailing(t *testing.T) {
	logs := captureLog(t)
	h := Handler(time.Second,
		Check{"db", func(context.Context) error { return errors.New("secret dsn in here") }},
		Check{"gateway", pass},
		Check{"cache", func(context.Context) error { return errors.New("down") }},
	)
	rec, body := serve(t, h, http.MethodGet)
	if rec.Code != http.StatusServiceUnavailable || body.Status != "unhealthy" {
		t.Fatalf("%d %+v, want 503 unhealthy", rec.Code, body)
	}
	if got := strings.Join(body.Failing, ","); got != "db,cache" {
		t.Errorf("failing %q, want db,cache (in order)", got)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("body shows a check's error: %s", rec.Body.String())
	}
	if !strings.Contains(logs.String(), `level=WARN msg="Health check failing" check=db error="secret dsn in here"`) {
		t.Errorf("failure not logged:\n%s", logs.String())
	}
}

func TestHandlerAllPass(t *testing.T) {
	logs := captureLog(t)
	rec, body := serve(t, Handler(time.Second, Check{"a", pass}, Check{"b", pass}), http.MethodGet)
	if rec.Code != http.StatusOK || body.Status != "ok" {
		t.Fatalf("%d %+v, want 200 ok", rec.Code, body)
	}
	if logs.Len() != 0 {
		t.Errorf("logged while healthy:\n%s", logs.String())
	}
}

func TestHandlerTimeout(t *testing.T) {
	captureLog(t)
	release := make(chan struct{})
	defer close(release)
	h := Handler(50*time.Millisecond,
		Check{"honours ctx", func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}},
		Check{"ignores ctx", func(context.Context) error {
			<-release
			return nil
		}},
		Check{"fast", pass},
	)
	start := time.Now()
	rec, body := serve(t, h, http.MethodGet)
	if d := time.Since(start); d > time.Second {
		t.Errorf("took %s, past its 50ms timeout", d)
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	if got := strings.Join(body.Failing, ","); got != "honours ctx,ignores ctx" {
		t.Errorf("failing %q", got)
	}
}

// Checks run concurrently: two that each take most of the timeout pass.
func TestHandlerConcurrent(t *testing.T) {
	slow := func(context.Context) error { time.Sleep(150 * time.Millisecond); return nil }
	rec, body := serve(t, Handler(250*time.Millisecond, Check{"a", slow}, Check{"b", slow}, Check{"c", slow}), http.MethodGet)
	if rec.Code != http.StatusOK {
		t.Fatalf("%d %+v, want 200: checks ran one after another", rec.Code, body)
	}
}

func TestHandlerPanic(t *testing.T) {
	logs := captureLog(t)
	rec, body := serve(t, Handler(time.Second, Check{"boom", func(context.Context) error { panic("kaboom") }}), http.MethodGet)
	if rec.Code != http.StatusServiceUnavailable || strings.Join(body.Failing, ",") != "boom" {
		t.Fatalf("%d %+v, want 503 boom", rec.Code, body)
	}
	if !strings.Contains(logs.String(), "panic: kaboom") {
		t.Errorf("panic not logged:\n%s", logs.String())
	}
}

func TestHandlerMethods(t *testing.T) {
	h := Handler(0)
	rec, _ := serve(t, h, http.MethodHead)
	if rec.Code != http.StatusOK {
		t.Errorf("HEAD %d", rec.Code)
	}
	rec, _ = serve(t, h, http.MethodPost)
	if rec.Code != http.StatusMethodNotAllowed || rec.Header().Get("Allow") != "GET, HEAD" {
		t.Errorf("POST %d Allow %q", rec.Code, rec.Header().Get("Allow"))
	}
}

func TestHandlerPanicsOnBadChecks(t *testing.T) {
	for name, checks := range map[string][]Check{
		"no name":   {{"", pass}},
		"no func":   {{"db", nil}},
		"duplicate": {{"db", pass}, {"db", pass}},
	} {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("no panic")
				}
			}()
			Handler(0, checks...)
		})
	}
}

// A failing check logs when it starts failing and when it recovers, not
// on every probe.
func TestHandlerLogsChangesOnly(t *testing.T) {
	logs := captureLog(t)
	var down atomic.Bool
	h := Handler(time.Second,
		Check{"db", func(context.Context) error {
			if down.Load() {
				return errors.New("closed")
			}
			return nil
		}},
		Check{"other", pass},
	)
	probe := func(want int) {
		t.Helper()
		if rec, _ := serve(t, h, http.MethodGet); rec.Code != want {
			t.Fatalf("status %d, want %d", rec.Code, want)
		}
	}
	probe(http.StatusOK)
	down.Store(true)
	probe(http.StatusServiceUnavailable)
	probe(http.StatusServiceUnavailable)
	probe(http.StatusServiceUnavailable)
	down.Store(false)
	probe(http.StatusOK)
	probe(http.StatusOK)
	down.Store(true)
	probe(http.StatusServiceUnavailable)

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	want := []string{
		`level=WARN msg="Health check failing" check=db error=closed`,
		`level=INFO msg="Health check recovered" check=db`,
		`level=WARN msg="Health check failing" check=db error=closed`,
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d log lines, want %d:\n%s", len(lines), len(want), logs.String())
	}
	for i, w := range want {
		if !strings.HasSuffix(lines[i], w) {
			t.Errorf("line %d = %s, want suffix %s", i, lines[i], w)
		}
	}
}

// Concurrent probes log a change once.
func TestHandlerLogsChangeOnceConcurrently(t *testing.T) {
	logs := captureLog(t)
	h := Handler(time.Second, Check{"db", func(context.Context) error { return errors.New("closed") }})
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path, nil))
		}()
	}
	wg.Wait()
	if n := strings.Count(logs.String(), "Health check failing"); n != 1 {
		t.Errorf("logged %d times, want once:\n%s", n, logs.String())
	}
}

// A client that cancels its request neither fails a check nor changes the
// logged state: a genuine failure afterwards still logs its WARN.
func TestHandlerClientCancel(t *testing.T) {
	logs := captureLog(t)
	var down atomic.Bool
	h := Handler(time.Second, Check{"db", func(ctx context.Context) error {
		if down.Load() {
			return errors.New("closed")
		}
		return ctx.Err() // fails only if the client's cancel reaches the check
	}})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, Path, nil).WithContext(ctx))
	if rec.Code != http.StatusOK {
		t.Errorf("cancelled request: status %d, want 200 (the check saw the client's cancel)", rec.Code)
	}
	if logs.Len() != 0 {
		t.Errorf("cancelled request logged:\n%s", logs.String())
	}

	// Cancelled while the check genuinely fails: still no state change.
	down.Store(true)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, Path, nil).WithContext(ctx))
	if logs.Len() != 0 {
		t.Errorf("cancelled request logged:\n%s", logs.String())
	}

	if rec, _ := serve(t, h, http.MethodGet); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	if n := strings.Count(logs.String(), `level=WARN msg="Health check failing" check=db error=closed`); n != 1 {
		t.Errorf("genuine failure logged %d times, want once:\n%s", n, logs.String())
	}
}

// End to end: Probe against Handler over a real loopback listener.
func TestProbeHandler(t *testing.T) {
	captureLog(t)
	var healthy atomic.Bool
	healthy.Store(true)
	srv := httptest.NewServer(Handler(0, Check{"db", func(context.Context) error {
		if !healthy.Load() {
			return errors.New("closed")
		}
		return nil
	}}))
	defer srv.Close()
	if err := Probe(srv.URL+Path, DefaultTimeout); err != nil {
		t.Fatalf("healthy: %v", err)
	}
	healthy.Store(false)
	err := Probe(srv.URL+Path, DefaultTimeout)
	if err == nil || !strings.Contains(err.Error(), `503 Service Unavailable: {"status":"unhealthy","failing":["db"]}`) {
		t.Fatalf("unhealthy: %v", err)
	}
}
