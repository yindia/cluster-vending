package naas

import (
	"bytes"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"text/template"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	cloud "github.com/yindia/cluster-vending/api/v1alpha1"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

func accessCluster() *cloudv1.ClusterSpec {
	return &cloudv1.ClusterSpec{Provider: &cloudv1.ClusterSpec_Aws{Aws: &cloudv1.AwsClusterSpec{}}, Access: &cloudv1.AccessSpec{Oidc: &cloudv1.OidcIdentityProvider{
		IssuerUrl: "https://login.example.test", ClientId: "kubernetes", GroupsClaim: "groups", GroupsPrefix: "oidc:",
	}}}
}

func TestValidateAccessGroupsExactCaseAndBounds(t *testing.T) {
	if err := ValidateAccessGroups([]string{"Team", "team", "finance viewers", "研究者"}, nil); err != nil {
		t.Fatal(err)
	}
	for _, group := range []string{"", " ", " group", "group ", "group\nname", "group\x7f", "system:masters", "SYSTEM:authenticated", "system:serviceaccount:ns:sa", "team*", "team?", strings.Repeat("x", 257), string([]byte{0xff})} {
		if err := ValidateAccessGroups([]string{group}, nil); err == nil {
			t.Errorf("accepted unsafe group %q", group)
		}
	}
	for _, access := range []cloud.SubTenantAccess{
		{AdminGroups: []string{"team", "team"}}, {AdminGroups: []string{"team"}, ViewerGroups: []string{"team"}},
		{ViewerGroups: slices.Repeat([]string{"team"}, 33)},
	} {
		if err := ValidateAccessGroups(access.AdminGroups, access.ViewerGroups); err == nil {
			t.Errorf("accepted duplicate/over-limit groups: %+v", access)
		}
	}
	if err := ValidateAccessGroups([]string{strings.Repeat("界", 256)}, nil); err != nil {
		t.Fatal("bounds must count Unicode characters, not UTF-8 bytes", err)
	}
}

func TestResolveAccessExactGroupsStableIDsAndEffectiveOIDC(t *testing.T) {
	spec := accessCluster()
	access := &cloud.SubTenantAccess{AdminGroups: []string{"Team", "team"}, ViewerGroups: []string{"Readers"}}
	resolved, err := ResolveAccess(access, spec, openGroups)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Bindings) != 3 || !strings.HasPrefix(resolved.OIDCRevision, "sha256:") {
		t.Fatal("missing bounded bindings or issuer-bound revision", resolved)
	}
	groups := map[string]string{}
	for _, binding := range resolved.Bindings {
		groups[binding.Group] = binding.Role
		if len(binding.ID) != 32 || len("naas-access-"+binding.ID) > 63 {
			t.Fatal("unsafe generated binding identity", binding)
		}
	}
	if !reflect.DeepEqual(groups, map[string]string{"oidc:Team": WorkloadAdminRole, "oidc:team": WorkloadAdminRole, "oidc:Readers": WorkloadViewerRole}) {
		t.Fatal("changed exact claim case or role", groups)
	}
	access.AdminGroups[0], access.AdminGroups[1] = access.AdminGroups[1], access.AdminGroups[0]
	reordered, err := ResolveAccess(access, spec, openGroups)
	if err != nil || !reflect.DeepEqual(resolved, reordered) {
		t.Fatal("reordering changed binding identity", err)
	}
	access.ViewerGroups = append(access.ViewerGroups, "Extra")
	added, err := ResolveAccess(access, spec, openGroups)
	if err != nil {
		t.Fatal(err)
	}
	for _, binding := range resolved.Bindings {
		if !slices.Contains(added.Bindings, binding) {
			t.Fatal("adding a group renamed an existing binding")
		}
	}
	data := resolved.BindingData()
	data[0].(map[string]any)["role"] = "cluster-admin"
	if resolved.BindingData()[0].(map[string]any)["role"] == "cluster-admin" {
		t.Fatal("template data aliases resolution")
	}
	spec.Access.Oidc.ClientId = "different-audience"
	changed, err := ResolveAccess(access, spec, openGroups)
	if err != nil || changed.OIDCRevision == resolved.OIDCRevision || slices.Contains(changed.Bindings, resolved.Bindings[0]) {
		t.Fatal("OIDC audience change reused previously qualified binding identity", err)
	}
	spec.Access.Oidc = &cloudv1.OidcIdentityProvider{Entra: &cloudv1.EntraIdentityProvider{TenantId: "directory", ClientId: "public-client"}}
	entra, err := ResolveAccess(&cloud.SubTenantAccess{ViewerGroups: []string{"GroupObjectID"}}, spec, openGroups)
	if err != nil || entra.Bindings[0].Group != "entra:GroupObjectID" {
		t.Fatal("did not use the shared Entra mapping", entra, err)
	}
	spec.Access.Oidc = accessCluster().Access.Oidc
	spec.Access.Oidc.GroupsPrefix = ""
	unprefixed, err := ResolveAccess(&cloud.SubTenantAccess{ViewerGroups: []string{"Exact"}}, spec, openGroups)
	if err != nil || unprefixed.Bindings[0].Group != "Exact" {
		t.Fatal("invented a prefix not configured on EKS", err)
	}
}

