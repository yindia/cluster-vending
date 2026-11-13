package deprecation_test

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakediscovery "k8s.io/client-go/discovery/fake"
	fakemetadata "k8s.io/client-go/metadata/fake"
	clienttesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/yindia/cluster-vending/pkg/deprecation"
)

func kubeReader(t *testing.T) *deprecation.KubeReader {
	t.Helper()
	secrets := fake.NewClientBuilder().WithObjects(
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "web", Name: "sh.helm.release.v1.site.v2",
			Labels: map[string]string{"owner": "helm", "status": "deployed"}}, Data: map[string][]byte{"release": []byte("stored")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "web", Name: "sh.helm.release.v1.site.v1",
			Labels: map[string]string{"owner": "helm", "status": "superseded"}}, Data: map[string][]byte{"release": []byte("old")}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "web", Name: "sh.helm.release.v1.empty.v1",
			Labels: map[string]string{"owner": "helm", "status": "deployed"}}},
		&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "web", Name: "password"}, Data: map[string][]byte{"release": []byte("not helm")}},
	).Build()
	disco := &fakediscovery.FakeDiscovery{Fake: &clienttesting.Fake{}}
	disco.Resources = []*metav1.APIResourceList{
		{GroupVersion: "networking.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "ingresses", Kind: "Ingress", Verbs: metav1.Verbs{"get", "list"}},
			{Name: "ingresses/status", Kind: "Ingress", Verbs: metav1.Verbs{"get"}},
		}},
		{GroupVersion: "v1", APIResources: []metav1.APIResource{{Name: "bindings", Kind: "Binding", Verbs: metav1.Verbs{"create"}}}},
	}
	scheme := runtime.NewScheme()
	if err := metav1.AddMetaToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "networking.k8s.io", Version: "v1", Kind: "IngressList"}, &metav1.PartialObjectMetadataList{})
	meta := fakemetadata.NewSimpleMetadataClient(scheme, &metav1.PartialObjectMetadata{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "Ingress"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "web", Name: "site",
			Annotations: map[string]string{deprecation.LastAppliedAnnotation: `{"apiVersion":"extensions/v1beta1","kind":"Ingress"}`}}})
	return &deprecation.KubeReader{Discovery: disco, Metadata: meta, Secrets: secrets}
}

func TestKubeReaderListsWhatCanBeListed(t *testing.T) {
	resources, err := kubeReader(t).Resources(t.Context())

	if err != nil {
		t.Fatalf("Resources: %v", err)
	}
	want := deprecation.Resource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses", Kind: "Ingress"}
	if len(resources) != 1 || resources[0] != want {
		t.Fatalf("resources = %+v, want only %+v", resources, want)
	}
}

func TestKubeReaderReadsMetadataAndHelmSecrets(t *testing.T) {
	r := kubeReader(t)

	items, next, err := r.ListMeta(t.Context(), deprecation.Resource{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses", Kind: "Ingress"}, 10, "")

	if err != nil || next != "" || len(items) != 1 || items[0].Namespace != "web" || items[0].Name != "site" ||
		!strings.Contains(items[0].Annotations[deprecation.LastAppliedAnnotation], "extensions/v1beta1") {
		t.Fatalf("ListMeta = %+v, %q, %v", items, next, err)
	}
	secrets, next, err := r.ListHelmSecrets(t.Context(), 10, "")
	if err != nil || next != "" || len(secrets) != 1 || secrets[0].Name != "sh.helm.release.v1.site.v2" || string(secrets[0].Release) != "stored" {
		t.Fatalf("ListHelmSecrets = %+v, %q, %v", secrets, next, err)
	}
}

// The whole path: client-go, the scan and the rules.
func TestScanThroughKubeReader(t *testing.T) {
	report, err := rules(t).Scan(t.Context(), kubeReader(t), "1.37")

	if err != nil || report.Total != 1 || report.Findings[0].Name != "site" || !report.Findings[0].Removed {
		t.Fatalf("Scan = %+v, %v", report, err)
	}
}

func TestNewKubeReader(t *testing.T) {
	kubeconfig := `
apiVersion: v1
kind: Config
clusters:
- name: prod
  cluster: {server: "https://prod.example.com"}
users:
- name: prod
  user: {token: t}
contexts:
- name: prod
  context: {cluster: prod, user: prod}
current-context: prod
`
	r, err := deprecation.NewKubeReader([]byte(kubeconfig))
	if err != nil || r.Discovery == nil || r.Metadata == nil || r.Secrets == nil {
		t.Fatalf("NewKubeReader = %+v, %v", r, err)
	}
	if _, err := deprecation.NewKubeReader([]byte("not a kubeconfig")); err == nil || !strings.Contains(err.Error(), "kubeconfig") {
		t.Fatalf("a bad kubeconfig: %v", err)
	}
}

// A kubeconfig read from a Secret must never make the controller run a
// command or load an auth plugin.
func TestNewKubeReaderRefusesExecAndAuthProviders(t *testing.T) {
	const base = `
apiVersion: v1
kind: Config
clusters:
- name: prod
  cluster: {server: "https://prod.example.com"}
contexts:
- name: prod
  context: {cluster: prod, user: prod}
current-context: prod
users:
- name: prod
  user: `
	tests := []struct{ name, user, want string }{
		{"exec plugin", `{exec: {apiVersion: client.authentication.k8s.io/v1, command: aws, args: [eks, get-token], interactiveMode: Never}}`, "exec"},
		{"auth provider", `{auth-provider: {name: oidc}}`, "auth provider"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := deprecation.NewKubeReader([]byte(base + tt.user + "\n"))

			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("NewKubeReader = %v, want a refusal naming %q", err, tt.want)
			}
			// The same refusal for whatever else uses such a kubeconfig.
			if _, err := deprecation.RESTConfig([]byte(base + tt.user + "\n")); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("RESTConfig = %v, want a refusal naming %q", err, tt.want)
			}
		})
	}
}
