package cli

import (
	"fmt"
	"io"
	"strings"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

var accountHeader = []string{"NAME", "ID", "ACCOUNT", "STATE", "AGE"}

func accountRow(a *cloudv1.CloudAccount) []string {
	state := word(a.GetStatus().GetState().String(), "CLOUD_ACCOUNT_STATE_")
	if a.GetStatus().GetPause() != nil {
		state += " (paused)"
	}
	return []string{orNone(a.GetAccountName()), orNone(a.GetId()), orNone(a.GetAws().GetAccountId()), state, age(a.GetCreateTime())}
}

func printAccounts(w io.Writer, accounts []*cloudv1.CloudAccount) error {
	if len(accounts) == 0 {
		_, err := fmt.Fprintln(w, "No cloud accounts found.")
		return err
	}
	rows := make([][]string, 0, len(accounts))
	for _, a := range accounts {
		rows = append(rows, accountRow(a))
	}
	return table(w, accountHeader, rows)
}

func mutatedAccount(w io.Writer, a *cloudv1.CloudAccount, verb string) error {
	if err := mutated(w, "cloudaccount", a.GetAccountName(), a.GetId(), verb); err != nil {
		return err
	}
	return table(w, accountHeader, [][]string{accountRow(a)})
}

// describeAccount is one cloud account: its fields, its resources and
// conditions.
func describeAccount(w io.Writer, a *cloudv1.CloudAccount) error {
	st := a.GetStatus()
	kv := [][2]string{
		{"Name", a.GetAccountName()}, {"ID", a.GetId()}, {"Tenant", a.GetTenantId()},
		{"State", word(st.GetState().String(), "CLOUD_ACCOUNT_STATE_")}, {"AWS Account", a.GetAws().GetAccountId()},
		{"Role", a.GetAws().GetRole()}, {"Namespace", st.GetNamespace()}, {"Provider Config", st.GetProviderConfigName()},
		{"Identity", st.GetIdentity()},
	}
	if p := st.GetPause(); p != nil {
		kv = append(kv, [2]string{"Paused", pauseLine(p)})
	}
	kv = append(kv, [2]string{"Age", age(a.GetCreateTime())})
	if err := fields(w, kv); err != nil {
		return err
	}
	if err := section(w, "Resources", resourceHeader, resourceRows(st.GetManagedResources())); err != nil {
		return err
	}
	return section(w, "Conditions", conditionHeader, conditionRows(st.GetConditions()))
}

// printWhoAmI is who the API sees the caller as, and what they may do.
func printWhoAmI(w io.Writer, m *cloudv1.WhoAmIResponse) error {
	user := m.GetUsername()
	if m.GetAnonymous() {
		user = "anonymous"
	}
	if err := fields(w, [][2]string{{"User", user}, {"Subject", m.GetSubject()}, {"Issuer", m.GetIssuer()},
		{"Roles", strings.Join(m.GetRoles(), ",")}}); err != nil {
		return err
	}
	rows := make([][]string, 0, len(m.GetGrants()))
	for _, g := range m.GetGrants() {
		rows = append(rows, []string{word(g.GetRole().String(), "ACCESS_ROLE_"), orNone(strings.Join(g.GetTenants(), ","))})
	}
	return section(w, "Grants", []string{"ROLE", "TENANTS"}, rows)
}
