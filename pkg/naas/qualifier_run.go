package naas

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
)

// Timing of a run; a QualifierRunner may shorten them (tests).
const (
	defaultQualifierPoll  = 2 * time.Second
	defaultSetupTimeout   = 10 * time.Minute
	defaultProbeTimeout   = 2 * time.Minute
	qualifierServerName   = "qual-server"
	qualifierServerPort   = 8080
	qualifierQuotaPods    = "10"
	qualifierTestTenantID = "naas-qualifier"
)

// QualifierClients are the runner's two kinds of identity: its own
// ServiceAccount (Admin), and the two test identities it impersonates.
type QualifierClients struct {
	Admin client.Client
	As    func(side QualifierSide) (client.Client, error)
}

// ProbeFunc runs script in a test pod in namespace, as c, and reports
// whether it exited 0.
type ProbeFunc func(ctx context.Context, c client.Client, namespace, name, script string) (bool, error)

// QualificationCheck is one required check.
type QualificationCheck struct {
	Name string
	Run  func(ctx context.Context, env *QualifierEnv) error
}

// QualifierEnv is what a check works with once both test sub-tenants exist.
type QualifierEnv struct {
	Admin client.Client
	Sides [2]QualifierSide
	As    [2]client.Client
	probe func(ctx context.Context, side int, name, script string) (bool, error)
	// control is the memoized positive control: side 0 reaches its own
	// server. Deny checks require it, so a broken probe image cannot pass them.
	control *error
}

// QualifierRunner runs the suite once (the naas-qualifier Job's body).
type QualifierRunner struct {
	Clients    QualifierClients
	Key        ClusterQualificationKey
	JobUID     string
	ProbeImage string
	// Poll, SetupTimeout and ProbeTimeout default to 2s, 10m and 2m.
	Poll, SetupTimeout, ProbeTimeout time.Duration
	// Now defaults to time.Now; Probe to a pod probe; Checks to the suite.
	Now    func() time.Time
	Probe  ProbeFunc
	Checks []QualificationCheck
}

func (r *QualifierRunner) defaults() {
	if r.Poll <= 0 {
		r.Poll = defaultQualifierPoll
	}
	if r.SetupTimeout <= 0 {
		r.SetupTimeout = defaultSetupTimeout
	}
	if r.ProbeTimeout <= 0 {
		r.ProbeTimeout = defaultProbeTimeout
	}
	if r.Now == nil {
		r.Now = time.Now
	}
	if r.Probe == nil {
		r.Probe = PodProbe(r.ProbeImage, r.Poll)
	}
	if r.Checks == nil {
		r.Checks = QualificationSuite()
	}
}

// Run creates the two test sub-tenants, runs every check, deletes them and
// returns the result. A setup failure fails every check with its reason; the
// run itself errs only when its inputs are unusable.
func (r *QualifierRunner) Run(ctx context.Context) (*QualifierResult, error) {
	if err := r.Key.Validate(); err != nil {
		return nil, err
	}
	if r.Clients.Admin == nil || r.Clients.As == nil {
		return nil, fmt.Errorf("the qualifier needs its own client and the impersonating clients")
	}
	r.defaults()
	result := &QualifierResult{Version: QualifierResultVersion, Key: r.Key, JobUID: r.JobUID, StartedAt: r.Now().UTC(), Checks: map[string]bool{}, Failures: map[string]string{}}
	env, setupErr := r.setup(ctx)
	for _, check := range r.Checks {
		var err error
		if setupErr != nil {
			err = fmt.Errorf("setup: %w", setupErr)
		} else {
			checkCtx, cancel := context.WithTimeout(ctx, r.ProbeTimeout*3)
			err = check.Run(checkCtx, env)
			cancel()
		}
		result.Checks[check.Name] = err == nil
		if err != nil {
			result.Failures[check.Name] = truncate(err.Error(), maxFailureMessage)
		}
	}
	result.LeftOver = r.cleanup(context.WithoutCancel(ctx))
	result.FinishedAt = r.Now().UTC()
	return result, nil
}

