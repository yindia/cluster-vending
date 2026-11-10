package config_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/config"
	"github.com/yindia/cluster-vending/pkg/database"
)

const (
	mgmtAccount = "111122223333"
	mgmtRole    = "arn:aws:iam::111122223333:role/platform/cluster-vending"
)

// lookup is a fake flag set or environment.
func lookup(kv map[string]string) config.LookupFunc {
	return func(key string) (string, bool) {
		v, ok := kv[key]
		return v, ok
	}
}

// awsEnv is the minimal environment LoadServe accepts.
func awsEnv() map[string]string {
	return map[string]string{
		config.EnvAWSManagementAccountID: mgmtAccount,
		config.EnvAWSManagementRoleARN:   mgmtRole,
	}
}

func kubeconfigFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatalf("write kubeconfig: %v", err)
	}
	return path
}

func requireErrorNaming(t *testing.T, err error, names ...string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want an error naming %v, got nil", names)
	}
	for _, name := range names {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not name %s", err, name)
		}
	}
}

func TestLoadGlobalDefaults(t *testing.T) {
	cfg, err := config.LoadGlobal(nil, lookup(nil), nil)
	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	want := config.Global{LogLevel: "info", LogFormat: "json"}
	if cfg != want {
		t.Fatalf("defaults = %+v, want %+v", cfg, want)
	}
}

func TestLoadGlobalFlagBeatsEnvBeatsDefault(t *testing.T) {
	path := kubeconfigFile(t)
	flags := lookup(map[string]string{
		config.FlagLogLevel:    "debug",
		config.FlagKubeContext: "kind-dev",
	})
	env := lookup(map[string]string{
		config.EnvLogLevel:    "warn", // loses to the flag
		config.EnvLogFormat:   "text", // no flag: wins over the default
		config.EnvKubeconfig:  path,
		config.EnvKubeContext: "prod", // loses to the flag
	})

	cfg, err := config.LoadGlobal(flags, env, nil)

	if err != nil {
		t.Fatalf("LoadGlobal: %v", err)
	}
	want := config.Global{Kubeconfig: path, KubeContext: "kind-dev", LogLevel: "debug", LogFormat: "text"}
	if cfg != want {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
}

func TestLoadGlobalRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name     string
		flags    map[string]string
		env      map[string]string
		wantName string
	}{
		{name: "unknown log level", env: map[string]string{config.EnvLogLevel: "verbose"}, wantName: config.EnvLogLevel},
		{name: "unknown log format", flags: map[string]string{config.FlagLogFormat: "xml"}, wantName: config.FlagLogFormat},
		{name: "missing kubeconfig", env: map[string]string{config.EnvKubeconfig: "/does/not/exist"}, wantName: config.EnvKubeconfig},
		{name: "kubeconfig is a directory", env: map[string]string{config.EnvKubeconfig: os.TempDir()}, wantName: config.EnvKubeconfig},
		{name: "empty env is not a default", env: map[string]string{config.EnvLogLevel: ""}, wantName: config.EnvLogLevel},
		{name: "empty flag is not a default", flags: map[string]string{config.FlagKubeContext: ""}, wantName: config.FlagKubeContext},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.LoadGlobal(lookup(tt.flags), lookup(tt.env), nil)
			requireErrorNaming(t, err, tt.wantName)
		})
	}
}

