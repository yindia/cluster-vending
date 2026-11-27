package cloudstore

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/yindia/cluster-vending/config/crd"
	"github.com/yindia/cluster-vending/pkg/buildserial"
)

// Startup readiness must wait for every embedded platform CRD to be served.
// Otherwise a newly added kind can fail requests after /readyz becomes healthy.
func TestStartupKindsCoverEmbeddedDefinitions(t *testing.T) {
	definitions, err := crd.All()
	if err != nil {
		t.Fatal(err)
	}
	wanted := make(map[string]bool, len(definitions))
	for _, definition := range definitions {
		kind, _, err := unstructured.NestedString(definition.Object, "spec", "names", "kind")
		if err != nil {
			t.Fatal(err)
		}
		wanted[kind] = true
	}
	for _, gvk := range Kinds() {
		if !wanted[gvk.Kind] {
			t.Errorf("startup waits for unknown or duplicate kind %s", gvk)
		}
		delete(wanted, gvk.Kind)
	}
	for kind := range wanted {
		t.Errorf("startup does not wait for embedded kind %s", kind)
	}
}

// installWorld is a management cluster whose CRDs are existing, and the
// names of the CRDs Install applies.
func installWorld(t *testing.T, existing ...*unstructured.Unstructured) (client.Client, *[]string) {
	t.Helper()
	applied := &[]string{}
	objects := make([]client.Object, 0, len(existing))
	for _, e := range existing {
		objects = append(objects, e)
	}
	c := fake.NewClientBuilder().WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
		Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
			raw, err := json.Marshal(obj)
			if err != nil {
				return err
			}
			u := &unstructured.Unstructured{}
			if err := u.UnmarshalJSON(raw); err != nil {
				return err
			}
			if buildserial.Of(u.GetAnnotations()) != buildserial.Serial {
				t.Errorf("applied %s without the build's serial", u.GetName())
			}
			*applied = append(*applied, u.GetName())
			return nil
		},
	}).Build()
	return c, applied
}

func crdNamed(t *testing.T, name string, serial int64) *unstructured.Unstructured {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetAPIVersion("apiextensions.k8s.io/v1")
	u.SetKind("CustomResourceDefinition")
	u.SetName(name)
	if serial > 0 {
		u.SetAnnotations(map[string]string{buildserial.Annotation: strconv.FormatInt(serial, 10)})
	}
	return u
}

// Install stamps every CRD with the build's serial, and does not apply one
// a newer build installed (a rollback, an old pod): its older schema would
// prune the newer fields. The refusal fails the ready check, naming it.
func TestInstallRefusesToDowngradeACRD(t *testing.T) {
	t.Cleanup(buildserial.ResetForTest)
	definitions, err := crd.All()
	if err != nil {
		t.Fatal(err)
	}
	if got := definitions[0].GetAnnotations()[buildserial.Annotation]; got != "" {
		t.Fatalf("the embedded CRD carries a serial already: %q", got)
	}
	newer, older := definitions[0].GetName(), definitions[1].GetName()
	c, applied := installWorld(t, crdNamed(t, newer, buildserial.Serial+1), crdNamed(t, older, buildserial.Serial-1))

	if err := Install(t.Context(), c); err != nil {
		t.Fatal(err)
	}
	if slices.Contains(*applied, newer) {
		t.Fatalf("applied %s over a newer build's", newer)
	}
	if !slices.Contains(*applied, older) || len(*applied) != len(definitions)-1 {
		t.Fatalf("applied %v; want every CRD but %s", *applied, newer)
	}
	if err := buildserial.Check(nil); err == nil || !strings.Contains(err.Error(), newer) {
		t.Fatalf("ready check = %v; want it to name %s", err, newer)
	}
}

// What Install applies carries this build's serial.
func TestInstallStampsTheSerial(t *testing.T) {
	definitions, err := crd.All()
	if err != nil {
		t.Fatal(err)
	}
	stamped := stampedCRD(definitions[0])
	if got := buildserial.Of(stamped.GetAnnotations()); got != buildserial.Serial {
		t.Fatalf("serial %d, want %d", got, buildserial.Serial)
	}
	if definitions[0].GetAnnotations()[buildserial.Annotation] != "" {
		t.Fatal("stampedCRD changed its input")
	}
}
