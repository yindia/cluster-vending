// Local-only Keycloak configuration generator. Credentials never go to stdout.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"

	"github.com/yindia/cluster-vending/pkg/auth"
)

const issuer = "http://localhost:18080/realms/cluster-vending"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "keycloak:", err)
		os.Exit(1)
	}
}

func run() error {
	base := flag.String("base", "hack/config.local.yaml", "personal config to copy; never modified")
	state := flag.String("state", "hack/.keycloak", "private generated files")
	redirect := flag.String("cli-redirect", "http://localhost:18081/callback", "exact CLI loopback callback")
	probe := flag.Bool("probe", false, "verify existing API requires authentication")
	checkPort := flag.Bool("check-keycloak-port", false, "refuse a conflicting listener before creating Keycloak")
	checkOfflineMigration := flag.Bool("check-offline-migration", false, "verify an existing realm differs only by local offline-access grants")
	grantOffline := flag.Bool("grant-offline-access", false, "enable offline access for the local users without resetting the realm")
	flag.Parse()
	if *checkOfflineMigration {
		return checkOfflineRoleMigration(*state)
	}
	if *grantOffline {
		return grantLocalOfflineAccess(*state)
	}
	if *checkPort {
		conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(context.Background(), "tcp", "127.0.0.1:18080")
		if errors.Is(err, syscall.ECONNREFUSED) {
			return nil
		}
		if err != nil {
			return errors.New("cannot verify whether port 18080 is available")
		}
		_ = conn.Close()
		return errors.New("port 18080 is occupied; coordinate stopping that process before starting Keycloak")
	}
	if *probe {
		return probeAPI()
	}
	return prepare(*base, *state, *redirect)
}

// localAuth is the server's auth section for the local realm:
// authentication only, as everywhere (the API has no roles).
func localAuth() auth.Config {
	return auth.Config{
		Mode:    auth.ModeRequired,
		Issuers: []auth.Issuer{{Name: "keycloak", Issuer: issuer, Audiences: []string{"cv-api"}, UsernameClaim: "preferred_username"}},
	}
}

// secretKeys are the generated credentials of the local realm. The users
// keep their names, so an existing secrets.env still reads.
var secretKeys = []string{"KEYCLOAK_ADMIN_PASSWORD", "KEYCLOAK_UI_CLIENT_SECRET", "KEYCLOAK_CLI_CLIENT_SECRET", "KEYCLOAK_ACME_ADMIN_PASSWORD", "KEYCLOAK_ACME_VIEWER_PASSWORD", "AUTH_SECRET"}

// localUsers are the realm's users and the secrets.env key of each one's
// password. They are plain users: no groups or roles, since any user who
// can log in may do everything. The names are kept from the time they
// stood for roles, so the browser tests and existing secrets still work.
var localUsers = [][2]string{{"acme-admin", "KEYCLOAK_ACME_ADMIN_PASSWORD"}, {"acme-viewer", "KEYCLOAK_ACME_VIEWER_PASSWORD"}}

// isLocalUser reports whether name is one of localUsers.
func isLocalUser(name string) bool {
	for _, u := range localUsers {
		if u[0] == name {
			return true
		}
	}
	return false
}

func prepare(base, state, redirect string) error {
	// Exact loopback callbacks only; no wildcard redirect URIs.
	host, port, err := net.SplitHostPort(strings.TrimSuffix(strings.TrimPrefix(redirect, "http://"), "/callback"))
	portNumber, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || portNumber < 1 || portNumber > 65535 || (host != "localhost" && host != "127.0.0.1") || !strings.HasPrefix(redirect, "http://") || !strings.HasSuffix(redirect, "/callback") {
		return errors.New("CLI redirect must be http://localhost:<port>/callback or http://127.0.0.1:<port>/callback")
	}
	if err := os.MkdirAll(state, 0700); err != nil {
		return err
	}
	if err := os.Chmod(state, 0700); err != nil { //nolint:gosec // Private directory requires owner traversal; files below use 0600.
		return err
	}
	secretPath := filepath.Join(state, "secrets.env")
	secrets, err := readSecrets(secretPath)
	if err != nil {
		return err
	}
	if secrets == nil {
		secrets = map[string]string{}
		for _, key := range secretKeys {
			value := make([]byte, 32)
			if _, err := rand.Read(value); err != nil {
				return err
			}
			secrets[key] = hex.EncodeToString(value)
		}
		if err := writePrivate(secretPath, envFile(secrets)); err != nil {
			return err
		}
	}
	ui := map[string]string{"AUTH_ENABLED": "true", "KEYCLOAK_ISSUER": issuer, "KEYCLOAK_CLIENT_ID": "cv-ui", "KEYCLOAK_CLIENT_SECRET": secrets["KEYCLOAK_UI_CLIENT_SECRET"], "AUTH_SECRET": secrets["AUTH_SECRET"], "AUTH_URL": "http://localhost:3000"}
	if err := writePrivate(filepath.Join(state, "ui.env"), envFile(ui)); err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(state, "container.env"), envFile(map[string]string{"KC_BOOTSTRAP_ADMIN_USERNAME": "admin", "KC_BOOTSTRAP_ADMIN_PASSWORD": secrets["KEYCLOAK_ADMIN_PASSWORD"]})); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(state, "import"), 0700); err != nil {
		return err
	}
	realm, err := json.MarshalIndent(localRealm(secrets, redirect), "", "  ")
	if err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(state, "import", "cluster-vending-realm.json"), realm); err != nil {
		return err
	}
	raw, err := os.ReadFile(base)
	if err != nil {
		return fmt.Errorf("read base config: %w", err)
	}
	var settings map[string]any
	if err := yaml.Unmarshal(raw, &settings); err != nil {
		return fmt.Errorf("read base YAML: %w", err)
	}
	if settings == nil {
		settings = map[string]any{}
	}
	settings["auth"] = localAuth()
	settings["http-addr"] = "127.0.0.1:8080"
	config, err := yaml.Marshal(settings)
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(state, "server.yaml"), config)
}

