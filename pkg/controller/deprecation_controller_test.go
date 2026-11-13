package controller

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/deprecation"
	"github.com/yindia/cluster-vending/pkg/reconcile/deprecations"
)

// appliedIngress is a workload cluster with one Ingress applied with an
// API version removed in 1.22.
type appliedIngress struct{}

func (appliedIngress) Resources(context.Context) ([]deprecation.Resource, error) {
	return []deprecation.Resource{{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses", Kind: "Ingress"}}, nil
}

func (appliedIngress) ListMeta(context.Context, deprecation.Resource, int64, string) ([]deprecation.Meta, string, error) {
	return []deprecation.Meta{{Namespace: "web", Name: "site", Annotations: map[string]string{
		deprecation.LastAppliedAnnotation: `{"apiVersion":"extensions/v1beta1","kind":"Ingress"}`}}}, "", nil
}

func (appliedIngress) ListHelmSecrets(context.Context, int64, string) ([]deprecation.HelmSecret, string, error) {
	return nil, "", nil
}

// scanWorld is a running cluster "prod" on Kubernetes 1.35 with its
// kubeconfig Secret, and the scanner on a fake workload cluster.
func scanWorld(t *testing.T) (world, *DeprecationReconciler, *[]string) {
	t.Helper()
	w := newWorld(t)
	ctx := t.Context()
	obj := handMadeCluster(t, "prod", "net")
	obj.Labels = map[string]string{cloudstore.LabelTenant: tenant, cloudstore.LabelID: "01a1d0a6-69d5-7e15-965e-fc6a53fa3d33"}
	w.must(t, w.k.Create(ctx, obj))
	obj.Status.Phase, obj.Status.CurrentKubernetesVersion = cloudv1alpha1.ClusterPhaseRunning, "1.35.4"
	obj.Status.KubeconfigSecret = namespace + "/prod-kubeconfig" // as the cluster reconcile records it
	w.must(t, w.k.Status().Update(ctx, obj))
	w.must(t, w.k.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "prod-kubeconfig"},
		Data: map[string][]byte{"kubeconfig": []byte("kubeconfig of prod")}}))

	r, err := NewDeprecationReconciler(w.deps)
	w.must(t, err)
	var connected []string
	r.scanner.Connect = func(kubeconfig []byte) (deprecation.Reader, error) {
		connected = append(connected, string(kubeconfig))
		return appliedIngress{}, nil
	}
	return w, r, &connected
}

func TestRunningClusterIsScanned(t *testing.T) {
	w, r, connected := scanWorld(t)

	res, err := r.Reconcile(t.Context(), request("prod"))

	if err != nil || res.RequeueAfter != deprecations.Interval {
		t.Fatalf("Reconcile = %+v, %v", res, err)
	}
	d := w.cluster(t, "prod").Status.Deprecations
	if d == nil || d.TargetVersion != "1.36" || d.Total != 1 || d.Removed != 1 || d.Error != "" || d.ScannedAt == nil || len(d.Findings) != 1 {
		t.Fatalf("status.deprecations = %+v", d)
	}
	if f := d.Findings[0]; f.Namespace != "web" || f.Name != "site" || f.APIVersion != "extensions/v1beta1" ||
		f.Source != cloudv1alpha1.DeprecationSourceManifest || !f.Removed {
		t.Errorf("finding = %+v", f)
	}
	cond := meta.FindStatusCondition(w.cluster(t, "prod").Status.Conditions, cloudv1alpha1.ConditionDeprecationScan)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "RemovedAPIsInUse" {
		t.Errorf("condition = %+v", cond)
	}
	if len(*connected) != 1 || (*connected)[0] != "kubeconfig of prod" {
		t.Errorf("connected with %q", *connected)
	}

	// Again right away: the scan stands, the workload cluster is not read.
	res, err = r.Reconcile(t.Context(), request("prod"))
	if err != nil || res.RequeueAfter <= 0 || res.RequeueAfter > deprecations.Interval || len(*connected) != 1 {
		t.Fatalf("second Reconcile = %+v, %v, %d connections", res, err, len(*connected))
	}
}

// The release of the cluster's channel says where its minor goes next.
func TestScanTargetsTheReleasesUpgrade(t *testing.T) {
	w, r, _ := scanWorld(t)
	ctx := t.Context()
	release := DefaultRelease()
	for i := range release.Spec.Content.Kubernetes {
		if release.Spec.Content.Kubernetes[i].Minor == "1.35" {
			release.Spec.Content.Kubernetes[i].UpgradeTo = "1.37"
		}
	}
	w.must(t, w.k.Create(ctx, release))
	w.must(t, w.k.Create(ctx, &cloudv1alpha1.ReleaseChannel{ObjectMeta: metav1.ObjectMeta{Name: "stable"},
		Status: cloudv1alpha1.ReleaseChannelStatus{Release: release.Name}}))

	_, err := r.Reconcile(ctx, request("prod"))

	if d := w.cluster(t, "prod").Status.Deprecations; err != nil || d == nil || d.TargetVersion != "1.37" {
		t.Fatalf("Reconcile: %v, status.deprecations = %+v", err, d)
	}
}

