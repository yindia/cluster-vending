package naas

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/util/version"
	"sigs.k8s.io/controller-runtime/pkg/client"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The protected namespace and the data key of every qualification record.
const QualificationNamespace = "naas-system"
const QualificationKey = "qualification.json"

// ClusterQualificationName is the one qualification record of a NaaS cluster
// (PROP-05 6.1). The naas-qualifier Job's result is recorded there by the
// controller; an operator may write it by hand in the same format where the
// Job cannot run.
const ClusterQualificationName = "naas-qualified-cluster"

// ClusterQualificationVersion is the record format. The per-allocation,
// one-hour "v1" record is retired and no longer opens access.
const ClusterQualificationVersion = "v2"

const (
	// MaxQualificationValidity bounds a record: after it the checks run again,
	// whatever the key.
	MaxQualificationValidity = 30 * 24 * time.Hour
	// QualificationRenewBefore is how long before a passed record's expiry the
	// controller starts the renewal run. The old record stays valid while it
	// runs, so access does not close.
	QualificationRenewBefore = 3 * 24 * time.Hour
	// QualificationRetryAfter is how long a failed record stands before the
	// controller runs the checks again for the same key.
	QualificationRetryAfter = time.Hour
	// maxQualificationBytes bounds the record read.
	maxQualificationBytes = 64 * 1024
)

// The approved runtime checks (RequiredQualificationChecks).
const (
	CheckNamespaceRBAC        = "namespace-rbac"
	CheckCrossNamespaceRBAC   = "cross-namespace-rbac"
	CheckAdmissionGuards      = "admission-guards"
	CheckAmbientMTLS          = "ambient-mtls-hbone"
	CheckCrossSubTenant       = "cross-subtenant-traffic"
	CheckDNS                  = "dns-udp-tcp"
	CheckEgressDeny           = "egress-default-deny"
	CheckRestrictedPSS        = "restricted-pss"
	CheckSharedQuota          = "shared-quota"
	CheckGroupDelegation      = "group-delegation-approved"
	CheckAdmissionPoolOutage  = "admission-platform-pool-outage"
	qualificationCheckCount   = 11
	qualificationEvidenceSize = 2048
)

// RequiredQualificationChecks are assertions made only by the qualifier Job
// or a trusted operator. Static template matching never manufactures them.
// The eleventh check (PROP-05 decision 7): Capsule admission stays available
// while the platform pool is down.
func RequiredQualificationChecks() []string {
	return []string{CheckNamespaceRBAC, CheckCrossNamespaceRBAC, CheckAdmissionGuards, CheckAmbientMTLS, CheckCrossSubTenant,
		CheckDNS, CheckEgressDeny, CheckRestrictedPSS, CheckSharedQuota, CheckGroupDelegation, CheckAdmissionPoolOutage}
}

// ClusterQualificationKey is what the checks exercise on a cluster: the
// shared templates and what enforces them. Any change ends a record.
// IstioVersion and OIDCRevision are empty when the cluster has none.
type ClusterQualificationKey struct {
	ClusterUID        string `json:"clusterUID"`
	BootstrapRevision string `json:"bootstrapRevision"`
	KubernetesMinor   string `json:"kubernetesMinor"`
	CapsuleVersion    string `json:"capsuleVersion"`
	IstioVersion      string `json:"istioVersion"`
	OIDCRevision      string `json:"oidcRevision"`
}

// Validate requires the parts every NaaS cluster has.
func (k ClusterQualificationKey) Validate() error {
	if k.ClusterUID == "" || k.BootstrapRevision == "" || k.KubernetesMinor == "" || k.CapsuleVersion == "" {
		return fmt.Errorf("qualification key requires the cluster UID, bootstrap revision, Kubernetes minor and installed Capsule version")
	}
	return nil
}

// Hash is a short stable digest of the key, for object names and labels.
func (k ClusterQualificationKey) Hash() string {
	raw, _ := json.Marshal(k)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:8])
}

