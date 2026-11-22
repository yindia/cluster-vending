package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func realmBytes(t *testing.T, roles bool) []byte {
	t.Helper()
	realm := localRealm(map[string]string{}, "http://localhost:18081/callback")
	if !roles {
		for _, item := range realm["users"].([]any) {
			delete(item.(map[string]any), "realmRoles")
		}
	}
	raw, err := json.Marshal(realm)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestOfflineMigrationOnlyChangesFixtureOfflineGrants(t *testing.T) {
	before, after := realmBytes(t, false), realmBytes(t, true)
	if !onlyOfflineRoleAddition(before, after) || !onlyOfflineRoleAddition(after, after) {
		t.Fatal("expected offline-only migration and exact rerun to be supported")
	}
	for _, changed := range []string{
		strings.Replace(string(after), "acme-admin@localhost.test", "someone@localhost.test", 1),
		strings.Replace(string(after), "18081", "18082", 1),
		strings.Replace(string(after), `"realmRoles":["offline_access"]`, `"realmRoles":["offline_access","admin"]`, 1),
		strings.Replace(string(after), `"realmRoles":["offline_access"]`, `"realmRoles":[{"unexpected":"role"}]`, 1),
	} {
		if onlyOfflineRoleAddition(before, []byte(changed)) {
			t.Fatal("migration accepted unrelated or malformed realm changes")
		}
	}
	if onlyOfflineRoleAddition(after, before) {
		t.Fatal("migration accepted removal of the required role")
	}
}

func TestOfflineAccessMigrationIsNarrowAndIdempotent(t *testing.T) {
	assigned := map[string]bool{}
	writes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/realms/master/protocol/openid-connect/token" {
			if r.Method != http.MethodPost || r.ParseForm() != nil || r.Form.Get("password") != "fixture-password" || r.Form.Get("client_id") != "admin-cli" {
				t.Error("invalid private admin request")
			}
			_, _ = w.Write([]byte(`{"access_token":"fixture-admin-token"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture-admin-token" {
			t.Error("missing administrator bearer credential")
		}
		switch {
		case r.URL.Path == "/admin/realms/cluster-vending/roles/offline_access":
			_, _ = w.Write([]byte(`{"id":"offline-role","name":"offline_access"}`))
		case r.URL.Path == "/admin/realms/cluster-vending/users":
			name := r.URL.Query().Get("username")
			if (name != "acme-admin" && name != "acme-viewer") || r.URL.Query().Get("exact") != "true" {
				t.Error("migration attempted an unexpected user lookup")
			}
			_ = json.NewEncoder(w).Encode([]map[string]string{{"id": name, "username": name}})
		case strings.HasSuffix(r.URL.Path, "/role-mappings/realm/composite"):
			if assigned[r.URL.Path] {
				_, _ = w.Write([]byte(`[{"id":"offline-role","name":"offline_access"}]`))
			} else {
				_, _ = w.Write([]byte(`[]`))
			}
		case strings.HasSuffix(r.URL.Path, "/role-mappings/realm") && r.Method == http.MethodPost:
			var roles []localRealmRole
			if json.NewDecoder(r.Body).Decode(&roles) != nil || len(roles) != 1 || roles[0].ID != "offline-role" || roles[0].Name != "offline_access" {
				t.Error("migration attempted a broader role grant")
			}
			assigned[r.URL.Path+"/composite"] = true
			writes++
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Error("migration attempted an unexpected endpoint or method")
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	for range 2 {
		if err := ensureOfflineAccess(t.Context(), server.Client(), server.URL, "fixture-password"); err != nil {
			t.Fatal(err)
		}
	}
	if writes != 2 {
		t.Fatalf("expected exactly one role addition per fixture user, got %d", writes)
	}
}

func TestOfflineAccessMigrationNeverEchoesProviderBodies(t *testing.T) {
	const credential = "private-credential-must-not-appear"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(credential))
	}))
	defer server.Close()
	err := ensureOfflineAccess(t.Context(), server.Client(), server.URL, credential)
	if err == nil || strings.Contains(err.Error(), credential) {
		t.Fatal("expected a sanitized maintenance error")
	}
}
