package cluster

import (
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/yaml"

	"github.com/yindia/cluster-vending/pkg/updatemask"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	reconcilecluster "github.com/yindia/cluster-vending/pkg/reconcile/cluster"
)

func TestCreateNaaSDefaultAndExplicitValues(t *testing.T) {
	for _, test := range []struct {
		name  string
		value *bool
	}{{"omitted", nil}, {"false", ptr.To(false)}, {"true", ptr.To(true)}} {
		t.Run(test.name, func(t *testing.T) {
			w := newWorld(t, interceptor.Funcs{}, nil)
			req := createRequest(w, "naas-"+test.name)
			req.Cluster.Spec.Naas = test.value
			out, err := w.svc.CreateCluster(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			if out.GetCluster().GetSpec().GetNaas() != ptr.Deref(test.value, false) {
				t.Fatalf("created spec: %v", out.GetCluster().GetSpec())
			}
			obj := &cloudv1alpha1.Cluster{}
			if err := w.storeKube.Get(t.Context(), client.ObjectKey{Namespace: worldNS, Name: req.Cluster.Name}, obj); err != nil {
				t.Fatal(err)
			}
			if ptr.Deref(obj.Spec.NaaS, false) != ptr.Deref(test.value, false) {
				t.Fatalf("CRD NaaS: %v", obj.Spec.NaaS)
			}
			asked, err := w.svc.GetCluster(t.Context(), &cloudv1.GetClusterRequest{TenantId: worldTenant, ClusterId: out.Cluster.Id, AsRequested: true})
			if err != nil {
				t.Fatal(err)
			}
			if (asked.Cluster.Spec.Naas == nil) != (test.value == nil) || asked.Cluster.Spec.GetNaas() != ptr.Deref(test.value, false) {
				t.Fatalf("input presence lost: %v", asked.Cluster.Spec)
			}
		})
	}
}

func TestNaaSUpdateMaskCanClearFalseAndDoesNotChangeUnmaskedValue(t *testing.T) {
	w := newWorld(t, interceptor.Funcs{}, nil)
	before := w.cluster(t)
	if _, err := w.svc.UpdateCluster(t.Context(), updateRequest(w, &cloudv1.ClusterSpec{Naas: ptr.To(true)}, "spec.naas")); err != nil {
		t.Fatal(err)
	}
	enabled := w.cluster(t)
	if enabled.Generation != before.Generation+1 || enabled.State != reconcilecluster.StateUpdating {
		t.Fatalf("update did not schedule reconciliation: %+v", enabled)
	}
	if _, err := w.svc.UpdateCluster(t.Context(), updateRequest(w, &cloudv1.ClusterSpec{Naas: ptr.To(false)}, "display_name")); err != nil {
		t.Fatal(err)
	}
	spec, err := reconcilecluster.SpecOf(w.cluster(t))
	if err != nil || !spec.GetNaas() {
		t.Fatalf("unmasked NaaS changed: %v %v", spec, err)
	}
	if _, err := w.svc.UpdateCluster(t.Context(), updateRequest(w, &cloudv1.ClusterSpec{Naas: ptr.To(false)}, "spec.naas")); err != nil {
		t.Fatal(err)
	}
	spec, err = reconcilecluster.SpecOf(w.cluster(t))
	if err != nil || spec.GetNaas() || spec.Naas == nil {
		t.Fatalf("explicit false not stored: %v %v", spec, err)
	}
	obj := &cloudv1alpha1.Cluster{}
	if err := w.storeKube.Get(t.Context(), client.ObjectKey{Namespace: worldNS, Name: w.row.Name}, obj); err != nil {
		t.Fatal(err)
	}
	if obj.Spec.NaaS == nil || *obj.Spec.NaaS {
		t.Fatalf("CRD explicit false missing: %v", obj.Spec.NaaS)
	}
}

func TestNaaSReviewedPlansDescribeInstallRemoveAndBindExactRequest(t *testing.T) {
	w := appliedWorld(t)
	for _, enabled := range []bool{true, false} {
		before := w.cluster(t)
		req := planRequest(w, &cloudv1.ClusterSpec{Naas: ptr.To(enabled)}, "spec.naas")
		req.Cluster.Etag = before.ETag
		out, err := w.svc.UpdateCluster(t.Context(), req)
		if err != nil {
			t.Fatal(err)
		}
		plan := out.GetPlan()
		action := cloudv1.PlannedAction_PLANNED_ACTION_CREATE
		if !enabled {
			action = cloudv1.PlannedAction_PLANNED_ACTION_DELETE
		}
		found := false
		for _, change := range plan.GetChanges() {
			if change.SpecPath == "spec.naas" {
				found = true
				if change.Action != action || change.Kind != "Release" || change.ExternalName != "capsule" || change.RetainsCloudResource {
					t.Fatalf("wrong Capsule change: %v", change)
				}
				if !enabled && (!strings.Contains(change.Detail, "turn off namespace tenancy controls") || !strings.Contains(change.Detail, "data are not deleted")) {
					t.Fatalf("missing uninstall impact: %v", change)
				}
			}
		}
		if !found || plan.GetPlanId() == "" || !plan.GetApplicable() {
			t.Fatalf("missing applicable Capsule plan: %v", plan)
		}
		if w.cluster(t).ETag != before.ETag {
			t.Fatal("preview mutated stored cluster")
		}
		altered := proto.CloneOf(req)
		altered.ValidateOnly = false
		altered.PlanId = plan.PlanId
		altered.Cluster.Spec.Naas = ptr.To(!enabled)
		if _, err := w.svc.UpdateCluster(t.Context(), altered); connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Fatalf("changed request reused plan: %v", err)
		}
		req.ValidateOnly = false
		req.PlanId = plan.PlanId
		if _, err := w.svc.UpdateCluster(t.Context(), req); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNaaSPlanBindsComponentOwnedReleaseVersion(t *testing.T) {
	w := appliedWorld(t)
	req := planRequest(w, &cloudv1.ClusterSpec{Naas: ptr.To(true)}, "spec.naas")
	preview, err := w.svc.UpdateCluster(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	release := &unstructured.Unstructured{}
	release.SetGroupVersionKind(crossplaneaws.HelmReleaseGVK)
	release.SetNamespace(worldNS)
	release.SetName(crossplaneaws.HelmReleaseObjectName(crossplaneaws.ClusterPrefix(worldTenant, w.row.ID, w.row.Name), "capsule"))
	if err := w.kube.Create(t.Context(), release); err != nil {
		t.Fatal(err)
	}
	req.ValidateOnly = false
	req.PlanId = preview.GetPlan().GetPlanId()
	if _, err := w.svc.UpdateCluster(t.Context(), req); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("changed Release accepted old plan: %v", err)
	}
}

func TestNaaSYAMLExplicitFalseInfersMaskButOmissionDoesNot(t *testing.T) {
	current := &cloudv1.Cluster{Spec: &cloudv1.ClusterSpec{Naas: ptr.To(true)}}
	for _, input := range []string{"spec:\n  naas: false\n", "spec: {}\n"} {
		raw, err := yaml.YAMLToJSON([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		desired := &cloudv1.Cluster{}
		if err := protojson.Unmarshal(raw, desired); err != nil {
			t.Fatal(err)
		}
		changes, err := updatemask.Diff(current, desired, updatemask.Cluster)
		if err != nil {
			t.Fatal(err)
		}
		paths := updatemask.Paths(changes)
		if desired.Spec.Naas != nil {
			if len(paths) != 1 || paths[0] != "spec.naas" {
				t.Fatalf("explicit false mask: %v", paths)
			}
		} else if len(paths) != 0 {
			t.Fatalf("omitted NaaS changed: %v", paths)
		}
	}
}

func TestNaaSPlanWarnsWhenPlatformReleaseMoveMustFinish(t *testing.T) {
	for _, phase := range []cloudv1.ClusterReleaseMovePhase{cloudv1.ClusterReleaseMovePhase_CLUSTER_RELEASE_MOVE_PHASE_COMPONENTS, cloudv1.ClusterReleaseMovePhase_CLUSTER_RELEASE_MOVE_PHASE_HALTED} {
		t.Run(phase.String(), func(t *testing.T) {
			w := newWorld(t, interceptor.Funcs{}, nil)
			row := w.cluster(t)
			status := &cloudv1.ClusterStatus{ReleaseMove: &cloudv1.ClusterReleaseMove{Phase: phase, Current: "old-release", Target: "new-release", Step: 1, Steps: 2}}
			raw, err := protojson.Marshal(status)
			if err != nil {
				t.Fatal(err)
			}
			row.Status = raw
			plan := &cloudv1.Plan{Applicable: true}
			if _, err := w.svc.planNaaS(t.Context(), row, &cloudv1.ClusterSpec{Naas: ptr.To(true)}, plan); err != nil {
				t.Fatal(err)
			}
			warnings := strings.Join(plan.Warnings, " ")
			if !strings.Contains(warnings, "after the current platform upgrade finishes") || !strings.Contains(warnings, "halted upgrade must be resolved first") {
				t.Fatalf("missing release move warning: %v", plan.Warnings)
			}
		})
	}
}
