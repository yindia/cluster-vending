package component

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/fault"
)

// Placeholders are what the platform team writes in a definition's
// platform values and IAM policy as ${name}, and the controller fills in
// per cluster before it installs the component. The list is fixed: each
// name has one source, and a definition naming any other is invalid.
// Tenant values are taken literally and never expanded.
//
// The functions here only read the kinds they are given (Cluster,
// Network, CloudAccount): no client, no store.
const (
	VarRegion                 = "region"
	VarAccountID              = "account.id"
	VarClusterName            = "cluster.name"
	VarClusterEndpoint        = "cluster.endpoint"
	VarClusterOIDCIssuer      = "cluster.oidcIssuer"
	VarClusterOIDCProviderARN = "cluster.oidcProviderArn"
	VarClusterNodeRoleName    = "cluster.nodeRoleName"
	VarClusterNodeRoleARN     = "cluster.nodeRoleArn"
	VarNetworkVPCID           = "network.vpcId"
	VarIAMRoleARN             = "iam.roleArn"
	VarIAMRoleName            = "iam.roleName"
	VarIAMPolicyARN           = "iam.policyArn"

	// VarSystemNodeSelector and VarSystemTolerations are whole-value
	// placeholders: a string that is exactly ${system.nodeSelector} becomes
	// the default node pool's labels (a map), ${system.tolerations} a list
	// tolerating each of its taints ({key, operator: Equal, value,
	// effect}). Platform components use them to run on the default node
	// pool, which tenant workloads stay off. Only in platform values.
	VarSystemNodeSelector = "system.nodeSelector"
	VarSystemTolerations  = "system.tolerations"
)

// systemPrefix starts the whole-value placeholders.
const systemPrefix = "system."

// wholeValue reports whether name is replaced by a structured value and
// may only be a field's entire value.
func wholeValue(name string) bool { return strings.HasPrefix(name, systemPrefix) }

// iamPrefix starts the placeholders of the component's own role and
// policy: not available in that policy.
const iamPrefix = "iam."

// placeholderNames is every name, in the order the docs list them.
var placeholderNames = []string{
	VarRegion, VarAccountID, VarClusterName, VarClusterEndpoint, VarClusterOIDCIssuer, VarClusterOIDCProviderARN,
	VarClusterNodeRoleName, VarClusterNodeRoleARN, VarNetworkVPCID, VarIAMRoleARN, VarIAMRoleName, VarIAMPolicyARN,
	VarSystemNodeSelector, VarSystemTolerations,
}

// PlaceholderNames is every placeholder name, in a fixed order.
func PlaceholderNames() []string { return slices.Clone(placeholderNames) }

// ErrNotReady is what Expand returns, wrapped, when a placeholder is
// known but has no value yet (the cluster's endpoint before it is up, the
// role before it exists): the caller waits and tries again, it never
// installs a blank.
var ErrNotReady = errors.New("placeholder values are not known yet")

// Vars are placeholder values by name. An empty or missing value is not
// known yet. A key "why:<name>" may say why a value is missing. The
// whole-value placeholders (system.*) hold their value as JSON.
type Vars map[string]string

// whyKey is the key of the reason a value is missing.
func whyKey(name string) string { return "why:" + name }

// IAMOutputs are the IAM names the controller reads from the management
// cluster: the component's role and policy (empty until they exist), and
// the IAM name of the cluster's node role (crossplaneaws.NodeRoleName:
// the name an import recorded, or the platform's).
type IAMOutputs struct {
	RoleARN, RoleName, PolicyARN string
	NodeRoleName                 string
}

