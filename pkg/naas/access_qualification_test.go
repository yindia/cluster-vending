package naas

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

var qualificationNow = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)

func qualificationKeyFixture() ClusterQualificationKey {
	return ClusterQualificationKey{ClusterUID: "cluster-uid", BootstrapRevision: "bootstrap-revision", KubernetesMinor: "1.35",
		CapsuleVersion: "0.15.2", IstioVersion: "1.28.0", OIDCRevision: "sha256:oidc"}
}

func passedRecord(key ClusterQualificationKey, issued time.Time, validity time.Duration) *ClusterQualification {
	q := &ClusterQualification{Version: ClusterQualificationVersion, Key: key, IssuedAt: issued, ExpiresAt: issued.Add(validity),
		Evidence: "job naas-system/naas-qualifier-x uid job-uid", Checks: map[string]bool{}}
	for _, check := range RequiredQualificationChecks() {
		q.Checks[check] = true
	}
	return q
}

func TestRequiredChecksIncludeTheEleventh(t *testing.T) {
	checks := RequiredQualificationChecks()
	if len(checks) != 11 || !slices.Contains(checks, CheckAdmissionPoolOutage) {
		t.Fatalf("checks = %v", checks)
	}
	suite := QualificationSuite()
	if len(suite) != len(checks) {
		t.Fatalf("suite has %d checks, required %d", len(suite), len(checks))
	}
	for i := range suite {
		if suite[i].Name != checks[i] {
			t.Fatalf("suite[%d] = %s, want %s", i, suite[i].Name, checks[i])
		}
	}
}

// The record's key and validity: match, key change, 30 days, overlap.
func TestClusterQualificationCurrent(t *testing.T) {
	key := qualificationKeyFixture()
	day := 24 * time.Hour
	tests := []struct {
		name   string
		record func() *ClusterQualification
		key    func(ClusterQualificationKey) ClusterQualificationKey
		now    time.Time
		ok     bool
		failed []string
	}{
		{name: "match", record: func() *ClusterQualification {
			return passedRecord(key, qualificationNow.Add(-day), MaxQualificationValidity)
		}, now: qualificationNow, ok: true},
		{name: "kept more than an hour", record: func() *ClusterQualification {
			return passedRecord(key, qualificationNow.Add(-2*time.Hour), MaxQualificationValidity)
		}, now: qualificationNow, ok: true},
		{name: "bootstrap revision changed", record: func() *ClusterQualification { return passedRecord(key, qualificationNow, MaxQualificationValidity) },
			key: func(k ClusterQualificationKey) ClusterQualificationKey { k.BootstrapRevision = "next"; return k }, now: qualificationNow},
		{name: "kubernetes minor changed", record: func() *ClusterQualification { return passedRecord(key, qualificationNow, MaxQualificationValidity) },
			key: func(k ClusterQualificationKey) ClusterQualificationKey { k.KubernetesMinor = "1.36"; return k }, now: qualificationNow},
		{name: "capsule changed", record: func() *ClusterQualification { return passedRecord(key, qualificationNow, MaxQualificationValidity) },
			key: func(k ClusterQualificationKey) ClusterQualificationKey { k.CapsuleVersion = "0.16.0"; return k }, now: qualificationNow},
		{name: "istio changed", record: func() *ClusterQualification { return passedRecord(key, qualificationNow, MaxQualificationValidity) },
			key: func(k ClusterQualificationKey) ClusterQualificationKey { k.IstioVersion = "1.29.0"; return k }, now: qualificationNow},
		{name: "oidc changed", record: func() *ClusterQualification { return passedRecord(key, qualificationNow, MaxQualificationValidity) },
			key: func(k ClusterQualificationKey) ClusterQualificationKey { k.OIDCRevision = "sha256:other"; return k }, now: qualificationNow},
		{name: "after 30 days", record: func() *ClusterQualification {
			return passedRecord(key, qualificationNow.Add(-31*day), MaxQualificationValidity)
		}, now: qualificationNow},
		{name: "renewal window is still valid", record: func() *ClusterQualification {
			return passedRecord(key, qualificationNow.Add(-29*day), MaxQualificationValidity)
		}, now: qualificationNow, ok: true},
		{name: "future issue", record: func() *ClusterQualification {
			return passedRecord(key, qualificationNow.Add(time.Hour), MaxQualificationValidity)
		}, now: qualificationNow},
		{name: "failed checks are named", record: func() *ClusterQualification {
			q := passedRecord(key, qualificationNow, MaxQualificationValidity)
			q.Checks[CheckEgressDeny], q.Checks[CheckAdmissionPoolOutage] = false, false
			return q
		}, now: qualificationNow, failed: []string{CheckEgressDeny, CheckAdmissionPoolOutage}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			want := key
			if tt.key != nil {
				want = tt.key(key)
			}
			q := tt.record()
			expires, err := q.Current(want, tt.now)
			if tt.ok {
				if err != nil || !expires.Equal(q.ExpiresAt) {
					t.Fatalf("Current = %v, %v", expires, err)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted a record that is not current")
			}
			var failed *QualificationFailedError
			if tt.failed != nil && (!errors.As(err, &failed) || !slices.Equal(failed.Checks, tt.failed)) {
				t.Fatalf("err = %v, want failed %v", err, tt.failed)
			}
		})
	}
}

