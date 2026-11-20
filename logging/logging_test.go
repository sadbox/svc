package logging

import (
	"bytes"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// One format for both logging packages: time, level, msg, then the fields.
func TestFormat(t *testing.T) {
	old := slog.Default()
	t.Cleanup(func() { slog.SetDefault(old) })
	var buf bytes.Buffer
	slog.SetDefault(New(&buf, nil))

	slog.Warn("Saved thread reminders", "guild_id", 42, "note", "two words")
	log.Printf("from the %s package", "log")
	slog.Debug("not at INFO")

	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	want := []string{
		`^time=\S+ level=WARN msg="Saved thread reminders" guild_id=42 note="two words"$`,
		`^time=\S+ level=INFO msg="from the log package"$`,
	}
	if len(lines) != len(want) {
		t.Fatalf("got %d lines, want %d:\n%s", len(lines), len(want), buf.String())
	}
	for i, re := range want {
		if !regexp.MustCompile(re).Match(lines[i]) {
			t.Errorf("line %d = %s\nwant match for %s", i, lines[i], re)
		}
	}
}

// A value can't break out of its line.
func TestNewlinesStayInTheLine(t *testing.T) {
	var buf bytes.Buffer
	New(&buf, nil).Info("request", "user_agent", "x\nlevel=ERROR msg=forged")
	if n := bytes.Count(buf.Bytes(), []byte("\n")); n != 1 {
		t.Errorf("one record wrote %d lines:\n%s", n, buf.String())
	}
}

func TestParseLevel(t *testing.T) {
	for in, want := range map[string]slog.Level{
		"":        slog.LevelInfo,
		"  ":      slog.LevelInfo,
		"debug":   slog.LevelDebug,
		"DEBUG":   slog.LevelDebug,
		" warn ":  slog.LevelWarn,
		"error":   slog.LevelError,
		"ERROR+2": slog.LevelError + 2,
	} {
		got, err := parseLevel(in)
		if err != nil || got != want {
			t.Errorf("parseLevel(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if got, err := parseLevel("verbose"); err == nil || got != slog.LevelInfo {
		t.Errorf("parseLevel(verbose) = %v, %v; want INFO and an error", got, err)
	}
}

// Init and Fatal, in a child process: stderr, the level from LOG_LEVEL,
// the default logger replaced, exit 1.
func TestInitAndFatal(t *testing.T) {
	if os.Getenv("LOGGING_TEST_CHILD") == "1" {
		Init()
		slog.Debug("debug line")
		slog.Info("info line")
		os.Stdout.WriteString("stdout stays clean\n")
		Fatal("fatal line", "error", "boom")
	}
	for _, tc := range []struct {
		level       string
		want, unwnt []string
	}{
		{"", []string{"level=INFO msg=\"info line\"", "level=ERROR msg=\"fatal line\" error=boom"}, []string{"debug line"}},
		{"debug", []string{"level=DEBUG msg=\"debug line\"", "info line"}, nil},
		{"error", []string{"fatal line"}, []string{"info line", "debug line"}},
		{"loud", []string{"level=WARN msg=\"Ignoring LOG_LEVEL, using INFO\"", "info line"}, []string{"debug line"}},
	} {
		t.Run("LOG_LEVEL="+tc.level, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestInitAndFatal$")
			cmd.Env = append(os.Environ(), "LOGGING_TEST_CHILD=1", EnvLevel+"="+tc.level)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			exit, ok := err.(*exec.ExitError)
			if !ok || exit.ExitCode() != 1 {
				t.Fatalf("exit %v, want 1; stderr:\n%s", err, stderr.String())
			}
			if got := stdout.String(); got != "stdout stays clean\n" {
				t.Errorf("stdout %q", got)
			}
			for _, w := range tc.want {
				if !strings.Contains(stderr.String(), w) {
					t.Errorf("stderr lacks %q:\n%s", w, stderr.String())
				}
			}
			for _, u := range tc.unwnt {
				if strings.Contains(stderr.String(), u) {
					t.Errorf("stderr has %q:\n%s", u, stderr.String())
				}
			}
		})
	}
}
