package aws

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"time"

	protectionv1beta1 "github.com/crossplane/crossplane/apis/v2/protection/v1beta1"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	awsiam "github.com/upbound/provider-aws/v2/apis/namespaced/iam/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane"
	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
	"github.com/yindia/cluster-vending/pkg/helmrel"
)

// HelmReleaseGVK is provider-helm's namespaced Release
// (crossplane.HelmProviderVersion, helm.m.crossplane.io/v1beta1): a chart
// installed into a workload cluster through the cluster's Helm
// ProviderConfig. Unstructured, like the ProviderConfig
// (HelmProviderConfigGVK); every field written here is one of that
// version's CustomResourceDefinition (TestHelmReleaseFields pins them):
//
//	metadata.annotations["crossplane.io/external-name"]  the Helm release name
//	spec.providerConfigRef.{kind,name}                    the cluster's ProviderConfig
//	spec.rollbackLimit                                    retries of a failed install or upgrade
//	spec.forProvider.chart.{repository,name,version}
//	spec.forProvider.namespace                            created unless skipCreateNamespace
//	spec.forProvider.values                               free-form object
//	spec.forProvider.wait, waitTimeout, maxHistory        wait for the rollout; how long; revisions kept
//	spec.forProvider.skipCRDs
//	spec.forProvider.insecureSkipTLSVerify, plainHTTP
//	status.atProvider.state                               "deployed" once Helm installed it
var HelmReleaseGVK = schema.GroupVersionKind{Group: "helm.m.crossplane.io", Version: "v1beta1", Kind: "Release"}

const (
	// HelmDeployed is a Release's status.atProvider.state once Helm
	// installed or upgraded it.
	HelmDeployed = "deployed"
	// helmRollbackLimit is how often provider-helm retries a failed
	// install or upgrade (by rolling back) before it gives the release up:
	// large, a chart that cannot install yet (its nodes are not there) is
	// retried, not abandoned.
	helmRollbackLimit = 100
	// DefaultHelmTimeout is how long Helm waits for a chart to roll out when
	// the definition sets no install.timeout.
	DefaultHelmTimeout = 10 * time.Minute
	// helmMaxHistory is how many revisions Helm keeps per release: a failed
	// attempt is one revision per backoff step, so a few are enough to roll
	// back to and to read.
	helmMaxHistory = 5
	// maxHelmObjectName bounds a Release object's name (a DNS label).
	maxHelmObjectName = 63
	// crdsReleaseSuffix ends the Helm release name of a component's chart
	// of CustomResourceDefinitions.
	crdsReleaseSuffix = "-crds"
)

var helmReleaseKind = UsageRef{APIVersion: HelmReleaseGVK.GroupVersion().String(), Kind: HelmReleaseGVK.Kind}

// HelmChart is a chart at a version.
type HelmChart struct {
	// Repository is an https:// repository URL or an oci:// registry path.
	Repository, Name, Version        string
	InsecureSkipTLSVerify, PlainHTTP bool
}

// HelmReleasePlan is one chart installed into the workload cluster.
type HelmReleasePlan struct {
	// Name is the Helm release name in the workload cluster.
	Name  string
	Chart HelmChart
	// Values are the chart's values, final: placeholders filled in.
	Values map[string]any
	// SkipCRDs leaves the chart's own crds/ directory out (a chart of the
	// CustomResourceDefinitions is installed before it).
	SkipCRDs bool
	// Timeout is how long Helm waits for the chart's workloads to roll out
	// (the definition's install.timeout); 0: DefaultHelmTimeout.
	Timeout time.Duration
}

// timeout is the plan's wait timeout, defaulted.
func (r HelmReleasePlan) timeout() time.Duration {
	if r.Timeout <= 0 {
		return DefaultHelmTimeout
	}
	return r.Timeout
}

// Digest is the digest of the Release the plan renders (helmrel.Digest):
// what decides whether the live Release is written again.
func (r HelmReleasePlan) Digest() string {
	return helmrel.Digest(helmrel.Spec{Repository: r.Chart.Repository, Chart: r.Chart.Name, Version: r.Chart.Version,
		Values: r.Values, SkipCRDs: r.SkipCRDs, Timeout: r.timeout()})
}

// PlatformComponentPlan is one Platform component on one cluster,
// resolved: versions from the cluster's release, values final.
type PlatformComponentPlan struct {
	// Name is the catalog name; Namespace where its charts install.
	Name, Namespace string
	// Requires are the components (of the same plan) installed before it.
	Requires []string
	// Sequence: the component starts once every component of a lower
	// sequence is deployed and Ready.
	Sequence int32
	// IAM is the component's role; nil for none.
	IAM *ComponentIAMPlan
	// CRDs is the chart of its CustomResourceDefinitions, installed before
	// Main; nil for none.
	CRDs *HelmReleasePlan
	Main HelmReleasePlan
	// Companions are installed after Main, in order.
	Companions []HelmReleasePlan
	// WaitsFor, when set, says what the component's values still need (a
	// value that does not exist yet): its IAM role is applied, its charts
	// are held back.
	WaitsFor string
}

