package deprecation

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// LastAppliedAnnotation is where kubectl apply keeps the manifest it was
// given.
const LastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

// Limits of one scan.
const (
	// PageSize is how many objects one list call asks for.
	PageSize = 250
	// HelmPageSize is the same for Helm release Secrets, which can be a
	// megabyte each.
	HelmPageSize = 20
	// ScanTimeout bounds a whole scan.
	ScanTimeout = 2 * time.Minute
	// MaxFindings is how many findings a report keeps (Total counts all).
	MaxFindings = 200
	// maxHelmManifest bounds a decompressed Helm release (a gzip bomb
	// guard; Helm itself caps a release Secret at 1 MiB compressed).
	maxHelmManifest = 32 << 20
)

// skippedResources are never listed: high-volume kinds that controllers
// write, not people (a Helm release that declares one is still found).
var skippedResources = map[string]bool{"events": true, "leases": true, "endpoints": true, "endpointslices": true,
	"replicasets": true}

// Resource is one listable resource of a cluster.
type Resource struct {
	Group, Version, Resource, Kind string
}

// Meta is the part of a live object a scan reads.
type Meta struct {
	Namespace, Name string
	Annotations     map[string]string
}

// HelmSecret is a Helm v3 release Secret: Release is its "release" key
// as stored (base64 of gzipped JSON).
type HelmSecret struct {
	Namespace, Name string
	Release         []byte
}

// Reader reads a workload cluster. Lists are paged: next is the token of
// the following page, empty on the last.
type Reader interface {
	// Resources are the cluster's listable resources, one (the preferred)
	// version per group.
	Resources(ctx context.Context) ([]Resource, error)
	// ListMeta lists the objects of r, metadata only.
	ListMeta(ctx context.Context, r Resource, limit int64, token string) (items []Meta, next string, err error)
	// ListHelmSecrets lists the Secrets of deployed Helm v3 releases.
	ListHelmSecrets(ctx context.Context, limit int64, token string) (items []HelmSecret, next string, err error)
}

// Report is the outcome of a scan.
type Report struct {
	// Target is the Kubernetes minor the cluster was judged against.
	Target string
	// Total and Removed count every finding; Findings holds at most
	// MaxFindings of them, removed first.
	Total, Removed int
	Findings       []Finding
	// Skipped are the resources that could not be listed (forbidden,
	// gone), by name; the scan goes on without them.
	Skipped []string
}

// Truncated: the report holds fewer findings than were found.
func (r Report) Truncated() bool { return r.Total > len(r.Findings) }

// Summarize is the report of findings: counted in full, kept up to max.
func Summarize(target string, findings []Finding, max int) Report {
	r := Report{Target: target, Total: len(findings)}
	for _, f := range findings {
		if f.Removed {
			r.Removed++
		}
	}
	r.Findings = findings[:min(len(findings), max)]
	return r
}

// Scan reads the cluster's manifests (last-applied annotations and Helm
// releases) and reports those deprecated or removed at target. It fails
// only when the cluster cannot be read at all; a resource that cannot be
// listed is skipped and named in the report.
func (s RuleSet) Scan(ctx context.Context, cluster Reader, target string) (Report, error) {
	if _, ok := parseVersion(target); !ok {
		return Report{}, fmt.Errorf("target version %q is not a Kubernetes version", target)
	}
	ctx, cancel := context.WithTimeout(ctx, ScanTimeout)
	defer cancel()

	objects, skipped, err := s.applied(ctx, cluster)
	if err != nil {
		return Report{}, err
	}
	helm, err := helmObjects(ctx, cluster)
	switch {
	case ctx.Err() != nil:
		return Report{}, fmt.Errorf("scan: %w", ctx.Err())
	case err != nil:
		skipped = append(skipped, "helm releases")
	}
	findings, err := s.Evaluate(append(objects, helm...), target)
	if err != nil {
		return Report{}, err
	}
	report := Summarize(target, findings, MaxFindings)
	slices.Sort(skipped)
	report.Skipped = skipped
	return report, nil
}

// watched says whether objects of r can match a rule: a manifest counts
// only when it has the object's kind (as Pluto), so a kind no rule names
// is not listed at all.
func (s RuleSet) watched(r Resource) bool {
	if skippedResources[r.Resource] || strings.Contains(r.Resource, "/") {
		return false
	}
	for _, rule := range s.Rules {
		if rule.Kind == r.Kind {
			return true
		}
		if group, _, _ := strings.Cut(rule.APIVersion, "/"); rule.Kind == "" && group == r.Group {
			return true
		}
	}
	return false
}

// applied are the manifests kubectl apply left on live objects.
func (s RuleSet) applied(ctx context.Context, cluster Reader) (objects []Object, skipped []string, err error) {
	resources, err := cluster.Resources(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("discover the cluster's resources: %w", err)
	}
	for _, r := range resources {
		if !s.watched(r) {
			continue
		}
		found, err := appliedOf(ctx, cluster, r)
		if ctx.Err() != nil {
			return nil, nil, fmt.Errorf("scan: %w", ctx.Err())
		}
		if err != nil {
			skipped = append(skipped, r.Resource)
			continue
		}
		objects = append(objects, found...)
	}
	return objects, skipped, nil
}