// testTenant is a test sub-tenant's Tenant: the normal renderer, with the
// side's group bound as namespace admin and access open.
func (r *QualifierRunner) testTenant(side QualifierSide) (*unstructured.Unstructured, error) {
	sub := &cloud.SubTenant{ObjectMeta: metav1.ObjectMeta{Name: side.Tenant, UID: types.UID("naas-qualifier-" + side.Tenant)}, Spec: cloud.SubTenantSpec{
		Name:               side.Tenant,
		TenantRef:          cloud.SubTenantTenantReference{Name: qualifierTestTenantID, UID: qualifierTestTenantID},
		ClusterRef:         cloud.SubTenantClusterReference{ID: qualifierTestTenantID, UID: r.Key.ClusterUID},
		NamespaceKeys:      []cloud.SubTenantNamespaceKey{QualifierNamespaceKey},
		Quota:              cloud.SubTenantQuota{NamespaceCount: 1, Hard: map[string]string{"pods": qualifierQuotaPods, "requests.cpu": "1", "requests.memory": "1Gi", "limits.cpu": "2", "limits.memory": "2Gi"}},
		PolicyProfile:      "restricted-ambient-v1",
		DeletionProtection: ptr.To(false),
	}}
	tenant, err := RenderTenant(sub, r.Key.KubernetesMinor, r.Key.BootstrapRevision)
	if err != nil {
		return nil, err
	}
	access := &ResolvedAccess{OIDCRevision: "qualifier", Bindings: []GroupBinding{{ID: "qualifier-" + side.Group, Group: side.Group, Role: WorkloadAdminRole}}}
	if err := unstructured.SetNestedSlice(tenant.Object, access.BindingData(), "spec", "data", "naas", "accessBindings"); err != nil {
		return nil, err
	}
	labels := tenant.GetLabels()
	labels[LabelAccessReady] = "true"
	labels[LabelQualifier] = "test-tenant"
	tenant.SetLabels(labels)
	return tenant, nil
}

func (r *QualifierRunner) setup(ctx context.Context) (*QualifierEnv, error) {
	ctx, cancel := context.WithTimeout(ctx, r.SetupTimeout)
	defer cancel()
	env := &QualifierEnv{Admin: r.Clients.Admin, Sides: QualifierSides()}
	for i, side := range env.Sides {
		c, err := r.Clients.As(side)
		if err != nil {
			return nil, fmt.Errorf("impersonate %s: %w", side.Group, err)
		}
		env.As[i] = c
		if err := r.ensureTenant(ctx, side); err != nil {
			return nil, err
		}
	}
	for i, side := range env.Sides {
		if err := r.waitGenerated(ctx, side); err != nil {
			return nil, err
		}
		if err := r.startServer(ctx, env.As[i], side); err != nil {
			return nil, err
		}
	}
	env.probe = func(ctx context.Context, side int, name, script string) (bool, error) {
		probeCtx, cancel := context.WithTimeout(ctx, r.ProbeTimeout)
		defer cancel()
		return r.Probe(probeCtx, env.As[side], env.Sides[side].Namespace, name, script)
	}
	return env, nil
}

