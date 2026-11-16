package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestLoginPrivateSecretFlagsAndCallback(t *testing.T) {
	command := newLoginCmd()
	if command.Flags().Lookup("client-secret") != nil {
		t.Fatal("secret text must not be accepted in argv")
	}
	for _, name := range []string{"client-secret-env", "client-secret-file", "client-secret-prompt", "callback-port", "redirect-host"} {
		if command.Flags().Lookup(name) == nil {
			t.Fatalf("missing login flag %s", name)
		}
	}
	if err := command.ParseFlags([]string{"--callback-port", "18081", "--redirect-host", "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	port, err := command.Flags().GetInt("callback-port")
	if err != nil || port != 18081 {
		t.Fatal("callback port flag not retained")
	}
}

func TestLoginRejectsNonTTYSecretPrompt(t *testing.T) {
	command := newLoginCmd()
	command.SetIn(strings.NewReader("not-a-terminal"))
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	if err := command.ParseFlags([]string{"--browser", "--issuer", "https://issuer.example", "--client-id", "cv-cli-confidential", "--client-secret-prompt"}); err != nil {
		t.Fatal(err)
	}
	err := command.RunE(command, nil)
	if err == nil || !strings.Contains(err.Error(), "requires a terminal") {
		t.Fatalf("non-TTY prompt error: %v", err)
	}
}
