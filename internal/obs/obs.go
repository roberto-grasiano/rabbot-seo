// Package obs configures structured logging (slog), provides a rotating
// file-log writer (lumberjack) for callers that opt into file output, and
// defines the canonical log-attribute key constants.
package obs

import (
	"io"
	"log/slog"
	"strings"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Canonical slog attribute keys. Use these everywhere for consistent log shape.
const (
	KeyComponent   = "component"
	KeySite        = "site"
	KeyURL         = "url"
	KeySiteID      = "site_id"
	KeyURLID       = "url_id"
	KeyFetchClass  = "fetch_class"
	KeyHTTPStatus  = "http_status"
	KeyDetector    = "detector"
	KeyRuleID      = "rule_id"
	KeyChangeType  = "change_type"
	KeySeverity    = "severity"
	KeyFingerprint = "fingerprint"
	KeyGroupKey    = "group_key"
	KeyNotifier    = "notifier"
	KeyDurationMS  = "duration_ms"
	KeyError       = "error"
	KeyAction      = "action"
)

// parseLevel maps a config string to an slog.Level. Unknown values => Info.
func parseLevel(level string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// NewLogger builds a JSON slog.Logger writing to w at the given level.
func NewLogger(w io.Writer, level string) *slog.Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: parseLevel(level)})
	return slog.New(h)
}

// FileWriter returns a size-rotating file writer for the given path, suitable
// for use as the io.Writer passed to NewLogger when file output is desired.
// The caller owns its lifecycle: pass it to NewLogger, and Close it on
// shutdown. lumberjack also rotates on demand via its Rotate method, but this
// package does not wire that to any signal; callers that want SIGHUP-triggered
// rotation must invoke Rotate themselves.
func FileWriter(path string) *lumberjack.Logger {
	return &lumberjack.Logger{
		Filename:   path,
		MaxSize:    50, // megabytes
		MaxBackups: 5,
		MaxAge:     28, // days
		Compress:   true,
	}
}
