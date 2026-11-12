package deprecation

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/metadata"
	"k8s.io/client-go/rest"
)

const (
	// lastAppliedAnnotation is where kubectl apply keeps the object as it
	// was applied.
	lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"
	// helmReleaseSelector selects the Secrets of the releases Helm v3 has
	// deployed; superseded and failed revisions are left out.
	helmReleaseSelector = "owner=helm,status=deployed"
	// helmReleaseKey is the key of a release Secret that holds the release.
	helmReleaseKey = "release"

	// metadataPageSize is how many objects one metadata list call asks for.
	metadataPageSize = 500
	// releasePageSize is how many release Secrets one list call asks for.
	// It is small because a release Secret can be close to 1 MiB.
	releasePageSize = 20

	// requestTimeout bounds each request of ScanCluster when the config
	// sets no timeout of its own.
	requestTimeout = 30 * time.Second
	// scanUserAgent is how ScanCluster names itself to the API server.
	scanUserAgent = "cluster-vending-deprecation-scan"

	supportedMajor = "1"
	verbList       = "list"
)

// Discovery is what a scan asks a cluster about itself. A
// discovery.DiscoveryInterface of client-go satisfies it.
type Discovery interface {
	ServerVersion() (*version.Info, error)
	ServerPreferredResources() ([]*metav1.APIResourceList, error)
}

// Scanner reads a cluster. The three clients are of the same cluster.
type Scanner struct {
	Discovery Discovery
	Metadata  metadata.Interface
	Secrets   corev1client.SecretsGetter
}

// ScanCluster scans the cluster cfg reaches. cfg is not changed. It must
// carry its credentials itself: one that names an exec plugin or an auth
// provider is refused, so that a scan never runs a command the config
// chose.
func ScanCluster(ctx context.Context, cfg *rest.Config) (Result, error) {
	if cfg == nil {
		return Result{}, errors.New("deprecation scan: no cluster config")
	}
	if cfg.ExecProvider != nil || cfg.AuthProvider != nil {
		return Result{}, errors.New("deprecation scan: cluster config uses an exec plugin or auth provider; it must carry a token or client certificate")
	}
	cfg = rest.CopyConfig(cfg)
	if cfg.Timeout == 0 {
		cfg.Timeout = requestTimeout
	}
	cfg.UserAgent = scanUserAgent

	disco, err := discovery.NewDiscoveryClientForConfig(cfg)
	if err != nil {
		return Result{}, fmt.Errorf("deprecation scan: discovery client: %w", err)
	}
	meta, err := metadata.NewForConfig(cfg)
	if err != nil {
		return Result{}, fmt.Errorf("deprecation scan: metadata client: %w", err)
	}
	core, err := corev1client.NewForConfig(cfg)
	if err != nil {
		return Result{}, fmt.Errorf("deprecation scan: core client: %w", err)
	}
	return Scanner{Discovery: disco, Metadata: meta, Secrets: core}.Scan(ctx)
}

// Scan reads the cluster's version, the last-applied configuration of the
// objects whose kind has a rule, and the manifests of the deployed Helm
// releases, and returns the objects written with a deprecated version.
//
// A resource the server no longer lists and a release that cannot be
// decoded are skipped; any other failure fails the scan.
func (s Scanner) Scan(ctx context.Context) (Result, error) {
	clusterVersion, err := s.serverVersion()
	if err != nil {
		return Result{}, err
	}
	resources, err := s.resources()
	if err != nil {
		return Result{}, err
	}

	var findings []Finding
	for _, gvr := range resources {
		if err := ctx.Err(); err != nil {
			return Result{}, fmt.Errorf("deprecation scan: %w", err)
		}
		found, err := s.scanResource(ctx, gvr)
		if err != nil {
			return Result{}, err
		}
		findings = append(findings, found...)
	}
	found, err := s.scanReleases(ctx)
	if err != nil {
		return Result{}, err
	}
	findings = append(findings, found...)

	sortFindings(findings)
	return Result{Version: clusterVersion, Findings: findings}, nil
}

// serverVersion is the minor version the cluster runs.
func (s Scanner) serverVersion() (Version, error) {
	info, err := s.Discovery.ServerVersion()
	if err != nil {
		return 0, fmt.Errorf("deprecation scan: server version: %w", err)
	}
	if info == nil {
		return 0, errors.New("deprecation scan: server version: none reported")
	}
	if info.Major != supportedMajor {
		return 0, fmt.Errorf("deprecation scan: server version: major version %q is not %s", info.Major, supportedMajor)
	}
	v, err := ParseMinor(info.Minor)
	if err != nil {
		return 0, fmt.Errorf("deprecation scan: server version: %w", err)
	}
	return v, nil
}