// ensureTenant creates a side's Tenant, or reuses one a previous run left. A
// Tenant of that name that is not the qualifier's is refused: it is never
// adopted or changed.
func (r *QualifierRunner) ensureTenant(ctx context.Context, side QualifierSide) error {
	desired, err := r.testTenant(side)
	if err != nil {
		return err
	}
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(TenantGVK)
	switch err := r.Clients.Admin.Get(ctx, client.ObjectKey{Name: side.Tenant}, current); {
	case apierrors.IsNotFound(err):
		if err := r.Clients.Admin.Create(ctx, desired); err != nil {
			return fmt.Errorf("create test Tenant %s: %w", side.Tenant, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("read test Tenant %s: %w", side.Tenant, err)
	case current.GetLabels()[LabelQualifier] != "test-tenant":
		return fmt.Errorf("a Tenant named %s exists that is not the qualifier's; it is not changed", side.Tenant)
	}
	next := MergeTenant(current, desired)
	if err := r.Clients.Admin.Update(ctx, next); err != nil {
		return fmt.Errorf("update left-over test Tenant %s: %w", side.Tenant, err)
	}
	return nil
}

// waitGenerated waits for Capsule to generate the side's namespace and the
// access RoleBinding of its group.
func (r *QualifierRunner) waitGenerated(ctx context.Context, side QualifierSide) error {
	return r.poll(ctx, fmt.Sprintf("namespace %s and its access binding", side.Namespace), func() (bool, error) {
		ns := &corev1.Namespace{}
		if err := r.Clients.Admin.Get(ctx, client.ObjectKey{Name: side.Namespace}, ns); err != nil {
			return false, client.IgnoreNotFound(err)
		}
		if ns.Labels["capsule.clastix.io/tenant"] != side.Tenant {
			return false, fmt.Errorf("namespace %s is not generated for %s", side.Namespace, side.Tenant)
		}
		bindings := &rbacv1.RoleBindingList{}
		if err := r.Clients.Admin.List(ctx, bindings, client.InNamespace(side.Namespace)); err != nil {
			return false, err
		}
		for _, b := range bindings.Items {
			for _, s := range b.Subjects {
				if s.Kind == rbacv1.GroupKind && s.Name == side.Group {
					return true, nil
				}
			}
		}
		return false, nil
	})
}

// startServer runs the side's echo server and its Service, as the side's own
// identity, and waits for it to be Ready.
func (r *QualifierRunner) startServer(ctx context.Context, c client.Client, side QualifierSide) error {
	pod := probePod(r.ProbeImage, side.Namespace, qualifierServerName, fmt.Sprintf("mkdir -p /tmp/www && echo ok > /tmp/www/index.html && exec httpd -f -p %d -h /tmp/www", qualifierServerPort))
	pod.Labels = map[string]string{"app": qualifierServerName}
	if err := c.Create(ctx, pod); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("start the test server in %s: %w", side.Namespace, err)
	}
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: qualifierServerName, Namespace: side.Namespace}, Spec: corev1.ServiceSpec{
		Selector: map[string]string{"app": qualifierServerName},
		Ports:    []corev1.ServicePort{{Port: qualifierServerPort, TargetPort: intstr.FromInt32(qualifierServerPort)}},
	}}
	if err := c.Create(ctx, svc); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create the test Service in %s: %w", side.Namespace, err)
	}
	return r.poll(ctx, "the test server in "+side.Namespace, func() (bool, error) {
		current := &corev1.Pod{}
		if err := c.Get(ctx, client.ObjectKeyFromObject(pod), current); err != nil {
			return false, err
		}
		if current.Status.Phase == corev1.PodFailed || current.Status.Phase == corev1.PodSucceeded {
			return false, fmt.Errorf("the test server in %s exited", side.Namespace)
		}
		return podReady(current), nil
	})
}

// cleanup deletes both test Tenants (their namespaces go with them) and
// returns those it could not delete.
func (r *QualifierRunner) cleanup(ctx context.Context) []string {
	var left []string
	for _, side := range QualifierSides() {
		tenant := &unstructured.Unstructured{}
		tenant.SetGroupVersionKind(TenantGVK)
		if err := r.Clients.Admin.Get(ctx, client.ObjectKey{Name: side.Tenant}, tenant); err != nil {
			if !apierrors.IsNotFound(err) {
				left = append(left, side.Tenant)
			}
			continue
		}
		if tenant.GetLabels()[LabelQualifier] != "test-tenant" {
			continue // not ours: setup refused it, and it is not touched
		}
		uid := tenant.GetUID()
		if err := r.Clients.Admin.Delete(ctx, tenant, client.Preconditions{UID: &uid}, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			left = append(left, side.Tenant)
		}
	}
	return left
}

