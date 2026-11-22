package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

// This migration accepts only the narrowly scoped fixture-role addition. It
// never treats changed passwords, client settings or tenant grants as applied.
func checkOfflineRoleMigration(state string) error {
	previous, err := os.ReadFile(filepath.Join(state, "applied-realm.json"))
	if err != nil {
		return errors.New("cannot read previous realm settings")
	}
	next, err := os.ReadFile(filepath.Join(state, "import", "cluster-vending-realm.json"))
	if err != nil || !onlyOfflineRoleAddition(previous, next) {
		return errors.New("realm changes exceed the supported offline-access migration")
	}
	return nil
}

func onlyOfflineRoleAddition(previous, next []byte) bool {
	var before, after map[string]any
	if json.Unmarshal(previous, &before) != nil || json.Unmarshal(next, &after) != nil {
		return false
	}
	normalize := func(realm map[string]any, requireRole bool) bool {
		users, ok := realm["users"].([]any)
		if !ok {
			return false
		}
		seen := map[string]bool{}
		for _, entry := range users {
			user, ok := entry.(map[string]any)
			if !ok {
				return false
			}
			name, _ := user["username"].(string)
			if !isLocalUser(name) {
				continue
			}
			if seen[name] {
				return false
			}
			seen[name] = true
			var remaining []any
			found := false
			if roles, exists := user["realmRoles"]; exists {
				list, ok := roles.([]any)
				if !ok {
					return false
				}
				for _, role := range list {
					value, ok := role.(string)
					if !ok {
						return false
					}
					if value == "offline_access" {
						if found {
							return false
						}
						found = true
					} else {
						remaining = append(remaining, role)
					}
				}
			}
			if requireRole && !found {
				return false
			}
			if len(remaining) == 0 {
				delete(user, "realmRoles")
			} else {
				user["realmRoles"] = remaining
			}
		}
		return len(seen) == 2
	}
	return normalize(before, false) && normalize(after, true) && reflect.DeepEqual(before, after)
}

type localRealmRole struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func grantLocalOfflineAccess(state string) error {
	secrets, err := readSecrets(filepath.Join(state, "secrets.env"))
	if err != nil || secrets["KEYCLOAK_ADMIN_PASSWORD"] == "" {
		return errors.New("private local administrator credentials are unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return ensureOfflineAccess(ctx, client, "http://localhost:18080", secrets["KEYCLOAK_ADMIN_PASSWORD"])
}

func ensureOfflineAccess(ctx context.Context, client *http.Client, base, password string) error {
	form := url.Values{"client_id": {"admin-cli"}, "grant_type": {"password"}, "username": {"admin"}, "password": {password}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/realms/master/protocol/openid-connect/token", strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("cannot prepare local administrator authentication")
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if err := localAdminRequest(client, request, &token); err != nil || token.AccessToken == "" {
		return errors.New("local administrator authentication failed; realm was not reset")
	}
	call := func(method, path string, payload any, result any) error {
		var body io.Reader
		if payload != nil {
			data, err := json.Marshal(payload)
			if err != nil {
				return errors.New("cannot prepare offline-access grant")
			}
			body = bytes.NewReader(data)
		}
		req, err := http.NewRequestWithContext(ctx, method, base+"/admin/realms/cluster-vending"+path, body)
		if err != nil {
			return errors.New("cannot prepare local realm request")
		}
		req.Header.Set("Authorization", "Bearer "+token.AccessToken)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		return localAdminRequest(client, req, result)
	}
	var role localRealmRole
	if err := call(http.MethodGet, "/roles/offline_access", nil, &role); err != nil || role.ID == "" || role.Name != "offline_access" {
		return errors.New("built-in offline_access role is unavailable")
	}
	for _, u := range localUsers {
		username := u[0]
		var users []struct {
			ID       string `json:"id"`
			Username string `json:"username"`
		}
		if err := call(http.MethodGet, "/users?username="+url.QueryEscape(username)+"&exact=true", nil, &users); err != nil || len(users) != 1 || users[0].Username != username || users[0].ID == "" {
			return errors.New("local fixture user lookup failed or is ambiguous")
		}
		path := "/users/" + url.PathEscape(users[0].ID) + "/role-mappings/realm"
		var assigned []localRealmRole
		if err := call(http.MethodGet, path+"/composite", nil, &assigned); err != nil {
			return errors.New("cannot verify local offline-access role mappings")
		}
		found := false
		for _, current := range assigned {
			if current.ID == role.ID && current.Name == role.Name {
				found = true
			}
		}
		if !found {
			if err := call(http.MethodPost, path, []localRealmRole{role}, nil); err != nil {
				return errors.New("cannot grant local offline access")
			}
		}
	}
	return nil
}

// Provider bodies and transport errors may contain credentials. Return only a
// bounded status classification; never echo them into bootstrap logs.
func localAdminRequest(client *http.Client, request *http.Request, result any) error {
	response, err := client.Do(request)
	if err != nil {
		return errors.New("local realm request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("local realm request returned HTTP %d", response.StatusCode)
	}
	if result == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(result); err != nil {
		return errors.New("local realm response was invalid")
	}
	return nil
}