// VarsFor are the placeholder values of a component on cluster, from the
// cluster, its network and account and the IAM outputs; what is not known
// yet is left empty. A nil network or account leaves their values out.
func VarsFor(cluster *cloudv1alpha1.Cluster, network *cloudv1alpha1.Network, account *cloudv1alpha1.CloudAccount, iam IAMOutputs) Vars {
	v := Vars{
		VarRegion: cluster.Spec.Region, VarClusterName: cluster.Name,
		VarClusterOIDCIssuer: cluster.Status.OIDCIssuerURL,
		VarIAMRoleARN:        iam.RoleARN, VarIAMRoleName: iam.RoleName, VarIAMPolicyARN: iam.PolicyARN,
		VarClusterNodeRoleName: iam.NodeRoleName,
		VarSystemNodeSelector:  systemNodeSelector(cluster.Spec.DefaultNodePool),
		VarSystemTolerations:   systemTolerations(cluster.Spec.DefaultNodePool),
	}
	if cluster.Status.Endpoint != nil {
		v[VarClusterEndpoint] = cluster.Status.Endpoint.URL
	}
	if network != nil && network.Status.Resources != nil {
		v[VarNetworkVPCID] = network.Status.Resources.CloudNetworkID
	}
	if account != nil && account.Spec.AWS != nil {
		v[VarAccountID] = account.Spec.AWS.AccountID
	}
	arn := func(resource string) string {
		if v[VarAccountID] == "" || v[VarRegion] == "" {
			return ""
		}
		return "arn:" + crossplaneaws.Partition(v[VarRegion]) + ":iam::" + v[VarAccountID] + ":" + resource
	}
	if iam.NodeRoleName != "" {
		v[VarClusterNodeRoleARN] = arn("role/" + iam.NodeRoleName)
	}
	switch {
	case !clusterIRSA(cluster):
		v[whyKey(VarClusterOIDCProviderARN)] = fmt.Sprintf("IRSA is off on cluster %s (spec.aws.irsa)", cluster.Name)
	case cluster.Status.OIDCIssuerURL != "":
		v[VarClusterOIDCProviderARN] = arn("oidc-provider/" + strings.TrimPrefix(cluster.Status.OIDCIssuerURL, "https://"))
	}
	return v
}

// systemNodeSelector is the default pool's labels as JSON; "{}" for none.
func systemNodeSelector(pool *cloudv1alpha1.NodePoolSpec) string {
	labels := map[string]string{}
	if pool != nil {
		for k, v := range pool.Labels {
			labels[k] = v
		}
	}
	return mustJSON(labels)
}

// systemTolerations are tolerations of each of the default pool's taints,
// as JSON; "[]" for none.
func systemTolerations(pool *cloudv1alpha1.NodePoolSpec) string {
	out := []map[string]string{}
	if pool != nil {
		for _, t := range pool.Taints {
			out = append(out, map[string]string{"key": t.Key, "operator": "Equal", "value": t.Value, "effect": string(t.Effect)})
		}
	}
	return mustJSON(out)
}

// mustJSON encodes maps and lists of strings, which cannot fail.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("encode %T: %v", v, err))
	}
	return string(b)
}

// clusterIRSA reports whether the cluster has its IAM OIDC provider.
func clusterIRSA(c *cloudv1alpha1.Cluster) bool {
	return c.Spec.AWS != nil && c.Spec.AWS.IRSA != nil && *c.Spec.AWS.IRSA
}

// NotReadyError names the placeholders without a value; it is ErrNotReady.
type NotReadyError struct {
	// Missing are the names, each with why when it is known.
	Missing []string
}

func (e *NotReadyError) Error() string {
	return "waiting for " + strings.Join(e.Missing, ", ")
}

// Is makes errors.Is(err, ErrNotReady) true.
func (e *NotReadyError) Is(target error) bool { return target == ErrNotReady }

// scan splits s into literal text and placeholders: fn is called for each
// placeholder name, and the result is s with each replaced by what fn
// returns. "$${" is a literal "${". An unterminated "${" is an error.
func scan(s string, fn func(name string) string) (string, error) {
	var out strings.Builder
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "$${"):
			out.WriteString("${")
			i += 3
		case strings.HasPrefix(s[i:], "${"):
			end := strings.IndexByte(s[i+2:], '}')
			if end < 0 {
				return "", fmt.Errorf("unterminated placeholder in %q", s)
			}
			out.WriteString(fn(s[i+2 : i+2+end]))
			i += 2 + end + 1
		default:
			out.WriteByte(s[i])
			i++
		}
	}
	return out.String(), nil
}

// walk calls fn on every string of doc (map values and list items, not
// keys) with its path, and returns a new document with each string
// replaced by fn's result (a string, or a map or list for a whole-value
// placeholder). doc is not changed.
func walk(doc any, path string, fn func(path, s string) (any, error)) (any, error) {
	switch d := doc.(type) {
	case string:
		return fn(path, d)
	case map[string]any:
		out := make(map[string]any, len(d))
		for k, v := range d {
			child := k
			if path != "" {
				child = path + "." + k
			}
			nv, err := walk(v, child, fn)
			if err != nil {
				return nil, err
			}
			out[k] = nv
		}
		return out, nil
	case []any:
		out := make([]any, len(d))
		for i, v := range d {
			nv, err := walk(v, fmt.Sprintf("%s[%d]", path, i), fn)
			if err != nil {
				return nil, err
			}
			out[i] = nv
		}
		return out, nil
	default:
		return doc, nil
	}
}

