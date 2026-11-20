package healthcheck

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRequested(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"svc"}, false},
		{[]string{"svc", "-healthcheck"}, true},
		{[]string{"svc", "--healthcheck"}, true},
		{[]string{"svc", "-healthcheck=true"}, true}, // detected, then rejected by run
		{[]string{"svc", "-healthcheck", "extra"}, true},
		{[]string{"svc", "healthcheck"}, false},
		{[]string{"svc", "---healthcheck"}, false},
		{[]string{"svc", "-healthchecks"}, false},
		{[]string{"svc", "-config", "x", "-healthcheck"}, false}, // first argument only
		{[]string{"caddy", "run", "--config", "/etc/caddy/Caddyfile"}, false},
	} {
		if got := requested(tc.args); got != tc.want {
			t.Errorf("requested(%q) = %t, want %t", tc.args, got, tc.want)
		}
	}
}

func noEnv(string) string { return "" }

// countingServer answers every request with status and counts them.
func countingServer(t *testing.T, status int) (*httptest.Server, *atomic.Int32) {
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n.Add(1)
		w.WriteHeader(status)
		w.Write([]byte("body text\n"))
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

func TestRunNotRequestedHasNoSideEffects(t *testing.T) {
	srv, hits := countingServer(t, http.StatusOK)
	getenv := func(k string) string {
		t.Errorf("read env %s without -healthcheck", k)
		return ""
	}
	for _, args := range [][]string{{"svc"}, {"svc", "-daemon"}, {"svc", "serve", "-healthcheck"}} {
		var stderr strings.Builder
		if _, ok := run(args, getenv, &stderr, srv.URL, time.Second); ok {
			t.Errorf("run(%q) took the health check path", args)
		}
		if stderr.Len() != 0 {
			t.Errorf("run(%q) wrote %q", args, stderr.String())
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("%d requests without -healthcheck", n)
	}
}

func TestRunUsage(t *testing.T) {
	srv, hits := countingServer(t, http.StatusOK)
	for _, args := range [][]string{{"svc", "-healthcheck=true"}, {"svc", "--healthcheck", "-config", "x"}} {
		var stderr strings.Builder
		code, ok := run(args, noEnv, &stderr, srv.URL, time.Second)
		if !ok || code != 1 {
			t.Errorf("run(%q) = %d, %t; want 1, true", args, code, ok)
		}
		if !strings.Contains(stderr.String(), "usage") {
			t.Errorf("run(%q) stderr %q, want usage", args, stderr.String())
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("%d requests on a usage error", n)
	}
}

func TestRun(t *testing.T) {
	ok, _ := countingServer(t, http.StatusOK)
	noContent, _ := countingServer(t, http.StatusNoContent)
	failing, _ := countingServer(t, http.StatusInternalServerError)
	unavailable, _ := countingServer(t, http.StatusServiceUnavailable)
	redirect := httptest.NewServer(http.RedirectHandler("http://192.0.2.1/healthz", http.StatusFound))
	defer redirect.Close()

	release := make(chan struct{})
	hung := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer hung.Close()
	defer close(release)

	// Headers at once, then a body that never finishes.
	stall := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer stall.Close()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refused := "http://" + l.Addr().String() + Path
	l.Close()

	for _, tc := range []struct {
		name, url, stderr string
		want              int
	}{
		{"200", ok.URL + Path, "", 0},
		{"204", noContent.URL, "", 0},
		{"500", failing.URL, "500 Internal Server Error: body text", 1},
		{"503", unavailable.URL, "503", 1},
		{"redirect not followed", redirect.URL, "302 Found (redirect to http://192.0.2.1/healthz)", 1},
		{"timeout", hung.URL, "Timeout", 1},
		{"body stalls", stall.URL, "reading body", 1},
		{"refused", refused, "connection refused", 1},
		{"not loopback", "http://192.0.2.1:8080/healthz", "loopback", 1},
		{"hostname", "http://localhost:8080/healthz", "loopback", 1},
		{"https", "https://127.0.0.1:8080/healthz", "scheme", 1},
		{"no URL", "", "no URL", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stderr strings.Builder
			start := time.Now()
			code, ok := run([]string{"svc", "-healthcheck"}, noEnv, &stderr, tc.url, 200*time.Millisecond)
			if !ok || code != tc.want {
				t.Fatalf("run = %d, %t; want %d (stderr %q)", code, ok, tc.want, stderr.String())
			}
			if tc.want == 0 && stderr.Len() != 0 {
				t.Errorf("healthy, but stderr %q", stderr.String())
			}
			if !strings.Contains(stderr.String(), tc.stderr) {
				t.Errorf("stderr %q, want it to contain %q", stderr.String(), tc.stderr)
			}
			if d := time.Since(start); d > 2*time.Second {
				t.Errorf("took %s, past its 200ms timeout", d)
			}
		})
	}
}

func TestRunEnvOverride(t *testing.T) {
	ok, hits := countingServer(t, http.StatusOK)
	getenv := func(k string) string {
		if k == EnvURL {
			return ok.URL + Path
		}
		return ""
	}
	var stderr strings.Builder
	if code, _ := run([]string{"svc", "-healthcheck"}, getenv, &stderr, "http://127.0.0.1:1/healthz", time.Second); code != 0 {
		t.Fatalf("exit %d with %s set, stderr %q", code, EnvURL, stderr.String())
	}
	if hits.Load() != 1 {
		t.Errorf("%s not used", EnvURL)
	}
}

// The proxy environment never applies: the transport has no Proxy func.
func TestClientIgnoresProxyEnv(t *testing.T) {
	tr, ok := newClient(time.Second).Transport.(*http.Transport)
	if !ok {
		t.Fatal("not an *http.Transport")
	}
	if tr.Proxy != nil {
		t.Error("transport has a Proxy func")
	}
}

func TestListenURL(t *testing.T) {
	for addr, want := range map[string]string{
		":8080":          "http://127.0.0.1:8080/healthz",
		"0.0.0.0:9000":   "http://127.0.0.1:9000/healthz",
		"[::]:9001":      "http://127.0.0.1:9001/healthz",
		"127.0.0.1:8099": "http://127.0.0.1:8099/healthz",
		"[::1]:8080":     "http://[::1]:8080/healthz",
		"8080":           "http://8080/healthz", // no port: Probe rejects it
	} {
		if got := ListenURL(addr); got != want {
			t.Errorf("ListenURL(%q) = %q, want %q", addr, got, want)
		}
	}
	if err := checkLoopback(ListenURL("8080")); err == nil {
		t.Error("a portless addr passed the loopback check")
	}
}

// Main itself, in a child process: it exits with the probe's code when
// asked, and returns (the service starts) when not.
func TestMainProcess(t *testing.T) {
	if url, ok := os.LookupEnv("HEALTHCHECK_TEST_CHILD"); ok {
		os.Args = append([]string{"svc"}, strings.Fields(os.Getenv("HEALTHCHECK_TEST_ARGS"))...)
		Main(url)
		os.Stdout.WriteString("service started\n")
		os.Exit(42)
	}
	ok, _ := countingServer(t, http.StatusOK)
	down, _ := countingServer(t, http.StatusServiceUnavailable)
	for _, tc := range []struct {
		name, url, args string
		want            int
	}{
		{"healthy", ok.URL + Path, "-healthcheck", 0},
		{"unhealthy", down.URL + Path, "--healthcheck", 1},
		{"not requested", ok.URL + Path, "", 42},
		{"other flags", ok.URL + Path, "-daemon -config x", 42},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestMainProcess$")
			cmd.Env = append(os.Environ(), "HEALTHCHECK_TEST_CHILD="+tc.url, "HEALTHCHECK_TEST_ARGS="+tc.args)
			out, err := cmd.CombinedOutput()
			code := 0
			if exit, ok := err.(*exec.ExitError); ok {
				code = exit.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if code != tc.want {
				t.Fatalf("exit %d, want %d; output:\n%s", code, tc.want, out)
			}
		})
	}
}