func (r *QualifierRunner) poll(ctx context.Context, what string, done func() (bool, error)) error {
	ticker := time.NewTicker(r.Poll)
	defer ticker.Stop()
	for {
		ok, err := done()
		if err != nil {
			return err
		}
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w", what, ctx.Err())
		case <-ticker.C:
		}
	}
}

// WriteQualifierResult leaves the result for the controller in
// naas-system/naas-qualifier-result.
func WriteQualifierResult(ctx context.Context, c client.Client, result *QualifierResult) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	cm := &corev1.ConfigMap{}
	err = c.Get(ctx, client.ObjectKey{Namespace: QualificationNamespace, Name: QualifierResultName}, cm)
	switch {
	case apierrors.IsNotFound(err):
		cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: QualificationNamespace, Name: QualifierResultName, Labels: qualifierLabels()},
			Data: map[string]string{QualificationKey: string(raw)}}
		return c.Create(ctx, cm)
	case err != nil:
		return err
	}
	cm.Data = map[string]string{QualificationKey: string(raw)}
	return c.Update(ctx, cm)
}

// ReadQualifierResult reads the runner's result: nil without one.
func ReadQualifierResult(ctx context.Context, c client.Reader) (*QualifierResult, error) {
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: QualificationNamespace, Name: QualifierResultName}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	raw := cm.Data[QualificationKey]
	if len(raw) == 0 || len(raw) > maxQualificationBytes {
		return nil, errors.New("qualifier result is missing or oversized")
	}
	result := &QualifierResult{}
	if err := json.Unmarshal([]byte(raw), result); err != nil {
		return nil, fmt.Errorf("qualifier result is invalid: %w", err)
	}
	if result.Version != QualifierResultVersion {
		return nil, fmt.Errorf("qualifier result version %q is not %s", result.Version, QualifierResultVersion)
	}
	return result, nil
}

// RunQualifierJob is the Job's body: it decodes the key the controller
// passed, runs the suite with runner (its clients and probe image set) and
// leaves the result for the controller. The result is written even when
// checks fail; only an unusable input or an unwritable result is an error.
func RunQualifierJob(ctx context.Context, runner *QualifierRunner, keyJSON, jobUID string) (*QualifierResult, error) {
	var key ClusterQualificationKey
	decoder := json.NewDecoder(strings.NewReader(keyJSON))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&key); err != nil {
		return nil, fmt.Errorf("qualification key: %w", err)
	}
	if jobUID == "" {
		return nil, errors.New("the Job UID is required: the controller records only its own Job's result")
	}
	runner.Key, runner.JobUID = key, jobUID
	result, err := runner.Run(ctx)
	if err != nil {
		return nil, err
	}
	if err := WriteQualifierResult(context.WithoutCancel(ctx), runner.Clients.Admin, result); err != nil {
		return result, fmt.Errorf("write the qualifier result: %w", err)
	}
	return result, nil
}

// ImpersonatingClients returns the qualifier's clients from its in-cluster
// config: its own, and one per test side that impersonates the side's user
// and group, which its ClusterRole permits by name only.
func ImpersonatingClients(cfg *rest.Config, scheme *runtime.Scheme) (QualifierClients, error) {
	admin, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return QualifierClients{}, err
	}
	return QualifierClients{Admin: admin, As: func(side QualifierSide) (client.Client, error) {
		impersonated := rest.CopyConfig(cfg)
		impersonated.Impersonate = rest.ImpersonationConfig{UserName: side.User, Groups: []string{side.Group}}
		return client.New(impersonated, client.Options{Scheme: scheme})
	}}, nil
}