func TestLoadServeDefaults(t *testing.T) {
	cfg, err := config.LoadServe(nil, lookup(awsEnv()), nil)
	if err != nil {
		t.Fatalf("LoadServe: %v", err)
	}
	want := config.Serve{
		HTTPAddr:         ":8080",
		ShutdownTimeout:  30 * time.Second,
		InstallProviders: true,
		AWS: config.AWS{
			ManagementAccountID: mgmtAccount,
			ManagementRoleARN:   mgmtRole,
		},
		Database: database.DefaultConfig(),
	}
	if cfg != want {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
}

func TestLoadServeFlagsOverrideEnv(t *testing.T) {
	flags := lookup(map[string]string{
		config.FlagHTTPAddr:        "127.0.0.1:9090",
		config.FlagShutdownTimeout: "5s",
	})
	env := awsEnv()
	env[config.EnvHTTPAddr] = ":7070"

	cfg, err := config.LoadServe(flags, lookup(env), nil)

	if err != nil {
		t.Fatalf("LoadServe: %v", err)
	}
	if cfg.HTTPAddr != "127.0.0.1:9090" || cfg.ShutdownTimeout != 5*time.Second {
		t.Fatalf("flags not applied: %+v", cfg)
	}
}

func TestLoadServeRequiresManagementIdentity(t *testing.T) {
	_, err := config.LoadServe(nil, lookup(nil), nil)
	requireErrorNaming(t, err, config.EnvAWSManagementAccountID, config.EnvAWSManagementRoleARN)
}

func TestLoadServeRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name     string
		key      string // env variable to override
		value    string
		wantName string
	}{
		{"addr without port", config.EnvHTTPAddr, "localhost", config.EnvHTTPAddr},
		{"unparsable timeout", config.EnvShutdownTimeout, "soon", config.EnvShutdownTimeout},
		{"zero timeout", config.EnvShutdownTimeout, "0s", config.EnvShutdownTimeout},
		{"short account id", config.EnvAWSManagementAccountID, "12345", config.EnvAWSManagementAccountID},
		{"non-numeric account id", config.EnvAWSManagementAccountID, "11112222333x", config.EnvAWSManagementAccountID},
		{"role arn is a user", config.EnvAWSManagementRoleARN, "arn:aws:iam::111122223333:user/bob", config.EnvAWSManagementRoleARN},
		{"role arn in another account", config.EnvAWSManagementRoleARN, "arn:aws:iam::444455556666:role/cluster-vending", config.EnvAWSManagementRoleARN},
		{"role arn in another partition", config.EnvAWSManagementRoleARN, "arn:aws-cn:iam::111122223333:role/cluster-vending", config.EnvAWSManagementRoleARN},
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

func TestLoadServeReportsAllProblemsTogether(t *testing.T) {
	_, err := config.LoadServe(nil, lookup(map[string]string{
		config.EnvHTTPAddr:        "nope",
		config.EnvShutdownTimeout: "soon",
	}), nil)
	requireErrorNaming(t, err, config.EnvHTTPAddr, config.EnvShutdownTimeout, config.EnvAWSManagementAccountID)
}

func TestSettingsAreUniqueAndPrefixed(t *testing.T) {
	flags, envs := map[string]bool{}, map[string]bool{}
	all := slices.Concat(config.GlobalSettings(), config.ServeSettings(), config.ClientSettings())
	for _, s := range all {
		if flags[s.Flag] || envs[s.Env] {
			t.Errorf("duplicate setting %s", s)
		}
		flags[s.Flag], envs[s.Env] = true, true
		if !strings.HasPrefix(s.Env, "CV_") {
			t.Errorf("%s: environment variable must start with CV_", s)
		}
		if s.Usage == "" {
			t.Errorf("%s has no usage text", s)
		}
	}
}

func TestSettingsReturnCopies(t *testing.T) {
	config.GlobalSettings()[0].Default = "changed"
	if config.GlobalSettings()[0].Default == "changed" {
		t.Fatal("GlobalSettings exposes the internal table")
	}
}

func TestTenantRoleARN(t *testing.T) {
	got, err := config.TenantRoleARN("444455556666", "acme-admin")
	if err != nil || got != "arn:aws:iam::444455556666:role/acme-admin" {
		t.Fatalf("TenantRoleARN = %q, %v", got, err)
	}
	for _, bad := range [][2]string{{"444455556666", "team/admin"}, {"444455556666", ""}, {"4444", "acme-admin"}} {
		if _, err := config.TenantRoleARN(bad[0], bad[1]); err == nil {
			t.Errorf("TenantRoleARN(%q, %q) accepted", bad[0], bad[1])
		}
	}
}

func TestLoadServeStaticKeys(t *testing.T) {
	env := awsEnv()
	cfg, err := config.LoadServe(nil, lookup(env), nil)
	if err != nil || cfg.AWS.StaticKeys != nil {
		t.Fatalf("without keys: %+v, %v", cfg.AWS.StaticKeys, err)
	}
	env[config.EnvAWSAccessKeyID], env[config.EnvAWSSecretAccessKey] = "AKID", "secret"
	cfg, err = config.LoadServe(nil, lookup(env), nil)
	if err != nil || cfg.AWS.StaticKeys == nil || cfg.AWS.StaticKeys.AccessKeyID != "AKID" {
		t.Fatalf("with keys: %+v, %v", cfg.AWS.StaticKeys, err)
	}
	delete(env, config.EnvAWSSecretAccessKey)
	if cfg, _ := config.LoadServe(nil, lookup(env), nil); cfg.AWS.StaticKeys != nil {
		t.Error("a key ID without a secret must be ignored")
	}
}

func TestLoadServeInstallProviders(t *testing.T) {
	env := awsEnv()
	env[config.EnvInstallProviders] = "false"
	cfg, err := config.LoadServe(nil, lookup(env), nil)
	if err != nil || cfg.InstallProviders {
		t.Fatalf("got %v, %v; want install-providers false", cfg.InstallProviders, err)
	}
	env[config.EnvInstallProviders] = "maybe"
	if _, err := config.LoadServe(nil, lookup(env), nil); err == nil {
		t.Fatal("want an error for install-providers=maybe")
	}
}
