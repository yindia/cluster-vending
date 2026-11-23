package naas

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// probeConnectTimeout is each network probe's own timeout, in seconds.
const probeConnectTimeout = 5

// QualificationSuite is the eleven required checks, in suite order.
func QualificationSuite() []QualificationCheck {
	return []QualificationCheck{
		{CheckNamespaceRBAC, checkNamespaceRBAC},
		{CheckCrossNamespaceRBAC, checkCrossNamespaceRBAC},
		{CheckAdmissionGuards, checkAdmissionGuards},
		{CheckAmbientMTLS, checkAmbientMTLS},
		{CheckCrossSubTenant, checkCrossSubTenant},
		{CheckDNS, checkDNS},
		{CheckEgressDeny, checkEgressDeny},
		{CheckRestrictedPSS, checkRestrictedPSS},
		{CheckSharedQuota, checkSharedQuota},
		{CheckGroupDelegation, checkGroupDelegation},
		{CheckAdmissionPoolOutage, checkAdmissionPoolOutage},
	}
}

// namespace-rbac: a test group may manage workloads in its own namespace.
func checkNamespaceRBAC(ctx context.Context, env *QualifierEnv) error {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "qual-rbac", Namespace: env.Sides[0].Namespace}, Data: map[string]string{"probe": "ok"}}
	if err := env.As[0].Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("%s cannot create a ConfigMap in its own namespace: %w", env.Sides[0].Group, err)
	}
	if err := env.As[0].Delete(ctx, cm); err != nil {
		return fmt.Errorf("%s cannot delete a ConfigMap in its own namespace: %w", env.Sides[0].Group, err)
	}
	return nil
}

// cross-namespace-rbac: a test group reaches neither the other sub-tenant's
// namespace nor the protected platform namespace.
func checkCrossNamespaceRBAC(ctx context.Context, env *QualifierEnv) error {
	for _, probe := range []struct {
		side      int
		namespace string
	}{{0, env.Sides[1].Namespace}, {1, env.Sides[0].Namespace}, {0, QualificationNamespace}} {
		err := env.As[probe.side].List(ctx, &corev1.ConfigMapList{}, client.InNamespace(probe.namespace))
		if err := mustBeForbidden(err, fmt.Sprintf("%s listing ConfigMaps in %s", env.Sides[probe.side].Group, probe.namespace)); err != nil {
			return err
		}
	}
	return nil
}

// admission-guards: Capsule refuses Services other than ClusterIP and
// external IPs.
func checkAdmissionGuards(ctx context.Context, env *QualifierEnv) error {
	ns := env.Sides[0].Namespace
	for name, spec := range map[string]corev1.ServiceSpec{
		"qual-lb":       {Type: corev1.ServiceTypeLoadBalancer, Ports: []corev1.ServicePort{{Port: 80}}},
		"qual-nodeport": {Type: corev1.ServiceTypeNodePort, Ports: []corev1.ServicePort{{Port: 80}}},
		"qual-external": {Type: corev1.ServiceTypeClusterIP, ExternalIPs: []string{"203.0.113.10"}, Ports: []corev1.ServicePort{{Port: 80}}},
	} {
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Spec: spec}
		if err := mustBeRefused(ctx, env.As[0], svc, "a "+name+" Service"); err != nil {
			return err
		}
	}
	return nil
}

// ambient-mtls-hbone: the namespace is in the ambient mesh with strict mTLS,
// and traffic within the sub-tenant flows.
func checkAmbientMTLS(ctx context.Context, env *QualifierEnv) error {
	side := env.Sides[0]
	ns := &corev1.Namespace{}
	if err := env.Admin.Get(ctx, client.ObjectKey{Name: side.Namespace}, ns); err != nil {
		return err
	}
	if ns.Labels["istio.io/dataplane-mode"] != "ambient" {
		return fmt.Errorf("namespace %s is not in the ambient mesh", side.Namespace)
	}
	list := &unstructured.UnstructuredList{}
	list.SetAPIVersion("security.istio.io/v1")
	list.SetKind("PeerAuthenticationList")
	if err := env.Admin.List(ctx, list, client.InNamespace(side.Namespace)); err != nil {
		return fmt.Errorf("read PeerAuthentications in %s: %w", side.Namespace, err)
	}
	strict := slices.ContainsFunc(list.Items, func(p unstructured.Unstructured) bool {
		mode, _, _ := unstructured.NestedString(p.Object, "spec", "mtls", "mode")
		return mode == "STRICT"
	})
	if !strict {
		return fmt.Errorf("no STRICT PeerAuthentication in %s", side.Namespace)
	}
	return env.positiveControl(ctx)
}

