package config_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/database"
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
  management-account-id: "111122223333"
  management-role-arn: arn:aws:iam::111122223333:role/platform/cluster-vending
database:
  driver: postgres
  dsn: postgres://cv@db:5432/cv?sslmode=require
  max-open-conns: 20
`)
	flat := mustLoadFile(t, `
log-level: debug
http-addr: "127.0.0.1:9090"
shutdown-timeout: 5s
aws-management-account-id: "111122223333"
aws-management-role-arn: arn:aws:iam::111122223333:role/platform/cluster-vending
database-driver: postgres
database-dsn: postgres://cv@db:5432/cv?sslmode=require
database-max-open-conns: 20
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
			want := database.Config{Driver: "postgres", DSN: "postgres://cv@db:5432/cv?sslmode=require", MaxOpenConns: 20}
			if s.HTTPAddr != "127.0.0.1:9090" || s.ShutdownTimeout != 5*time.Second ||
				s.AWS.ManagementAccountID != mgmtAccount || s.Database != want {
				t.Fatalf("LoadServe = %+v", s)
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
		{name: "unknown key", body: "http-adress: :9090\n", wantName: "http-adress"}, //nolint:misspell // a deliberate typo
		{name: "unknown nested key", body: "database:\n  drvier: postgres\n", wantName: "database-drvier"},
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

func TestDatabaseDefaultsToLocalSQLite(t *testing.T) {
	s, err := config.LoadServe(nil, lookup(awsEnv()), nil)
	if err != nil {
		t.Fatalf("LoadServe: %v", err)
	}
	if s.Database != database.DefaultConfig() {
		t.Fatalf("database = %+v, want %+v", s.Database, database.DefaultConfig())
	}
}

func TestDatabaseSettingsAreValidated(t *testing.T) {
	tests := []struct {
		name, key, value, wantName string
	}{
		{"unknown driver", config.EnvDatabaseDriver, "mysql", config.EnvDatabaseDriver},
		{"open conns not a number", config.EnvDatabaseMaxOpenConns, "many", config.EnvDatabaseMaxOpenConns},
		{"negative idle conns", config.EnvDatabaseMaxIdleConns, "-1", config.EnvDatabaseMaxIdleConns},
		{"bad lifetime", config.EnvDatabaseConnMaxLifetime, "forever", config.EnvDatabaseConnMaxLifetime},
		{"negative lifetime", config.EnvDatabaseConnMaxLifetime, "-1m", config.EnvDatabaseConnMaxLifetime},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := awsEnv()
			env[tt.key] = tt.value
			_, err := config.LoadServe(nil, lookup(env), nil)
			requireErrorNaming(t, err, tt.wantName)
		})
	}
}

func TestDatabaseFromEnv(t *testing.T) {
	env := awsEnv()
	env[config.EnvDatabaseDriver] = "postgres"
	env[config.EnvDatabaseDSN] = "host=db user=cv dbname=cv"
	env[config.EnvDatabaseConnMaxLifetime] = "30m"

	s, err := config.LoadServe(nil, lookup(env), nil)

	if err != nil {
		t.Fatalf("LoadServe: %v", err)
	}
	want := database.Config{Driver: "postgres", DSN: "host=db user=cv dbname=cv", ConnMaxLifetime: 30 * time.Minute}
	if s.Database != want {
		t.Fatalf("database = %+v, want %+v", s.Database, want)
	}
}
