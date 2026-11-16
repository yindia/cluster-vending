package cli

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	"k8s.io/apimachinery/pkg/util/duration"
	"sigs.k8s.io/yaml"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// Output formats (--output). table is the default: a kubectl-style table
// for a list, a describe-like block for one object; wide adds columns to
// lists; yaml and json print the whole object.
const (
	OutputTable = "table"
	OutputWide  = "wide"
	OutputYAML  = "yaml"
	OutputJSON  = "json"
)

// OutputHelp is the --output flag's help text.
const OutputHelp = "output format: table (default), wide, yaml or json"

// none is kubectl's spelling of an empty value.
const none = "<none>"

// clock is the time ages are counted from; tests replace it.
var clock = time.Now

// isTable reports whether format is one of the table formats.
func isTable(format string) bool { return format == OutputTable || format == OutputWide }

// Print writes msg to w in format: a table (table, wide) or the whole
// object (yaml, json). A message without a table form is printed as YAML.
func Print(w io.Writer, msg proto.Message, format string) error {
	switch format {
	case OutputTable, OutputWide:
		ok, err := printTable(w, msg, format == OutputWide)
		if ok || err != nil {
			return err
		}
		return printData(w, msg, OutputYAML)
	case OutputYAML, OutputJSON, "":
		return printData(w, msg, format)
	default:
		return fmt.Errorf("--output must be table, wide, yaml or json, got %q", format)
	}
}

func printData(w io.Writer, msg proto.Message, format string) error {
	js, err := protojson.MarshalOptions{Multiline: true, Indent: "  "}.Marshal(msg)
	if err != nil {
		return err
	}
	if format == OutputJSON {
		_, err = fmt.Fprintln(w, string(js))
		return err
	}
	y, err := yaml.JSONToYAML(js)
	if err != nil {
		return err
	}
	_, err = w.Write(y)
	return err
}

// printTable writes msg as a table when it has a table form; false when
// it has none.
func printTable(w io.Writer, msg proto.Message, wide bool) (bool, error) {
	switch m := msg.(type) {
	case *cloudv1.ListClustersResponse:
		return true, printClusters(w, m.GetClusters(), wide)
	case *cloudv1.GetClusterResponse:
		return true, describeCluster(w, m.GetCluster())
	case *cloudv1.CreateClusterResponse:
		return true, mutatedCluster(w, m.GetCluster(), "created")
	case *cloudv1.UpdateClusterResponse:
		if m.GetPlan() != nil && !m.GetUpdateRecorded() {
			return true, printPlan(w, m.GetPlan())
		}
		return true, mutatedCluster(w, m.GetCluster(), "updated")
	case *cloudv1.DeleteClusterResponse:
		return true, mutatedCluster(w, m.GetCluster(), "deletion requested")
	case *cloudv1.RetryClusterResponse:
		return true, printRetry(w, m.GetItems())
	case *cloudv1.ImportClusterResponse:
		return true, mutatedCluster(w, m.GetCluster(), "imported")
	case *cloudv1.TakeOverClusterResponse:
		return true, mutatedCluster(w, m.GetCluster(), "taken over")
	case *cloudv1.ListNetworksResponse:
		return true, printNetworks(w, m.GetNetworks(), wide)
	case *cloudv1.GetNetworkResponse:
		return true, describeNetwork(w, m.GetNetwork())
	case *cloudv1.CreateNetworkResponse:
		return true, mutatedNetwork(w, m.GetNetwork(), "created")
	case *cloudv1.UpdateNetworkResponse:
		if m.GetPlan() != nil && !m.GetUpdateRecorded() {
			return true, printPlan(w, m.GetPlan())
		}
		return true, mutatedNetwork(w, m.GetNetwork(), "updated")
	case *cloudv1.DeleteNetworkResponse:
		return true, mutatedNetwork(w, m.GetNetwork(), "deletion requested")
	case *cloudv1.ImportNetworkResponse:
		return true, mutatedNetwork(w, m.GetNetwork(), "imported")
	case *cloudv1.TakeOverNetworkResponse:
		return true, mutatedNetwork(w, m.GetNetwork(), "taken over")
	case *cloudv1.ListCloudAccountsResponse:
		return true, printAccounts(w, m.GetAccounts())
	case *cloudv1.GetCloudAccountResponse:
		return true, describeAccount(w, m.GetAccount())
	case *cloudv1.BootstrapTenantResponse:
		return true, mutatedAccount(w, m.GetAccount(), "bootstrapped")
	case *cloudv1.VerifyCloudAccountResponse:
		return true, mutatedAccount(w, m.GetAccount(), "access check requested")
	case *cloudv1.DeleteCloudAccountResponse:
		return true, mutatedAccount(w, m.GetAccount(), "deleted")
	case *cloudv1.GetComponentResponse:
		return true, describeComponent(w, m.GetComponent())
	case *cloudv1.EnableComponentResponse:
		return true, mutatedComponent(w, m.GetComponent(), "enabled")
	case *cloudv1.UpdateComponentResponse:
		return true, mutatedComponent(w, m.GetComponent(), "updated")
	case *cloudv1.DisableComponentResponse:
		return true, mutatedComponent(w, m.GetComponent(), "disable requested")
	case *cloudv1.WhoAmIResponse:
		return true, printWhoAmI(w, m)
	case *cloudv1.ListReleasesResponse:
		return true, printReleases(w, m.GetReleases(), wide)
	case *cloudv1.GetReleaseResponse:
		return true, describeRelease(w, m.GetRelease(), wide)
	case *cloudv1.ListReleaseChannelsResponse:
		return true, printChannels(w, m.GetChannels(), wide)
	case *cloudv1.GetReleaseChannelResponse:
		return true, describeChannel(w, m, wide)
	}
	return false, nil
}

