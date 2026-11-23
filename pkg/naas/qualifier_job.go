package naas

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
)

// The naas-qualifier (PROP-05 6.1): a Job in naas-system that runs the
// required checks against two throwaway sub-tenants and leaves its result
// for the controller to record.
const (
	QualifierName = "naas-qualifier"
	// CapsuleNamespace is where the pinned Capsule chart runs.
	CapsuleNamespace = "capsule-system"
	// CapsuleWebhookService fronts the Capsule admission webhooks.
	CapsuleWebhookService = "capsule-webhook-service"
	// QualifierResultName is the ConfigMap the runner writes its result to.
	QualifierResultName = "naas-qualifier-result"
	// LabelQualifier marks every object of the qualifier: the Job, its RBAC,
	// its result, the record and the test Tenants.
	LabelQualifier = "cloud.cluster-vending.io/naas-qualifier"
	// LabelQualificationKey is the key hash a Job runs for.
	LabelQualificationKey = "cloud.cluster-vending.io/qualification-key"
	// QualifierTenantPrefix is reserved: no sub-tenant may be named with it.
	QualifierTenantPrefix = "naas-qual-"
	// QualifierNamespaceKey is the one namespace key of each test Tenant.
	QualifierNamespaceKey = "probe"
	// qualifierDeadline bounds a run; a Job past it has failed.
	qualifierDeadline = 30 * time.Minute
	// qualifierTTL removes a finished Job the controller did not delete.
	qualifierTTL = 24 * time.Hour
)

// QualifierSide is one of the two throwaway sub-tenants and the identity the
// runner impersonates for it.
type QualifierSide struct {
	Tenant, Namespace, Group, User string
}

// QualifierSides are the two test sub-tenants: test identities are two
// groups (PROP-05 decision 2), impersonated through a role limited to them.
func QualifierSides() [2]QualifierSide {
	side := func(suffix string) QualifierSide {
		tenant := QualifierTenantPrefix + suffix
		return QualifierSide{Tenant: tenant, Namespace: tenant + "-" + QualifierNamespaceKey, Group: "cv-naas-qual-" + suffix, User: "cv-naas-qual-user-" + suffix}
	}
	return [2]QualifierSide{side("a"), side("b")}
}

// QualifierOptions are the inputs of one run.
type QualifierOptions struct {
	// Image runs the access-qualification binary (naas.qualifierImage).
	Image string
	// ProbeImage runs the test pods.
	ProbeImage string
	Key        ClusterQualificationKey
	// At names the run; renewals of the same key are distinct Jobs.
	At time.Time
}

// QualifierJobName is the Job of a run: one per key and start minute.
func QualifierJobName(key ClusterQualificationKey, at time.Time) string {
	return fmt.Sprintf("%s-%s-%d", QualifierName, key.Hash()[:8], at.Unix()/60)
}

func qualifierLabels() map[string]string {
	return map[string]string{LabelQualifier: "true", "app.kubernetes.io/name": QualifierName, "app.kubernetes.io/managed-by": "cluster-vending"}
}