// PlatformPlan are the Platform components of one cluster, in install
// order (pkg/component.PlatformComponents).
type PlatformPlan struct {
	// The cluster's: tenant namespace, IDs and prefix (ClusterPlan.Prefix).
	Namespace, TenantID, ClusterID, ClusterPrefix string
	// NodeGroupName is the object name of the cluster's node group: no
	// chart is installed before it is Ready (there is nowhere to run it).
	// Empty: not waited for.
	NodeGroupName string
	Components    []PlatformComponentPlan
	// External are components a component of the plan requires that are
	// installed by another plan of the same cluster (a tenant component
	// that requires a platform component): by name, the last Release
	// object of each. A component of this plan waits for it, and a Usage
	// keeps it until this plan's component is gone.
	External map[string]string
	// ReleaseGate, when set, decides for each Release whether it is written
	// now (the digest differs from the live one, nothing else of the
	// cluster is installing). Nil: every Release is applied.
	ReleaseGate func(ctx context.Context, release *unstructured.Unstructured) sequence.Decision
}

// HelmReleaseObjectName is the name of the Release object of a Helm
// release of a cluster: "<cluster prefix>-<release>", cut and suffixed
// with a hash when longer than a DNS label.
func HelmReleaseObjectName(clusterPrefix, release string) string {
	name := clusterPrefix + "-" + release
	if len(name) <= maxHelmObjectName {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return strings.TrimRight(name[:maxHelmObjectName-9], "-.") + "-" + hex.EncodeToString(sum[:])[:8]
}

// CRDsReleaseName is the Helm release name of a component's chart of
// CustomResourceDefinitions.
func CRDsReleaseName(component string) string { return component + crdsReleaseSuffix }

// releases are the component's Helm releases in install order: the CRD
// chart, the main chart, the companions.
func (c PlatformComponentPlan) releases() []HelmReleasePlan {
	var out []HelmReleasePlan
	if c.CRDs != nil {
		out = append(out, *c.CRDs)
	}
	out = append(out, c.Main)
	return append(out, c.Companions...)
}

// ReleaseObjectNames are the names of the component's Release objects, in
// install order.
func (p PlatformPlan) ReleaseObjectNames(c PlatformComponentPlan) []string {
	releases := c.releases()
	out := make([]string, 0, len(releases))
	for _, r := range releases {
		out = append(out, HelmReleaseObjectName(p.ClusterPrefix, r.Name))
	}
	return out
}

// ReleaseDigests are the spec digests the plan renders for the component's
// Release objects, by object name: what a Release must have to be current.
func (p PlatformPlan) ReleaseDigests(c PlatformComponentPlan) map[string]string {
	out := map[string]string{}
	for _, r := range c.releases() {
		out[HelmReleaseObjectName(p.ClusterPrefix, r.Name)] = r.Digest()
	}
	return out
}

// helmRelease renders one Release of component into namespace.
func (p PlatformPlan) helmRelease(component, namespace string, r HelmReleasePlan) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(HelmReleaseGVK)
	u.SetNamespace(p.Namespace)
	u.SetName(HelmReleaseObjectName(p.ClusterPrefix, r.Name))
	u.SetLabels(crossplane.ComponentLabels(p.TenantID, p.ClusterID, component))
	u.SetAnnotations(map[string]string{ExternalNameAnnotation: r.Name, helmrel.DigestAnnotation: r.Digest()})
	chart := map[string]any{"repository": r.Chart.Repository, "name": r.Chart.Name, "version": r.Chart.Version}
	// wait: Ready means the workloads rolled out, not that manifests were
	// written; waitTimeout is how long that may take.
	forProvider := map[string]any{"chart": chart, "namespace": namespace,
		"wait": true, "waitTimeout": r.timeout().String(), "maxHistory": int64(helmMaxHistory)}
	if len(r.Values) > 0 {
		forProvider["values"] = runtime.DeepCopyJSON(r.Values)
	}
	if r.SkipCRDs {
		forProvider["skipCRDs"] = true
	}
	if r.Chart.InsecureSkipTLSVerify {
		forProvider["insecureSkipTLSVerify"] = true
	}
	if r.Chart.PlainHTTP {
		forProvider["plainHTTP"] = true
	}
	u.Object["spec"] = map[string]any{
		"providerConfigRef": map[string]any{"kind": HelmProviderConfigGVK.Kind, "name": HelmProviderConfigName(p.ClusterPrefix)},
		"rollbackLimit":     int64(helmRollbackLimit),
		"forProvider":       forProvider,
	}
	return u
}

// named is an empty Release with the object name, to wait for.
func (p PlatformPlan) named(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(HelmReleaseGVK)
	u.SetNamespace(p.Namespace)
	u.SetName(name)
	return u
}

// installed is what waits for the Release name: it is Ready and Helm
// reports it deployed. provider-helm reports a release Ready as soon as
// Helm has applied the chart, before its workloads run; whether they do
// is not checked here (later).
func (p PlatformPlan) installed(name string) []sequence.Condition {
	r := p.named(name)
	return []sequence.Condition{sequence.Ready(r), sequence.Field(r, HelmDeployed, "status", "atProvider", "state")}
}

