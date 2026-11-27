package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

var componentHeader = []string{"NAME", "ID", "MANAGED BY", "VERSION", "PHASE", "AGE", "MESSAGE"}

// noID is the ID column of a component that has none: a platform
// component (part of the cluster, not an object of its own) or one the
// tenant has not enabled. Not <none>: nothing is missing.
const noID = "-"

// componentRow is a component's row of component list.
func componentRow(c *cloudv1.Component) []string {
	st := c.GetStatus()
	id := c.GetId()
	if id == "" {
		id = noID
	}
	return []string{c.GetComponent(), id, managedBy(c.GetManagement()), orNone(c.GetVersion()),
		word(st.GetPhase().String(), "COMPONENT_PHASE_"), age(st.GetSince()), orNone(oneLine(st.GetMessage()))}
}

func mutatedComponent(w io.Writer, c *cloudv1.Component, verb string) error {
	if err := mutated(w, "component", c.GetComponent(), c.GetId(), verb); err != nil {
		return err
	}
	return table(w, componentHeader, [][]string{componentRow(c)})
}

// describeComponent is one component: its fields, why it is not ready,
// and its conditions.
func describeComponent(w io.Writer, c *cloudv1.Component) error {
	st := c.GetStatus()
	if err := fields(w, [][2]string{
		{"Name", c.GetComponent()}, {"ID", c.GetId()}, {"Tenant", c.GetTenantId()}, {"Cluster", c.GetClusterId()},
		{"Managed By", managedBy(c.GetManagement())}, {"Version", c.GetVersion()}, {"Release", st.GetRelease()},
		{"Phase", word(st.GetPhase().String(), "COMPONENT_PHASE_")}, {"Chart", st.GetChart()}, {"Helm State", st.GetHelmState()},
		{"Message", oneLine(st.GetMessage())}, {"Last Error", oneLine(st.GetLastError())}, {"Age", age(st.GetSince())},
	}); err != nil {
		return err
	}
	return section(w, "Conditions", conditionHeader, conditionRows(st.GetConditions()))
}

// iamRoleSummary is the catalog's IAM ROLE column: how the pods get the
// role and which service accounts may use it; <none> for no role.
func iamRoleSummary(r *cloudv1.ComponentIamRole) string {
	if r == nil {
		return none
	}
	return iamAccess(r.GetAccess()) + " (" + strings.Join(serviceAccounts(r), ", ") + ")"
}

func iamAccess(a cloudv1.ComponentIamAccess) string {
	if a == cloudv1.ComponentIamAccess_COMPONENT_IAM_ACCESS_POD_IDENTITY {
		return "pod-identity"
	}
	return "irsa"
}

func serviceAccounts(r *cloudv1.ComponentIamRole) []string {
	out := make([]string, 0, len(r.GetServiceAccounts()))
	for _, sa := range r.GetServiceAccounts() {
		out = append(out, sa.GetNamespace()+"/"+sa.GetName())
	}
	return out
}

// componentIAMRole is component get's IAM Role section: the role the
// platform creates in the tenant's account for the component, from the
// catalog entry of the component's cluster.
func componentIAMRole(ctx context.Context, c Clients, o Options, comp *cloudv1.Component) error {
	res, err := call(ctx, &cloudv1.ListComponentDefinitionsRequest{TenantId: o.Tenant, ClusterId: comp.GetClusterId()},
		c.Component.ListComponentDefinitions)
	if err != nil {
		_, werr := fmt.Fprintf(o.Out, "\nIAM Role:\n  unknown: the catalog cannot be read: %v\n", err)
		return werr
	}
	var role *cloudv1.ComponentIamRole
	for _, d := range res.(*cloudv1.ListComponentDefinitionsResponse).GetDefinitions() {
		if d.GetName() == comp.GetComponent() {
			role = d.GetCloudResources().GetIamRole()
		}
	}
	return describeIAMRole(o.Out, role)
}

// describeIAMRole writes the IAM Role section: access, service accounts,
// managed policies and the policy document (placeholders as the
// definition has them).
func describeIAMRole(w io.Writer, r *cloudv1.ComponentIamRole) error {
	if _, err := fmt.Fprintln(w, "\nIAM Role:"); err != nil {
		return err
	}
	indented := &prefixWriter{w: w, prefix: "  "}
	if r == nil {
		_, err := fmt.Fprintln(indented, "none: the platform creates no IAM role for this component")
		return err
	}
	if err := fields(indented, [][2]string{
		{"Access", iamAccess(r.GetAccess())}, {"Service Accounts", strings.Join(serviceAccounts(r), ", ")},
		{"Managed Policies", strings.Join(r.GetManagedPolicyArns(), ", ")},
	}); err != nil {
		return err
	}
	if r.GetPolicy() == "" {
		return nil
	}
	policy := r.GetPolicy()
	var pretty bytes.Buffer
	if json.Indent(&pretty, []byte(policy), "  ", "  ") == nil {
		policy = pretty.String()
	}
	_, err := fmt.Fprintf(indented, "Policy:\n  %s\n", policy)
	return err
}
