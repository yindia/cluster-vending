package live

import (
	"context"
	"fmt"

	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	awseks "github.com/upbound/provider-aws/v2/apis/namespaced/eks/v1beta1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	"github.com/yindia/cluster-vending/pkg/database/cloudstore"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// userAgent names the live reader in the workload clusters' audit logs.
const userAgent = "cluster-vending-live"

// KubeOpener resolves a cluster from the management cluster: its Cluster
// object (by ID, in the tenant's namespace, with the tenant's label), then
// the live identity of PROP-02 7.1: the "<prefix>-live-auth" ClusterAuth,
// Ready, and the Secret "<prefix>-live-kubeconfig" it writes. It reads no
// other Secret: without the live identity a cluster has no live reads, and
// the controller's kubeconfig is never a fallback.
type KubeOpener struct {
	Management client.Reader
	Limits     Limits
	// NewLister builds the cluster's Lister from its live kubeconfig; nil is
	// NewDynamicLister.
	NewLister func(kubeconfig []byte, lim Limits) (Lister, error)
}

var errNoIdentity = &Error{Reason: ReasonNoLiveIdentity, Message: MessageNoLiveIdentity}

// Open is the Lister of the tenant's cluster.
func (o *KubeOpener) Open(ctx context.Context, tenantID, clusterID string) (Lister, error) {
	ns := kube.TenantNamespace(tenantID)
	list := &cloudv1alpha1.ClusterList{}
	if err := o.Management.List(ctx, list, client.InNamespace(ns), client.MatchingLabels{cloudstore.LabelID: clusterID}); err != nil {
		return nil, fmt.Errorf("read the cluster %s from the management cluster: %w", clusterID, err)
	}
	if len(list.Items) != 1 || list.Items[0].Labels[cloudstore.LabelTenant] != tenantID {
		return nil, newError(ReasonUnreachable, "no cluster")
	}
	prefix := crossplaneaws.ClusterPrefix(tenantID, clusterID, list.Items[0].Name)
	auth := &awseks.ClusterAuth{}
	err := o.Management.Get(ctx, client.ObjectKey{Namespace: ns, Name: crossplaneaws.LiveClusterAuthName(prefix)}, auth)
	switch {
	case apierrors.IsNotFound(err):
		return nil, errNoIdentity
	case err != nil:
		return nil, fmt.Errorf("read the live ClusterAuth of %s: %w", clusterID, err)
	case auth.GetCondition(xpv2.TypeReady).Status != corev1.ConditionTrue:
		return nil, errNoIdentity
	}
	secret := &corev1.Secret{}
	err = o.Management.Get(ctx, client.ObjectKey{Namespace: ns, Name: crossplaneaws.LiveKubeconfigSecretName(prefix)}, secret)
	switch {
	case apierrors.IsNotFound(err):
		return nil, errNoIdentity
	case err != nil:
		return nil, fmt.Errorf("read the live kubeconfig of %s: %w", clusterID, err)
	}
	kubeconfig := secret.Data[crossplaneaws.KubeconfigSecretKey]
	if len(kubeconfig) == 0 {
		return nil, errNoIdentity
	}
	newLister := o.NewLister
	if newLister == nil {
		newLister = NewDynamicLister
	}
	l, err := newLister(kubeconfig, o.Limits)
	if err != nil {
		return nil, newError(ReasonUnreachable, "the live kubeconfig cannot be used: %v", err)
	}
	return l, nil
}

// NewDynamicLister is a Lister over the kubeconfig, with the cluster's
// client-side limiter (QPS, Burst) and the build deadline as its timeout.
// It lists JSON: the unstructured client has no protobuf encoding.
func NewDynamicLister(kubeconfig []byte, lim Limits) (Lister, error) {
	cfg, err := clientcmd.RESTConfigFromKubeConfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	cfg.QPS, cfg.Burst, cfg.Timeout, cfg.UserAgent = lim.QPS, lim.Burst, lim.NodesDeadline, userAgent
	c, err := dynamic.NewForConfig(cfg)
	if err != nil {
		return nil, err
	}
	return dynamicLister{c: c}, nil
}

type dynamicLister struct{ c dynamic.Interface }

func (d dynamicLister) List(ctx context.Context, gvr schema.GroupVersionResource, limit int64, cont string) (*unstructured.UnstructuredList, error) {
	return d.c.Resource(gvr).List(ctx, metav1.ListOptions{Limit: limit, Continue: cont})
}
