package telemetry

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const scope = "github.com/sadbox/svc/telemetry"

// Metric names (as Grafana Cloud's Prometheus shows them: dots become
// underscores and the unit is appended):
//
//	sadbox.job.runs                      sadbox_job_runs_total{job_name, outcome}  (counter)
//	sadbox.job.duration                  sadbox_job_duration_seconds{job_name, outcome}  (histogram)
//	sadbox.job.last_success_timestamp    sadbox_job_last_success_timestamp_seconds{job_name}  (gauge, Unix seconds)
//
// outcome is "success" or "failure".
//
// The counter is what a "has it succeeded lately" alert keys on:
// increase(sadbox_job_runs_total{outcome="success"}[25h]) < 1. Each
// success is one increment, remembered by Prometheus rather than by the
// process, so a restart (a reset to 0, which increase() handles) loses
// none: a daily job that last succeeded before an auto-update restart still
// counts. Both outcomes read 0 from the first export of a process, so the
// first success is a visible 0 -> 1 step, not a series that appears at 1.
//
// The gauge is for dashboards ("last success, N ago"). It lives in the
// process: absent after a restart until the next success, unless the
// service seeds it from its own records ([Job.Seed]).
const (
	jobRunsName        = "sadbox.job.runs"
	jobDurationName    = "sadbox.job.duration"
	jobLastSuccessName = "sadbox.job.last_success_timestamp"
)

// JobName is the attribute naming a [Job].
const JobName = attribute.Key("job.name")

var (
	outcomeSuccess = attribute.String("outcome", "success")
	outcomeFailure = attribute.String("outcome", "failure")
)

// Job is one named unit of background work: a poll pass, a periodic save,
// a scheduled task, a "still connected" check. [Job.Run] times it, counts
// its outcome, traces it, and records when it last succeeded; that last
// success is what replaces an UptimeRobot heartbeat ping.
//
// Jobs are process-wide by name: NewJob twice with one name gives one
// series. Safe for concurrent use.
type Job struct {
	name string
	attr attribute.KeyValue

	mu                  sync.Mutex
	last                time.Time // last success; zero before the first
	successes, failures int64     // since the process started
}

var jobs = struct {
	sync.Mutex
	byName   map[string]*Job
	once     sync.Once
	duration metric.Float64Histogram
}{byName: map[string]*Job{}}

// durationBuckets, in seconds: 5ms (a save) to 10 minutes (a backup).
var durationBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600,
}

// initJobInstruments creates the instruments once, on the global
// MeterProvider. Before [Start] that is OpenTelemetry's delegating global,
// which forwards to the real provider once it's set, so a Job may be made
// in a package-level var.
func initJobInstruments() {
	m := otel.Meter(scope)
	var err error
	jobs.duration, err = m.Float64Histogram(jobDurationName,
		metric.WithUnit("s"),
		metric.WithDescription("Duration of a background job's runs, by outcome."),
		metric.WithExplicitBucketBoundaries(durationBuckets...),
	)
	if err != nil {
		otel.Handle(err)
	}
	// Observable, not a plain counter: the callback reports every job's
	// counts, zeros included, at every export, and a Job made before Start
	// (a package-level var) loses nothing to the global's delegation.
	_, err = m.Int64ObservableCounter(jobRunsName,
		metric.WithUnit("{run}"),
		metric.WithDescription("Runs of a background job since the process started, by outcome."),
		metric.WithInt64Callback(observeRuns),
	)
	if err != nil {
		otel.Handle(err)
	}
	_, err = m.Float64ObservableGauge(jobLastSuccessName,
		metric.WithUnit("s"),
		metric.WithDescription("Unix time of a background job's last successful run."),
		metric.WithFloat64Callback(observeLastSuccess),
	)
	if err != nil {
		otel.Handle(err)
	}
}

func allJobs() []*Job {
	jobs.Lock()
	defer jobs.Unlock()
	all := make([]*Job, 0, len(jobs.byName))
	for _, j := range jobs.byName {
		all = append(all, j)
	}
	return all
}

func observeRuns(_ context.Context, o metric.Int64Observer) error {
	for _, j := range allJobs() {
		j.mu.Lock()
		ok, failed := j.successes, j.failures
		j.mu.Unlock()
		o.Observe(ok, metric.WithAttributes(j.attr, outcomeSuccess))
		o.Observe(failed, metric.WithAttributes(j.attr, outcomeFailure))
	}
	return nil
}

func observeLastSuccess(_ context.Context, o metric.Float64Observer) error {
	for _, j := range allJobs() {
		if t := j.LastSuccess(); !t.IsZero() {
			o.Observe(float64(t.UnixNano())/1e9, metric.WithAttributes(j.attr))
		}
	}
	return nil
}

// NewJob returns the Job called name. Keep names few and fixed
// ("sync_pass", "save_guilds"): each is a label value.
func NewJob(name string) *Job {
	jobs.once.Do(initJobInstruments)
	jobs.Lock()
	defer jobs.Unlock()
	if j, ok := jobs.byName[name]; ok {
		return j
	}
	j := &Job{name: name, attr: JobName.String(name)}
	jobs.byName[name] = j
	return j
}

// Run runs fn once as this job, in a span named "job <name>" (a child of
// any span in ctx), and records its duration and outcome. A nil error is a
// success and moves the last-success time to now. fn's error is returned
// as it is. A panic is recorded as a failure and re-panics.
func (j *Job) Run(ctx context.Context, fn func(context.Context) error) (err error) {
	ctx, span := otel.Tracer(scope).Start(ctx, "job "+j.name,
		trace.WithAttributes(j.attr))
	start := time.Now()
	defer func() {
		if p := recover(); p != nil {
			j.finish(span, start, fmt.Errorf("panic: %v", p))
			panic(p)
		}
		j.finish(span, start, err)
	}()
	return fn(ctx)
}

func (j *Job) finish(span trace.Span, start time.Time, err error) {
	end := time.Now()
	outcome := outcomeSuccess
	if err != nil {
		outcome = outcomeFailure
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		j.mu.Lock()
		j.failures++
		j.mu.Unlock()
	} else {
		j.succeeded(end)
	}
	span.End(trace.WithTimestamp(end))
	if jobs.duration != nil {
		jobs.duration.Record(context.Background(), end.Sub(start).Seconds(),
			metric.WithAttributes(j.attr, outcome))
	}
}

// Succeeded records a success now without timing anything: for a
// condition rather than a run (a bot's gateway is ready, a token still
// authenticates), checked where the heartbeat used to be pinged. It counts
// as a run.
func (j *Job) Succeeded() { j.succeeded(time.Now()) }

func (j *Job) succeeded(t time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.successes++
	if t.After(j.last) {
		j.last = t
	}
}

// Seed sets the last-success time from the service's own durable records
// (a scheduler's table, a state file) at startup, so the gauge survives a
// restart. An older t than the one held is ignored; nothing is counted.
func (j *Job) Seed(t time.Time) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if t.After(j.last) {
		j.last = t
	}
}

// LastSuccess is the time of the last success, zero before the first.
func (j *Job) LastSuccess() time.Time {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.last
}
