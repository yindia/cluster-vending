package cli

import (
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"google.golang.org/protobuf/proto"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The columns of the console's Sub Tenants table, and the cluster (a list
// can span the tenant's clusters).
var subTenantHeader = []string{"NAME", "ID", "CLUSTER", "NAMESPACES", "PROTECTION", "STATUS", "AGE"}

// printSubTenantTable writes the sub-tenant responses as tables; false for
// any other message.
func printSubTenantTable(w io.Writer, msg proto.Message, wide bool) (bool, error) {
	switch m := msg.(type) {
	case *cloudv1.ListSubTenantsResponse:
		return true, printSubTenants(w, m.GetSubtenants(), wide)
	case *cloudv1.GetSubTenantResponse:
		return true, describeSubTenant(w, m.GetSubtenant())
	case *cloudv1.CreateSubTenantResponse:
		return true, mutatedSubTenant(w, m.GetSubtenant(), "created")
	case *cloudv1.UpdateSubTenantResponse:
		return true, mutatedSubTenant(w, m.GetSubtenant(), "updated")
	case *cloudv1.DeleteSubTenantResponse:
		return true, mutatedSubTenant(w, m.GetSubtenant(), "deletion requested")
	}
	return false, nil
}

// subTenantProtected: deletion protection is on unless set to false.
func subTenantProtected(s *cloudv1.SubTenantSpec) bool {
	return s.DeletionProtection == nil || s.GetDeletionProtection()
}

func subTenantState(st *cloudv1.SubTenantStatus) string {
	return word(st.GetState().String(), "SUB_TENANT_STATE_")
}

// subTenantRow is a sub-tenant's row; wide adds its status message.
func subTenantRow(s *cloudv1.SubTenant, wide bool) []string {
	protection := "Off"
	if subTenantProtected(s.GetSpec()) {
		protection = "Protected"
	}
	r := []string{orNone(s.GetSpec().GetName()), orNone(s.GetId()), orNone(s.GetClusterId()),
		strconv.Itoa(len(s.GetSpec().GetNamespaceKeys())), protection, subTenantState(s.GetStatus()), age(s.GetCreateTime())}
	if wide {
		r = append(r, orNone(oneLine(s.GetStatus().GetMessage())))
	}
	return r
}

func printSubTenants(w io.Writer, subtenants []*cloudv1.SubTenant, wide bool) error {
	if len(subtenants) == 0 {
		_, err := fmt.Fprintln(w, "No sub-tenants found.")
		return err
	}
	header := subTenantHeader
	if wide {
		header = append(append([]string{}, header...), "MESSAGE")
	}
	rows := make([][]string, 0, len(subtenants))
	for _, s := range subtenants {
		rows = append(rows, subTenantRow(s, wide))
	}
	return table(w, header, rows)
}

func mutatedSubTenant(w io.Writer, s *cloudv1.SubTenant, verb string) error {
	if err := mutated(w, "subtenant", s.GetSpec().GetName(), s.GetId(), verb); err != nil {
		return err
	}
	return table(w, subTenantHeader, [][]string{subTenantRow(s, false)})
}

// describeSubTenant is one sub-tenant: its fields, quota, access, the
// namespaces it has, what blocks its deletion, and its conditions.
func describeSubTenant(w io.Writer, s *cloudv1.SubTenant) error {
	spec, st := s.GetSpec(), s.GetStatus()
	protection := "off"
	if subTenantProtected(spec) {
		protection = "on (set spec.deletionProtection: false to delete)"
	}
	kv := [][2]string{
		{"Name", spec.GetName()}, {"ID", s.GetId()}, {"Tenant", s.GetTenantId()}, {"Cluster", s.GetClusterId()},
		{"Display name", spec.GetDisplayName()}, {"Status", subTenantState(st)},
		{"Generation", fmt.Sprintf("%d (observed %d)", s.GetGeneration(), st.GetObservedGeneration())},
		{"Namespace keys", strings.Join(spec.GetNamespaceKeys(), ",")},
		{"Namespace budget", strconv.Itoa(int(spec.GetQuota().GetNamespaceCount()))},
		{"Policy profile", spec.GetPolicyProfile()}, {"Deletion protection", protection},
		{"Capsule tenant", st.GetRemoteCapsuleTenant()}, {"Message", oneLine(st.GetMessage())}, {"Age", age(s.GetCreateTime())},
	}
	if err := fields(w, kv); err != nil {
		return err
	}
	if err := section(w, "Quota", []string{"RESOURCE", "HARD"}, subTenantQuotaRows(spec.GetQuota())); err != nil {
		return err
	}
	if err := section(w, "Access", []string{"KIND", "WHO", "ROLE"}, subTenantAccessRows(spec.GetAccess())); err != nil {
		return err
	}
	if err := section(w, "Namespaces", []string{"KEY", "NAME", "READY"}, subTenantNamespaceRows(st.GetNamespaces())); err != nil {
		return err
	}
	blockers := make([][]string, 0, len(st.GetDeletionBlockers()))
	for _, b := range st.GetDeletionBlockers() {
		blockers = append(blockers, []string{oneLine(b)})
	}
	if err := section(w, "Deletion blockers", []string{"BLOCKER"}, blockers); err != nil {
		return err
	}
	return section(w, "Conditions", conditionHeader, conditionRows(st.GetConditions()))
}

func subTenantQuotaRows(q *cloudv1.SubTenantQuota) [][]string {
	keys := make([]string, 0, len(q.GetHard()))
	for k := range q.GetHard() {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	rows := make([][]string, 0, len(keys))
	for _, k := range keys {
		rows = append(rows, []string{k, q.GetHard()[k]})
	}
	return rows
}

func subTenantAccessRows(a *cloudv1.SubTenantAccess) [][]string {
	rows := make([][]string, 0, len(a.GetAdminGroups())+len(a.GetViewerGroups())+len(a.GetBindings()))
	for _, g := range a.GetAdminGroups() {
		rows = append(rows, []string{"group", g, "admin"})
	}
	for _, g := range a.GetViewerGroups() {
		rows = append(rows, []string{"group", g, "viewer"})
	}
	for _, b := range a.GetBindings() {
		rows = append(rows, []string{"identity", b.GetIdentityRef(), b.GetRoleProfile()})
	}
	return rows
}

// subTenantNamespaceRows are the sub-tenant's namespaces: READY is their Ready
// condition, <none> when not reported.
func subTenantNamespaceRows(namespaces []*cloudv1.SubTenantNamespaceStatus) [][]string {
	rows := make([][]string, 0, len(namespaces))
	for _, n := range namespaces {
		ready := none
		for _, c := range n.GetConditions() {
			if c.GetType() == "Ready" {
				ready = conditionWord(c.GetStatus())
			}
		}
		rows = append(rows, []string{n.GetKey(), orNone(n.GetName()), ready})
	}
	return rows
}