// newTable is a tab writer as kubectl's: columns three spaces apart.
func newTable(w io.Writer) *tabwriter.Writer { return tabwriter.NewWriter(w, 6, 4, 3, ' ', 0) }

// row writes one tab-separated row.
func row(w io.Writer, cols ...string) error {
	_, err := fmt.Fprintln(w, strings.Join(cols, "\t"))
	return err
}

// table writes a header and rows, flushed.
func table(w io.Writer, header []string, rows [][]string) error {
	tw := newTable(w)
	if err := row(tw, header...); err != nil {
		return err
	}
	for _, r := range rows {
		if err := row(tw, r...); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// section writes a describe section: its title, then rows indented two
// spaces under a header, or <none>.
func section(w io.Writer, title string, header []string, rows [][]string) error {
	if _, err := fmt.Fprintf(w, "\n%s:\n", title); err != nil {
		return err
	}
	if len(rows) == 0 {
		_, err := fmt.Fprintf(w, "  %s\n", none)
		return err
	}
	indented := &prefixWriter{w: w, prefix: "  "}
	return table(indented, header, rows)
}

// fields writes describe's "Key:   value" lines, aligned.
func fields(w io.Writer, kv [][2]string) error {
	tw := newTable(w)
	for _, f := range kv {
		if err := row(tw, f[0]+":", orNone(f[1])); err != nil {
			return err
		}
	}
	return tw.Flush()
}

// prefixWriter puts prefix before every line written through it.
type prefixWriter struct {
	w      io.Writer
	prefix string
	mid    bool
}

func (p *prefixWriter) Write(b []byte) (int, error) {
	var out []byte
	for _, c := range b {
		if !p.mid {
			out = append(out, p.prefix...)
			p.mid = true
		}
		out = append(out, c)
		if c == '\n' {
			p.mid = false
		}
	}
	if _, err := p.w.Write(out); err != nil {
		return 0, err
	}
	return len(b), nil
}

// orNone is s, or <none> when it is empty.
func orNone(s string) string {
	if s == "" {
		return none
	}
	return s
}

// age is how long ago t was, as kubectl prints an AGE; <none> without t.
func age(t *timestamppb.Timestamp) string {
	if t == nil || !t.IsValid() || t.AsTime().IsZero() {
		return none
	}
	return duration.HumanDuration(clock().Sub(t.AsTime()))
}

// word is an enum value as a word: CLUSTER_STATE_IMPORT_BLOCKED with
// prefix CLUSTER_STATE_ is ImportBlocked; UNSPECIFIED is <none>.
func word(name, prefix string) string {
	name = strings.TrimPrefix(name, prefix)
	if name == "" || name == "UNSPECIFIED" {
		return none
	}
	var b strings.Builder
	for _, part := range strings.Split(strings.ToLower(name), "_") {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]) + part[1:])
	}
	return b.String()
}

func conditionWord(s cloudv1.ConditionStatus) string {
	return word(s.String(), "CONDITION_STATUS_")
}

