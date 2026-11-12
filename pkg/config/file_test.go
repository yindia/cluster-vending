package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/config"
)

func writeYAML(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func mustLoadFile(t *testing.T, body string) config.LookupFunc {
	t.Helper()
	file, err := config.LoadFile(writeYAML(t, body))
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	return file
}

func TestLoadFileNestedAndFlatKeysAreEquivalent(t *testing.T) {
	nested := mustLoadFile(t, `
log-level: debug
http-addr: "127.0.0.1:9090"
shutdown-timeout: 5s
aws:
  management-role-arn: arn:aws:iam::111122223333:role/platform/cluster-vending
`)
	flat := mustLoadFile(t, `
log-level: debug
http-addr: "127.0.0.1:9090"
shutdown-timeout: 5s
aws-management-role-arn: arn:aws:iam::111122223333:role/platform/cluster-vending
`)
	for name, file := range map[string]config.LookupFunc{"nested": nested, "flat": flat} {
		t.Run(name, func(t *testing.T) {
			g, err := config.LoadGlobal(nil, lookup(nil), file)
			if err != nil || g.LogLevel != "debug" {
				t.Fatalf("LoadGlobal = %+v, %v", g, err)
			}
			s, err := config.LoadServe(nil, lookup(nil), file)
			if err != nil {
				t.Fatalf("LoadServe: %v", err)
			}
			if s.HTTPAddr != "127.0.0.1:9090" || s.ShutdownTimeout != 5*time.Second {
				t.Fatalf("LoadServe = %+v", s)
			}
			c, err := config.LoadController(nil, lookup(nil), file)
			if err != nil || c.AWS.ManagementAccountID != mgmtAccount || c.AWS.ManagementRoleARN != mgmtRole {
				t.Fatalf("LoadController = %+v, %v", c.AWS, err)
			}
		})
	}
}

func TestFlagBeatsEnvBeatsFileBeatsDefault(t *testing.T) {
	file := mustLoadFile(t, "log-level: warn\nlog-format: text\ncontext: from-file\n")
	flags := lookup(map[string]string{config.FlagLogLevel: "error"})
	env := lookup(map[string]string{config.EnvLogFormat: "json", config.EnvLogLevel: "debug"})

	g, err := config.LoadGlobal(flags, env, file)

	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	if g.LogLevel != "error" || g.LogFormat != "json" || g.KubeContext != "from-file" {
		t.Fatalf("got %+v; want flag log-level, env log-format, file context", g)
	}
}

func TestLoadFileRejects(t *testing.T) {
	tests := []struct {
		name, body, wantName string
	}{
		{name: "unknown key", body: "http-adress: :9090\n", wantName: "http-adress"},                              //nolint:misspell // a deliberate typo
		{name: "unknown nested key", body: "aws:\n  managment-role-arn: x\n", wantName: "aws-managment-role-arn"}, //nolint:misspell // a deliberate typo
		{name: "list value", body: "log-level: [debug]\n", wantName: "log-level"},
		{name: "config inside the file", body: "config: other.yaml\n", wantName: "config"},
		{name: "not a mapping", body: "- a\n- b\n", wantName: "mapping"},
		{name: "invalid yaml", body: "log-level: [\n", wantName: "yaml"},
		{name: "null value", body: "log-level:\n", wantName: "log-level"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.LoadFile(writeYAML(t, tt.body))
			requireErrorNaming(t, err, tt.wantName)
		})
	}
}

func TestLoadFileMissing(t *testing.T) {
	_, err := config.LoadFile(filepath.Join(t.TempDir(), "nope.yaml"))
	requireErrorNaming(t, err, "nope.yaml")
}

func TestFileFrom(t *testing.T) {
	path := writeYAML(t, "log-level: warn\n")

	none, err := config.FileFrom(nil, lookup(nil))
	if err != nil || none != nil {
		t.Fatalf("no --config: got %v, %v; want nil, nil", none, err)
	}

	fromEnv, err := config.FileFrom(nil, lookup(map[string]string{config.EnvConfig: path}))
	if err != nil {
		t.Fatalf("FileFrom(env): %v", err)
	}
	if v, ok := fromEnv(config.FlagLogLevel); !ok || v != "warn" {
		t.Fatalf("file lookup = %q, %v", v, ok)
	}

	_, err = config.FileFrom(lookup(map[string]string{config.FlagConfig: "/does/not/exist.yaml"}), lookup(nil))
	requireErrorNaming(t, err, "/does/not/exist.yaml")
}
