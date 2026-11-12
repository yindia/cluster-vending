package deprecation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/version"
	"k8s.io/client-go/discovery"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/metadata"
	metadatafake "k8s.io/client-go/metadata/fake"
	"k8s.io/client-go/rest"
	clienttesting "k8s.io/client-go/testing"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
)

// stubDiscovery answers discovery from fixed values.
type stubDiscovery struct {
	info         *version.Info
	versionErr   error
	resources    []*metav1.APIResourceList
	resourcesErr error
}

func (d stubDiscovery) ServerVersion() (*version.Info, error) { return d.info, d.versionErr }

func (d stubDiscovery) ServerPreferredResources() ([]*metav1.APIResourceList, error) {
	return d.resources, d.resourcesErr
}

var listVerbs = metav1.Verbs{"get", "list", "watch"}

// servedResources is what a 1.31 cluster serves of the kinds the tests use,
// with resources a scan must leave alone.
func servedResources() []*metav1.APIResourceList {
	return []*metav1.APIResourceList{
		{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "pods", Kind: "Pod", Namespaced: true, Verbs: listVerbs},
			{Name: "events", Kind: "Event", Namespaced: true, Verbs: listVerbs},
		}},
		{GroupVersion: "apps/v1", APIResources: []metav1.APIResource{
			{Name: "deployments", Kind: "Deployment", Namespaced: true, Verbs: listVerbs},
			{Name: "deployments/status", Kind: "Deployment", Namespaced: true, Verbs: listVerbs},
			{Name: "controllerrevisions", Kind: "ControllerRevision", Namespaced: true, Verbs: listVerbs},
		}},
		{GroupVersion: "networking.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "ingresses", Kind: "Ingress", Namespaced: true, Verbs: listVerbs},
		}},
		{GroupVersion: "rbac.authorization.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "clusterroles", Kind: "ClusterRole", Verbs: listVerbs},
		}},
		{GroupVersion: "authorization.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "subjectaccessreviews", Kind: "SubjectAccessReview", Verbs: metav1.Verbs{"create"}},
		}},
		{GroupVersion: "not/a/group/version", APIResources: []metav1.APIResource{
			{Name: "deployments", Kind: "Deployment", Verbs: listVerbs},
		}},
		nil,
	}
}

func servedVersion() *version.Info { return &version.Info{Major: "1", Minor: "31+"} }

// applied is the metadata of a served object; lastAppliedAPIVersion is the
// version its last-applied configuration was written with, empty for an
// object that was not applied with kubectl.
func applied(servedAPIVersion, kind, namespace, name, lastAppliedAPIVersion string) *metav1.PartialObjectMetadata {
	obj := &metav1.PartialObjectMetadata{
		TypeMeta:   metav1.TypeMeta{APIVersion: servedAPIVersion, Kind: kind},
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
	}
	if lastAppliedAPIVersion != "" {
		obj.Annotations = map[string]string{
			lastAppliedAnnotation: fmt.Sprintf(`{"apiVersion":%q,"kind":%q,"metadata":{"name":%q}}`, lastAppliedAPIVersion, kind, name),
		}
	}
	return obj
}

func releaseSecret(namespace, name, status string, payload []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace,
			Name:      name,
			Labels:    map[string]string{"owner": "helm", "status": status},
		},
		Type: "helm.sh/release.v1",
		Data: map[string][]byte{helmReleaseKey: payload},
	}
}

func newMetadataClient(t *testing.T, objects ...runtime.Object) *metadatafake.FakeMetadataClient {
	t.Helper()
	scheme := metadatafake.NewTestScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatalf("add meta to scheme: %v", err)
	}
	return metadatafake.NewSimpleMetadataClient(scheme, objects...)
}

