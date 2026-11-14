package config_test

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yindia/cluster-vending/pkg/config"
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

// awsEnv is the minimal environment LoadController accepts.
func awsEnv() map[string]string {
	return map[string]string{
		config.EnvAWSManagementRoleARN: mgmtRole,
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
	if !reflect.DeepEqual(cfg, want) {
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

// The API server has three settings, and needs none of AWS, of a store or
// of the providers: it only validates and stores the desired state.
func TestLoadServeDefaults(t *testing.T) {
	cfg, err := config.LoadServe(nil, lookup(nil), nil)
	if err != nil {
		t.Fatalf("LoadServe: %v", err)
	}
	want := config.Serve{HTTPAddr: ":8080", ShutdownTimeout: 30 * time.Second}
	if !reflect.DeepEqual(cfg, want) {
		t.Fatalf("got %+v, want %+v", cfg, want)
	}
	var flags []string
	for _, s := range config.ServeSettings() {
		flags = append(flags, s.Flag)
	}
	if want := []string{config.FlagConfig, config.FlagHTTPAddr, config.FlagShutdownTimeout,
		config.FlagAWSManagementRoleARN, config.FlagCloudFormationTemplateURL}; !slices.Equal(flags, want) {
		t.Errorf("serve settings = %v, want only %v", flags, want)
	}
}

// The account onboarding settings are optional; when set they must be a
// role ARN and an https URL.
func TestLoadServeOnboarding(t *testing.T) {
	cfg, err := config.LoadServe(lookup(map[string]string{
		config.FlagAWSManagementRoleARN:      "arn:aws:iam::111122223333:role/platform",
		config.FlagCloudFormationTemplateURL: "https://bucket.s3.amazonaws.com/cv",
	}), lookup(nil), nil)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ManagementRoleARN != "arn:aws:iam::111122223333:role/platform" || cfg.CloudFormationTemplateURL != "https://bucket.s3.amazonaws.com/cv" {
		t.Errorf("got %+v", cfg)
	}

	_, err = config.LoadServe(lookup(map[string]string{
		config.FlagAWSManagementRoleARN:      "platform",
		config.FlagCloudFormationTemplateURL: "http://bucket/cv",
	}), lookup(nil), nil)
	requireErrorNaming(t, err, config.FlagAWSManagementRoleARN, config.FlagCloudFormationTemplateURL)
}

func TestLoadServeFlagsOverrideEnv(t *testing.T) {
	flags := lookup(map[string]string{
		config.FlagHTTPAddr:        "127.0.0.1:9090",
		config.FlagShutdownTimeout: "5s",
	})
	env := map[string]string{config.EnvHTTPAddr: ":7070"}

	cfg, err := config.LoadServe(flags, lookup(env), nil)

	if err != nil {
		t.Fatalf("LoadServe: %v", err)
	}
	if cfg.HTTPAddr != "127.0.0.1:9090" || cfg.ShutdownTimeout != 5*time.Second {
		t.Fatalf("flags not applied: %+v", cfg)
	}
}

// Keys in the environment (local testing) are noticed, not required: the
// AWS SDK reads them itself.
func TestStaticKeysFromTheEnvironment(t *testing.T) {
	env := awsEnv()
	cfg, err := config.LoadController(nil, lookup(env), nil)
	if err != nil || cfg.AWS.StaticKeys != nil {
		t.Fatalf("without keys: %+v, %v", cfg.AWS.StaticKeys, err)
	}
	env[config.EnvAWSAccessKeyID], env[config.EnvAWSSecretAccessKey] = "AKID", "secret"
	cfg, err = config.LoadController(nil, lookup(env), nil)
	if err != nil || cfg.AWS.StaticKeys == nil || cfg.AWS.StaticKeys.AccessKeyID != "AKID" {
		t.Fatalf("with keys: %+v, %v", cfg.AWS.StaticKeys, err)
	}
	delete(env, config.EnvAWSSecretAccessKey)
	if cfg, _ := config.LoadController(nil, lookup(env), nil); cfg.AWS.StaticKeys != nil {
		t.Error("a key ID without a secret must be ignored")
	}
}

// The controller's own service account is optional (unset outside the
// cluster): a name, in the controller's own namespace.
func TestControllerServiceAccount(t *testing.T) {
	env := awsEnv()
	cfg, err := config.LoadController(nil, lookup(env), nil)
	if err != nil || cfg.ServiceAccount != nil {
		t.Fatalf("unset: %+v, %v", cfg.ServiceAccount, err)
	}
	env[config.EnvServiceAccount], env[config.EnvLeaseNamespace] = "cluster-vending-controller-manager", "cluster-vending-system"
	cfg, err = config.LoadController(nil, lookup(env), nil)
	want := config.ServiceAccountRef{Namespace: "cluster-vending-system", Name: "cluster-vending-controller-manager"}
	if err != nil || cfg.ServiceAccount == nil || *cfg.ServiceAccount != want {
		t.Fatalf("set: %+v, %v", cfg.ServiceAccount, err)
	}
	for _, bad := range []string{"ns/name", "Bad_Name", "-x"} {
		env[config.EnvServiceAccount] = bad
		_, err := config.LoadController(nil, lookup(env), nil)
		requireErrorNaming(t, err, config.EnvServiceAccount)
	}
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := config.LoadServe(nil, lookup(map[string]string{tt.key: tt.value}), nil)
			requireErrorNaming(t, err, tt.wantName)
		})
	}
}

// The management identity the controller needs is checked: a role ARN of
// the aws partition (its account is the management account).
func TestManagementIdentityIsChecked(t *testing.T) {
	tests := []struct {
		name, key, value string
	}{
		{"role arn is a user", config.EnvAWSManagementRoleARN, "arn:aws:iam::111122223333:user/bob"},
		{"role arn in another partition", config.EnvAWSManagementRoleARN, "arn:aws-cn:iam::111122223333:role/cluster-vending"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := awsEnv()
			env[tt.key] = tt.value
			_, err := config.LoadController(nil, lookup(env), nil)
			requireErrorNaming(t, err, tt.key)
		})
	}
}

func TestLoadServeReportsAllProblemsTogether(t *testing.T) {
	_, err := config.LoadServe(nil, lookup(map[string]string{
		config.EnvHTTPAddr:        "nope",
		config.EnvShutdownTimeout: "soon",
	}), nil)
	requireErrorNaming(t, err, config.EnvHTTPAddr, config.EnvShutdownTimeout)
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