// PlatformSet is the plan as a set to apply in order. Every chart waits
// until the cluster can be reached and has nodes: its Helm ProviderConfig
// exists (the kubeconfig) and its node group is Ready. Then:
//
//   - a component's IAM role first (ComponentIAMSet);
//   - its chart of CustomResourceDefinitions, then its chart (once the
//     role's policies are attached and the CRD chart is deployed), then
//     its companions, each once the one before is deployed;
//   - a component after the last chart of every component it requires,
//     one of another plan included (External);
//   - a component whose values wait (WaitsFor) keeps its charts back and
//     says what for; a component after it waits with it.
//
// Usages delete in reverse: a chart is uninstalled before the chart it
// was installed after, a component before the components it requires, and
// a component's charts before its role loses its policies.
func PlatformSet(p PlatformPlan) sequence.Set {
	var set sequence.Set
	config := &unstructured.Unstructured{}
	config.SetGroupVersionKind(HelmProviderConfigGVK)
	config.SetNamespace(p.Namespace)
	config.SetName(HelmProviderConfigName(p.ClusterPrefix))
	reachable := []sequence.Condition{sequence.Ready(config)}
	if p.NodeGroupName != "" {
		reachable = append(reachable, sequence.Ready(&awseks.NodeGroup{
			ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: p.NodeGroupName}}))
	}
	last := map[string]string{}    // component -> its last Release object
	waiting := map[string]string{} // Release object -> what its values wait for
	var pairs []UsagePair
	for _, c := range p.Components {
		after := slices.Clone(reachable)
		var users []string // Release objects installed before this component's first
		for _, lower := range p.Components {
			if lower.Sequence >= c.Sequence {
				continue
			}
			if name, ok := last[lower.Name]; ok {
				after = append(after, p.installed(name)...)
				users = append(users, name)
			}
		}
		for _, req := range c.Requires {
			name, ok := last[req]
			if !ok {
				name, ok = p.External[req]
			}
			if ok && !slices.Contains(users, name) {
				after = append(after, p.installed(name)...)
				users = append(users, name)
			}
		}
		var attachments []string
		if c.IAM != nil {
			set = append(set, ComponentIAMSet(*c.IAM)...)
			attachments = c.IAM.attachmentNames()
			for _, a := range attachments {
				after = append(after, sequence.Ready(&awsiam.RolePolicyAttachment{
					ObjectMeta: metav1.ObjectMeta{Namespace: p.Namespace, Name: a}}))
			}
		}
		previous := ""
		for _, r := range c.releases() {
			obj := p.helmRelease(c.Name, c.Namespace, r)
			item := sequence.Item{Object: obj, After: after}
			if gate := p.ReleaseGate; gate != nil {
				item.Gate = func(ctx context.Context, o client.Object) sequence.Decision {
					return gate(ctx, o.(*unstructured.Unstructured))
				}
			}
			if c.WaitsFor != "" {
				item.Build = waits(c.WaitsFor)
				waiting[obj.GetName()] = c.WaitsFor
			}
			set = append(set, item)
			if previous == "" {
				for _, a := range attachments {
					pairs = append(pairs, UsagePair{Of: attachmentKind.named(a), By: helmReleaseKind.named(obj.GetName())})
				}
				for _, u := range users {
					pairs = append(pairs, UsagePair{Of: helmReleaseKind.named(u), By: helmReleaseKind.named(obj.GetName())})
				}
			} else {
				pairs = append(pairs, UsagePair{Of: helmReleaseKind.named(previous), By: helmReleaseKind.named(obj.GetName())})
			}
			previous = obj.GetName()
			after = append(slices.Clone(reachable), p.installed(previous)...)
		}
		last[c.Name] = previous
	}
	for _, u := range Usages(p.Namespace, p.ClusterPrefix, crossplane.ClusterLabels(p.TenantID, p.ClusterID), pairs) {
		set = append(set, sequence.Item{Object: u})
	}
	set = usagesAfterTheirUsers(set, nil)
	// A Usage of a chart that is held back is held back with it: its user
	// does not exist yet.
	for i, item := range set {
		u, ok := item.Object.(*protectionv1beta1.Usage)
		if !ok || u.Spec.By == nil || u.Spec.By.ResourceRef == nil || u.Spec.By.Kind != HelmReleaseGVK.Kind {
			continue
		}
		if reason, held := waiting[u.Spec.By.ResourceRef.Name]; held {
			set[i].Build = waits(reason)
		}
	}
	return set
}

// waits is the Build of an object that cannot be rendered yet.
func waits(reason string) func(context.Context, sequence.Observed) (client.Object, string, error) {
	return func(context.Context, sequence.Observed) (client.Object, string, error) { return nil, reason, nil }
}

// PlatformDeleteOrder is the order a cluster's platform components are
// deleted in: every object at once; the Usages of PlatformSet order them.
var PlatformDeleteOrder = sequence.DeleteOrder{}