// resources are the listable resources whose kind has a rule. A group whose
// discovery failed (an unavailable aggregated API) is left out; the rest is
// used.
func (s Scanner) resources() ([]schema.GroupVersionResource, error) {
	lists, err := s.Discovery.ServerPreferredResources()
	if err != nil && !discovery.IsGroupDiscoveryFailedError(err) {
		return nil, fmt.Errorf("deprecation scan: discover resources: %w", err)
	}
	var resources []schema.GroupVersionResource
	seen := map[schema.GroupVersionResource]struct{}{}
	for _, list := range lists {
		if list == nil {
			continue
		}
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		for i := range list.APIResources {
			res := &list.APIResources[i]
			if !scannable(gv.Group, res) {
				continue
			}
			gvr := gv.WithResource(res.Name)
			if _, dup := seen[gvr]; dup {
				continue
			}
			seen[gvr] = struct{}{}
			resources = append(resources, gvr)
		}
	}
	return resources, nil
}

// scannable reports whether a resource served in group can be listed and
// is of a kind with a rule.
func scannable(group string, res *metav1.APIResource) bool {
	if strings.Contains(res.Name, "/") {
		return false
	}
	if !slices.Contains(res.Verbs, verbList) {
		return false
	}
	return servesRuleKind(group, res.Kind)
}

// scanResource lists a resource in all namespaces, a page at a time, and
// keeps the objects whose last-applied configuration matches a rule.
func (s Scanner) scanResource(ctx context.Context, gvr schema.GroupVersionResource) ([]Finding, error) {
	var findings []Finding
	opts := metav1.ListOptions{Limit: metadataPageSize}
	for {
		page, err := s.Metadata.Resource(gvr).List(ctx, opts)
		if apierrors.IsNotFound(err) || apierrors.IsMethodNotSupported(err) {
			return findings, nil
		}
		if err != nil {
			return nil, fmt.Errorf("deprecation scan: list %s: %w", gvr.String(), err)
		}
		for i := range page.Items {
			if finding, ok := lastAppliedFinding(&page.Items[i]); ok {
				findings = append(findings, finding)
			}
		}
		if page.Continue == "" {
			return findings, nil
		}
		opts.Continue = page.Continue
	}
}

// lastAppliedFinding is the finding for an object whose last-applied
// configuration was written with a deprecated version. The name and
// namespace are the live object's.
func lastAppliedFinding(item *metav1.PartialObjectMetadata) (Finding, bool) {
	annotation, ok := item.Annotations[lastAppliedAnnotation]
	if !ok {
		return Finding{}, false
	}
	obj, ok := lastApplied(annotation)
	if !ok {
		return Finding{}, false
	}
	rule, ok := Match(obj.APIVersion, obj.Kind)
	if !ok {
		return Finding{}, false
	}
	obj.Namespace = item.Namespace
	obj.Name = item.Name
	return Finding{Object: obj, Rule: rule}, true
}

// scanReleases reads the deployed Helm releases of all namespaces, a page
// at a time, and keeps the objects of their manifests that match a rule. A
// release that cannot be decoded is skipped.
func (s Scanner) scanReleases(ctx context.Context) ([]Finding, error) {
	var findings []Finding
	opts := metav1.ListOptions{LabelSelector: helmReleaseSelector, Limit: releasePageSize}
	for {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("deprecation scan: %w", err)
		}
		page, err := s.Secrets.Secrets(metav1.NamespaceAll).List(ctx, opts)
		if err != nil {
			return nil, fmt.Errorf("deprecation scan: list helm release secrets: %w", err)
		}
		for i := range page.Items {
			findings = append(findings, releaseFindings(page.Items[i].Data[helmReleaseKey])...)
		}
		if page.Continue == "" {
			return findings, nil
		}
		opts.Continue = page.Continue
	}
}

// releaseFindings are the objects of a release's manifest that match a
// rule; none when the release cannot be decoded.
func releaseFindings(payload []byte) []Finding {
	_, objects, err := releaseObjects(payload)
	if err != nil {
		return nil
	}
	var findings []Finding
	for _, obj := range objects {
		if rule, ok := Match(obj.APIVersion, obj.Kind); ok {
			findings = append(findings, Finding{Object: obj, Rule: rule})
		}
	}
	return findings
}

// sortFindings puts findings in the order Result.Findings documents:
// Rule.RemovedIn, then the object's kind, namespace, name and source. The
// written apiVersion and the Helm release break what ties remain, so the
// order is the same on every scan.
func sortFindings(findings []Finding) {
	slices.SortFunc(findings, func(a, b Finding) int {
		return cmp.Or(
			cmp.Compare(a.Rule.RemovedIn, b.Rule.RemovedIn),
			cmp.Compare(a.Object.Kind, b.Object.Kind),
			cmp.Compare(a.Object.Namespace, b.Object.Namespace),
			cmp.Compare(a.Object.Name, b.Object.Name),
			cmp.Compare(a.Object.Source, b.Object.Source),
			cmp.Compare(a.Object.APIVersion, b.Object.APIVersion),
			cmp.Compare(a.Object.HelmRelease, b.Object.HelmRelease),
		)
	})
}
