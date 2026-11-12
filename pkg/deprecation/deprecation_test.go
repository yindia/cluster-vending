package deprecation_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/yindia/cluster-vending/pkg/deprecation"
)

func rules(t *testing.T) deprecation.RuleSet {
	t.Helper()
	set, err := deprecation.Default()
	if err != nil {
		t.Fatalf("Default: %v", err)
	}
	return set
}

func TestShippedRulesParse(t *testing.T) {
	set := rules(t)

	if len(set.Rules) < 100 {
		t.Fatalf("only %d rules: the data file is not Pluto's", len(set.Rules))
	}
	if set.TargetVersions["cert-manager"] == "" || set.TargetVersions["istio"] == "" {
		t.Errorf("target versions of the other components are missing: %v", set.TargetVersions)
	}
}

// The newest Kubernetes minor the data knows a removal in. A target after
// it finds what was removed up to here and nothing newer: either that
// minor removes nothing, or Pluto has not recorded it yet. When this
// fails after `make deprecation-rules`, update it and docs/deprecations.md.
func TestNewestRemovalTheRulesKnow(t *testing.T) {
	if got := rules(t).NewestRemoval(); got != "1.36" {
		t.Fatalf("newest removal = %s, want 1.36", got)
	}
}

func TestParseRefusesBadData(t *testing.T) {
	tests := []struct{ name, data, want string }{
		{"not yaml", "deprecated-versions: [", "deprecation rules"},
		{"unknown field", "something-else: 1\n", "deprecation rules"},
		{"no version", "deprecated-versions:\n- kind: Ingress\n  component: k8s\n", "no version or component"},
		{"bad release", "deprecated-versions:\n- version: a/v1\n  kind: A\n  component: k8s\n  removed-in: soon\n", "not a version"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := deprecation.Parse([]byte(tt.data))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Parse = %v, want an error with %q", err, tt.want)
			}
		})
	}
}

func obj(apiVersion, kind, namespace, name string) deprecation.Object {
	return deprecation.Object{APIVersion: apiVersion, Kind: kind, Namespace: namespace, Name: name, Source: deprecation.SourceManifest}
}

func TestEvaluate(t *testing.T) {
	set := rules(t)
	objects := []deprecation.Object{
		obj("apps/v1", "Deployment", "web", "current"),                                        // never deprecated
		obj("flowcontrol.apiserver.k8s.io/v1beta3", "FlowSchema", "", "fs"),                   // removed in 1.32
		obj("storage.k8s.io/v1beta1", "VolumeAttributesClass", "", "vac"),                     // deprecated 1.34, not removed
		obj("resource.k8s.io/v1alpha3", "DeviceClass", "", "gpu"),                             // removed in 1.34
		obj("flowcontrol.apiserver.k8s.io/v1beta3", "FlowSchema", "", "fs"),                   // the same object again
		obj("cert-manager.io/v1alpha2", "Certificate", "web", "tls"),                          // another component: deprecated at its own version
		obj("authentication.istio.io/v1alpha1", "Policy", "mesh", "any-kind-of-this-version"), // a rule without a kind
	}
	tests := []struct {
		name        string
		target      string
		wantNames   []string
		wantRemoved int
	}{
		{"deprecated only, and the other components", "1.30", []string{"any-kind-of-this-version", "fs", "tls"}, 1},
		{"at the removal of one", "1.32", []string{"fs", "any-kind-of-this-version", "gpu", "tls"}, 2},
		{"all of them", "1.37", []string{"gpu", "fs", "any-kind-of-this-version", "vac", "tls"}, 3},
		{"a patch version is its minor", "v1.34.2", []string{"gpu", "fs", "any-kind-of-this-version", "vac", "tls"}, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := set.Evaluate(objects, tt.target)

			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			var names []string
			removed := 0
			for _, f := range got {
				names = append(names, f.Name)
				if f.Removed {
					removed++
				}
			}
			if strings.Join(names, ",") != strings.Join(tt.wantNames, ",") || removed != tt.wantRemoved {
				t.Fatalf("got %v (%d removed), want %v (%d removed)", names, removed, tt.wantNames, tt.wantRemoved)
			}
		})
	}
}

