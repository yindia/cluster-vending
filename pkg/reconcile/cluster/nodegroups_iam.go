package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// karpenterIAMWait says what of Karpenter's IAM is not ready for the node
// classes, "" when everything is: the node role, its four attachments and
// its EC2_LINUX access entry are Ready, and Karpenter's controller policy
// as applied (the live Policy, Ready) allows iam:PassRole on the node
// role. A node class applied earlier makes Karpenter call
// AddRoleToInstanceProfile, which is denied until the policy has caught
// up. It reads a handful of the cluster's own objects, never a list.
func (s *Reconciler) karpenterIAMWait(ctx context.Context, namespace, prefix, accountID, region string) (string, error) {
	objects := crossplaneaws.KarpenterNodeIAMObjects(namespace, prefix)
	refs := make([]managed.Ref, 0, len(objects))
	for _, o := range objects {
		ref, err := managed.RefOf(o, s.deps.Kube.Scheme())
		if err != nil {
			return "", err
		}
		refs = append(refs, ref)
	}
	views, err := s.deps.Objects().Observe(ctx, refs)
	if err != nil {
		return "", err
	}
	var waits []string
	notReady := map[string][]string{}
	var kinds []string
	for _, v := range views {
		if st := v.GetState(); st != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY &&
			st != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_UPDATING {
			if _, seen := notReady[v.GetKind()]; !seen {
				kinds = append(kinds, v.GetKind())
			}
			notReady[v.GetKind()] = append(notReady[v.GetKind()], v.GetName())
		}
	}
	for _, kind := range kinds {
		waits = append(waits, fmt.Sprintf("%s %s not Ready", kind, strings.Join(notReady[kind], ", ")))
	}
	roleARN := "arn:" + crossplaneaws.Partition(region) + ":iam::" + accountID + ":role/" + crossplaneaws.KarpenterNodeRoleName(prefix)
	policyName := crossplaneaws.ComponentPolicyName(crossplaneaws.ComponentIAMPlan{ClusterPrefix: prefix, Component: karpenterComponent})
	if w, err := s.policyWait(ctx, namespace, policyName, roleARN); err != nil {
		return "", err
	} else if w != "" {
		waits = append(waits, w)
	}
	return strings.Join(waits, "; "), nil
}

// policyWait says why the live Policy does not yet allow iam:PassRole on
// roleARN; "" when it does.
func (s *Reconciler) policyWait(ctx context.Context, namespace, name, roleARN string) (string, error) {
	policy := &awsiam.Policy{}
	if err := s.deps.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, policy); err != nil {
		if client.IgnoreNotFound(err) == nil {
			return "Policy " + name + " does not exist yet", nil
		}
		return "", err
	}
	ref, err := managed.RefOf(policy, s.deps.Kube.Scheme())
	if err != nil {
		return "", err
	}
	views, err := s.deps.Objects().Observe(ctx, []managed.Ref{ref})
	if err != nil {
		return "", err
	}
	if st := views[0].GetState(); st != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_READY &&
		st != cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_UPDATING {
		return "Policy " + name + " not Ready", nil
	}
	// What AWS has (atProvider) is what is applied; a provider that has not
	// reported it yet leaves the spec, once the provider saw its latest
	// generation.
	document := ptr.Deref(policy.Status.AtProvider.Policy, "")
	if document == "" {
		if seen := views[0].GetObservedGeneration(); seen != 0 && seen < views[0].GetGeneration() {
			return "Policy " + name + " has not applied its latest document", nil
		}
		document = ptr.Deref(policy.Spec.ForProvider.Policy, "")
	}
	if !allowsPassRole(document, roleARN) {
		return "Policy " + name + " does not allow iam:PassRole on " + roleARN + " yet", nil
	}
	return "", nil
}

// allowsPassRole reports whether an IAM policy document has an Allow
// statement for iam:PassRole on roleARN (directly or through a wildcard).
func allowsPassRole(document, roleARN string) bool {
	if strings.HasPrefix(document, "%7B") { // AWS returns documents URL-encoded
		if decoded, err := url.QueryUnescape(document); err == nil {
			document = decoded
		}
	}
	var doc struct {
		Statement json.RawMessage `json:"Statement"`
	}
	if json.Unmarshal([]byte(document), &doc) != nil {
		return false
	}
	var statements []struct {
		Effect   string          `json:"Effect"`
		Action   json.RawMessage `json:"Action"`
		Resource json.RawMessage `json:"Resource"`
	}
	if json.Unmarshal(doc.Statement, &statements) != nil {
		var one struct {
			Effect   string          `json:"Effect"`
			Action   json.RawMessage `json:"Action"`
			Resource json.RawMessage `json:"Resource"`
		}
		if json.Unmarshal(doc.Statement, &one) != nil {
			return false
		}
		statements = append(statements, one)
	}
	for _, st := range statements {
		if st.Effect == "Allow" && anyMatches(stringsOf(st.Action), "iam:PassRole") && anyMatches(stringsOf(st.Resource), roleARN) {
			return true
		}
	}
	return false
}

// stringsOf reads a JSON string or list of strings.
func stringsOf(raw json.RawMessage) []string {
	var one string
	if json.Unmarshal(raw, &one) == nil {
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(raw, &many)
	return many
}

// anyMatches reports whether one of the patterns (IAM wildcards: a "*"
// matches any run) matches s, case-insensitively for actions' sake only
// where they differ in case.
func anyMatches(patterns []string, s string) bool {
	for _, p := range patterns {
		if wildcardMatch(strings.ToLower(p), strings.ToLower(s)) {
			return true
		}
	}
	return false
}

func wildcardMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == s
	}
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for _, part := range parts[1 : len(parts)-1] {
		i := strings.Index(s, part)
		if i < 0 {
			return false
		}
		s = s[i+len(part):]
	}
	return strings.HasSuffix(s, parts[len(parts)-1])
}
