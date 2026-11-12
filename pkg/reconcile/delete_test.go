package reconcile

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/pkg/database/model"
	"github.com/yindia/cluster-vending/pkg/kube"
)

func TestDeleteRecorded(t *testing.T) {
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	var deleted []string
	k := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Delete: func(_ context.Context, _ client.WithWatch, obj client.Object, _ ...client.DeleteOption) error {
			deleted = append(deleted, obj.GetName())
			switch obj.GetName() {
			case "role": // in use by the cluster: the Usage webhook refuses
				return apierrors.NewConflict(schema.GroupResource{Resource: "roles"}, "role", errors.New("in use"))
			case "broken":
				return errors.New("connection refused")
			}
			return nil
		},
	}).Build()
	rec := func(kind, name string) model.ManagedResource {
		return model.ManagedResource{APIVersion: "x/v1", Kind: kind, Namespace: "t", Name: name}
	}
	records := []model.ManagedResource{ // creation order
		rec("Role", "role"), rec("Role", "broken"), rec("Cluster", "eks"), rec("NodeGroup", "ng"),
		rec("Usage", "usage-eks-by-ng"),
	}
	err = Deps{Kube: k}.DeleteRecorded(context.Background(), records)

	if got := strings.Join(deleted, ","); got != "ng,eks,broken,role" {
		t.Errorf("deleted %s, want ng,eks,broken,role (reverse order, Usage skipped, continuing past errors)", got)
	}
	if err == nil || !strings.Contains(err.Error(), "broken") || strings.Contains(err.Error(), "role role") {
		t.Errorf("err = %v, want only the non-409 failure", err)
	}
}