func TestClusterThatCannotBeScanned(t *testing.T) {
	tests := []struct {
		name      string
		change    func(*testing.T, world, *cloudv1alpha1.Cluster)
		wantAfter bool // looked at again later, nothing recorded
		wantError string
	}{
		{"not running yet", func(t *testing.T, w world, obj *cloudv1alpha1.Cluster) {
			obj.Status.Phase = cloudv1alpha1.ClusterPhaseProvisioning
			w.must(t, w.k.Status().Update(t.Context(), obj))
		}, true, ""},
		// An imported cluster is only observed until its takeover: it has
		// no ClusterAuth, so no kubeconfig to scan with. That is not a
		// failed scan to record every quarter of an hour.
		{"imported, read-only: no kubeconfig yet", func(t *testing.T, w world, obj *cloudv1alpha1.Cluster) {
			obj.Status.Phase, obj.Status.KubeconfigSecret = cloudv1alpha1.ClusterPhaseImported, ""
			w.must(t, w.k.Status().Update(t.Context(), obj))
		}, true, ""},
		{"no kubeconfig Secret named", func(t *testing.T, w world, obj *cloudv1alpha1.Cluster) {
			obj.Status.KubeconfigSecret = ""
			w.must(t, w.k.Status().Update(t.Context(), obj))
		}, false, "kubeconfig: the cluster has no kubeconfig Secret yet"},
		{"a Secret of another namespace", func(t *testing.T, w world, obj *cloudv1alpha1.Cluster) {
			obj.Status.KubeconfigSecret = "kube-system/prod-kubeconfig"
			w.must(t, w.k.Status().Update(t.Context(), obj))
		}, false, "kubeconfig: secret kube-system/prod-kubeconfig is not in the cluster's namespace"},
		{"the Secret is gone", func(t *testing.T, w world, _ *cloudv1alpha1.Cluster) {
			w.must(t, w.k.Delete(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "prod-kubeconfig"}}))
		}, false, "kubeconfig: secret prod-kubeconfig not found"},
		{"the Secret has no kubeconfig", func(t *testing.T, w world, _ *cloudv1alpha1.Cluster) {
			secret := &corev1.Secret{}
			w.must(t, w.k.Get(t.Context(), request("prod-kubeconfig").NamespacedName, secret))
			secret.Data = nil
			w.must(t, w.k.Update(t.Context(), secret))
		}, false, `kubeconfig: secret prod-kubeconfig has no "kubeconfig" key`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w, r, connected := scanWorld(t)
			tt.change(t, w, w.cluster(t, "prod"))

			res, err := r.Reconcile(t.Context(), request("prod"))

			// Never an error: the cluster's own reconcile is not involved.
			if err != nil || res.RequeueAfter <= 0 || len(*connected) != 0 {
				t.Fatalf("Reconcile = %+v, %v, %d connections", res, err, len(*connected))
			}
			obj := w.cluster(t, "prod")
			if tt.wantAfter {
				if obj.Status.Deprecations != nil || res.RequeueAfter != notScannableDelay {
					t.Fatalf("recorded %+v, again after %s", obj.Status.Deprecations, res.RequeueAfter)
				}
				return
			}
			cond := meta.FindStatusCondition(obj.Status.Conditions, cloudv1alpha1.ConditionDeprecationScan)
			if obj.Status.Deprecations == nil || obj.Status.Deprecations.Error != tt.wantError ||
				cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "ScanFailed" || res.RequeueAfter != deprecations.RetryInterval {
				t.Fatalf("status.deprecations = %+v, condition %+v, again after %s", obj.Status.Deprecations, cond, res.RequeueAfter)
			}
		})
	}
}

// forbidSecrets is a client whose reads of Secrets are refused, as the
// API server refuses them in a tenant namespace without the reader binding.
type forbidSecrets struct{ client.Client }

func (c forbidSecrets) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*corev1.Secret); ok {
		return apierrors.NewForbidden(schema.GroupResource{Resource: "secrets"}, key.Name, errors.New("no RBAC rule"))
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// The controller reads a kubeconfig only where the tenant namespace binds
// it to the reader role. Without the binding the scan fails with a message
// that names it, and the cluster's own reconcile is not involved.
func TestScanWithoutTheKubeconfigReaderBinding(t *testing.T) {
	w, r, connected := scanWorld(t)
	r.deps.Direct = forbidSecrets{r.deps.Direct}

	res, err := r.Reconcile(t.Context(), request("prod"))

	if err != nil || res.RequeueAfter != deprecations.RetryInterval || len(*connected) != 0 {
		t.Fatalf("Reconcile = %+v, %v, %d connections", res, err, len(*connected))
	}
	want := "kubeconfig: the controller may not read secret prod-kubeconfig in namespace " + namespace +
		": the cluster-vending-kubeconfig-reader RoleBinding is missing there (the cloud account's reconcile applies it; --service-account names who it binds)"
	if d := w.cluster(t, "prod").Status.Deprecations; d == nil || d.Error != want {
		t.Fatalf("status.deprecations = %+v, want error %q", d, want)
	}
}

func TestScanOfAClusterThatIsGone(t *testing.T) {
	_, r, _ := scanWorld(t)
	if res, err := r.Reconcile(t.Context(), request("gone")); err != nil || res.RequeueAfter != 0 {
		t.Fatalf("Reconcile = %+v, %v", res, err)
	}
}

// Every Kubernetes minor the shipped release offers is known to the rule
// data: it has removals there, or it is listed as having none.
func TestRulesKnowEveryOfferedMinor(t *testing.T) {
	rules, err := deprecation.Default()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range DefaultRelease().Spec.Content.Kubernetes {
		removals, why := rules.RemovalsIn(k.Minor), deprecation.NoKnownRemovals[k.Minor]
		switch {
		case removals == 0 && why == "":
			t.Errorf("Kubernetes %s: the rule data (%s) names no removal and deprecation.NoKnownRemovals does not say why", k.Minor, deprecation.DataSource)
		case removals > 0 && why != "":
			t.Errorf("Kubernetes %s is in deprecation.NoKnownRemovals but the rule data removes %d API versions in it", k.Minor, removals)
		}
		if _, err := rules.Evaluate(nil, k.Minor); err != nil {
			t.Errorf("Kubernetes %s is not a target: %v", k.Minor, err)
		}
	}
}