// ClusterQualificationKeyFor builds the cluster's current key from fresh
// management reads: the observed Kubernetes minor, the installed Capsule and
// Istio versions, the verified bootstrap revision and the effective OIDC
// mapping.
func ClusterQualificationKeyFor(parent *cloud.Cluster, revision string, spec *cloudv1.ClusterSpec) (ClusterQualificationKey, error) {
	key := ClusterQualificationKey{ClusterUID: string(parent.UID), BootstrapRevision: revision,
		CapsuleVersion: componentVersion(parent, "capsule"), IstioVersion: componentVersion(parent, "istiod", "istio")}
	if v, err := version.ParseGeneric(parent.Status.CurrentKubernetesVersion); err == nil {
		key.KubernetesMinor = fmt.Sprintf("%d.%d", v.Major(), v.Minor())
	}
	oidcRevision, err := OIDCRevision(spec)
	if err != nil && !errors.Is(err, ErrAccessOIDCNotConfigured) {
		return key, err
	}
	key.OIDCRevision = oidcRevision
	return key, key.Validate()
}

// componentVersion is the first named component's installed chart version,
// or the pinned one before it was first observed Ready.
func componentVersion(parent *cloud.Cluster, names ...string) string {
	for _, name := range names {
		for _, component := range parent.Status.PlatformComponents {
			if component.Name != name {
				continue
			}
			if component.InstalledVersion != "" {
				return component.InstalledVersion
			}
			return component.Version
		}
	}
	return ""
}

// QualifierJobRef is the Job whose result a record holds.
type QualifierJobRef struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
}

// ClusterQualification is the per-cluster record. Every required check is
// present; a false one is a failed run, which closes access on the cluster.
type ClusterQualification struct {
	Version   string                  `json:"version"`
	Key       ClusterQualificationKey `json:"key"`
	IssuedAt  time.Time               `json:"issuedAt"`
	ExpiresAt time.Time               `json:"expiresAt"`
	Evidence  string                  `json:"evidence"`
	Checks    map[string]bool         `json:"checks"`
	// Failures say why a check failed, by check (bounded).
	Failures map[string]string `json:"failures,omitempty"`
	// Job is set when the qualifier Job produced the record; nil for an
	// operator's record.
	Job *QualifierJobRef `json:"job,omitempty"`
}

// FailedChecks are the required checks that did not pass, in suite order.
func (q *ClusterQualification) FailedChecks() []string {
	var failed []string
	for _, check := range RequiredQualificationChecks() {
		if !q.Checks[check] {
			failed = append(failed, check)
		}
	}
	return failed
}

// ErrNoQualification is a cluster without a record.
var ErrNoQualification = errors.New("the cluster has no qualification record")

// QualificationFailedError is a current record whose run failed: access is
// closed on the cluster, naming the checks.
type QualificationFailedError struct{ Checks []string }

func (e *QualificationFailedError) Error() string {
	return "cluster qualification failed: " + strings.Join(e.Checks, ", ")
}

// ReadClusterQualification reads the record from the protected namespace and
// checks its form, not whether it matches a key: nil and ErrNoQualification
// without a record. Fixed SubTenant roles never grant permissions in this
// namespace.
func ReadClusterQualification(ctx context.Context, workload client.Reader) (*ClusterQualification, error) {
	namespace := &corev1.Namespace{}
	if err := workload.Get(ctx, client.ObjectKey{Name: QualificationNamespace}, namespace); err != nil {
		return nil, fmt.Errorf("protected qualification namespace is unavailable: %w", err)
	}
	if namespace.UID == "" || !namespace.DeletionTimestamp.IsZero() || namespace.Labels["capsule.clastix.io/tenant"] != "" || namespace.Labels[LabelSubTenantID] != "" {
		return nil, fmt.Errorf("qualification namespace must be an active platform namespace outside Capsule tenant ownership")
	}
	for _, owner := range namespace.OwnerReferences {
		if owner.Kind == "Tenant" && owner.APIVersion == TenantGVK.GroupVersion().String() {
			return nil, fmt.Errorf("qualification namespace must not be owned by a Capsule Tenant")
		}
	}
	cm := &corev1.ConfigMap{}
	if err := workload.Get(ctx, client.ObjectKey{Namespace: QualificationNamespace, Name: ClusterQualificationName}, cm); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrNoQualification
		}
		return nil, fmt.Errorf("read protected runtime qualification: %w", err)
	}
	if !cm.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("runtime qualification is being revoked")
	}
	return DecodeClusterQualification(cm.Data[QualificationKey])
}