func newScanner(t *testing.T, metadataObjects []runtime.Object, secrets ...runtime.Object) (Scanner, *metadatafake.FakeMetadataClient) {
	t.Helper()
	meta := newMetadataClient(t, metadataObjects...)
	return Scanner{
		Discovery: stubDiscovery{info: servedVersion(), resources: servedResources()},
		Metadata:  meta,
		Secrets:   kubefake.NewClientset(secrets...).CoreV1(),
	}, meta
}

const pdbManifest = `---
# Source: storefront/templates/pdb.yaml
apiVersion: policy/v1beta1
kind: PodDisruptionBudget
metadata:
  name: web
---
# Source: storefront/templates/service.yaml
apiVersion: v1
kind: Service
metadata:
  name: web
`

func TestScannerScan(t *testing.T) {
	// Arrange
	objects := []runtime.Object{
		applied("apps/v1", "Deployment", "shop", "old", "extensions/v1beta1"),
		applied("apps/v1", "Deployment", "shop", "current", "apps/v1"),
		applied("apps/v1", "Deployment", "shop", "unannotated", ""),
		applied("networking.k8s.io/v1", "Ingress", "shop", "front", "networking.k8s.io/v1beta1"),
		applied("rbac.authorization.k8s.io/v1", "ClusterRole", "", "reader", "rbac.authorization.k8s.io/v1beta1"),
		// Not of a kind with a rule: never listed, whatever it says.
		applied("v1", "Pod", "shop", "pod", "extensions/v1beta1"),
	}
	scanner, meta := newScanner(t, objects,
		releaseSecret("shop", "sh.helm.release.v1.storefront.v2", "deployed", releasePayload(t, "shop", "storefront", pdbManifest)),
		releaseSecret("shop", "sh.helm.release.v1.storefront.v1", "superseded", releasePayload(t, "shop", "storefront", pdbManifest)),
		releaseSecret("shop", "sh.helm.release.v1.broken.v1", "deployed", []byte("!!! not a release !!!")),
		releaseSecret("shop", "sh.helm.release.v1.nokey.v1", "deployed", nil),
	)

	// Act
	result, err := scanner.Scan(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if result.Version != 31 {
		t.Errorf("Version = %s, want 1.31", result.Version)
	}
	want := []Object{
		{APIVersion: "extensions/v1beta1", Kind: "Deployment", Namespace: "shop", Name: "old", Source: SourceLastApplied},
		{APIVersion: "rbac.authorization.k8s.io/v1beta1", Kind: "ClusterRole", Name: "reader", Source: SourceLastApplied},
		{APIVersion: "networking.k8s.io/v1beta1", Kind: "Ingress", Namespace: "shop", Name: "front", Source: SourceLastApplied},
		{APIVersion: "policy/v1beta1", Kind: "PodDisruptionBudget", Name: "web", Source: SourceHelmRelease, HelmRelease: "shop/storefront"},
	}
	if got := findingObjects(result.Findings); !slices.Equal(got, want) {
		t.Errorf("findings =\n%+v\nwant\n%+v", got, want)
	}
	for _, finding := range result.Findings {
		if finding.Rule.APIVersion != finding.Object.APIVersion || finding.Rule.Kind != finding.Object.Kind {
			t.Errorf("finding %+v carries the rule of %s %s", finding.Object, finding.Rule.APIVersion, finding.Rule.Kind)
		}
	}
	wantListed := []string{"clusterroles", "deployments", "ingresses"}
	if got := listedResources(meta); !slices.Equal(got, wantListed) {
		t.Errorf("listed resources = %v, want %v", got, wantListed)
	}
}

func findingObjects(findings []Finding) []Object {
	objects := make([]Object, 0, len(findings))
	for _, finding := range findings {
		objects = append(objects, finding.Object)
	}
	return objects
}

func listedResources(meta *metadatafake.FakeMetadataClient) []string {
	var listed []string
	for _, action := range meta.Actions() {
		if action.GetVerb() == verbList {
			listed = append(listed, action.GetResource().Resource)
		}
	}
	slices.Sort(listed)
	return slices.Compact(listed)
}

func TestScannerScanToleratesPartialDiscovery(t *testing.T) {
	// Arrange
	scanner, _ := newScanner(t, []runtime.Object{
		applied("apps/v1", "Deployment", "shop", "old", "apps/v1beta2"),
	})
	scanner.Discovery = stubDiscovery{
		info:      servedVersion(),
		resources: servedResources(),
		resourcesErr: &discovery.ErrGroupDiscoveryFailed{Groups: map[schema.GroupVersion]error{
			{Group: "metrics.k8s.io", Version: "v1beta1"}: errors.New("the server is currently unable to handle the request"),
		}},
	}

	// Act
	result, err := scanner.Scan(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if len(result.Findings) != 1 || result.Findings[0].Object.Name != "old" {
		t.Errorf("findings = %+v, want the one deployment", result.Findings)
	}
}

func TestScannerScanSkipsResourcesThatCannotBeListed(t *testing.T) {
	gone := schema.GroupResource{Group: "apps", Resource: "deployments"}
	tests := []struct {
		name string
		err  error
	}{
		{name: "not found", err: apierrors.NewNotFound(gone, "")},
		{name: "method not supported", err: apierrors.NewMethodNotSupported(gone, verbList)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			scanner, meta := newScanner(t, []runtime.Object{
				applied("networking.k8s.io/v1", "Ingress", "shop", "front", "extensions/v1beta1"),
			})
			meta.PrependReactor(verbList, "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, tt.err
			})

			// Act
			result, err := scanner.Scan(context.Background())

			// Assert
			if err != nil {
				t.Fatalf("Scan() error = %v", err)
			}
			if len(result.Findings) != 1 || result.Findings[0].Object.Kind != "Ingress" {
				t.Errorf("findings = %+v, want the one ingress", result.Findings)
			}
		})
	}
}

