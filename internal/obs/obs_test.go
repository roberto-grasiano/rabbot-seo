package obs

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestNewLoggerLevel(t *testing.T) {
	tests := []struct {
		name     string
		level    string
		logDebug bool
		wantOut  bool
	}{
		{"info hides debug", "info", true, false},
		{"debug shows debug", "debug", true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger := NewLogger(&buf, tc.level)
			logger.Debug("hello")
			got := buf.Len() > 0
			if got != tc.wantOut {
				t.Errorf("debug-line-emitted=%v, want %v", got, tc.wantOut)
			}
		})
	}
}

func TestLogKeyConstants(t *testing.T) {
	var buf bytes.Buffer
	logger := NewLogger(&buf, "info")
	logger.LogAttrs(context.Background(), slog.LevelInfo, "fetch done",
		slog.String(KeyComponent, "fetcher"),
		slog.String(KeyFetchClass, "ok"),
		slog.Int(KeyHTTPStatus, 200),
	)
	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatalf("log line is not JSON: %v", err)
	}
	if rec[KeyComponent] != "fetcher" {
		t.Errorf("component=%v, want fetcher", rec[KeyComponent])
	}
	if rec[KeyFetchClass] != "ok" {
		t.Errorf("fetch_class=%v, want ok", rec[KeyFetchClass])
	}
}