// resourceRows are a status's managed resources for a Resources section.
func resourceRows(resources []*cloudv1.ManagedResource) [][]string {
	rows := make([][]string, 0, len(resources))
	for _, r := range resources {
		state := word(r.GetState().String(), "MANAGED_RESOURCE_STATE_")
		if r.GetState() == cloudv1.ManagedResourceState_MANAGED_RESOURCE_STATE_MISSING && r.GetMessage() == "deleted" {
			state = "Deleted"
		}
		if r.GetPaused() {
			state += " (paused)"
		}
		rows = append(rows, []string{r.GetKind(), r.GetName(), orNone(r.GetExternalName()), state,
			conditionWord(r.GetSynced()), conditionWord(r.GetReady()), orNone(oneLine(r.GetMessage()))})
	}
	return rows
}

var resourceHeader = []string{"KIND", "NAME", "EXTERNAL-NAME", "STATE", "SYNCED", "READY", "MESSAGE"}

// conditionRows are conditions for a Conditions section.
func conditionRows(conditions []*cloudv1.Condition) [][]string {
	rows := make([][]string, 0, len(conditions))
	for _, c := range conditions {
		rows = append(rows, []string{c.GetType(), conditionWord(c.GetStatus()), orNone(c.GetReason()), orNone(oneLine(c.GetMessage()))})
	}
	return rows
}

var conditionHeader = []string{"TYPE", "STATUS", "REASON", "MESSAGE"}

// eventRows are recent events for an Events section, as recorded (newest
// first).
func eventRows(events []*cloudv1.Event) [][]string {
	rows := make([][]string, 0, len(events))
	for _, e := range events {
		rows = append(rows, []string{age(e.GetTime()), word(e.GetType().String(), "EVENT_TYPE_"), orNone(e.GetReason()), orNone(oneLine(e.GetMessage()))})
	}
	return rows
}

var eventHeader = []string{"LAST SEEN", "TYPE", "REASON", "MESSAGE"}

func frozenWrite(w io.Writer, frozen *cloudv1.Freeze) error {
	if frozen == nil || frozen.GetLetThrough() {
		return nil
	}
	_, err := fmt.Fprintln(w, "The platform is frozen; this request is stored and applies after admin unfreeze.")
	return err
}

func freezeLine(p *cloudv1.Freeze) string {
	if p == nil {
		return ""
	}
	state := "everything"
	if p.GetLetThrough() {
		state = "let through; the platform stays frozen"
	}
	if p.GetReason() != "" {
		state += ": " + p.GetReason()
	}
	detail := "by " + orNone(p.GetBy())
	if p.GetScope() != "" && p.GetScope() != "platform" {
		detail = p.GetScope() + ", " + detail
	}
	if p.GetSince() != nil {
		detail += " since " + p.GetSince().AsTime().UTC().Format(time.RFC3339)
	}
	if p.GetUntil() != nil {
		detail += ", until " + p.GetUntil().AsTime().UTC().Format(time.RFC3339)
	}
	return state + " (" + detail + ")"
}

// mutated writes kubectl's "kind/name (id) verb" line.
func mutated(w io.Writer, kind, name, id, verb string) error {
	_, err := fmt.Fprintf(w, "%s/%s (%s) %s\n", kind, orNone(name), orNone(id), verb)
	return err
}

// printPlan writes an update plan: its changes, then its warnings.
func printPlan(w io.Writer, p *cloudv1.Plan) error {
	state := "not applicable"
	if p.GetApplicable() {
		state = "applicable"
	}
	if _, err := fmt.Fprintf(w, "Plan %s (%s)\n", orNone(p.GetPlanId()), state); err != nil {
		return err
	}
	rows := make([][]string, 0, len(p.GetChanges()))
	for _, c := range p.GetChanges() {
		rows = append(rows, []string{word(c.GetAction().String(), "PLANNED_ACTION_"), c.GetKind(), orNone(c.GetName()),
			orNone(c.GetExternalName()), orNone(oneLine(c.GetDetail()))})
	}
	if len(rows) == 0 {
		if _, err := fmt.Fprintln(w, "No changes."); err != nil {
			return err
		}
	} else if err := table(w, []string{"ACTION", "KIND", "NAME", "EXTERNAL-NAME", "DETAIL"}, rows); err != nil {
		return err
	}
	if len(p.GetWarnings()) == 0 {
		return nil
	}
	if _, err := fmt.Fprintln(w, "\nWarnings:"); err != nil {
		return err
	}
	for _, warning := range p.GetWarnings() {
		if _, err := fmt.Fprintf(w, "  %s\n", warning); err != nil {
			return err
		}
	}
	return nil
}
