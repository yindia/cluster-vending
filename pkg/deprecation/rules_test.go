package deprecation

import (
	"strings"
	"testing"
)

func TestRulesTableInvariants(t *testing.T) {
	seen := map[ruleKey]struct{}{}
	for _, rule := range Rules() {
		key := ruleKey{apiVersion: rule.APIVersion, kind: rule.Kind}
		name := rule.APIVersion + " " + rule.Kind
		if _, dup := seen[key]; dup {
			t.Errorf("%s: listed twice", name)
		}
		seen[key] = struct{}{}
		if rule.Kind == "" {
			t.Errorf("%s: no kind", name)
		}
		if !strings.Contains(rule.APIVersion, "/") {
			t.Errorf("%s: apiVersion has no group", name)
		}
		if rule.DeprecatedIn >= rule.RemovedIn {
			t.Errorf("%s: deprecated in %s, not before removal in %s", name, rule.DeprecatedIn, rule.RemovedIn)
		}
		if rule.Replacement == rule.APIVersion {
			t.Errorf("%s: replacement is the deprecated version", name)
		}
	}
	if len(seen) == 0 {
		t.Fatal("the rules table is empty")
	}
}

func TestRulesReturnsACopy(t *testing.T) {
	// Arrange
	rules := Rules()
	first := rules[0]

	// Act
	rules[0] = Rule{}

	// Assert
	if got := Rules()[0]; got != first {
		t.Errorf("Rules()[0] = %+v after the caller changed its copy, want %+v", got, first)
	}
	if _, ok := Match(first.APIVersion, first.Kind); !ok {
		t.Errorf("Match(%q, %q) no longer matches", first.APIVersion, first.Kind)
	}
}

func TestMatch(t *testing.T) {
	tests := []struct {
		name            string
		apiVersion      string
		kind            string
		want            bool
		wantReplacement string
		wantRemovedIn   Version
	}{
		{name: "deployment of extensions moves to apps/v1", apiVersion: "extensions/v1beta1", kind: "Deployment", want: true, wantReplacement: "apps/v1", wantRemovedIn: 16},
		{name: "ingress of networking v1beta1 was removed in 1.22", apiVersion: "networking.k8s.io/v1beta1", kind: "Ingress", want: true, wantReplacement: "networking.k8s.io/v1", wantRemovedIn: 22},
		{name: "pod security policy has no replacement", apiVersion: "policy/v1beta1", kind: "PodSecurityPolicy", want: true, wantReplacement: "", wantRemovedIn: 25},
		{name: "flow schema v1beta3 was removed in 1.32", apiVersion: "flowcontrol.apiserver.k8s.io/v1beta3", kind: "FlowSchema", want: true, wantReplacement: "flowcontrol.apiserver.k8s.io/v1", wantRemovedIn: 32},
		{name: "a served version does not match", apiVersion: "apps/v1", kind: "Deployment", want: false},
		{name: "a deprecated version of another kind does not match", apiVersion: "extensions/v1beta1", kind: "Pod", want: false},
		{name: "kind is compared exactly", apiVersion: "extensions/v1beta1", kind: "deployment", want: false},
		{name: "empty input does not match", apiVersion: "", kind: "", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rule, ok := Match(tt.apiVersion, tt.kind)

			if ok != tt.want {
				t.Fatalf("Match(%q, %q) matched = %v, want %v", tt.apiVersion, tt.kind, ok, tt.want)
			}
			if !ok {
				return
			}
			if rule.Replacement != tt.wantReplacement || rule.RemovedIn != tt.wantRemovedIn {
				t.Errorf("rule = %+v, want replacement %q removed in %s", rule, tt.wantReplacement, tt.wantRemovedIn)
			}
		})
	}
}

func TestServesRuleKind(t *testing.T) {
	tests := []struct {
		name  string
		group string
		kind  string
		want  bool
	}{
		{name: "the group a kind moved to", group: "networking.k8s.io", kind: "Ingress", want: true},
		{name: "the group a kind moved from", group: "extensions", kind: "Ingress", want: true},
		{name: "a kind removed without replacement, in its last group", group: "policy", kind: "PodSecurityPolicy", want: true},
		{name: "a kind with a rule in an unrelated group", group: "example.com", kind: "Ingress", want: false},
		{name: "a kind without a rule", group: "apps", kind: "ControllerRevision", want: false},
		{name: "the core group", group: "", kind: "Event", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := servesRuleKind(tt.group, tt.kind); got != tt.want {
				t.Errorf("servesRuleKind(%q, %q) = %v, want %v", tt.group, tt.kind, got, tt.want)
			}
		})
	}
}

func TestGroupOf(t *testing.T) {
	if got := groupOf("apps/v1"); got != "apps" {
		t.Errorf(`groupOf("apps/v1") = %q, want "apps"`, got)
	}
	if got := groupOf("v1"); got != "" {
		t.Errorf(`groupOf("v1") = %q, want the empty core group`, got)
	}
}