func TestEvaluateFillsTheFinding(t *testing.T) {
	got, err := rules(t).Evaluate([]deprecation.Object{obj("resource.k8s.io/v1alpha3", "DeviceClass", "", "gpu")}, "1.36")

	if err != nil || len(got) != 1 {
		t.Fatalf("Evaluate = %v, %v", got, err)
	}
	want := deprecation.Finding{Kind: "DeviceClass", Name: "gpu", APIVersion: "resource.k8s.io/v1alpha3", Component: "k8s",
		DeprecatedIn: "v1.32.0", RemovedIn: "v1.34.0", ReplacementAPI: "resource.k8s.io/v1", ReplacementAvailableIn: "v1.34.0",
		Source: deprecation.SourceManifest, Removed: true}
	if got[0] != want {
		t.Fatalf("got %+v\nwant %+v", got[0], want)
	}
}

func TestEvaluateRefusesABadTarget(t *testing.T) {
	for _, target := range []string{"", "latest", "1", "1.x", "1.2.3.4"} {
		if _, err := rules(t).Evaluate(nil, target); err == nil {
			t.Errorf("target %q accepted", target)
		}
	}
}

func TestBlocksUpgrade(t *testing.T) {
	removed := deprecation.Finding{Namespace: "web", Kind: "Ingress", Name: "site", APIVersion: "extensions/v1beta1",
		ReplacementAPI: "networking.k8s.io/v1", Removed: true}
	clusterScoped := deprecation.Finding{Kind: "DeviceClass", Name: "gpu", APIVersion: "resource.k8s.io/v1alpha3", Removed: true}
	deprecated := deprecation.Finding{Kind: "VolumeAttributesClass", Name: "vac"}
	tests := []struct {
		name     string
		findings []deprecation.Finding
		want     bool
		reason   string
	}{
		{"nothing found", nil, false, ""},
		{"deprecated only", []deprecation.Finding{deprecated}, false, ""},
		{"one removed", []deprecation.Finding{deprecated, removed}, true,
			"1 object(s) use an API version removed in Kubernetes 1.37, e.g. Ingress web/site (extensions/v1beta1, use networking.k8s.io/v1)"},
		{"cluster-scoped, no replacement", []deprecation.Finding{clusterScoped, removed}, true,
			"2 object(s) use an API version removed in Kubernetes 1.37, e.g. DeviceClass gpu (resource.k8s.io/v1alpha3)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, reason := deprecation.BlocksUpgrade(tt.findings, "1.37")
			if got != tt.want || reason != tt.reason {
				t.Fatalf("BlocksUpgrade = %v, %q; want %v, %q", got, reason, tt.want, tt.reason)
			}
		})
	}
}

func TestTarget(t *testing.T) {
	tests := []struct{ current, upgradeTo, want string }{
		{"1.36", "", "1.37"},
		{"1.36.2", "", "1.37"},
		{"v1.35.4-eks-1", "", "1.36"},
		{"1.35", "1.36", "1.36"},
		{"1.35", "not-a-version", "1.36"},
	}
	for _, tt := range tests {
		got, err := deprecation.Target(tt.current, tt.upgradeTo)
		if err != nil || got != tt.want {
			t.Errorf("Target(%q, %q) = %q, %v; want %q", tt.current, tt.upgradeTo, got, err, tt.want)
		}
	}
	if _, err := deprecation.Target("", ""); err == nil {
		t.Error("an empty current version accepted")
	}
}

func TestSummarizeCountsAllAndKeepsSome(t *testing.T) {
	findings := []deprecation.Finding{{Name: "a", Removed: true}, {Name: "b", Removed: true}, {Name: "c"}}

	r := deprecation.Summarize("1.37", findings, 2)

	if r.Total != 3 || r.Removed != 2 || len(r.Findings) != 2 || !r.Truncated() || r.Target != "1.37" {
		t.Fatalf("got %+v", r)
	}
	if deprecation.Summarize("1.37", findings, 10).Truncated() {
		t.Error("a report that holds everything is not truncated")
	}
}