// Placeholders lists the placeholder names doc uses, sorted, each once;
// unknown names included. Malformed strings are skipped (Validate reports
// them).
func Placeholders(doc any) []string {
	seen := map[string]bool{}
	_, _ = walk(doc, "", func(_, s string) (any, error) {
		_, _ = scan(s, func(name string) string { seen[name] = true; return "" })
		return s, nil
	})
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// Validate checks that every placeholder in doc is on the list and well
// formed; an Invalid fault names the first that is not and its path.
func Validate(doc any) error {
	return validate(doc, false)
}

// validate checks doc's placeholders. inPolicy is the IAM policy and the
// managed policy ARNs: there the component's own iam.* names and the
// whole-value system.* names are refused.
func validate(doc any, inPolicy bool) error {
	_, err := walk(doc, "", func(path, s string) (any, error) {
		var bad string
		if _, err := scan(s, func(name string) string {
			if bad == "" && (!slices.Contains(placeholderNames, name) ||
				inPolicy && (strings.HasPrefix(name, iamPrefix) || wholeValue(name)) ||
				wholeValue(name) && s != "${"+name+"}") {
				bad = name
			}
			return ""
		}); err != nil {
			return "", fault.Newf(fault.Invalid, "%s: %v", orRoot(path), err)
		}
		switch {
		case bad == "":
			return s, nil
		case !slices.Contains(placeholderNames, bad):
			return "", fault.Newf(fault.Invalid, "%s: unknown placeholder ${%s}; known: %s", orRoot(path), bad, strings.Join(placeholderNames, ", "))
		case inPolicy && wholeValue(bad):
			return "", fault.Newf(fault.Invalid, "%s: ${%s} is only for platform values, not the IAM policy", orRoot(path), bad)
		case inPolicy:
			return "", fault.Newf(fault.Invalid, "%s: ${%s} names the component's own role or policy and cannot be used here", orRoot(path), bad)
		default:
			return "", fault.Newf(fault.Invalid, "%s: ${%s} must be the whole value of the field, not part of a string", orRoot(path), bad)
		}
	})
	return err
}

func orRoot(path string) string {
	if path == "" {
		return "(root)"
	}
	return path
}

// Expand returns a copy of doc with every placeholder replaced by its
// value. An unknown or malformed placeholder is an Invalid fault naming it
// and its path; known ones without a value are a *NotReadyError
// (ErrNotReady) naming each. doc is not changed.
func Expand(doc any, vars Vars) (any, error) {
	if err := Validate(doc); err != nil {
		return nil, err
	}
	missing := map[string]bool{}
	out, err := walk(doc, "", func(path, s string) (any, error) {
		if name, ok := wholeValueName(s); ok {
			return wholeValueOf(path, name, vars, missing)
		}
		return scan(s, func(name string) string {
			if vars[name] == "" {
				missing[name] = true
			}
			return vars[name]
		})
	})
	if err != nil {
		return nil, err
	}
	if len(missing) > 0 {
		names := make([]string, 0, len(missing))
		for name := range missing {
			entry := "${" + name + "}"
			if why := vars[whyKey(name)]; why != "" {
				entry += " (" + why + ")"
			}
			names = append(names, entry)
		}
		slices.Sort(names)
		return nil, &NotReadyError{Missing: names}
	}
	return out, nil
}

// wholeValueName is the name of a whole-value placeholder s is exactly.
func wholeValueName(s string) (string, bool) {
	inner, ok := strings.CutPrefix(s, "${")
	if !ok {
		return "", false
	}
	name, ok := strings.CutSuffix(inner, "}")
	return name, ok && wholeValue(name) && !strings.ContainsAny(name, "${}")
}

// wholeValueOf decodes the structured value of a whole-value placeholder;
// an empty one is recorded as missing.
func wholeValueOf(path, name string, vars Vars, missing map[string]bool) (any, error) {
	raw := vars[name]
	if raw == "" {
		missing[name] = true
		return nil, nil
	}
	var v any
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return nil, fmt.Errorf("%s: value of ${%s}: %w", orRoot(path), name, err)
	}
	return v, nil
}

// ExpandString is Expand of one string (a managed policy ARN).
func ExpandString(s string, vars Vars) (string, error) {
	out, err := Expand(s, vars)
	if err != nil {
		return "", err
	}
	str, ok := out.(string)
	if !ok {
		return "", fault.Newf(fault.Invalid, "%q is not a string value", s)
	}
	return str, nil
}