func localRealm(secrets map[string]string, redirect string) map[string]any {
	audience := map[string]any{"name": "cv-api-audience", "protocol": "openid-connect", "protocolMapper": "oidc-audience-mapper", "config": map[string]string{"included.custom.audience": "cv-api", "access.token.claim": "true", "id.token.claim": "false"}}
	client := func(id string, public bool, secret string, redirects []string) map[string]any {
		c := map[string]any{"clientId": id, "protocol": "openid-connect", "enabled": true, "publicClient": public, "standardFlowEnabled": true, "implicitFlowEnabled": false, "directAccessGrantsEnabled": false, "serviceAccountsEnabled": false, "redirectUris": redirects, "defaultClientScopes": []string{"profile", "email"}, "optionalClientScopes": []string{"offline_access"}, "attributes": map[string]string{"pkce.code.challenge.method": "S256"}, "protocolMappers": []any{audience}}
		if !public {
			c["secret"] = secret
			c["clientAuthenticatorType"] = "client-secret"
		}
		if id == "cv-ui" {
			c["rootUrl"] = "http://localhost:3000"
			c["baseUrl"] = "http://localhost:3000/"
			c["attributes"].(map[string]string)["post.logout.redirect.uris"] = "http://localhost:3000##http://localhost:3000/"
		}
		return c
	}
	user := func(name, password string) map[string]any {
		return map[string]any{"username": name, "enabled": true, "emailVerified": true, "email": name + "@localhost.test", "firstName": "Local", "lastName": name, "credentials": []any{map[string]any{"type": "password", "value": password, "temporary": false}}, "realmRoles": []string{"offline_access"}}
	}
	users := make([]any, 0, len(localUsers))
	for _, u := range localUsers {
		users = append(users, user(u[0], secrets[u[1]]))
	}
	return map[string]any{
		"realm": "cluster-vending", "enabled": true, "sslRequired": "none", "registrationAllowed": false, "resetPasswordAllowed": false, "accessTokenLifespan": 300,
		"clients": []any{client("cv-ui", false, secrets["KEYCLOAK_UI_CLIENT_SECRET"], []string{"http://localhost:3000/api/auth/callback/keycloak"}), client("cv-cli", true, "", []string{redirect}), client("cv-cli-confidential", false, secrets["KEYCLOAK_CLI_CLIENT_SECRET"], []string{redirect})},
		"users":   users,
	}
}

func readSecrets(path string) (map[string]string, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		return nil, errors.New("secrets.env must have mode 0600")
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || len(value) != 64 {
			return nil, errors.New("invalid secrets.env; refusing to rotate existing credentials")
		}
		if _, err := hex.DecodeString(value); err != nil {
			return nil, errors.New("invalid secrets.env; expected generated hexadecimal values")
		}
		out[key] = value
	}
	for _, key := range secretKeys {
		if out[key] == "" {
			return nil, errors.New("incomplete secrets.env; refusing to rotate existing credentials")
		}
	}
	return out, nil
}

func envFile(values map[string]string) []byte {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var out strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&out, "%s=%s\n", key, values[key])
	}
	return []byte(out.String())
}

func writePrivate(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".keycloak-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func probeAPI() error {
	// 10 means there is no listener. Other failures must never start a duplicate.
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(context.Background(), "tcp", "127.0.0.1:8080")
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) {
			os.Exit(10)
		}
		return err
	}
	_ = conn.Close()
	client := &http.Client{Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	for _, token := range []string{"", "Bearer invalid-local-token"} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, "http://127.0.0.1:8080/cloud.v1.TenantService/WhoAmI", strings.NewReader("{}"))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", token)
		}
		res, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("cannot verify existing API listener: %w", err)
		}
		var body struct {
			Code string `json:"code"`
		}
		err = json.NewDecoder(res.Body).Decode(&body)
		_ = res.Body.Close()
		if err != nil || res.StatusCode != http.StatusUnauthorized || body.Code != "unauthenticated" {
			return errors.New("existing listener on 8080 does not enforce required authentication; stop it explicitly and rerun")
		}
	}
	return nil
}