// When the controller starts a run: none, other key, renewal window, failed retry.
func TestClusterQualificationNeedsRun(t *testing.T) {
	key := qualificationKeyFixture()
	day := 24 * time.Hour
	failed := passedRecord(key, qualificationNow.Add(-10*time.Minute), MaxQualificationValidity)
	failed.Checks[CheckDNS] = false
	other := key
	other.BootstrapRevision = "next"
	tests := []struct {
		name   string
		record *ClusterQualification
		key    ClusterQualificationKey
		want   bool
	}{
		{"no record", nil, key, true},
		{"fresh record", passedRecord(key, qualificationNow.Add(-day), MaxQualificationValidity), key, false},
		{"key changed", passedRecord(key, qualificationNow.Add(-day), MaxQualificationValidity), other, true},
		{"inside renewal window, still valid", passedRecord(key, qualificationNow.Add(-28*day), MaxQualificationValidity), key, true},
		{"just before renewal window", passedRecord(key, qualificationNow.Add(-26*day), MaxQualificationValidity), key, false},
		{"recently failed waits", failed, key, false},
		{"failed long ago retries", func() *ClusterQualification {
			q := passedRecord(key, qualificationNow.Add(-2*time.Hour), MaxQualificationValidity)
			q.Checks[CheckDNS] = false
			return q
		}(), key, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.record.NeedsRun(tt.key, qualificationNow); got != tt.want {
				t.Fatalf("NeedsRun = %t, want %t", got, tt.want)
			}
		})
	}
	// The overlap: inside the renewal window the old record still opens access.
	renewing := passedRecord(key, qualificationNow.Add(-28*day), MaxQualificationValidity)
	if _, err := renewing.Current(key, qualificationNow); err != nil || !renewing.NeedsRun(key, qualificationNow) {
		t.Fatalf("renewal does not overlap: %v", err)
	}
}