// cross-subtenant-traffic: neither test sub-tenant reaches the other's server.
func checkCrossSubTenant(ctx context.Context, env *QualifierEnv) error {
	if err := env.positiveControl(ctx); err != nil {
		return err
	}
	for side, target := range map[int]int{0: 1, 1: 0} {
		if err := env.expect(ctx, side, "qual-cross", fetch(env.Sides[target].Namespace), false); err != nil {
			return err
		}
	}
	return nil
}

// dns-udp-tcp: cluster DNS answers over UDP, and its TCP port is reachable.
func checkDNS(ctx context.Context, env *QualifierEnv) error {
	script := fmt.Sprintf("nslookup kubernetes.default.svc.cluster.local && nc -z -w %d kube-dns.kube-system.svc.cluster.local 53", probeConnectTimeout)
	return env.expect(ctx, 0, "qual-dns", script, true)
}

// egress-default-deny: nothing outside the cluster is reachable.
func checkEgressDeny(ctx context.Context, env *QualifierEnv) error {
	if err := env.positiveControl(ctx); err != nil {
		return err
	}
	return env.expect(ctx, 0, "qual-egress", fmt.Sprintf("nc -z -w %d 1.1.1.1 443", probeConnectTimeout), false)
}

// restricted-pss: a privileged pod is refused.
func checkRestrictedPSS(ctx context.Context, env *QualifierEnv) error {
	pod := probePod("probe", env.Sides[0].Namespace, "qual-privileged", "true")
	pod.Spec.SecurityContext = nil
	pod.Spec.Containers[0].SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
	return mustBeRefused(ctx, env.As[0], pod, "a privileged pod")
}

// shared-quota: the sub-tenant's budget is projected into its namespace.
func checkSharedQuota(ctx context.Context, env *QualifierEnv) error {
	quotas := &corev1.ResourceQuotaList{}
	if err := env.As[0].List(ctx, quotas, client.InNamespace(env.Sides[0].Namespace)); err != nil {
		return fmt.Errorf("read ResourceQuotas in %s: %w", env.Sides[0].Namespace, err)
	}
	want := resource.MustParse(qualifierQuotaPods)
	for _, q := range quotas.Items {
		if pods, ok := q.Spec.Hard[corev1.ResourcePods]; ok && pods.Cmp(want) == 0 {
			return nil
		}
	}
	return fmt.Errorf("no ResourceQuota in %s carries the sub-tenant budget (pods=%s)", env.Sides[0].Namespace, qualifierQuotaPods)
}

// group-delegation-approved: only the approved group is bound, only to the
// approved roles, and the group cannot grant itself more.
func checkGroupDelegation(ctx context.Context, env *QualifierEnv) error {
	for i, side := range env.Sides {
		other := env.Sides[1-i]
		bindings := &rbacv1.RoleBindingList{}
		if err := env.Admin.List(ctx, bindings, client.InNamespace(side.Namespace)); err != nil {
			return fmt.Errorf("read RoleBindings in %s: %w", side.Namespace, err)
		}
		found := false
		for _, b := range bindings.Items {
			for _, s := range b.Subjects {
				if s.Name == other.Group {
					return fmt.Errorf("%s is bound in %s", other.Group, side.Namespace)
				}
				if s.Kind == rbacv1.GroupKind && s.Name == side.Group {
					if b.RoleRef.Kind != "ClusterRole" || (b.RoleRef.Name != WorkloadAdminRole && b.RoleRef.Name != WorkloadViewerRole) || len(b.Subjects) != 1 {
						return fmt.Errorf("binding %s/%s is not an approved delegation", side.Namespace, b.Name)
					}
					found = true
				}
			}
		}
		if !found {
			return fmt.Errorf("%s has no approved binding in %s", side.Group, side.Namespace)
		}
	}
	escalate := &rbacv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "qual-escalate", Namespace: env.Sides[0].Namespace},
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "cluster-admin"},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.GroupKind, APIGroup: rbacv1.GroupName, Name: env.Sides[0].Group}}}
	return mustBeRefused(ctx, env.As[0], escalate, "a RoleBinding to cluster-admin")
}

// admission-platform-pool-outage (PROP-05 decision 7): Capsule's fail-closed
// webhooks are served from the bootstrap group, so they stay available when
// the platform pool is down.
func checkAdmissionPoolOutage(ctx context.Context, env *QualifierEnv) error {
	svc := &corev1.Service{}
	if err := env.Admin.Get(ctx, client.ObjectKey{Namespace: CapsuleNamespace, Name: CapsuleWebhookService}, svc); err != nil {
		return fmt.Errorf("read the Capsule webhook Service: %w", err)
	}
	if len(svc.Spec.Selector) == 0 {
		return fmt.Errorf("the Capsule webhook Service has no selector")
	}
	pods := &corev1.PodList{}
	if err := env.Admin.List(ctx, pods, client.InNamespace(CapsuleNamespace), client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(svc.Spec.Selector)}); err != nil {
		return fmt.Errorf("read the Capsule webhook pods: %w", err)
	}
	ready := 0
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !podReady(pod) {
			continue
		}
		node := &corev1.Node{}
		if err := env.Admin.Get(ctx, client.ObjectKey{Name: pod.Spec.NodeName}, node); err != nil {
			return fmt.Errorf("read node %q of %s: %w", pod.Spec.NodeName, pod.Name, err)
		}
		if pool := node.Labels[crossplaneaws.PoolLabelKey]; pool != crossplaneaws.PoolSystem {
			return fmt.Errorf("capsule webhook pod %s runs on node %s of pool %q, not %q", pod.Name, node.Name, pool, crossplaneaws.PoolSystem)
		}
		ready++
	}
	if ready == 0 {
		return fmt.Errorf("no Ready Capsule webhook pod")
	}
	return nil
}

