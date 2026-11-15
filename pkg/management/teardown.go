package management

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/deprecation"
	"github.com/yindia/cluster-vending/pkg/reconcile"
)

// albController is the IngressClass controller of the AWS Load Balancer
// Controller; albClass the class name it serves by default (also the old
// kubernetes.io/ingress.class annotation's value).
const (
	albController          = "ingress.k8s.aws/alb"
	albClass               = "alb"
	ingressClassAnnotation = "kubernetes.io/ingress.class"
)

// Karpenter's kinds (karpenter.sh v1, karpenter.k8s.aws v1), read as
// unstructured: the platform does not import Karpenter's API.
var karpenterKinds = []struct {
	kind string
	gvk  schema.GroupVersionKind
}{
	{reconcile.KindNodePool, schema.GroupVersionKind{Group: "karpenter.sh", Version: "v1", Kind: "NodePool"}},
	{reconcile.KindNodeClaim, schema.GroupVersionKind{Group: "karpenter.sh", Version: "v1", Kind: "NodeClaim"}},
	{reconcile.KindEC2NodeClass, schema.GroupVersionKind{Group: "karpenter.k8s.aws", Version: "v1", Kind: "EC2NodeClass"}},
}

// Teardowns is reconcile.Teardowns with the kubeconfig Secrets the
// management cluster holds, read with c (as Workloads).
func Teardowns(c client.Reader) reconcile.Teardowns { return teardowns{c: c} }

type teardowns struct{ c client.Reader }

func (t teardowns) Open(ctx context.Context, namespace, secretRef string) (reconcile.Teardown, error) {
	kubeconfig, err := reconcile.ReadKubeconfig(ctx, t.c, namespace, secretRef)
	if err != nil {
		return nil, err
	}
	cfg, err := deprecation.RESTConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := networkingv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("workload cluster client: %w", err)
	}
	return TeardownOf(c), nil
}

// TeardownOf is the Teardown of the cluster c talks to.
func TeardownOf(c client.Client) reconcile.Teardown { return teardown{c: c} }

type teardown struct{ c client.Client }

func (t teardown) List(ctx context.Context) ([]reconcile.WorkloadObject, error) {
	ingresses, err := t.albIngresses(ctx)
	if err != nil {
		return nil, err
	}
	out := ingresses
	services := &corev1.ServiceList{}
	if err := t.c.List(ctx, services); err != nil {
		return nil, fmt.Errorf("list services: %w", err)
	}
	for i := range services.Items {
		s := &services.Items[i]
		if s.Spec.Type == corev1.ServiceTypeLoadBalancer {
			out = append(out, workloadObject(reconcile.KindService, s))
		}
	}
	for _, k := range karpenterKinds {
		list := &unstructured.UnstructuredList{}
		list.SetGroupVersionKind(k.gvk.GroupVersion().WithKind(k.gvk.Kind + "List"))
		err := t.c.List(ctx, list)
		if meta.IsNoMatchError(err) {
			continue // Karpenter is not installed
		}
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", k.kind, err)
		}
		for i := range list.Items {
			out = append(out, workloadObject(k.kind, &list.Items[i]))
		}
	}
	return out, nil
}

// albIngresses are the Ingresses the AWS Load Balancer Controller serves:
// of an IngressClass whose controller it is (alb by default), or named by
// the old class annotation.
func (t teardown) albIngresses(ctx context.Context) ([]reconcile.WorkloadObject, error) {
	classes := map[string]bool{albClass: true}
	list := &networkingv1.IngressClassList{}
	if err := t.c.List(ctx, list); err != nil && !meta.IsNoMatchError(err) {
		return nil, fmt.Errorf("list ingress classes: %w", err)
	}
	for _, c := range list.Items {
		if c.Spec.Controller == albController {
			classes[c.Name] = true
		}
	}
	ingresses := &networkingv1.IngressList{}
	if err := t.c.List(ctx, ingresses); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list ingresses: %w", err)
	}
	var out []reconcile.WorkloadObject
	for i := range ingresses.Items {
		in := &ingresses.Items[i]
		class := in.Annotations[ingressClassAnnotation]
		if in.Spec.IngressClassName != nil {
			class = *in.Spec.IngressClassName
		}
		if classes[class] {
			out = append(out, workloadObject(reconcile.KindIngress, in))
		}
	}
	return out, nil
}

func (t teardown) Delete(ctx context.Context, o reconcile.WorkloadObject) error {
	var obj client.Object
	switch o.Kind {
	case reconcile.KindIngress:
		obj = &networkingv1.Ingress{}
	case reconcile.KindService:
		obj = &corev1.Service{}
	default:
		u := &unstructured.Unstructured{}
		for _, k := range karpenterKinds {
			if k.kind == o.Kind {
				u.SetGroupVersionKind(k.gvk)
			}
		}
		if u.GetKind() == "" {
			return fmt.Errorf("teardown: unknown kind %s", o.Kind)
		}
		obj = u
	}
	obj.SetNamespace(o.Namespace)
	obj.SetName(o.Name)
	return client.IgnoreNotFound(t.c.Delete(ctx, obj))
}

func workloadObject(kind string, o client.Object) reconcile.WorkloadObject {
	return reconcile.WorkloadObject{Kind: kind, Namespace: o.GetNamespace(), Name: o.GetName(),
		Deleting: o.GetDeletionTimestamp() != nil}
}