// helmSecret is a release as Helm v3 stores it in a Secret's "release"
// key: base64 of the gzipped JSON.
func helmSecret(t *testing.T, name, namespace, status string, revision int, manifest string) deprecation.HelmSecret {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"name": name, "namespace": namespace, "version": revision,
		"manifest": manifest, "info": map[string]string{"status": status}})
	if err != nil {
		t.Fatal(err)
	}
	var zipped bytes.Buffer
	zw := gzip.NewWriter(&zipped)
	if _, err := zw.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return deprecation.HelmSecret{Namespace: namespace, Name: "sh.helm.release.v1." + name,
		Release: []byte(base64.StdEncoding.EncodeToString(zipped.Bytes()))}
}

func TestDecodeHelmRelease(t *testing.T) {
	secret := helmSecret(t, "site", "web", "deployed", 3, "kind: Ingress\n")

	name, namespace, status, manifest, revision, err := deprecation.DecodeHelmRelease(secret.Release)

	if err != nil || name != "site" || namespace != "web" || status != "deployed" || revision != 3 || manifest != "kind: Ingress\n" {
		t.Fatalf("got %q %q %q %q %d, %v", name, namespace, status, manifest, revision, err)
	}
	plain := base64.StdEncoding.EncodeToString([]byte(`{"name":"plain","info":{"status":"deployed"}}`))
	if name, _, _, _, _, err := deprecation.DecodeHelmRelease([]byte(plain)); err != nil || name != "plain" {
		t.Errorf("a release that is not gzipped: %q, %v", name, err)
	}
	for _, bad := range []string{"not base64!", base64.StdEncoding.EncodeToString([]byte("not json")),
		base64.StdEncoding.EncodeToString([]byte{0x1f, 0x8b, 0x08, 0x00, 0xff})} {
		if _, _, _, _, _, err := deprecation.DecodeHelmRelease([]byte(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// fakeCluster is a workload cluster in memory, paged one item at a time.
type fakeCluster struct {
	resources    []deprecation.Resource
	objects      map[string][]deprecation.Meta
	helm         []deprecation.HelmSecret
	failResource string
	failHelm     bool
	failAll      error
	listed       []string
}

func (f *fakeCluster) Resources(context.Context) ([]deprecation.Resource, error) {
	return f.resources, f.failAll
}

func page[T any](items []T, token string) ([]T, string) {
	i := len(token) // the token is as long as the index
	if i >= len(items) {
		return nil, ""
	}
	next := ""
	if i+1 < len(items) {
		next = token + "x"
	}
	return items[i : i+1], next
}

func (f *fakeCluster) ListMeta(_ context.Context, r deprecation.Resource, _ int64, token string) ([]deprecation.Meta, string, error) {
	if token == "" {
		f.listed = append(f.listed, r.Resource)
	}
	if r.Resource == f.failResource {
		return nil, "", errors.New("forbidden")
	}
	items, next := page(f.objects[r.Resource], token)
	return items, next, nil
}

func (f *fakeCluster) ListHelmSecrets(_ context.Context, _ int64, token string) ([]deprecation.HelmSecret, string, error) {
	if f.failHelm {
		return nil, "", errors.New("forbidden")
	}
	items, next := page(f.helm, token)
	return items, next, nil
}

func applied(namespace, name, manifest string) deprecation.Meta {
	return deprecation.Meta{Namespace: namespace, Name: name,
		Annotations: map[string]string{deprecation.LastAppliedAnnotation: manifest}}
}

func cluster(t *testing.T) *fakeCluster {
	t.Helper()
	return &fakeCluster{
		resources: []deprecation.Resource{
			{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses", Kind: "Ingress"},
			{Group: "networking.k8s.io", Version: "v1", Resource: "ingresses/status", Kind: "Ingress"},
			{Group: "apps", Version: "v1", Resource: "deployments", Kind: "Deployment"},
			{Group: "", Version: "v1", Resource: "configmaps", Kind: "ConfigMap"},
			{Group: "events.k8s.io", Version: "v1", Resource: "events", Kind: "Event"},
			{Group: "authentication.istio.io", Version: "v1alpha1", Resource: "policies", Kind: "Policy"},
		},
		objects: map[string][]deprecation.Meta{
			"ingresses": {
				applied("web", "old", `{"apiVersion":"extensions/v1beta1","kind":"Ingress","metadata":{"name":"old","namespace":"web"}}`),
				applied("web", "new", `{"apiVersion":"networking.k8s.io/v1","kind":"Ingress","metadata":{"name":"new"}}`),
				{Namespace: "web", Name: "no-annotation"},
				applied("web", "broken", `{not json`),
				applied("web", "other-kind", `{"apiVersion":"extensions/v1beta1","kind":"Deployment"}`),
			},
			"deployments": {applied("web", "app", `{"apiVersion":"apps/v1beta2","kind":"Deployment"}`)},
			"configmaps":  {applied("web", "cm", `{"apiVersion":"extensions/v1beta1","kind":"ConfigMap"}`)},
			"events":      {applied("web", "ev", `{"apiVersion":"events.k8s.io/v1beta1","kind":"Event"}`)},
		},
		helm: []deprecation.HelmSecret{
			helmSecret(t, "site", "web", "deployed", 1, "apiVersion: extensions/v1beta1\nkind: Ingress\nmetadata:\n  name: stale-revision\n"),
			helmSecret(t, "site", "web", "deployed", 2, strings.Join([]string{
				"# Source: site/templates/pdb.yaml",
				"apiVersion: policy/v1beta1",
				"kind: PodDisruptionBudget",
				"metadata:",
				"  name: site",
				"---",
				"apiVersion: v1",
				"kind: List",
				"items:",
				"- apiVersion: batch/v1beta1",
				"  kind: CronJob",
				"  metadata: {name: nightly, namespace: jobs}",
				"---",
				"",
				"---",
				"not: [valid",
			}, "\n")),
			helmSecret(t, "gone", "web", "superseded", 9, "apiVersion: extensions/v1beta1\nkind: Ingress\nmetadata: {name: superseded}\n"),
			{Namespace: "web", Name: "sh.helm.release.v1.garbage", Release: []byte("garbage")},
		},
	}
}

func TestScanFindsAppliedAndHelmObjects(t *testing.T) {
	c := cluster(t)

	report, err := rules(t).Scan(context.Background(), c, "1.37")

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	var got []string
	for _, f := range report.Findings {
		got = append(got, strings.Join([]string{f.Namespace, f.Kind, f.Name, f.APIVersion, string(f.Source), f.HelmRelease}, " "))
	}
	want := []string{
		"jobs CronJob nightly batch/v1beta1 HelmRelease web/site",
		"web Deployment app apps/v1beta2 Manifest ",
		"web Ingress old extensions/v1beta1 Manifest ",
		"web PodDisruptionBudget site policy/v1beta1 HelmRelease web/site",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	if report.Total != 4 || report.Removed != 4 || report.Target != "1.37" || report.Truncated() || len(report.Skipped) != 0 {
		t.Errorf("report = %+v", report)
	}
	// Only kinds a rule names are listed: no ConfigMaps, no Events, no subresources.
	if strings.Join(c.listed, ",") != "ingresses,deployments,policies" {
		t.Errorf("listed %v", c.listed)
	}
}

func TestScanGoesOnWithoutWhatItCannotList(t *testing.T) {
	c := cluster(t)
	c.failResource, c.failHelm = "ingresses", true

	report, err := rules(t).Scan(context.Background(), c, "1.37")

	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if report.Total != 1 || report.Findings[0].Name != "app" {
		t.Errorf("findings = %+v", report.Findings)
	}
	if strings.Join(report.Skipped, ",") != "helm releases,ingresses" {
		t.Errorf("skipped = %v", report.Skipped)
	}
}

func TestScanFailsWhenTheClusterCannotBeRead(t *testing.T) {
	c := cluster(t)
	c.failAll = errors.New("connection refused")
	if _, err := rules(t).Scan(context.Background(), c, "1.37"); err == nil || !strings.Contains(err.Error(), "connection refused") {
		t.Errorf("Scan = %v", err)
	}
	if _, err := rules(t).Scan(context.Background(), cluster(t), "soon"); err == nil {
		t.Error("a bad target accepted")
	}
	done, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := rules(t).Scan(done, cluster(t), "1.37"); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled scan = %v", err)
	}
}