// pagedMetadata serves the pages of one resource by continue token and
// records the options of each call. Other resources go to the embedded
// client.
type pagedMetadata struct {
	metadata.Interface
	resource string
	pages    map[string]*metav1.PartialObjectMetadataList
	calls    *[]metav1.ListOptions
}

func (p pagedMetadata) Resource(gvr schema.GroupVersionResource) metadata.Getter {
	if gvr.Resource != p.resource {
		return p.Interface.Resource(gvr)
	}
	return pagedGetter{Getter: p.Interface.Resource(gvr), paged: p}
}

type pagedGetter struct {
	metadata.Getter
	paged pagedMetadata
}

func (g pagedGetter) List(_ context.Context, opts metav1.ListOptions) (*metav1.PartialObjectMetadataList, error) {
	*g.paged.calls = append(*g.paged.calls, opts)
	page, ok := g.paged.pages[opts.Continue]
	if !ok {
		return nil, fmt.Errorf("unexpected continue token %q", opts.Continue)
	}
	return page, nil
}

func TestScannerScanFollowsContinue(t *testing.T) {
	// Arrange
	const token = "next-page"
	var calls []metav1.ListOptions
	scanner, meta := newScanner(t, nil)
	scanner.Metadata = pagedMetadata{
		Interface: meta,
		resource:  "deployments",
		calls:     &calls,
		pages: map[string]*metav1.PartialObjectMetadataList{
			"": {
				ListMeta: metav1.ListMeta{Continue: token},
				Items:    []metav1.PartialObjectMetadata{*applied("apps/v1", "Deployment", "shop", "first", "extensions/v1beta1")},
			},
			token: {
				Items: []metav1.PartialObjectMetadata{*applied("apps/v1", "Deployment", "shop", "second", "apps/v1beta1")},
			},
		},
	}

	// Act
	result, err := scanner.Scan(context.Background())

	// Assert
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	names := []string{}
	for _, finding := range result.Findings {
		names = append(names, finding.Object.Name)
	}
	if !slices.Equal(names, []string{"first", "second"}) {
		t.Errorf("found %v, want both pages: [first second]", names)
	}
	want := []metav1.ListOptions{
		{Limit: metadataPageSize},
		{Limit: metadataPageSize, Continue: token},
	}
	if !slices.Equal(calls, want) {
		t.Errorf("list calls = %+v, want %+v", calls, want)
	}
}

