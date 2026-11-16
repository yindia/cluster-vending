package component

import (
	"encoding/json"
	"fmt"
	"slices"
	"unicode"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/fault"
)

// IAMAccess is how a component's pods get its IAM role.
type IAMAccess string

// IAM access modes; empty is IRSA.
const (
	IRSA        IAMAccess = "IRSA"
	PodIdentity IAMAccess = "PodIdentity"
)

// ServiceAccount is a service account in the workload cluster.
type ServiceAccount struct{ Namespace, Name string }

// IAMRole is the role a definition asks the platform to create.
type IAMRole struct {
	Access          IAMAccess
	ServiceAccounts []ServiceAccount
	// Policy is a policy document with placeholders; empty: none.
	Policy string
	// ManagedPolicyARNs may use placeholders (account.id).
	ManagedPolicyARNs []string
}

// UsesIRSA reports whether the role trusts the cluster's OIDC provider.
func (r IAMRole) UsesIRSA() bool { return r.Access != PodIdentity }

// CloudResources is what a definition asks the platform to create in the
// tenant's account (ComponentDefinition.spec.cloudResources).
type CloudResources struct {
	// IAMRole; nil: none.
	IAMRole *IAMRole
}

// CloudResourcesOf is the kind's spec.cloudResources as the checks take
// it; nil is none. An unset access is IRSA.
func CloudResourcesOf(in *cloudv1alpha1.ComponentCloudResources) CloudResources {
	if in == nil || in.IAMRole == nil {
		return CloudResources{}
	}
	r := in.IAMRole
	role := &IAMRole{Access: IAMAccess(r.Access), Policy: r.Policy, ManagedPolicyARNs: slices.Clone(r.ManagedPolicyARNs)}
	if role.Access == "" {
		role.Access = IRSA
	}
	for _, sa := range r.ServiceAccounts {
		role.ServiceAccounts = append(role.ServiceAccounts, ServiceAccount{Namespace: sa.Namespace, Name: sa.Name})
	}
	return CloudResources{IAMRole: role}
}

// ValidateCloudResources checks what the CRD cannot: the policy is a JSON
// policy document with Version and Statement, and the policy and the
// managed policy ARNs use only known placeholders, none of the
// component's own (iam.*). An Invalid fault says what is wrong.
func ValidateCloudResources(r CloudResources) error {
	if r.IAMRole == nil {
		return nil
	}
	role := r.IAMRole
	if role.Policy == "" && len(role.ManagedPolicyARNs) == 0 {
		return fault.New(fault.Invalid, "cloudResources.iamRole needs a policy or managedPolicyArns")
	}
	if len(role.ServiceAccounts) == 0 {
		return fault.New(fault.Invalid, "cloudResources.iamRole needs at least one service account")
	}
	for i, arn := range role.ManagedPolicyARNs {
		if err := validate(arn, true); err != nil {
			return fault.Newf(fault.Invalid, "cloudResources.iamRole.managedPolicyArns[%d]: %v", i, err)
		}
	}
	if role.Policy == "" {
		return nil
	}
	if n := policySize(role.Policy); n > MaxPolicySize {
		return fault.Newf(fault.Invalid, "cloudResources.iamRole.policy has %d characters without whitespace; AWS allows %d", n, MaxPolicySize)
	}
	doc, err := parsePolicy(role.Policy)
	if err != nil {
		return err
	}
	if err := validate(doc, true); err != nil {
		return fault.Newf(fault.Invalid, "cloudResources.iamRole.policy: %v", err)
	}
	return nil
}

// MaxPolicySize is AWS's limit on a managed policy document: characters
// other than whitespace.
const MaxPolicySize = 6144

// policySize counts the characters AWS counts: all but whitespace.
func policySize(policy string) int {
	n := 0
	for _, r := range policy {
		if !unicode.IsSpace(r) {
			n++
		}
	}
	return n
}

// parsePolicy reads a policy document: a JSON object with a Version string
// and a Statement (a list or one statement).
func parsePolicy(policy string) (map[string]any, error) {
	var doc map[string]any
	if err := json.Unmarshal([]byte(policy), &doc); err != nil {
		return nil, fault.Newf(fault.Invalid, "cloudResources.iamRole.policy is not a JSON object: %v", err)
	}
	if v, ok := doc["Version"].(string); !ok || v == "" {
		return nil, fault.New(fault.Invalid, "cloudResources.iamRole.policy has no Version")
	}
	switch doc["Statement"].(type) {
	case []any, map[string]any:
	default:
		return nil, fault.New(fault.Invalid, "cloudResources.iamRole.policy has no Statement")
	}
	return doc, nil
}

// ExpandPolicy is the role's policy document with its placeholders filled
// in, as JSON; errors as Expand's.
func ExpandPolicy(policy string, vars Vars) (string, error) {
	doc, err := parsePolicy(policy)
	if err != nil {
		return "", err
	}
	if err := validate(doc, true); err != nil {
		return "", err
	}
	out, err := Expand(doc, vars)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(out)
	if err != nil {
		return "", fmt.Errorf("encode the policy: %w", err)
	}
	if n := policySize(string(b)); n > MaxPolicySize {
		return "", fault.Newf(fault.Invalid, "expanded IAM policy has %d characters without whitespace; AWS allows %d", n, MaxPolicySize)
	}
	return string(b), nil
}

// CheckCluster checks that cluster c can have the component's cloud
// resources: an IRSA role needs the cluster's IAM OIDC provider. A
// FailedPrecondition fault names the cluster setting to turn on.
func CheckCluster(component string, r CloudResources, c Cluster) error {
	if r.IAMRole != nil && r.IAMRole.UsesIRSA() && !c.IRSA {
		return fault.Newf(fault.FailedPrecondition,
			"component %s uses IRSA, which cluster %s has off: set spec.aws.irsa to true on the cluster first", component, c.Name)
	}
	return nil
}
