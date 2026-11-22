package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"github.com/yindia/cluster-vending/pkg/auth"
	"github.com/yindia/cluster-vending/pkg/config"
)

func TestPreparePreservesPersonalSettingsAndCredentials(t *testing.T) {
	dir := t.TempDir()
	base, state := filepath.Join(dir, "personal.yaml"), filepath.Join(dir, "private")
	original := []byte("context: personal-context\naws-management-role-arn: arn:aws:iam::123456789012:role/personal\nlog-level: debug\nauth: {mode: none}\n")
	if err := os.WriteFile(base, original, 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := prepare(base, state, "http://localhost:18081/callback"); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(base)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatal("personal settings changed")
	}
	raw, err := os.ReadFile(filepath.Join(state, "server.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var settings map[string]any
	if err := yaml.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	if settings["context"] != "personal-context" || settings["aws-management-role-arn"] != "arn:aws:iam::123456789012:role/personal" || settings["http-addr"] != "127.0.0.1:8080" {
		t.Fatal("generated overlay did not preserve settings and loopback binding")
	}
	cfg, err := config.LoadAuth(filepath.Join(state, "server.yaml"))
	if err != nil || cfg.Mode != auth.ModeRequired || len(cfg.Issuers) != 1 || len(cfg.Ignored) != 0 {
		t.Fatalf("required auth, one issuer, no ignored role keys: %+v, %v", cfg, err)
	}
	realm := string(mustRead(t, filepath.Join(state, "import", "cluster-vending-realm.json")))
	for _, gone := range []string{"tenant-acme-admin", "tenant-acme-viewer", "oidc-group-membership-mapper"} {
		if strings.Contains(realm, gone) {
			t.Fatalf("the realm still has the role fixture %q", gone)
		}
	}
	for _, user := range []string{`"acme-admin"`, `"acme-viewer"`} {
		if !strings.Contains(realm, user) {
			t.Fatalf("the realm lost the local user %s", user)
		}
	}
	before, err := os.ReadFile(filepath.Join(state, "secrets.env"))
	if err != nil {
		t.Fatal(err)
	}
	if err := prepare(base, state, "http://localhost:18081/callback"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(state, "secrets.env"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("credentials rotated on rerun")
	}
	for _, path := range []string{"secrets.env", "ui.env", "container.env", "server.yaml", "import/cluster-vending-realm.json"} {
		info, err := os.Stat(filepath.Join(state, path))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("%s must be private", path)
		}
	}
	if !bytes.Contains(mustRead(t, filepath.Join(state, "ui.env")), []byte("AUTH_ENABLED=true\n")) {
		t.Fatal("UI auth must be enabled")
	}
}

func TestPrepareRejectsRemoteOrWildcardRedirect(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "base.yaml")
	if err := os.WriteFile(base, []byte("context: personal\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, redirect := range []string{"http://example.com:18081/callback", "http://localhost:*/callback", "https://localhost:18081/callback"} {
		if err := prepare(base, t.TempDir(), redirect); err == nil {
			t.Fatalf("accepted %s", redirect)
		}
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