func TestResolveAccessFailsClosedWithoutIdentityConfiguration(t *testing.T) {
	access := &cloud.SubTenantAccess{AdminGroups: []string{"admins"}}
	for _, spec := range []*cloudv1.ClusterSpec{nil, {}, {Access: accessCluster().Access}, {Provider: accessCluster().Provider}, {Provider: accessCluster().Provider, Access: &cloudv1.AccessSpec{Oidc: &cloudv1.OidcIdentityProvider{IssuerUrl: "https://example.test", ClientId: "client"}}}} {
		if _, err := ResolveAccess(access, spec, openGroups); !errors.Is(err, ErrAccessOIDCNotConfigured) {
			t.Errorf("OIDC prerequisite was not enforced: %v", err)
		}
	}
	for _, prefix := range []string{"system:", "SYSTEM:", "oidc:*", " oidc:", "oidc:\n"} {
		spec := accessCluster()
		spec.Access.Oidc.GroupsPrefix = prefix
		if _, err := ResolveAccess(access, spec, openGroups); err == nil {
			t.Errorf("accepted unsafe prefix %q", prefix)
		}
	}
	if _, err := ResolveAccess(&cloud.SubTenantAccess{ViewerGroups: []string{"oidc:already-prefixed"}}, accessCluster(), openGroups); err == nil {
		t.Fatal("accepted already-prefixed group")
	}
	if _, err := ResolveAccess(&cloud.SubTenantAccess{Bindings: []cloud.SubTenantAccessBinding{{IdentityRef: "admins", RoleProfile: "workload-editor"}}}, accessCluster(), openGroups); !errors.Is(err, ErrLegacyAccessBindings) {
		t.Fatal("legacy identityRef was accepted as a group", err)
	}
	for _, access := range []*cloud.SubTenantAccess{nil, {}} {
		if empty, err := ResolveAccess(access, nil, openGroups); err != nil || len(empty.Bindings) != 0 {
			t.Fatal("empty intent should grant nothing and require no OIDC", err)
		}
	}
}

func renderAccessGTR(t *testing.T, tenant *unstructured.Unstructured, namespace, ownerUID string) []*unstructured.Unstructured {
	t.Helper()
	var source string
	for _, obj := range mustLoad(t).Objects() {
		if obj.GetKind() == "GlobalTenantResource" && obj.GetName() == "naas-developer-access" {
			resources, _, _ := unstructured.NestedSlice(obj.Object, "spec", "resources")
			source = resources[0].(map[string]any)["generators"].([]any)[0].(map[string]any)["template"].(string)
		}
	}
	tpl, err := template.New("access").Option("missingkey=error").Parse(source)
	if err != nil {
		t.Fatal(err)
	}
	context := map[string]any{"tenant": tenant.Object, "namespace": map[string]any{"metadata": map[string]any{"name": namespace,
		"ownerReferences": []any{map[string]any{"apiVersion": TenantGVK.GroupVersion().String(), "kind": "Tenant", "name": tenant.GetName(), "uid": ownerUID}}}}}
	var output bytes.Buffer
	if err := tpl.Execute(&output, context); err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(output.String()) == "" {
		return nil
	}
	parsed, err := load(output.Bytes())
	if err != nil {
		t.Fatal(err, output.String())
	}
	return parsed.Objects()
}

