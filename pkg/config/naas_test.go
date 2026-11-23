package config_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/config"
)

func TestNaaSGroupAllowed(t *testing.T) {
	policy := config.NaaS{AllowedGroups: []string{"cv-subtenant-*", "payments-admins"}, DeniedGroups: []string{"cv-subtenant-platform*", "payments-admins"}}
	tests := []struct {
		name   string
		policy config.NaaS
		group  string
		want   bool
	}{
		{"exact allowed", config.NaaS{AllowedGroups: []string{"team-a"}}, "team-a", true},
		{"exact differs", config.NaaS{AllowedGroups: []string{"team-a"}}, "team-ab", false},
		{"exact is case sensitive", config.NaaS{AllowedGroups: []string{"team-a"}}, "Team-A", false},
		{"prefix allowed", policy, "cv-subtenant-acme-admin", true},
		{"prefix itself", policy, "cv-subtenant-", true},
		{"outside prefix", policy, "cv-other", false},
		{"deny prefix first", policy, "cv-subtenant-platform-ops", false},
		{"deny exact over allow exact", policy, "payments-admins", false},
		{"empty list allows nothing", config.NaaS{}, "anything", false},
		{"deny only allows nothing", config.NaaS{DeniedGroups: []string{"x"}}, "y", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.policy.GroupAllowed(tt.group); got != tt.want {
				t.Fatalf("GroupAllowed(%q) = %t, want %t", tt.group, got, tt.want)
			}
		})
	}
}

func TestNaaSCheckGroupsNamesThePatterns(t *testing.T) {
	policy := config.NaaS{AllowedGroups: []string{"cv-subtenant-*"}}
	if err := policy.CheckGroups([]string{"cv-subtenant-a"}, []string{"cv-subtenant-b"}); err != nil {
		t.Fatalf("allowed groups refused: %v", err)
	}
	err := policy.CheckGroups([]string{"cv-subtenant-a"}, []string{"admins"})
	if !errors.Is(err, config.ErrGroupNotAllowed) || !strings.Contains(err.Error(), `"admins"`) || !strings.Contains(err.Error(), "cv-subtenant-*") {
		t.Fatalf("CheckGroups = %v", err)
	}
	if err := (config.NaaS{}).CheckGroups([]string{"a"}); err == nil || !strings.Contains(err.Error(), "has not set naas.allowedGroups") {
		t.Fatalf("empty policy = %v", err)
	}
}

func TestLoadNaaS(t *testing.T) {
	tests := []struct {
		name, file string
		want       config.NaaS
		err        string
	}{
		{name: "no section", file: "http-addr: 127.0.0.1:9090\n"},
		{name: "section", file: "naas:\n  allowedGroups: [\"cv-subtenant-*\", team]\n  deniedGroups: [cv-platform-operators]\n  qualifierImage: registry.example/cv:1\n",
			want: config.NaaS{AllowedGroups: []string{"cv-subtenant-*", "team"}, DeniedGroups: []string{"cv-platform-operators"}, QualifierImage: "registry.example/cv:1"}},
		{name: "unknown key", file: "naas:\n  allowGroups: [a]\n", err: "allowGroups"},
		{name: "inner wildcard", file: "naas:\n  allowedGroups: [\"a*b\"]\n", err: "trailing"},
		{name: "empty pattern", file: "naas:\n  deniedGroups: [\"\"]\n", err: "is empty"},
		{name: "question mark", file: "naas:\n  allowedGroups: [\"a?\"]\n", err: "trailing"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := config.LoadNaaS(writeYAML(t, tt.file))
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got.AllowedGroups, ",") != strings.Join(tt.want.AllowedGroups, ",") || strings.Join(got.DeniedGroups, ",") != strings.Join(tt.want.DeniedGroups, ",") || got.QualifierImage != tt.want.QualifierImage {
				t.Fatalf("LoadNaaS = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// The flat settings ignore the naas section, as they do auth.
func TestLoadFileIgnoresTheNaaSSection(t *testing.T) {
	path := writeYAML(t, "http-addr: 127.0.0.1:9090\nnaas:\n  allowedGroups: [a]\n")
	if _, err := config.LoadFile(path); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.NaaSFrom(func(k string) (string, bool) {
		if k == config.FlagConfig {
			return path, true
		}
		return "", false
	}, nil)
	if err != nil || !cfg.GroupAllowed("a") {
		t.Fatalf("NaaSFrom = %+v, %v", cfg, err)
	}
	if cfg, err := config.NaaSFrom(nil, nil); err != nil || cfg.GroupAllowed("a") {
		t.Fatalf("no file = %+v, %v", cfg, err)
	}
}