func TestScannerScanFailures(t *testing.T) {
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "deployments"}, "", errors.New("no"))
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name    string
		ctx     context.Context
		arrange func(*Scanner, *metadatafake.FakeMetadataClient, *kubefake.Clientset)
		wantErr string
		wantIs  error
	}{
		{
			name: "the server version cannot be read",
			arrange: func(s *Scanner, _ *metadatafake.FakeMetadataClient, _ *kubefake.Clientset) {
				s.Discovery = stubDiscovery{versionErr: errors.New("connection refused")}
			},
			wantErr: "server version: connection refused",
		},
		{
			name: "the server reports no version",
			arrange: func(s *Scanner, _ *metadatafake.FakeMetadataClient, _ *kubefake.Clientset) {
				s.Discovery = stubDiscovery{}
			},
			wantErr: "none reported",
		},
		{
			name: "the major version is not 1",
			arrange: func(s *Scanner, _ *metadatafake.FakeMetadataClient, _ *kubefake.Clientset) {
				s.Discovery = stubDiscovery{info: &version.Info{Major: "2", Minor: "0"}}
			},
			wantErr: `major version "2" is not 1`,
		},
		{
			name: "the minor version is not a number",
			arrange: func(s *Scanner, _ *metadatafake.FakeMetadataClient, _ *kubefake.Clientset) {
				s.Discovery = stubDiscovery{info: &version.Info{Major: "1", Minor: "x"}}
			},
			wantErr: "no minor version",
		},
		{
			name: "discovery fails outright",
			arrange: func(s *Scanner, _ *metadatafake.FakeMetadataClient, _ *kubefake.Clientset) {
				s.Discovery = stubDiscovery{info: servedVersion(), resourcesErr: errors.New("unauthorized")}
			},
			wantErr: "discover resources: unauthorized",
		},
		{
			name: "a resource cannot be listed",
			arrange: func(_ *Scanner, meta *metadatafake.FakeMetadataClient, _ *kubefake.Clientset) {
				meta.PrependReactor(verbList, "deployments", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, forbidden
				})
			},
			wantErr: "list apps/v1, Resource=deployments",
			wantIs:  forbidden,
		},
		{
			name: "the release secrets cannot be listed",
			arrange: func(_ *Scanner, _ *metadatafake.FakeMetadataClient, kube *kubefake.Clientset) {
				kube.PrependReactor(verbList, "secrets", func(clienttesting.Action) (bool, runtime.Object, error) {
					return true, nil, errors.New("etcd is down")
				})
			},
			wantErr: "list helm release secrets: etcd is down",
		},
		{
			name:    "the scan is cancelled",
			ctx:     cancelled,
			arrange: func(*Scanner, *metadatafake.FakeMetadataClient, *kubefake.Clientset) {},
			wantIs:  context.Canceled,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Arrange
			meta := newMetadataClient(t, applied("apps/v1", "Deployment", "shop", "old", "extensions/v1beta1"))
			kube := kubefake.NewClientset()
			scanner := Scanner{
				Discovery: stubDiscovery{info: servedVersion(), resources: servedResources()},
				Metadata:  meta,
				Secrets:   kube.CoreV1(),
			}
			tt.arrange(&scanner, meta, kube)
			ctx := tt.ctx
			if ctx == nil {
				ctx = context.Background()
			}

			// Act
			result, err := scanner.Scan(ctx)

			// Assert
			if err == nil {
				t.Fatalf("Scan() = %+v, want an error", result)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
			if tt.wantIs != nil && !errors.Is(err, tt.wantIs) {
				t.Errorf("error = %v, want it to wrap %v", err, tt.wantIs)
			}
			if len(result.Findings) != 0 {
				t.Errorf("findings = %+v alongside the error, want none", result.Findings)
			}
		})
	}
}