func TestVerifyAccessQualificationReadsTheProtectedRecord(t *testing.T) {
	key := qualificationKeyFixture()
	tests := []struct {
		name   string
		mutate func(*ClusterQualification, *corev1.ConfigMap, *corev1.Namespace)
		ok     bool
	}{
		{name: "current record", ok: true},
		{name: "operator override without a job", ok: true, mutate: func(q *ClusterQualification, _ *corev1.ConfigMap, _ *corev1.Namespace) {
			q.Job, q.Evidence = nil, "https://evidence.example.test/manual-run"
		}},
		{name: "validity over 30 days", mutate: func(q *ClusterQualification, _ *corev1.ConfigMap, _ *corev1.Namespace) {
			q.ExpiresAt = q.IssuedAt.Add(MaxQualificationValidity + time.Hour)
		}},
		{name: "retired per-allocation format", mutate: func(q *ClusterQualification, _ *corev1.ConfigMap, _ *corev1.Namespace) { q.Version = "v1" }},
		{name: "missing check", mutate: func(q *ClusterQualification, _ *corev1.ConfigMap, _ *corev1.Namespace) {
			delete(q.Checks, CheckAdmissionPoolOutage)
		}},
		{name: "unknown check", mutate: func(q *ClusterQualification, _ *corev1.ConfigMap, _ *corev1.Namespace) {
			delete(q.Checks, CheckDNS)
			q.Checks["made-up"] = true
		}},
		{name: "missing evidence", mutate: func(q *ClusterQualification, _ *corev1.ConfigMap, _ *corev1.Namespace) { q.Evidence = "" }},
		{name: "record outside the protected namespace", mutate: func(_ *ClusterQualification, cm *corev1.ConfigMap, _ *corev1.Namespace) {
			cm.Namespace = "payments-app"
		}},
		{name: "tenant owned authority namespace", mutate: func(_ *ClusterQualification, _ *corev1.ConfigMap, ns *corev1.Namespace) {
			ns.Labels = map[string]string{LabelSubTenantID: "tenant-owned"}
		}},
		{name: "capsule owned authority namespace", mutate: func(_ *ClusterQualification, _ *corev1.ConfigMap, ns *corev1.Namespace) {
			ns.OwnerReferences = []metav1.OwnerReference{{APIVersion: TenantGVK.GroupVersion().String(), Kind: "Tenant", Name: "x", UID: "u", Controller: ptr.To(true)}}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			q := passedRecord(key, time.Now().Add(-time.Hour), MaxQualificationValidity)
			q.Job = &QualifierJobRef{Name: "naas-qualifier-x", UID: "job-uid"}
			cm, err := ClusterQualificationConfigMap(q)
			if err != nil {
				t.Fatal(err)
			}
			ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: QualificationNamespace, UID: "platform-namespace-uid"}}
			if tt.mutate != nil {
				tt.mutate(q, cm, ns)
				raw, _ := json.Marshal(q)
				cm.Data[QualificationKey] = string(raw)
			}
			scheme, err := kube.Scheme()
			if err != nil {
				t.Fatal(err)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, cm).Build()
			if _, err := VerifyAccessQualification(t.Context(), c, key, time.Now()); (err == nil) != tt.ok {
				t.Fatalf("ok=%t err=%v", tt.ok, err)
			}
		})
	}
}

func TestDecodeClusterQualificationRejectsMalformedPayloads(t *testing.T) {
	raw, _ := json.Marshal(passedRecord(qualificationKeyFixture(), qualificationNow, time.Hour))
	for _, payload := range []string{"", "{}", "{", string(raw) + " {}", string(raw[:len(raw)-1]) + `,"unknown":true}`, strings.Repeat(" ", 65537)} {
		if _, err := DecodeClusterQualification(payload); err == nil {
			t.Fatalf("accepted invalid qualification %.40q", payload)
		}
	}
	if _, err := DecodeClusterQualification(string(raw)); err != nil {
		t.Fatal(err)
	}
}

func TestClusterQualificationKeyFor(t *testing.T) {
	parent := &cloud.Cluster{ObjectMeta: metav1.ObjectMeta{UID: "cluster-uid"}, Status: cloud.ClusterStatus{CurrentKubernetesVersion: "1.35.2",
		PlatformComponents: []cloud.PlatformComponentStatus{{Name: "capsule", Version: "0.15.3", InstalledVersion: "0.15.2"}, {Name: "istiod", Version: "1.28.0"}}}}
	key, err := ClusterQualificationKeyFor(parent, "revision", accessCluster())
	if err != nil {
		t.Fatal(err)
	}
	if key.KubernetesMinor != "1.35" || key.CapsuleVersion != "0.15.2" || key.IstioVersion != "1.28.0" || !strings.HasPrefix(key.OIDCRevision, "sha256:") {
		t.Fatalf("key = %+v", key)
	}
	// Without OIDC the key is still complete: access cannot open, but the
	// cluster can be qualified.
	noOIDC, err := ClusterQualificationKeyFor(parent, "revision", nil)
	if err != nil || noOIDC.OIDCRevision != "" {
		t.Fatalf("no OIDC: %+v, %v", noOIDC, err)
	}
	parent.Status.PlatformComponents = nil
	if _, err := ClusterQualificationKeyFor(parent, "revision", nil); err == nil {
		t.Fatal("a key without Capsule was accepted")
	}
	if key.Hash() == noOIDC.Hash() || len(key.Hash()) != 16 {
		t.Fatalf("hash = %q, %q", key.Hash(), noOIDC.Hash())
	}
}
