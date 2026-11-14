package cli

import (
	"io"

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