// DecodeClusterQualification parses and checks the form of a record: the
// version, a bounded validity of at most 30 days, bounded evidence and
// exactly the required checks.
func DecodeClusterQualification(raw string) (*ClusterQualification, error) {
	if len(raw) == 0 || len(raw) > maxQualificationBytes {
		return nil, fmt.Errorf("runtime qualification is missing or oversized")
	}
	var q ClusterQualification
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&q); err != nil {
		return nil, fmt.Errorf("runtime qualification is invalid: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("runtime qualification has trailing content")
	}
	if q.Version != ClusterQualificationVersion {
		return nil, fmt.Errorf("runtime qualification must be a %s per-cluster record", ClusterQualificationVersion)
	}
	if q.IssuedAt.IsZero() || !q.ExpiresAt.After(q.IssuedAt) || q.ExpiresAt.Sub(q.IssuedAt) > MaxQualificationValidity {
		return nil, fmt.Errorf("runtime qualification has an invalid validity period (maximum 30 days)")
	}
	if strings.TrimSpace(q.Evidence) == "" || len(q.Evidence) > qualificationEvidenceSize {
		return nil, fmt.Errorf("runtime qualification requires a bounded evidence reference")
	}
	required := RequiredQualificationChecks()
	if len(q.Checks) != len(required) {
		return nil, fmt.Errorf("runtime qualification requires the complete approved test suite (%d checks)", len(required))
	}
	for check := range q.Checks {
		if !slices.Contains(required, check) {
			return nil, fmt.Errorf("runtime qualification has an unknown check %q", check)
		}
	}
	if len(q.Failures) > len(required) {
		return nil, fmt.Errorf("runtime qualification has too many failure messages")
	}
	return &q, nil
}

// Current says whether q is the passed, unexpired record of key at now. It
// returns q's expiry, or an error: a QualificationFailedError when the
// record for this key failed.
func (q *ClusterQualification) Current(key ClusterQualificationKey, now time.Time) (time.Time, error) {
	if q.Key != key {
		return time.Time{}, fmt.Errorf("cluster qualification does not match the current templates, Kubernetes minor, Capsule, Istio and OIDC")
	}
	if q.IssuedAt.After(now.Add(time.Minute)) || !q.ExpiresAt.After(now) {
		return time.Time{}, fmt.Errorf("cluster qualification is expired or not yet valid")
	}
	if failed := q.FailedChecks(); len(failed) > 0 {
		return time.Time{}, &QualificationFailedError{Checks: failed}
	}
	return q.ExpiresAt, nil
}

// NeedsRun says whether the qualifier must run for key at now: there is no
// usable record, it is for another key, it passed and is within
// QualificationRenewBefore of its expiry (the overlapping renewal), or it
// failed more than QualificationRetryAfter ago.
func (q *ClusterQualification) NeedsRun(key ClusterQualificationKey, now time.Time) bool {
	if q == nil || q.Key != key {
		return true
	}
	if len(q.FailedChecks()) > 0 {
		return !now.Before(q.IssuedAt.Add(QualificationRetryAfter))
	}
	return !now.Before(q.ExpiresAt.Add(-QualificationRenewBefore))
}

// VerifyAccessQualification reads the cluster's record and returns its expiry
// when it is current for key: passed, unexpired and for the same key. Expiry
// revocation is reconciliation-bound, not a Kubernetes RBAC hard TTL: the
// expiry is returned so the caller looks again no later than then.
func VerifyAccessQualification(ctx context.Context, workload client.Reader, key ClusterQualificationKey, now time.Time) (time.Time, error) {
	if err := key.Validate(); err != nil {
		return time.Time{}, err
	}
	q, err := ReadClusterQualification(ctx, workload)
	if err != nil {
		return time.Time{}, err
	}
	return q.Current(key, now)
}

// ClusterQualificationConfigMap is the record object, as the controller
// writes it.
func ClusterQualificationConfigMap(q *ClusterQualification) (*corev1.ConfigMap, error) {
	raw, err := json.Marshal(q)
	if err != nil {
		return nil, err
	}
	cm := &corev1.ConfigMap{Data: map[string]string{QualificationKey: string(raw)}}
	cm.Namespace, cm.Name = QualificationNamespace, ClusterQualificationName
	cm.Labels = map[string]string{LabelQualifier: "record"}
	return cm, nil
}