// RenderQualifier returns the qualifier's ServiceAccount, its ClusterRole and
// Role with their bindings, and the Job, in apply order. The roles grant only
// what the run needs: the two test Tenants, impersonation of the two test
// users and groups (by name), reads for the checks, and the result ConfigMap.
// The record itself is written by the controller, not by the Job.
func RenderQualifier(opts QualifierOptions) ([]client.Object, error) {
	if strings.TrimSpace(opts.Image) == "" || strings.TrimSpace(opts.ProbeImage) == "" {
		return nil, fmt.Errorf("the qualifier and probe images are required")
	}
	if err := opts.Key.Validate(); err != nil {
		return nil, err
	}
	key, err := json.Marshal(opts.Key)
	if err != nil {
		return nil, err
	}
	sides := QualifierSides()
	labels := qualifierLabels()
	meta := func(name, namespace string) metav1.ObjectMeta {
		return metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}
	}
	subject := []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: QualifierName, Namespace: QualificationNamespace}}
	sa := &corev1.ServiceAccount{ObjectMeta: meta(QualifierName, QualificationNamespace)}
	clusterRole := &rbacv1.ClusterRole{ObjectMeta: meta(QualifierName, ""), Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{TenantGVK.Group}, Resources: []string{"tenants"}, Verbs: []string{"get", "list", "create"}},
		{APIGroups: []string{TenantGVK.Group}, Resources: []string{"tenants"}, Verbs: []string{"update", "patch", "delete"}, ResourceNames: []string{sides[0].Tenant, sides[1].Tenant}},
		{APIGroups: []string{""}, Resources: []string{"users"}, Verbs: []string{"impersonate"}, ResourceNames: []string{sides[0].User, sides[1].User}},
		{APIGroups: []string{""}, Resources: []string{"groups"}, Verbs: []string{"impersonate"}, ResourceNames: []string{sides[0].Group, sides[1].Group}},
		{APIGroups: []string{""}, Resources: []string{"namespaces", "nodes"}, Verbs: []string{"get", "list"}},
		{APIGroups: []string{rbacv1.GroupName}, Resources: []string{"rolebindings"}, Verbs: []string{"get", "list"}},
		{APIGroups: []string{"security.istio.io"}, Resources: []string{"peerauthentications"}, Verbs: []string{"get", "list"}},
	}}
	clusterBinding := &rbacv1.ClusterRoleBinding{ObjectMeta: meta(QualifierName, ""), Subjects: subject,
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: QualifierName}}
	role := &rbacv1.Role{ObjectMeta: meta(QualifierName, QualificationNamespace), Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"create"}},
		{APIGroups: []string{""}, Resources: []string{"configmaps"}, Verbs: []string{"get", "update"}, ResourceNames: []string{QualifierResultName}},
	}}
	binding := &rbacv1.RoleBinding{ObjectMeta: meta(QualifierName, QualificationNamespace), Subjects: subject,
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: QualifierName}}
	// Check 11 reads where Capsule's webhook pods run.
	capsuleRole := &rbacv1.Role{ObjectMeta: meta(QualifierName, CapsuleNamespace), Rules: []rbacv1.PolicyRule{
		{APIGroups: []string{""}, Resources: []string{"pods", "services"}, Verbs: []string{"get", "list"}},
	}}
	capsuleBinding := &rbacv1.RoleBinding{ObjectMeta: meta(QualifierName, CapsuleNamespace), Subjects: subject,
		RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: QualifierName}}

	jobLabels := qualifierLabels()
	jobLabels[LabelQualificationKey] = opts.Key.Hash()
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: QualifierJobName(opts.Key, opts.At), Namespace: QualificationNamespace, Labels: jobLabels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To[int32](0),
			ActiveDeadlineSeconds:   ptr.To(int64(qualifierDeadline.Seconds())),
			TTLSecondsAfterFinished: ptr.To(int32(qualifierTTL.Seconds())),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: jobLabels},
				Spec: corev1.PodSpec{
					ServiceAccountName: QualifierName,
					RestartPolicy:      corev1.RestartPolicyNever,
					// The bootstrap group: the run must not depend on the pool it tests.
					NodeSelector: map[string]string{crossplaneaws.PoolLabelKey: crossplaneaws.PoolSystem},
					Tolerations:  []corev1.Toleration{{Key: crossplaneaws.PoolLabelKey, Operator: corev1.TolerationOpEqual, Value: crossplaneaws.PoolSystem, Effect: corev1.TaintEffectNoSchedule}},
					SecurityContext: &corev1.PodSecurityContext{RunAsNonRoot: ptr.To(true), RunAsUser: ptr.To[int64](65532),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
					Containers: []corev1.Container{{
						Name:  "qualifier",
						Image: opts.Image,
						Args:  []string{"--run", "--key=" + string(key), "--probe-image=" + opts.ProbeImage},
						Env: []corev1.EnvVar{{Name: "QUALIFIER_JOB_UID", ValueFrom: &corev1.EnvVarSource{
							FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.labels['batch.kubernetes.io/controller-uid']"}}}},
						SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), ReadOnlyRootFilesystem: ptr.To(true),
							Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("50m"), corev1.ResourceMemory: resource.MustParse("64Mi")},
							Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")},
						},
					}},
				},
			},
		}}
	return []client.Object{sa, clusterRole, clusterBinding, role, binding, capsuleRole, capsuleBinding, job}, nil
}

// QualifierResult is what the runner leaves in QualifierResultName. The
// controller records it only when JobUID is the Job it started and Key is
// the key it started it for.
type QualifierResult struct {
	Version    string                  `json:"version"`
	Key        ClusterQualificationKey `json:"key"`
	JobUID     string                  `json:"jobUID"`
	StartedAt  time.Time               `json:"startedAt"`
	FinishedAt time.Time               `json:"finishedAt"`
	Checks     map[string]bool         `json:"checks"`
	Failures   map[string]string       `json:"failures,omitempty"`
	// LeftOver names test Tenants the run could not delete.
	LeftOver []string `json:"leftOver,omitempty"`
}

// QualifierResultVersion is the result format.
const QualifierResultVersion = "v1"

// maxFailureMessage bounds each failure message of a result and a record.
const maxFailureMessage = 256

// Record is the per-cluster record of a run: every required check present
// (a missing one failed), valid for MaxQualificationValidity from its end,
// with the Job as evidence.
func (r *QualifierResult) Record(job QualifierJobRef, now time.Time) *ClusterQualification {
	issued := r.FinishedAt
	if issued.IsZero() || issued.After(now) {
		issued = now
	}
	q := &ClusterQualification{Version: ClusterQualificationVersion, Key: r.Key, IssuedAt: issued.UTC(), ExpiresAt: issued.Add(MaxQualificationValidity).UTC(),
		Evidence: fmt.Sprintf("job %s/%s uid %s", QualificationNamespace, job.Name, job.UID), Checks: map[string]bool{}, Job: &job}
	for _, check := range RequiredQualificationChecks() {
		q.Checks[check] = r.Checks[check]
		if message := r.Failures[check]; message != "" && !q.Checks[check] {
			if q.Failures == nil {
				q.Failures = map[string]string{}
			}
			q.Failures[check] = truncate(message, maxFailureMessage)
		}
	}
	return q
}

// FailedRunRecord is the record of a run that ended without a usable result
// (crashed, timed out, never scheduled): every check failed, so access closes.
func FailedRunRecord(key ClusterQualificationKey, job QualifierJobRef, reason string, now time.Time) *ClusterQualification {
	r := &QualifierResult{Key: key, FinishedAt: now, Checks: map[string]bool{}, Failures: map[string]string{}}
	for _, check := range RequiredQualificationChecks() {
		r.Failures[check] = reason
	}
	return r.Record(job, now)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
