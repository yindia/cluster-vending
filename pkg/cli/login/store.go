// Package login is how the CLI proves who its user is: it logs in at an
// OIDC issuer (flow.go: the device authorization grant, or the browser
// with PKCE), keeps the tokens in a file only the user can read
// (store.go), and hands the commands a bearer token, refreshing it when
// it has expired (source.go).
package login

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	// dirName is the CLI's directory in the user's configuration directory.
	dirName = "cluster-vending"
	// fileName holds the tokens of the last login.
	fileName = "token.json"

	dirMode  fs.FileMode = 0o700
	fileMode fs.FileMode = 0o600
)

// Credentials are the tokens of a login and what refreshing them needs.
type Credentials struct {
	// Issuer and ClientID are where and as which client the user logged
	// in; TokenURL is the issuer's token endpoint, Scopes what was asked
	// for.
	Issuer   string   `json:"issuer"`
	ClientID string   `json:"clientId"`
	TokenURL string   `json:"tokenUrl"`
	Scopes   []string `json:"scopes,omitempty"`
	// UseIDToken sends the ID token to the API, not the access token: for
	// issuers whose access tokens are not JWTs for this API.
	UseIDToken bool `json:"useIdToken,omitempty"`

	AccessToken  string `json:"accessToken"`
	IDToken      string `json:"idToken,omitempty"`
	RefreshToken string `json:"refreshToken,omitempty"`
	// Expiry is when the access token expires; zero when the issuer did
	// not say.
	Expiry time.Time `json:"expiry,omitzero"`
}

// Bearer is the token the API is called with.
func (c *Credentials) Bearer() string {
	if c.UseIDToken {
		return c.IDToken
	}
	return c.AccessToken
}

// Store is the file the credentials are kept in.
type Store struct{ Path string }

// DefaultStore is token.json in the CLI's directory of the user's
// configuration directory (os.UserConfigDir: ~/.config on Linux,
// ~/Library/Application Support on macOS, %AppData% on Windows).
func DefaultStore() (Store, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return Store{}, fmt.Errorf("no configuration directory for the login: %w", err)
	}
	return Store{Path: filepath.Join(dir, dirName, fileName)}, nil
}

// Load reads the credentials; nil when nobody is logged in. A file other
// users can read is refused: it holds tokens.
func (s Store) Load() (*Credentials, error) {
	info, err := os.Stat(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read the login: %w", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is readable by other users (mode %04o): it holds tokens; run `chmod 600 %s` or log in again",
			s.Path, info.Mode().Perm(), s.Path)
	}
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		return nil, fmt.Errorf("read the login: %w", err)
	}
	var c Credentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("%s is not a login file (%w): run `cluster-vending login` again", s.Path, err)
	}
	return &c, nil
}

// Save writes the credentials, readable by the user only, replacing the
// file in one step: a reader never sees half of it.
func (s Store) Save(c *Credentials) error {
	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("save the login: %w", err)
	}
	//nolint:gosec // the tokens are what this file is for: it is written 0600 in the user's own directory
	raw, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return fmt.Errorf("save the login: %w", err)
	}
	tmp, err := os.CreateTemp(dir, fileName+".*")
	if err != nil {
		return fmt.Errorf("save the login: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }() // a no-op once renamed
	if err := tmp.Chmod(fileMode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("save the login: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("save the login: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("save the login: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.Path); err != nil {
		return fmt.Errorf("save the login: %w", err)
	}
	return nil
}

// Delete removes the credentials; it is not an error that there are none.
// It reports whether there were any.
func (s Store) Delete() (bool, error) {
	err := os.Remove(s.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("remove the login: %w", err)
	}
	return true, nil
}