func appliedOf(ctx context.Context, cluster Reader, r Resource) ([]Object, error) {
	var out []Object
	for token := ""; ; {
		items, next, err := cluster.ListMeta(ctx, r, PageSize, token)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			if o, ok := lastApplied(item, r.Kind); ok {
				out = append(out, o)
			}
		}
		if token = next; token == "" {
			return out, nil
		}
	}
}

// stub is what a scan reads of a manifest.
type stub struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Items []json.RawMessage `json:"items"`
}

// lastApplied is the object the annotation of item declares; none when
// there is no annotation, it does not parse, or it is of another kind.
func lastApplied(item Meta, kind string) (Object, bool) {
	raw, ok := item.Annotations[LastAppliedAnnotation]
	if !ok {
		return Object{}, false
	}
	var m stub
	if err := json.Unmarshal([]byte(raw), &m); err != nil || m.Kind != kind || m.APIVersion == "" {
		return Object{}, false
	}
	return Object{APIVersion: m.APIVersion, Kind: m.Kind, Namespace: item.Namespace, Name: item.Name, Source: SourceManifest}, true
}

// helmRelease is what a scan reads of a stored Helm v3 release.
type helmRelease struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
	Version   int    `json:"version"`
	Manifest  string `json:"manifest"`
	Info      struct {
		Status string `json:"status"`
	} `json:"info"`
}

// DecodeHelmRelease reads the "release" key of a Helm v3 release Secret:
// base64 of the (gzipped) JSON release.
func DecodeHelmRelease(stored []byte) (name, namespace, status, manifest string, revision int, err error) {
	raw := make([]byte, base64.StdEncoding.DecodedLen(len(stored)))
	n, err := base64.StdEncoding.Decode(raw, bytes.TrimSpace(stored))
	if err != nil {
		return "", "", "", "", 0, fmt.Errorf("helm release: %w", err)
	}
	raw = raw[:n]
	if len(raw) > 3 && raw[0] == 0x1f && raw[1] == 0x8b && raw[2] == 0x08 {
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return "", "", "", "", 0, fmt.Errorf("helm release: %w", err)
		}
		defer func() { _ = zr.Close() }()
		if raw, err = io.ReadAll(io.LimitReader(zr, maxHelmManifest+1)); err != nil {
			return "", "", "", "", 0, fmt.Errorf("helm release: %w", err)
		}
		if len(raw) > maxHelmManifest {
			return "", "", "", "", 0, errors.New("helm release: larger than 32 MiB")
		}
	}
	var r helmRelease
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", "", "", "", 0, fmt.Errorf("helm release: %w", err)
	}
	return r.Name, r.Namespace, r.Info.Status, r.Manifest, r.Version, nil
}

// helmObjects are the objects of the newest deployed revision of every
// Helm release. A Secret that cannot be read as a release is passed over.
func helmObjects(ctx context.Context, cluster Reader) ([]Object, error) {
	type deployed struct {
		revision int
		manifest string
	}
	releases := map[string]deployed{}
	for token := ""; ; {
		items, next, err := cluster.ListHelmSecrets(ctx, HelmPageSize, token)
		if err != nil {
			return nil, err
		}
		for _, item := range items {
			name, namespace, status, manifest, revision, err := DecodeHelmRelease(item.Release)
			if err != nil || status != "deployed" {
				continue
			}
			if namespace == "" {
				namespace = item.Namespace
			}
			key := namespace + "/" + name
			if cur, ok := releases[key]; !ok || revision > cur.revision {
				releases[key] = deployed{revision, manifest}
			}
		}
		if token = next; token == "" {
			break
		}
	}
	var out []Object
	for key, r := range releases {
		namespace, _, _ := strings.Cut(key, "/")
		out = append(out, manifestObjects(r.manifest, namespace, key)...)
	}
	return out, nil
}

// manifestObjects are the objects of a multi-document YAML manifest;
// lists are opened. An object without a namespace gets the release's.
func manifestObjects(manifest, namespace, release string) []Object {
	var out []Object
	for _, doc := range strings.Split("\n"+manifest, "\n---") {
		if strings.TrimSpace(doc) == "" {
			continue
		}
		raw, err := yaml.YAMLToJSON([]byte(doc))
		if err != nil {
			continue
		}
		out = appendStub(out, raw, namespace, release)
	}
	return out
}

func appendStub(out []Object, raw []byte, namespace, release string) []Object {
	var m stub
	if err := json.Unmarshal(raw, &m); err != nil {
		return out
	}
	for _, item := range m.Items {
		out = appendStub(out, item, namespace, release)
	}
	if m.APIVersion == "" || m.Kind == "" || len(m.Items) > 0 {
		return out
	}
	if m.Metadata.Namespace != "" {
		namespace = m.Metadata.Namespace
	}
	return append(out, Object{APIVersion: m.APIVersion, Kind: m.Kind, Namespace: namespace, Name: m.Metadata.Name,
		Source: SourceHelmRelease, HelmRelease: release})
}
