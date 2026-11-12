package account

import (
	"strings"
	"testing"

	awsv1beta1 "github.com/upbound/provider-aws/v2/apis/namespaced/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/fault"
)

var localCredentials = &crossplaneaws.CredentialsSecret{Namespace: "crossplane-system", Name: "aws-local", Key: "credentials"}

// withCredentials is w with the providers' credentials Secret set, and
// the Secret created with the given keys when keys is not nil.
func withCredentials(t *testing.T, w world, keys map[string][]byte) world {
	t.Helper()
	if keys != nil {
		secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: localCredentials.Namespace, Name: localCredentials.Name},
			Data: keys}
		if err := w.kube.Create(t.Context(), secret); err != nil {
			t.Fatal(err)
		}
	}
	w.rec.deps.ProviderCredentials = localCredentials
	return w
}

func (w world) providerConfig(t *testing.T) *awsv1beta1.ProviderConfig {
	t.Helper()
	pc := &awsv1beta1.ProviderConfig{}
	key := client.ObjectKey{Namespace: namespace, Name: w.account.ProviderConfigName}
	if err := w.kube.Get(t.Context(), key, pc); err != nil {
		t.Fatalf("ProviderConfig: %v", err)
	}
	return pc
}

// With a credentials Secret every pass applies the Secret-based
// ProviderConfig, which still assumes the tenant role, and never flips it
// back to IRSA.
func TestApplyWithACredentialsSecret(t *testing.T) {
	w := withCredentials(t, newWorld(t), map[string][]byte{"credentials": []byte("[default]\n")})
	for pass := range 2 {
		if err := w.rec.Apply(t.Context(), w.account); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		pc := w.providerConfig(t)
		ref := pc.Spec.Credentials.SecretRef
		if pc.Spec.Credentials.Source != "Secret" || ref == nil || ref.Name != "aws-local" ||
			ref.Namespace != "crossplane-system" || ref.Key != "credentials" {
			t.Errorf("pass %d: credentials = %+v, want the Secret", pass, pc.Spec.Credentials)
		}
		if chain := pc.Spec.AssumeRoleChain; len(chain) != 1 || *chain[0].RoleARN != identity {
			t.Errorf("pass %d: assumeRoleChain = %+v, want the tenant role", pass, chain)
		}
	}
}

// A credentials Secret that is missing, or lacks its key, is a
// FailedPrecondition fault naming it, and nothing is applied.
func TestApplyRefusesAMissingCredentialsSecret(t *testing.T) {
	for name, keys := range map[string]map[string][]byte{
		"no Secret": nil,
		"no key":    {"other": []byte("x")},
	} {
		w := withCredentials(t, newWorld(t), keys)
		err := w.rec.Apply(t.Context(), w.account)
		if f, ok := fault.As(err); !ok || f.Kind != fault.FailedPrecondition || !strings.Contains(f.Message, "crossplane-system/aws-local") {
			t.Errorf("%s: %v, want a FailedPrecondition fault naming the Secret", name, err)
		}
		pc := &awsv1beta1.ProviderConfig{}
		if err := w.kube.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: w.account.ProviderConfigName}, pc); err == nil {
			t.Errorf("%s: a ProviderConfig was applied", name)
		}
	}
}
