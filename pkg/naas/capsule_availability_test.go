package naas

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/yindia/cluster-vending/pkg/kube"
)

func capsuleAvailabilityClient(t *testing.T) client.Client {
	t.Helper()
	data, err := os.ReadFile("testdata/capsule-serving.yaml")
	if err != nil {
		t.Fatal(err)
	}
	scheme, err := kube.Scheme()
	if err != nil {
		t.Fatal(err)
	}
	decoder := yaml.NewYAMLOrJSONDecoder(bytes.NewReader(data), 4096)
	var objects []client.Object
	for {
		var raw runtime.RawExtension
		if err := decoder.Decode(&raw); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		obj, _, err := serializer.NewCodecFactory(scheme).UniversalDeserializer().Decode(raw.Raw, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		objects = append(objects, obj.(client.Object))
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestCapsuleAvailabilityDuringRollingUpgrade(t *testing.T) {
	if err := ObserveCapsuleAvailability(t.Context(), capsuleAvailabilityClient(t)); err != nil {
		t.Fatal(err)
	}
}

func TestCapsuleAvailabilityFailsClosed(t *testing.T) {
	for _, tt := range []struct {
		name   string
		object client.Object
		mutate func(client.Object)
	}{
		{"no available replicas", &appsv1.Deployment{}, func(o client.Object) { o.(*appsv1.Deployment).Status.AvailableReplicas = 0 }},
		{"untrusted controller account", &appsv1.Deployment{}, func(o client.Object) { o.(*appsv1.Deployment).Spec.Template.Spec.ServiceAccountName = "untrusted" }},
		{"foreign ReplicaSet", &appsv1.ReplicaSet{}, func(o client.Object) { o.GetOwnerReferences()[0].UID = "other-deployment" }},
		{"foreign Pod UID", &corev1.Pod{}, func(o client.Object) { o.SetUID("replaced-pod") }},
		{"unready Pod", &corev1.Pod{}, func(o client.Object) { o.(*corev1.Pod).Status.Conditions[0].Status = corev1.ConditionFalse }},
		{"untrusted Pod account", &corev1.Pod{}, func(o client.Object) { o.(*corev1.Pod).Spec.ServiceAccountName = "untrusted" }},
		{"wrong Service selector", &corev1.Service{}, func(o client.Object) { o.(*corev1.Service).Spec.Selector = map[string]string{"foreign": "true"} }},
		{"unready endpoint", &discoveryv1.EndpointSlice{}, func(o client.Object) { o.(*discoveryv1.EndpointSlice).Endpoints[0].Conditions.Ready = ptr.To(false) }},
		{"terminating endpoint", &discoveryv1.EndpointSlice{}, func(o client.Object) {
			o.(*discoveryv1.EndpointSlice).Endpoints[0].Conditions.Terminating = ptr.To(true)
		}},
		{"foreign endpoint Service UID", &discoveryv1.EndpointSlice{}, func(o client.Object) { o.GetOwnerReferences()[0].UID = "foreign-service" }},
		{"missing CA", &admissionv1.ValidatingWebhookConfiguration{}, func(o client.Object) {
			o.(*admissionv1.ValidatingWebhookConfiguration).Webhooks[0].ClientConfig.CABundle = nil
		}},
		{"foreign webhook", &admissionv1.ValidatingWebhookConfiguration{}, func(o client.Object) {
			o.(*admissionv1.ValidatingWebhookConfiguration).Webhooks[0].ClientConfig.Service.Namespace = "other"
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := capsuleAvailabilityClient(t)
			switch tt.object.(type) {
			case *appsv1.Deployment:
				tt.object.SetName("capsule-controller-manager")
			case *appsv1.ReplicaSet:
				tt.object.SetName("capsule-old")
			case *corev1.Pod:
				tt.object.SetName("capsule-old-serving")
			case *corev1.Service:
				tt.object.SetName("capsule-webhook-service")
			case *discoveryv1.EndpointSlice:
				tt.object.SetName("capsule-webhook-serving")
			case *admissionv1.ValidatingWebhookConfiguration:
				tt.object.SetName("capsule-validating-webhook-configuration")
			}
			if _, clusterScoped := tt.object.(*admissionv1.ValidatingWebhookConfiguration); !clusterScoped {
				tt.object.SetNamespace("capsule-system")
			}
			if err := c.Get(t.Context(), client.ObjectKeyFromObject(tt.object), tt.object); err != nil {
				t.Fatal(err)
			}
			tt.mutate(tt.object)
			var err error
			switch tt.object.(type) {
			case *appsv1.Deployment, *corev1.Pod:
				err = c.Status().Update(t.Context(), tt.object)
				// Fake status subresources preserve spec, so update spec separately too.
			default:
				err = c.Update(t.Context(), tt.object)
			}
			if err != nil {
				t.Fatal(err)
			}
			tt.mutate(tt.object)
			if err = c.Update(t.Context(), tt.object); err != nil {
				t.Fatal(err)
			}
			if err = ObserveCapsuleAvailability(t.Context(), c); err == nil {
				t.Fatal("unavailable or foreign runtime accepted")
			}
		})
	}
}

func TestCapsuleAvailabilityReadError(t *testing.T) {
	c := capsuleAvailabilityClient(t)
	broken := &capsuleUnavailableReader{Reader: c}
	if err := ObserveCapsuleAvailability(t.Context(), broken); err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("read error not propagated: %v", err)
	}
}

type capsuleUnavailableReader struct{ client.Reader }

func (r *capsuleUnavailableReader) List(_ context.Context, _ client.ObjectList, _ ...client.ListOption) error {
	return errors.New("access denied")
}
