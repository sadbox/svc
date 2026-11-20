// Package healthcheck is the service standard for container health checks.
//
// Every service binary is its own health client: the Quadlet unit runs
//
//	HealthCmd=["/<binary>", "-healthcheck"]
//
// inside the container, and the binary GETs its own running instance's
// /healthz on loopback and exits 0 if it answers 2xx, 1 otherwise with the
// reason on stderr. The images are distroless (no shell, no curl), so the
// check can't be anything else.
//
// The client side is [Main], the first line of main:
//
//	func main() {
//		healthcheck.Main(healthcheck.ListenURL(":8080"))
//		// flag parsing, config, secrets, DB...
//	}
//
// The server side is [Handler], mounted at [Path].
package healthcheck

import (
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// Path is where every service serves its health endpoint.
	Path = "/healthz"

	// DefaultTimeout bounds the whole probe: connect, request and response.
	// It is under every Quadlet unit's HealthTimeout, so a hung server
	// fails the check with a reason instead of being killed by Podman.
	DefaultTimeout = 3 * time.Second
)

// Requested reports whether the process was started as the health check:
// os.Args[1] is -healthcheck or --healthcheck. Only the first argument is
// looked at, so a binary that hands the rest of argv to another CLI
// framework can check before delegating.
func Requested() bool { return requested(os.Args) }

func requested(args []string) bool {
	if len(args) < 2 {
		return false
	}
	name, _, _ := strings.Cut(args[1], "=")
	return name == "-healthcheck" || name == "--healthcheck"
}

// Main returns at once unless [Requested]. If it is, Main probes url with
// [Probe] and [DefaultTimeout] and exits the process: 0 if healthy, 1 otherwise, the reason on stderr.
//
// Call it first in main, before flag parsing and anything with a side
// effect: the check must not load config, read secrets or open a DB.
//
// -healthcheck must be the only argument. A value (-healthcheck=true) or
// further arguments fail the check (exit 1) rather than fall through to
// starting the service.
func Main(url string) {
	if code, ok := run(os.Args, os.Stderr, url, DefaultTimeout); ok {
		os.Exit(code)
	}
}

// run is Main without the process: it reports whether args asked for the
// health check and, if so, the exit code.
func run(args []string, stderr io.Writer, url string, timeout time.Duration) (code int, ok bool) {
	if !requested(args) {
		return 0, false
	}
	if len(args) != 2 || strings.Contains(args[1], "=") {
		fmt.Fprintf(stderr, "healthcheck: usage: %s -healthcheck (no value, no other arguments)\n", args[0])
		return 1, true
	}
	if err := Probe(url, timeout); err != nil {
		fmt.Fprintln(stderr, "healthcheck:", err)
		return 1, true
	}
	return 0, true
}

// Probe GETs rawURL and returns nil if it answers 2xx within timeout. The
// URL must be http:// with a literal loopback IP (127.0.0.0/8 or ::1):
// the check is for the process's own container, never another host.
// Proxy environment variables are ignored and redirects aren't followed
// (a 3xx is a failure).
func Probe(rawURL string, timeout time.Duration) error {
	if err := checkLoopback(rawURL); err != nil {
		return err
	}
	resp, err := newClient(timeout).Get(rawURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Read the body inside the timeout: a server that sends headers and
	// then stalls fails too. Only its start goes into the error.
	head, err := io.ReadAll(io.LimitReader(resp.Body, 512))
	if err == nil {
		_, err = io.Copy(io.Discard, resp.Body)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := fmt.Sprintf("GET %s: %s", rawURL, resp.Status)
		if loc := resp.Header.Get("Location"); loc != "" {
			msg += " (redirect to " + loc + ")"
		}
		if b := strings.TrimSpace(string(head)); b != "" {
			msg += ": " + b
		}
		return errors.New(msg)
	}
	if err != nil {
		return fmt.Errorf("GET %s: reading body: %w", rawURL, err)
	}
	return nil
}

// newClient is Probe's client: its own transport, with no proxy (not
// even from the environment: net/http skips it for loopback today, but
// that is its policy, not this package's) and no keep-alive.
func newClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout:   timeout,
		Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func checkLoopback(rawURL string) error {
	if rawURL == "" {
		return errors.New("no URL")
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return err
	}
	if u.Scheme != "http" {
		return fmt.Errorf("%s: scheme must be http", rawURL)
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%s: host must be a loopback IP", rawURL)
	}
	return nil
}

// ListenURL is the health URL of a server listening on addr (a
// net.Listen/http.Server address such as ":8080"): an empty or
// unspecified host (":8080", "0.0.0.0:8080", "[::]:8080") becomes
// 127.0.0.1. An addr that doesn't parse is used as is, so [Probe]
// reports it.
func ListenURL(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return "http://" + addr + Path
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + Path
}
