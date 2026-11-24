package telemetry

import (
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/grafana/pyroscope-go"
	"go.opentelemetry.io/otel/sdk/resource"
	semconv "go.opentelemetry.io/otel/semconv/v1.40.0"
)

// ProfileAddressEnv names the variable that turns continuous profiling on:
// the address of Alloy's pyroscope.receive_http on the service's network
// gateway (http://<gateway>:4040), set by the Quadlet unit. OpenTelemetry
// has no stable profiles signal, so this is the one variable of our own;
// the name is Pyroscope's own convention. Unset, nothing is profiled.
const ProfileAddressEnv = "PYROSCOPE_SERVER_ADDRESS"

// profileTypes: CPU, heap (in use and allocated) and goroutines. Mutex and
// block profiles need runtime sampling rates set and cost on every lock;
// nothing here needs them.
var profileTypes = []pyroscope.ProfileType{
	pyroscope.ProfileCPU,
	pyroscope.ProfileInuseObjects,
	pyroscope.ProfileInuseSpace,
	pyroscope.ProfileAllocObjects,
	pyroscope.ProfileAllocSpace,
	pyroscope.ProfileGoroutines,
}

// startProfiler pushes profiles to ProfileAddressEnv, named and tagged
// from res as the metrics and spans are: service_name is service.name
// (Grafana links a span's profile by it), with service_namespace and
// service_version as tags. Nothing else is a tag: no host, no request
// data. It returns a stop that uploads what's left (bounded by
// shutdownTimeout), or nil when profiling is off or the address is
// unusable; profiling never stops a service from starting.
func startProfiler(res *resource.Resource) (stop func()) {
	addr := strings.TrimSpace(os.Getenv(ProfileAddressEnv))
	if addr == "" {
		return nil
	}
	set := res.Set()
	name, _ := set.Value(semconv.ServiceNameKey)
	tags := map[string]string{}
	if v, ok := set.Value(semconv.ServiceNamespaceKey); ok {
		tags["service_namespace"] = v.AsString()
	}
	if v, ok := set.Value(semconv.ServiceVersionKey); ok {
		tags["service_version"] = v.AsString()
	}
	p, err := pyroscope.Start(pyroscope.Config{
		ApplicationName: name.AsString(),
		ServerAddress:   addr,
		Tags:            tags,
		ProfileTypes:    profileTypes,
		// No forced GC before each heap profile (every upload, 15s): the
		// in-use heap is then as of the last GC cycle, and profiling
		// doesn't change when the service collects (GOMEMLIMIT tuning).
		DisableGCRuns: true,
		Logger:        profileLogger{},
		// The library's own client waits 30s per upload, and the final
		// upload on stop is one of them: bounded like the OTLP flush. It
		// follows HTTP_PROXY/NO_PROXY, as the exporters do (the units put
		// the gateway in NO_PROXY).
		HTTPClient: &http.Client{
			Timeout: shutdownTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	})
	if err != nil {
		slog.Warn("telemetry: profiling off", "err", scrubURLs(err.Error()))
		return nil
	}
	return func() {
		if err := p.Stop(); err != nil {
			slog.Warn("telemetry: profiler stop", "err", scrubURLs(err.Error()))
		}
	}
}

// profileLogger sends the profiler's messages to slog: its errors (an
// upload failed) as warnings, the rest at debug. URLs are scrubbed as the
// exporters' errors are.
type profileLogger struct{}

func (profileLogger) Infof(format string, args ...any) {
	slog.Debug("telemetry: profiler", "msg", scrubURLs(fmt.Sprintf(format, args...)))
}

func (profileLogger) Debugf(format string, args ...any) {
	slog.Debug("telemetry: profiler", "msg", scrubURLs(fmt.Sprintf(format, args...)))
}

func (profileLogger) Errorf(format string, args ...any) {
	slog.Warn("telemetry: profiler", "err", scrubURLs(fmt.Sprintf(format, args...)))
}