// positiveControl: side 0 reaches its own server. Without it a probe that
// cannot run at all would pass every deny check.
func (env *QualifierEnv) positiveControl(ctx context.Context) error {
	if env.control == nil {
		err := env.expect(ctx, 0, "qual-control", fetch(env.Sides[0].Namespace), true)
		if err != nil {
			err = fmt.Errorf("positive control: %w", err)
		}
		env.control = &err
	}
	return *env.control
}

func (env *QualifierEnv) expect(ctx context.Context, side int, name, script string, succeed bool) error {
	ok, err := env.probe(ctx, side, name, script)
	if err != nil {
		return fmt.Errorf("probe %s: %w", name, err)
	}
	if ok != succeed {
		verb := "failed"
		if ok {
			verb = "succeeded"
		}
		return fmt.Errorf("probe %s in %s %s: %s", name, env.Sides[side].Namespace, verb, script)
	}
	return nil
}

func fetch(namespace string) string {
	return fmt.Sprintf("wget -q -T %d -O- http://%s.%s.svc.cluster.local:%d/", probeConnectTimeout, qualifierServerName, namespace, qualifierServerPort)
}

// mustBeForbidden passes on an RBAC refusal only.
func mustBeForbidden(err error, what string) error {
	switch {
	case err == nil:
		return fmt.Errorf("%s was allowed", what)
	case apierrors.IsForbidden(err):
		return nil
	default:
		return fmt.Errorf("%s: expected Forbidden: %w", what, err)
	}
}

// mustBeRefused creates obj and passes when the API refuses it (RBAC or
// admission). An accepted object is deleted again, and fails the check.
func mustBeRefused(ctx context.Context, c client.Client, obj client.Object, what string) error {
	err := c.Create(ctx, obj)
	if err == nil {
		cleanup := c.Delete(ctx, obj)
		return errors.Join(fmt.Errorf("%s was admitted in %s", what, obj.GetNamespace()), cleanup)
	}
	var status apierrors.APIStatus
	if errors.As(err, &status) && status.Status().Code >= 400 && status.Status().Code < 500 && !apierrors.IsAlreadyExists(err) && !apierrors.IsNotFound(err) {
		return nil
	}
	return fmt.Errorf("%s: expected a refusal: %w", what, err)
}

// probePod is a restricted-PSS pod that runs script once.
func probePod(image, namespace, name, script string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: map[string]string{LabelQualifier: "probe"}},
		Spec: corev1.PodSpec{
			RestartPolicy:                 corev1.RestartPolicyNever,
			AutomountServiceAccountToken:  ptr.To(false),
			TerminationGracePeriodSeconds: ptr.To[int64](1),
			SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To[int64](65534),
				SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
			Containers: []corev1.Container{{Name: "probe", Image: image, Command: []string{"sh", "-c", script},
				SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}}}},
		}}
}

func podReady(pod *corev1.Pod) bool {
	if pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// PodProbe runs each probe as a pod of image and waits for it to finish.
// The pod is deleted afterwards; the namespace goes with the test Tenant.
func PodProbe(image string, poll time.Duration) ProbeFunc {
	return func(ctx context.Context, c client.Client, namespace, name, script string) (bool, error) {
		pod := probePod(image, namespace, name, script)
		if err := c.Create(ctx, pod); err != nil {
			return false, fmt.Errorf("create probe pod: %w", err)
		}
		defer func() {
			// Best effort: a probe pod left behind is removed with its
			// namespace when the test Tenant is deleted.
			_ = c.Delete(context.WithoutCancel(ctx), pod)
		}()
		ticker := time.NewTicker(poll)
		defer ticker.Stop()
		for {
			current := &corev1.Pod{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(pod), current); err != nil {
				return false, fmt.Errorf("read probe pod: %w", err)
			}
			switch current.Status.Phase {
			case corev1.PodSucceeded:
				return true, nil
			case corev1.PodFailed:
				return false, nil
			}
			select {
			case <-ctx.Done():
				return false, fmt.Errorf("probe pod did not finish: %w", ctx.Err())
			case <-ticker.C:
			}
		}
	}
}
