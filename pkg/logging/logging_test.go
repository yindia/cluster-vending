package logging_test

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/logging"
)

func TestNew(t *testing.T) {
	tests := []struct {
		name, level, format string
		wantJSON            bool
		wantDebug           bool
	}{
		{"json info", "info", "json", true, false},
		{"text debug", "debug", "text", false, true},
		{"json error hides debug", "error", "json", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger, err := logging.New(&buf, tt.level, tt.format)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			logger.Debug("debug line")
			logger.Error("error line")

			out := buf.String()
			if got := strings.Contains(out, "debug line"); got != tt.wantDebug {
				t.Errorf("debug logged = %v, want %v; output %q", got, tt.wantDebug, out)
			}
			first, _, _ := strings.Cut(out, "\n")
			if got := json.Valid([]byte(first)); got != tt.wantJSON {
				t.Errorf("JSON output = %v, want %v; line %q", got, tt.wantJSON, first)
			}
		})
	}
}

func TestNewRejectsInvalidSettings(t *testing.T) {
	tests := []struct{ name, level, format string }{
		{"unknown level", "loud", "json"},
		{"unknown format", "info", "xml"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := logging.New(io.Discard, tt.level, tt.format); err == nil {
				t.Fatal("want error")
			}
		})
	}
}