func TestGroupAccessGTRScopesStableBindingsAndKeepsQualificationGate(t *testing.T) {
	sub := allocationFixture()
	sub.Spec.NamespaceKeys = []cloud.SubTenantNamespaceKey{"app", "worker"}
	sub.Spec.Access = &cloud.SubTenantAccess{AdminGroups: []string{`Team "Blue" {{ literal }}`}, ViewerGroups: []string{"Readers"}}
	remote, err := RenderTenantWithAccess(sub, "1.35", "revision", accessCluster(), openGroups)
	if err != nil {
		t.Fatal(err)
	}
	remote.SetUID("remote-uid")
	if len(renderAccessGTR(t, remote, remote.GetName()+"-app", "remote-uid")) != 0 {
		t.Fatal("configured groups opened access without qualification")
	}
	base, err := RenderTenant(sub, "1.35", "revision")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(remote.Object["spec"].(map[string]any)["owners"], base.Object["spec"].(map[string]any)["owners"]) || remote.GetAnnotations()[SpecAnnotation] == base.GetAnnotations()[SpecAnnotation] {
		t.Fatal("group intent changed owners or was omitted from the desired spec hash")
	}
	// Simulate a separately trusted qualification decision only in memory.
	labels := remote.GetLabels()
	labels[LabelAccessReady] = "true"
	remote.SetLabels(labels)
	var names []string
	for _, key := range []string{"app", "worker"} {
		namespace := remote.GetName() + "-" + key
		bindings := renderAccessGTR(t, remote, namespace, "remote-uid")
		if len(bindings) != 2 {
			t.Fatal("did not generate both group profiles", bindings)
		}
		for _, obj := range bindings {
			if obj.GetKind() != "RoleBinding" || obj.GetNamespace() != namespace || !strings.HasPrefix(obj.GetName(), "naas-access-") {
				t.Fatal("generated access outside the owned namespace", obj)
			}
			binding := &rbacv1.RoleBinding{}
			if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, binding); err != nil {
				t.Fatal(err)
			}
			if len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "Group" || !slices.Contains([]string{WorkloadAdminRole, WorkloadViewerRole}, binding.RoleRef.Name) {
				t.Fatal("unapproved subject or role", binding)
			}
			if binding.RoleRef.Name == WorkloadAdminRole && binding.Subjects[0].Name != `oidc:Team "Blue" {{ literal }}` {
				t.Fatal("claim quoting changed identity or evaluated template input")
			}
			names = append(names, obj.GetName())
		}
	}
	if !reflect.DeepEqual(names[:2], names[2:]) {
		t.Fatal("binding IDs depend on namespace or list position")
	}
	if len(renderAccessGTR(t, remote, "foreign-app", "remote-uid")) != 0 || len(renderAccessGTR(t, remote, remote.GetName()+"-app", "foreign-uid")) != 0 {
		t.Fatal("access rendered for unowned namespace identity")
	}
	sub.Spec.Access.AdminGroups = nil
	revoked, err := RenderTenantWithAccess(sub, "1.35", "revision", accessCluster(), openGroups)
	if err != nil {
		t.Fatal(err)
	}
	revoked.SetUID("remote-uid")
	revoked.SetLabels(labels)
	remaining := renderAccessGTR(t, revoked, remote.GetName()+"-app", "remote-uid")
	if len(remaining) != 1 || !slices.Contains(names, remaining[0].GetName()) {
		t.Fatal("group removal renamed a retained binding or retained removed access")
	}
}

func TestGroupAccessRolesAreNamespaceWorkloadOnly(t *testing.T) {
	b := mustLoad(t)
	roles := map[string]*rbacv1.ClusterRole{}
	for _, obj := range b.Objects() {
		if obj.GetKind() != "ClusterRole" || !slices.Contains([]string{WorkloadAdminRole, WorkloadViewerRole}, obj.GetName()) {
			continue
		}
		role := &rbacv1.ClusterRole{}
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(obj.Object, role); err != nil {
			t.Fatal(err)
		}
		roles[role.Name] = role
		if role.AggregationRule != nil {
			t.Fatal("workload roles must not aggregate unrelated permissions")
		}
		for _, rule := range role.Rules {
			for _, verb := range rule.Verbs {
				if slices.Contains([]string{"*", "bind", "escalate", "impersonate"}, verb) || role.Name == WorkloadViewerRole && !slices.Contains([]string{"get", "list", "watch"}, verb) {
					t.Fatal("unsafe workload verb", role.Name, verb)
				}
			}
			for _, group := range rule.APIGroups {
				if !slices.Contains([]string{"", "apps", "batch"}, group) {
					t.Fatal("security/cluster API exposed to workload role", role.Name, group)
				}
			}
			for _, resource := range rule.Resources {
				if !slices.Contains([]string{"deployments", "statefulsets", "daemonsets", "replicasets", "jobs", "cronjobs", "pods", "pods/log", "pods/exec", "pods/attach", "services", "configmaps", "persistentvolumeclaims", "events", "resourcequotas", "limitranges"}, resource) || role.Name == WorkloadViewerRole && slices.Contains([]string{"pods/exec", "pods/attach"}, resource) {
					t.Fatal("unsafe resource exposed to workload role", role.Name, resource)
				}
			}
		}
	}
	if len(roles) != 2 {
		t.Fatal("missing fixed workload roles")
	}
	for _, name := range []string{WorkloadAdminRole, WorkloadViewerRole} {
		if !bootstrapAllows(t, b, "naas-system", "provisioner", "", rbacv1.GroupName, "clusterroles", name, "bind") {
			t.Fatal("provisioner cannot bind fixed role", name)
		}
	}
	if bootstrapAllows(t, b, "naas-system", "provisioner", "", rbacv1.GroupName, "clusterroles", "cluster-admin", "bind") {
		t.Fatal("group access widened delegation to cluster-admin")
	}
}
