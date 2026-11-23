package naas

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

func TestRenderQualifier(t *testing.T) {
	key := qualificationKeyFixture()
	objects, err := RenderQualifier(QualifierOptions{Image: "registry.example/cv:1", ProbeImage: "busybox:1.36.1", Key: key, At: qualificationNow})
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	var job *batchv1.Job
	var clusterRole *rbacv1.ClusterRole
	var roles []*rbacv1.Role
	for _, obj := range objects {
		switch o := obj.(type) {
		case *corev1.ServiceAccount:
			kinds = append(kinds, "ServiceAccount")
		case *rbacv1.ClusterRole:
			kinds, clusterRole = append(kinds, "ClusterRole"), o
		case *rbacv1.ClusterRoleBinding:
			kinds = append(kinds, "ClusterRoleBinding")
		case *rbacv1.Role:
			kinds, roles = append(kinds, "Role"), append(roles, o)
		case *rbacv1.RoleBinding:
			kinds = append(kinds, "RoleBinding")
		case *batchv1.Job:
			kinds, job = append(kinds, "Job"), o
		}
		if obj.GetLabels()[LabelQualifier] != "true" {
			t.Errorf("%T %s is not labelled", obj, obj.GetName())
		}
	}
	if want := []string{"ServiceAccount", "ClusterRole", "ClusterRoleBinding", "Role", "RoleBinding", "Role", "RoleBinding", "Job"}; !slices.Equal(kinds, want) {
		t.Fatalf("kinds = %v, want %v (the Job last, after its RBAC)", kinds, want)
	}
	sides := QualifierSides()
	for _, rule := range clusterRole.Rules {
		if slices.Contains(rule.Verbs, "impersonate") {
			want := []string{sides[0].User, sides[1].User}
			if rule.Resources[0] == "groups" {
				want = []string{sides[0].Group, sides[1].Group}
			}
			if !slices.Equal(rule.ResourceNames, want) {
				t.Fatalf("impersonation is not limited to the test identities: %+v", rule)
			}
		}
		if slices.Contains(rule.Resources, "secrets") || slices.Contains(rule.Verbs, "*") || slices.Contains(rule.Verbs, "escalate") || slices.Contains(rule.Verbs, "bind") {
			t.Fatalf("over-broad rule: %+v", rule)
		}
		if slices.Contains(rule.Verbs, "delete") && !slices.Equal(rule.ResourceNames, []string{sides[0].Tenant, sides[1].Tenant}) {
			t.Fatalf("delete is not limited to the test Tenants: %+v", rule)
		}
	}
	// The record is the controller's: the Job may write only its result.
	for _, rule := range roles[0].Rules {
		if slices.Contains(rule.Verbs, "update") && !slices.Equal(rule.ResourceNames, []string{QualifierResultName}) {
			t.Fatalf("the Job can update more than its result: %+v", rule)
		}
	}
	if job.Name != QualifierJobName(key, qualificationNow) || job.Labels[LabelQualificationKey] != key.Hash() || *job.Spec.BackoffLimit != 0 || job.Spec.ActiveDeadlineSeconds == nil {
		t.Fatalf("job = %+v", job.ObjectMeta)
	}
	pod := job.Spec.Template.Spec
	if pod.ServiceAccountName != QualifierName || pod.NodeSelector[crossplaneaws.PoolLabelKey] != crossplaneaws.PoolSystem || !*pod.SecurityContext.RunAsNonRoot {
		t.Fatalf("pod = %+v", pod)
	}
	args := pod.Containers[0].Args
	if args[0] != "--run" || !strings.HasPrefix(args[1], "--key=") || args[2] != "--probe-image=busybox:1.36.1" {
		t.Fatalf("args = %v", args)
	}
	var echoed ClusterQualificationKey
	if err := json.Unmarshal([]byte(strings.TrimPrefix(args[1], "--key=")), &echoed); err != nil || echoed != key {
		t.Fatalf("key arg = %v, %v", echoed, err)
	}
	if !strings.Contains(pod.Containers[0].Env[0].ValueFrom.FieldRef.FieldPath, "controller-uid") {
		t.Fatal("the runner does not learn its Job UID")
	}
	// Renewals of the same key are new Jobs.
	if QualifierJobName(key, qualificationNow) == QualifierJobName(key, qualificationNow.Add(time.Hour)) {
		t.Fatal("renewal reuses the Job name")
	}
	if len(QualifierJobName(key, qualificationNow)) > 63 {
		t.Fatal("job name is too long for its pod label")
	}
}

func TestRenderQualifierRefusesIncompleteInputs(t *testing.T) {
	for _, opts := range []QualifierOptions{
		{ProbeImage: "busybox", Key: qualificationKeyFixture()},
		{Image: "cv", Key: qualificationKeyFixture()},
		{Image: "cv", ProbeImage: "busybox"},
	} {
		if _, err := RenderQualifier(opts); err == nil {
			t.Fatalf("rendered %+v", opts)
		}
	}
}
