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
// baseline before Sid removal: the five controller policies combined plus
// the interruption (SQS) policy, and platform placeholders substituted for
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
	if !ok || len(statements) != 18 {
		t.Fatalf("expected the 18 controller statements, got %v", doc["Statement"])
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
				component.VarRegion:               "ap-northeast-1",
				component.VarAccountID:            "111122223333",
				component.VarClusterName:          name,
				component.VarKarpenterNodeRoleARN: "arn:aws:iam::111122223333:role/" + strings.Repeat("r", 64),
				// A 40-character cluster name makes the longest queue name:
				// "<name>-<hash8>-karpenter".
				component.VarKarpenterInterruptionQueue: name + "-12345678-karpenter",
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
			if _, err := component.ExpandPolicy(karpenterPolicyBaseline(t), vars); err == nil || !strings.Contains(err.Error(), "expanded IAM policy") {
				t.Fatalf("oversized upstream baseline was not rejected before rendering: %v", err)
			}
			// Expand the trusted test fixture without the production budget guard
			// solely to compare permissions with the smaller deployed policy.
			var baselineDocument any
			if err := json.Unmarshal([]byte(karpenterPolicyBaseline(t)), &baselineDocument); err != nil {
				t.Fatal(err)
			}
			expanded, err := component.Expand(baselineDocument, vars)
			if err != nil {
				t.Fatal(err)
			}
			baselineBytes, err := json.Marshal(expanded)
			if err != nil {
				t.Fatal(err)
			}
			baseline := string(baselineBytes)
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

// The controller may read the interruption queue it is given, nothing
// else of SQS, and pass only the Karpenter node role (not the managed node
// group's) to EC2.
func TestKarpenterPolicyScopesSQSAndPassRole(t *testing.T) {
	var doc struct {
		Statement []struct {
			Effect   string
			Action   any
			Resource any
		}
	}
	if err := json.Unmarshal([]byte(karpenterPolicy(t)), &doc); err != nil {
		t.Fatal(err)
	}
	var sqs, passRole int
	for _, st := range doc.Statement {
		actions, _ := json.Marshal(st.Action)
		resource, _ := json.Marshal(st.Resource)
		switch {
		case strings.Contains(string(actions), "sqs:"):
			sqs++
			if string(actions) != `["sqs:DeleteMessage","sqs:GetQueueUrl","sqs:ReceiveMessage"]` ||
				string(resource) != `"arn:aws:sqs:${region}:${account.id}:${karpenter.interruptionQueue}"` {
				t.Errorf("SQS statement = %s on %s", actions, resource)
			}
		case strings.Contains(string(actions), "iam:PassRole"):
			passRole++
			if string(resource) != `"${karpenter.nodeRoleArn}"` {
				t.Errorf("iam:PassRole on %s, want the karpenter node role", resource)
			}
		}
	}
	if sqs != 1 || passRole != 1 {
		t.Errorf("%d SQS and %d PassRole statements, want one of each", sqs, passRole)
	}
	if strings.Contains(karpenterPolicy(t), "${cluster.nodeRoleArn}") {
		t.Error("the policy still names the managed node group's role")
	}
}
