// Package logging is the services' one log format: log/slog's text
// format, a line per record,
//
//	time=2026-11-18T12:58:47.123Z level=INFO msg="Saved thread reminders" guild_id=42 count=11
//
// so a level, a message and its fields can be picked out of any service's
// journal the same way. Without it slog's default writes the date and
// level as bare words ("2026/11/18 12:58:47 INFO Saved ...") and the
// standard log package writes no level at all.
package logging

import (
	"io"
	"log/slog"
	"os"
	"strings"
)

// EnvLevel names the variable [Init] reads the minimum level from: DEBUG,
// INFO, WARN or ERROR (any case, optionally with an offset such as
// "ERROR+2", as slog.Level parses them). Unset or empty means INFO.
const EnvLevel = "LOG_LEVEL"

// Init makes the format the process's default logger, on stderr, at the
// level from $LOG_LEVEL, and returns it. Call it first in main (after
// healthcheck.Main): a logger taken from slog.Default() before it keeps
// the old format. Lines written with the standard log package follow
// too, at INFO.
//
// A LOG_LEVEL that doesn't parse leaves the level at INFO and is logged
// at WARN, so a typo is visible rather than fatal.
func Init() *slog.Logger {
	level, err := parseLevel(os.Getenv(EnvLevel))
	l := New(os.Stderr, level)
	slog.SetDefault(l)
	if err != nil {
		l.Warn("Ignoring "+EnvLevel+", using INFO", "error", err)
	}
	return l
}

// New is a logger in the format writing to w, at level and above (nil
// means INFO).
func New(w io.Writer, level slog.Leveler) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: level}))
}

func parseLevel(s string) (slog.Level, error) {
	var l slog.Level
	if strings.TrimSpace(s) == "" {
		return slog.LevelInfo, nil
	}
	if err := l.UnmarshalText([]byte(strings.TrimSpace(s))); err != nil {
		return slog.LevelInfo, err
	}
	return l, nil
}

// Fatal logs at ERROR on slog.Default() and exits 1: log.Fatal's job, with
// a level.
func Fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}