func TestScannerScanStopsListingReleasesWhenCancelled(t *testing.T) {
	// Arrange: the context ends while the last resource is listed.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	scanner, meta := newScanner(t, nil)
	meta.PrependReactor(verbList, "clusterroles", func(clienttesting.Action) (bool, runtime.Object, error) {
		cancel()
		return false, nil, nil
	})

	// Act
	_, err := scanner.Scan(ctx)

	// Assert
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func TestSortFindings(t *testing.T) {
	// Arrange
	finding := func(removedIn Version, kind, namespace, name string, source Source) Finding {
		return Finding{
			Object: Object{Kind: kind, Namespace: namespace, Name: name, Source: source},
			Rule:   Rule{Kind: kind, RemovedIn: removedIn},
		}
	}
	want := []Finding{
		finding(16, "Deployment", "a", "web", SourceLastApplied),
		finding(16, "Deployment", "a", "web", SourceHelmRelease),
		finding(16, "Deployment", "a", "worker", SourceLastApplied),
		finding(16, "Deployment", "b", "api", SourceLastApplied),
		finding(16, "ReplicaSet", "a", "api", SourceLastApplied),
		finding(22, "ClusterRole", "", "reader", SourceLastApplied),
		finding(22, "Ingress", "a", "front", SourceLastApplied),
		finding(25, "CronJob", "a", "nightly", SourceHelmRelease),
	}
	got := slices.Clone(want)
	slices.Reverse(got)
	got[0], got[3] = got[3], got[0]

	// Act
	sortFindings(got)

	// Assert
	if !slices.Equal(got, want) {
		t.Errorf("sorted =\n%+v\nwant\n%+v", got, want)
	}
}

func TestScanClusterRefusesConfig(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *rest.Config
		wantErr string
	}{
		{name: "nil", cfg: nil, wantErr: "no cluster config"},
		{
			name: "an exec plugin",
			cfg: &rest.Config{
				Host:         "https://127.0.0.1:1",
				ExecProvider: &clientcmdapi.ExecConfig{Command: "/bin/false", APIVersion: "client.authentication.k8s.io/v1"},
			},
			wantErr: "exec plugin or auth provider",
		},
		{
			name: "an auth provider",
			cfg: &rest.Config{
				Host:         "https://127.0.0.1:1",
				AuthProvider: &clientcmdapi.AuthProviderConfig{Name: "oidc"},
			},
			wantErr: "exec plugin or auth provider",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ScanCluster(context.Background(), tt.cfg)

			if err == nil {
				t.Fatal("ScanCluster() succeeded, want a refusal")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantErr)
			}
		})
	}
}

func TestScanClusterLeavesTheConfigAlone(t *testing.T) {
	// Arrange: nothing listens on port 1, so the scan fails at its first
	// request, after the clients are built.
	const token = "s3cr3t-token"
	cfg := &rest.Config{Host: "https://127.0.0.1:1", BearerToken: token, Timeout: time.Second}
	before := rest.CopyConfig(cfg)

	// Act
	_, err := ScanCluster(context.Background(), cfg)

	// Assert
	if err == nil {
		t.Fatal("ScanCluster() succeeded against a closed port")
	}
	if !strings.Contains(err.Error(), "server version") {
		t.Errorf("error = %q, want it to say the server version could not be read", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error %q holds the bearer token", err)
	}
	if cfg.UserAgent != before.UserAgent || cfg.Timeout != before.Timeout || cfg.Host != before.Host {
		t.Errorf("config changed: user agent %q, timeout %s, host %q", cfg.UserAgent, cfg.Timeout, cfg.Host)
	}
}
