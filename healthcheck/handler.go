package healthcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"
)

// DefaultCheckTimeout is [Handler]'s timeout when given 0: under
// [DefaultTimeout], so a slow check answers the probe with a 503 that
// names it, not a client timeout.
const DefaultCheckTimeout = 2 * time.Second

// Check is one named health check, such as a DB ping. Func returns nil
// while healthy. It must honour ctx: [Handler] stops waiting for it when
// ctx is done.
type Check struct {
	Name string
	Func func(ctx context.Context) error
}

// response is the body of every answer: {"status":"ok"} or
// {"status":"unhealthy","failing":["db"]}.
type response struct {
	Status  string   `json:"status"`
	Failing []string `json:"failing,omitempty"`
}

// Handler serves the health endpoint ([Path]). On GET or HEAD it runs
// every check concurrently, bounded together by timeout (0 means
// [DefaultCheckTimeout]), and answers 200 {"status":"ok"} if all pass, 503
// {"status":"unhealthy","failing":[...]} otherwise, the failing names in
// the order given. With no checks it answers 200 whenever the server is
// up. Other methods get 405.
//
// The body names failing checks but never shows their errors: the
// endpoint may be reachable from outside. They go to slog.Default()
// instead, on changes only, not every probe: WARN with the error when a
// check starts failing, INFO when it recovers. Every check starts out
// counted as passing. A check that panics fails; one still running at the
// timeout fails as timed out.
//
// Handler panics if a check has no name or Func, or two share a name.
func Handler(timeout time.Duration, checks ...Check) http.Handler {
	if timeout <= 0 {
		timeout = DefaultCheckTimeout
	}
	seen := make(map[string]bool, len(checks))
	for _, c := range checks {
		if c.Name == "" || c.Func == nil {
			panic("healthcheck: Check needs a Name and a Func")
		}
		if seen[c.Name] {
			panic("healthcheck: duplicate Check " + c.Name)
		}
		seen[c.Name] = true
	}
	checks = append([]Check(nil), checks...)
	return &handler{timeout: timeout, checks: checks, failing: make([]bool, len(checks))}
}

type handler struct {
	timeout time.Duration
	checks  []Check

	mu      sync.Mutex
	failing []bool // per check, as of the last request to finish
}

func (h *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body := response{Status: "ok"}
	status := http.StatusOK
	errs := h.run(r.Context())
	h.logChanges(r.Context(), errs)
	for i, err := range errs {
		if err != nil {
			body.Failing = append(body.Failing, h.checks[i].Name)
		}
	}
	if len(body.Failing) > 0 {
		body.Status, status = "unhealthy", http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

// logChanges logs each check whose state differs from the last request's.
// Under the lock, so concurrent requests log a change once.
func (h *handler) logChanges(ctx context.Context, errs []error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i, err := range errs {
		switch failing := err != nil; {
		case failing && !h.failing[i]:
			slog.Default().WarnContext(ctx, "Health check failing", "check", h.checks[i].Name, "error", err)
		case !failing && h.failing[i]:
			slog.Default().InfoContext(ctx, "Health check recovered", "check", h.checks[i].Name)
		}
		h.failing[i] = err != nil
	}
}

// run returns each check's result, in order.
func (h *handler) run(ctx context.Context) []error {
	errs := make([]error, len(h.checks))
	if len(h.checks) == 0 {
		return errs
	}
	ctx, cancel := context.WithTimeout(ctx, h.timeout)
	defer cancel()
	type result struct {
		i   int
		err error
	}
	// Buffered: a check that outlives the timeout still finishes its send.
	results := make(chan result, len(h.checks))
	for i, c := range h.checks {
		go func() {
			defer func() {
				if p := recover(); p != nil {
					results <- result{i, fmt.Errorf("panic: %v", p)}
				}
			}()
			results <- result{i, c.Func(ctx)}
		}()
	}
	pending := make([]bool, len(h.checks))
	for i := range pending {
		pending[i] = true
	}
	for range h.checks {
		select {
		case res := <-results:
			errs[res.i], pending[res.i] = res.err, false
		case <-ctx.Done():
			for i, p := range pending {
				if p {
					errs[i] = fmt.Errorf("no answer within %s: %w", h.timeout, ctx.Err())
				}
			}
			return errs
		}
	}
	return errs
}
