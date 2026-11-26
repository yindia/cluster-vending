package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/config"
)

func TestLiveSettings(t *testing.T) {
	write := func(t *testing.T, body string) config.LookupFunc {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.yaml")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		file, err := config.LoadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return file
	}
	tests := []struct {
		name      string
		file      string
		env       map[string]string
		wantURL   string
		wantAllow bool
		wantErr   string
	}{
		{name: "off by default"},
		{name: "the live section", file: "live:\n  url: http://cluster-vending-live:8090\n  allowWithoutAuth: true\n",
			wantURL: "http://cluster-vending-live:8090", wantAllow: true},
		{name: "environment", env: map[string]string{config.EnvLiveURL: "https://live.internal"}, wantURL: "https://live.internal"},
		{name: "not a URL", env: map[string]string{config.EnvLiveURL: "cluster-vending-live"}, wantErr: "live-url"},
		{name: "not a bool", file: "live:\n  allowWithoutAuth: maybe\n", wantErr: "live-allowWithoutAuth"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var file config.LookupFunc
			if tt.file != "" {
				file = write(t, tt.file)
			}
			cfg, err := config.LoadServe(nil, lookup(tt.env), file)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one about %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if cfg.LiveURL != tt.wantURL || cfg.LiveAllowWithoutAuth != tt.wantAllow {
				t.Fatalf("live = %q %v", cfg.LiveURL, cfg.LiveAllowWithoutAuth)
			}
		})
	}
	live, err := config.LoadLive(nil, lookup(nil), write(t, "live:\n  addr: 127.0.0.1:9090\n"))
	if err != nil || live.Addr != "127.0.0.1:9090" || live.ShutdownTimeout != 30*time.Second {
		t.Fatalf("live command = %+v, %v", live, err)
	}
	if _, err := config.LoadLive(nil, lookup(map[string]string{config.EnvLiveAddr: "nope"}), nil); err == nil {
		t.Fatal("a bad address is refused")
	}
	if live.MetricsAddr != "" {
		t.Fatalf("metrics listener on by default: %q", live.MetricsAddr)
	}
	live, err = config.LoadLive(nil, lookup(nil), write(t, "live:\n  metricsAddr: :8091\n"))
	if err != nil || live.MetricsAddr != ":8091" {
		t.Fatalf("metrics address = %+v, %v", live, err)
	}
	if _, err := config.LoadLive(nil, lookup(map[string]string{config.EnvLiveMetricsAddr: "nope"}), nil); err == nil {
		t.Fatal("a bad metrics address is refused")
	}
}
