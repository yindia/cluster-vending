package policy

import (
	"fmt"
	"math"
	"strings"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func str(m map[string]any, path ...string) string {
	s, _, _ := unstructured.NestedString(m, path...)
	return clip(s, 2048)
}
func object(m map[string]any, path ...string) map[string]any {
	v, _, _ := unstructured.NestedMap(m, path...)
	return v
}
func items(m map[string]any, path ...string) []any {
	v, _, _ := unstructured.NestedSlice(m, path...)
	return v
}
func ref(m map[string]any) *cloudv1.PolicyObjectReference {
	return &cloudv1.PolicyObjectReference{ApiVersion: clip(str(m, "apiVersion"), 128), Kind: clip(str(m, "kind"), 128), Namespace: clip(str(m, "namespace"), 63), Name: clip(str(m, "name"), 253)}
}
func parseTime(s string) *timestamppb.Timestamp {
	t, e := time.Parse(time.RFC3339Nano, s)
	if e != nil {
		return nil
	}
	return timestamp(t)
}
func mode(action string) string {
	switch action {
	case "deny", "Resource Blocked":
		return "enforce"
	case "dryrun", "Resource Passed":
		return "audit"
	case "warn":
		return "warn"
	}
	return "unknown"
}
func report(p *cloudv1.ClusterPolicySnapshot, u *unstructured.Unstructured) {
	reportLimit(p, u, MaxRows)
}

// reportLimit is report keeping at most max violations (a snapshot written
// to the results bucket is bounded by its object size instead of MaxRows).
func reportLimit(p *cloudv1.ClusterPolicySnapshot, u *unstructured.Unstructured, max int) {
	for i, v := range items(u.Object, "results") {
		m, ok := v.(map[string]any)
		if !ok {
			notice(p, "Malformed policy report result.")
			continue
		}
		result := str(m, "result")
		switch result {
		case "pass", "skip":
			continue
		case "fail", "warn", "error":
		default:
			notice(p, "Unrecognized policy report result: "+result)
			continue
		}
		source := str(m, "source")
		engine := source
		kind := ""
		switch {
		case source == "ValidatingAdmissionPolicy":
			engine = "vap"
			kind = "ValidatingAdmissionPolicy"
		case source == "kyverno":
			engine = "kyverno"
			kind = ""
		case strings.HasPrefix(source, "Kyverno"):
			engine = "kyverno"
			kind = strings.TrimPrefix(source, "Kyverno")
		default:
			notice(p, "Report source is unknown: "+source)
		}
		name := str(m, "policy")
		ns := ""
		if a, b, ok := strings.Cut(name, "/"); ok {
			ns, name = a, b
			if source == "kyverno" {
				kind = "Policy"
			} else if engine == "kyverno" && !strings.HasPrefix(kind, "Namespaced") {
				kind = "Namespaced" + kind
			}
		}
		if name == "" {
			notice(p, "A report result has no policy identity.")
		}
		policyRef := &cloudv1.PolicyObjectReference{Kind: kind, Name: clip(name, 253), Namespace: clip(ns, 63)}
		targets := items(m, "resources")
		if len(targets) == 0 {
			targets = []any{object(u.Object, "scope")}
		}
		for j, t := range targets {
			if len(p.Violations) >= max {
				p.Summary.Truncated = true
				return
			}
			tm, _ := t.(map[string]any)
			r := ref(tm)
			if r.Name == "" {
				notice(p, "A report result has no resource identity.")
			}
			rule := str(m, "rule")
			if rule == "" {
				rule = str(m, "properties", "cel.validationIndex")
			}
			var observed *timestamppb.Timestamp
			secs, ok, _ := unstructured.NestedInt64(m, "timestamp", "seconds")
			if ok && secs > 0 {
				observed = timestamp(time.Unix(secs, 0))
			}
			p.Violations = append(p.Violations, &cloudv1.ClusterViolation{Id: identity(u.GetAPIVersion(), u.GetNamespace(), u.GetName(), fmt.Sprint(i, j)), Engine: engine, Policy: policyRef, Resource: r, Rule: rule, Message: str(m, "message"), Result: result, Severity: str(m, "severity"), Enforcement: "unknown", ObservedAt: observed})
		}
	}
}
func constraint(p *cloudv1.ClusterPolicySnapshot, u *unstructured.Unstructured) {
	constraintLimit(p, u, MaxRows)
}

func constraintLimit(p *cloudv1.ClusterPolicySnapshot, u *unstructured.Unstructured, max int) {
	rows := items(u.Object, "status", "violations")
	total, known, _ := unstructured.NestedInt64(u.Object, "status", "totalViolations")
	observed := parseTime(str(u.Object, "status", "auditTimestamp"))
	if !known || observed == nil {
		notice(p, "Gatekeeper constraint has no complete audit status: "+u.GetName())
	}
	if total > int64(len(rows)) {
		p.Summary.Truncated = true
		notice(p, "Gatekeeper limits the violations stored in constraint status.")
	}
	for i, v := range rows {
		if len(p.Violations) >= max {
			p.Summary.Truncated = true
			return
		}
		m, ok := v.(map[string]any)
		if !ok {
			notice(p, "Malformed Gatekeeper violation.")
			continue
		}
		if mode(str(m, "enforcementAction")) == "unknown" {
			notice(p, "Gatekeeper enforcement is unknown or scoped: "+str(m, "enforcementAction"))
		}
		r := ref(m)
		r.ApiVersion = str(m, "version")
		if g := str(m, "group"); g != "" {
			r.ApiVersion = g + "/" + r.ApiVersion
		}
		r.ApiVersion = clip(r.ApiVersion, 128)
		p.Violations = append(p.Violations, &cloudv1.ClusterViolation{Id: identity(u.GetKind(), u.GetName(), fmt.Sprint(i)), Engine: "gatekeeper", Policy: &cloudv1.PolicyObjectReference{ApiVersion: clip(u.GetAPIVersion(), 128), Kind: clip(u.GetKind(), 128), Name: clip(u.GetName(), 253), Namespace: clip(u.GetNamespace(), 63)}, Resource: r, Message: str(m, "message"), Result: "fail", Enforcement: mode(str(m, "enforcementAction")), ObservedAt: observed})
	}
}
func event(u *unstructured.Unstructured) *cloudv1.ClusterViolationEvent {
	m := u.Object
	reason := str(m, "reason")
	source := str(m, "source", "component")
	reporter := str(m, "reportingComponent")
	e := &cloudv1.ClusterViolationEvent{Id: u.GetNamespace() + "/" + u.GetName(), Reason: reason, Message: str(m, "message"), Action: str(m, "action"), Enforcement: "unknown"}
	switch {
	case reason == "PolicyViolation" && (source == "kyverno-admission" || reporter == "kyverno-admission"):
		involved := object(m, "involvedObject")
		related := object(m, "related")
		kind := str(involved, "kind")
		switch kind {
		case "Policy", "ClusterPolicy", "ValidatingPolicy", "NamespacedValidatingPolicy", "ImageValidatingPolicy", "NamespacedImageValidatingPolicy", "MutatingPolicy", "NamespacedMutatingPolicy", "GeneratingPolicy", "NamespacedGeneratingPolicy":
		default:
			return nil
		}
		if len(related) == 0 {
			return nil
		}
		e.Engine = "kyverno"
		e.Policy = ref(involved)
		e.Resource = ref(related)
		e.Enforcement = mode(e.Action)
	case (reason == "FailedAdmission" || reason == "WarningAdmission" || reason == "DryrunViolation") && (source == "gatekeeper-webhook" || reporter == "gatekeeper-webhook"):
		a := u.GetAnnotations()
		if a["process"] != "admission" || a["constraint_kind"] == "" {
			return nil
		}
		e.Engine = "gatekeeper"
		// Annotations are workload-controlled; clip like every other field.
		e.Policy = &cloudv1.PolicyObjectReference{Kind: clip(a["constraint_kind"], 128), Name: clip(a["constraint_name"], 253), Namespace: clip(a["constraint_namespace"], 63)}
		e.Resource = &cloudv1.PolicyObjectReference{Kind: clip(a["resource_kind"], 128), Name: clip(a["resource_name"], 253), Namespace: clip(a["resource_namespace"], 63), ApiVersion: clip(a["resource_api_version"], 128)}
		if g := clip(a["resource_group"], 128); g != "" {
			e.Resource.ApiVersion = clip(g+"/"+e.Resource.ApiVersion, 128)
		}
		e.Action = clip(a["constraint_action"], 128)
		e.Enforcement = mode(e.Action)
	default:
		return nil
	}
	for _, path := range [][]string{{"series", "lastObservedTime"}, {"lastTimestamp"}, {"eventTime"}, {"firstTimestamp"}, {"metadata", "creationTimestamp"}} {
		if e.LastObservedAt = parseTime(str(m, path...)); e.LastObservedAt != nil {
			break
		}
	}
	n, _, _ := unstructured.NestedInt64(m, "series", "count")
	if n <= 0 {
		n, _, _ = unstructured.NestedInt64(m, "count")
	}
	if n <= 0 {
		n = 1
	}
	e.Count = int32(min(n, math.MaxInt32))
	return e
}
