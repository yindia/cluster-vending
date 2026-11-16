package catalog_test

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/yindia/cluster-vending/pkg/component"
)

// This fixture freezes the catalog's upstream-derived v1.15.1 permission
// baseline before Sid removal: the five controller policies combined, the
// interruption/SQS policy omitted, and platform placeholders substituted for
// CloudFormation parameters. Compare entire statements, not selected actions.
func karpenterPolicyBaseline(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/karpenter-v1.15.1-controller-policy.json")
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func karpenterPolicy(t *testing.T) string {
	t.Helper()
	return byName(t)["karpenter"].Spec.CloudResources.IAMRole.Policy
}

func withoutStatementSIDs(t *testing.T, policy string) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal([]byte(policy), &doc); err != nil {
		t.Fatal(err)
	}
	statements, ok := doc["Statement"].([]any)
	if !ok || len(statements) != 17 {
		t.Fatalf("expected the 17 controller statements, got %v", doc["Statement"])
	}
	for _, statement := range statements {
		delete(statement.(map[string]any), "Sid")
	}
	return doc
}

func TestKarpenterPolicyPreservesEveryPermissionWithoutSIDs(t *testing.T) {
	policy := karpenterPolicy(t)
	var shipped map[string]any
	if err := json.Unmarshal([]byte(policy), &shipped); err != nil {
		t.Fatal(err)
	}
	for _, statement := range shipped["Statement"].([]any) {
		if _, present := statement.(map[string]any)["Sid"]; present {
			t.Fatal("optional Sid metadata returned to the size-constrained policy")
		}
	}
	want := withoutStatementSIDs(t, karpenterPolicyBaseline(t))
	if !reflect.DeepEqual(shipped, want) {
		t.Fatal("Karpenter policy permissions differ from the pre-change upstream-derived baseline")
	}
}

func TestExpandedKarpenterPolicyFitsIAMManagedPolicyBudget(t *testing.T) {
	for _, name := range []string{
		"inference-ap-northeast-1-dev",
		// Cluster.name permits at most 40 characters. Also budget a maximum
		// 64-character IAM role name, including imported node-role names.
		"a" + strings.Repeat("b", 39),
	} {
		t.Run(name, func(t *testing.T) {
			vars := component.Vars{
				component.VarRegion:             "ap-northeast-1",
				component.VarAccountID:          "111122223333",
				component.VarClusterName:        name,
				component.VarClusterNodeRoleARN: "arn:aws:iam::111122223333:role/" + strings.Repeat("r", 64),
			}
			policy, err := component.ExpandPolicy(karpenterPolicy(t), vars)
			if err != nil {
				t.Fatal(err)
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, []byte(policy)); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(compact.String(), "${") {
				t.Fatal("policy still contains unresolved placeholders")
			}
			if size := utf8.RuneCount(compact.Bytes()); size >= component.MaxPolicySize {
				t.Fatalf("expanded policy has %d characters, must stay below %d", size, component.MaxPolicySize)
			} else {
				t.Logf("expanded policy: %d characters; %d below IAM's limit", size, component.MaxPolicySize-size)
			}
			baseline, err := component.ExpandPolicy(karpenterPolicyBaseline(t), vars)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(withoutStatementSIDs(t, policy), withoutStatementSIDs(t, baseline)) {
				t.Fatal("placeholder expansion changed permission semantics")
			}
			// Reproduce why a realistic policy with the optional Sids breaks:
			// this test must fail if a later edit silently weakens the fixture.
			if len(baseline) <= component.MaxPolicySize {
				t.Fatalf("baseline no longer reproduces the expanded size failure: %d", len(baseline))
			}
		})
	}
}
