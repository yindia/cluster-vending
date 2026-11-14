package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"text/tabwriter"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/fieldmaskpb"
	"google.golang.org/protobuf/types/known/structpb"
	"sigs.k8s.io/yaml"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// The component commands (ComponentService). A component is enabled by its
// catalog name on a cluster, and addressed by its ID from then on.

// catalogDescription is a definition's description on one line, with why
// it is off in front when the platform switched it off.
func catalogDescription(d *cloudv1.ComponentDefinition) string {
	description := oneLine(d.GetDescription())
	if !d.GetDisabled() {
		return description
	}
	return strings.TrimSpace("[off: " + d.GetDisabledReason() + "] " + description)
}

// ComponentCatalog lists the catalog: every component, with who manages
// it. With --cluster, the version that cluster's release gives each.
func ComponentCatalog(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	res, err := call(ctx, &cloudv1.ListComponentDefinitionsRequest{TenantId: o.Tenant, ClusterId: o.Cluster},
		c.Component.ListComponentDefinitions)
	if err != nil || o.Output != OutputTable {
		return res, err
	}
	tw := tabwriter.NewWriter(o.Out, 0, 4, 2, ' ', 0)
	header := "NAME\tMANAGED BY\tCONFIGURABLE\tREQUIRES\tDESCRIPTION"
	if o.Cluster != "" {
		header = "NAME\tMANAGED BY\tCONFIGURABLE\tVERSION\tREQUIRES\tDESCRIPTION"
	}
	if _, err := fmt.Fprintln(tw, header); err != nil {
		return nil, err
	}
	for _, d := range res.(*cloudv1.ListComponentDefinitionsResponse).GetDefinitions() {
		cols := []string{d.GetName(), managedBy(d.GetManagement()), yesNo(d.GetConfigurable())}
		if o.Cluster != "" {
			version := "-"
			for _, v := range d.GetVersions() {
				version = v
			}
			cols = append(cols, version)
		}
		cols = append(cols, dash(strings.Join(d.GetRequires(), ", ")), dash(catalogDescription(d)))
		if _, err := fmt.Fprintln(tw, strings.Join(cols, "\t")); err != nil {
			return nil, err
		}
	}
	return nil, tw.Flush()
}

func managedBy(m cloudv1.ComponentManagement) string {
	if m == cloudv1.ComponentManagement_COMPONENT_MANAGEMENT_PLATFORM {
		return "platform"
	}
	return "tenant"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// ComponentTemplate prints a component's values template as the platform
// wrote it (YAML, comments kept), to edit and send with enable -f or
// update -f. A component that takes no values says so.
func ComponentTemplate(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	res, err := call(ctx, &cloudv1.GetComponentValuesRequest{Name: o.Component}, c.Component.GetComponentValues)
	if err != nil {
		return nil, err
	}
	_, err = io.WriteString(o.Out, res.(*cloudv1.GetComponentValuesResponse).GetUserValues())
	return nil, err
}

// loadValues reads a values file: a YAML or JSON object. No path, or an
// empty file: no values.
func loadValues(path string) (*structpb.Struct, error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("-f: %w", err)
	}
	return parseValues(raw, "-f "+path)
}

// parseValues reads values from YAML or JSON; what names the source in
// errors.
func parseValues(raw []byte, what string) (*structpb.Struct, error) {
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if doc == nil {
		return nil, nil // empty, or comments only
	}
	object, ok := doc.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: values must be an object (key: value), got %T", what, doc)
	}
	out, err := structpb.NewStruct(object)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	return out, nil
}

// valuesYAML are a component's values as YAML; "{}" for none.
func valuesYAML(values *structpb.Struct) ([]byte, error) {
	if len(values.GetFields()) == 0 {
		return []byte("{}\n"), nil
	}
	return yaml.Marshal(values.AsMap())
}

// printValid says what a --validate-only request would store.
func printValid(w io.Writer, component *cloudv1.Component) error {
	n := len(component.GetValues().GetFields())
	if n == 0 {
		_, err := fmt.Fprintln(w, "valid. No value differs from the defaults: nothing would be stored.")
		return err
	}
	out, err := valuesYAML(component.GetValues())
	if err != nil {
		return err
	}
	noun := "values differ"
	if n == 1 {
		noun = "value differs"
	}
	if _, err := fmt.Fprintf(w, "valid. %d %s from the defaults and would be stored:\n", n, noun); err != nil {
		return err
	}
	_, err = w.Write(indent(out))
	return err
}

// indent is text with two spaces before every line.
func indent(text []byte) []byte {
	var out bytes.Buffer
	for _, line := range bytes.SplitAfter(text, []byte("\n")) {
		if len(line) > 0 {
			out.WriteString("  ")
			out.Write(line)
		}
	}
	return out.Bytes()
}

// ComponentEnable enables the component --name (the argument) on
// --cluster, with the values of -f when given. --validate-only checks and
// says what would be stored; --wait follows the install.
func ComponentEnable(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	if o.Cluster == "" {
		return nil, errors.New("--cluster is required: the ID of the cluster to enable the component on")
	}
	values, err := loadValues(o.Input)
	if err != nil {
		return nil, err
	}
	req := &cloudv1.EnableComponentRequest{TenantId: o.Tenant, ValidateOnly: o.ValidateOnly,
		Component: &cloudv1.Component{ClusterId: o.Cluster, Component: o.Component, Values: values}}
	if !o.ValidateOnly {
		// The same input again returns the component the first call stored.
		if req.RequestId, err = requestID(o.Tenant, req); err != nil {
			return nil, err
		}
	}
	res, err := call(ctx, req, c.Component.EnableComponent)
	if err != nil {
		return nil, err
	}
	component := res.(*cloudv1.EnableComponentResponse).GetComponent()
	if o.ValidateOnly {
		if o.Output == OutputJSON {
			return component, nil
		}
		return nil, printValid(o.Out, component)
	}
	return component, waitComponent(ctx, c, o, component, false)
}

// ComponentUpdate replaces the values of the component o.ID with the
// values of -f.
func ComponentUpdate(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	if o.Input == "" {
		return nil, errors.New("-f is required: the values file (component get <id> --values prints the current values)")
	}
	values, err := loadValues(o.Input)
	if err != nil {
		return nil, err
	}
	component, err := updateComponent(ctx, c, o, values, o.ValidateOnly)
	if err != nil {
		return nil, err
	}
	if o.ValidateOnly {
		if o.Output == OutputJSON {
			return component, nil
		}
		return nil, printValid(o.Out, component)
	}
	return component, waitComponent(ctx, c, o, component, false)
}

// updateComponent sends the component's new values (replaced as a whole).
func updateComponent(ctx context.Context, c Clients, o Options, values *structpb.Struct, validateOnly bool) (*cloudv1.Component, error) {
	res, err := call(ctx, &cloudv1.UpdateComponentRequest{TenantId: o.Tenant, ValidateOnly: validateOnly,
		Component:  &cloudv1.Component{Id: o.ID, Etag: o.ETag, Values: values},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"values"}}}, c.Component.UpdateComponent)
	if err != nil {
		return nil, err
	}
	return res.(*cloudv1.UpdateComponentResponse).GetComponent(), nil
}

// ComponentDisable removes the component o.ID from its cluster.
func ComponentDisable(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	res, err := call(ctx, &cloudv1.DisableComponentRequest{TenantId: o.Tenant, ComponentId: o.ID, Etag: o.ETag},
		c.Component.DisableComponent)
	if err != nil {
		return nil, err
	}
	component := res.(*cloudv1.DisableComponentResponse).GetComponent()
	return component, waitComponent(ctx, c, o, component, true)
}

// ComponentGet shows the component o.ID; with --values only the values the
// tenant stored, as YAML to edit and send back with update.
func ComponentGet(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	res, err := call(ctx, &cloudv1.GetComponentRequest{TenantId: o.Tenant, ComponentId: o.ID}, c.Component.GetComponent)
	if err != nil {
		return nil, err
	}
	component := res.(*cloudv1.GetComponentResponse).GetComponent()
	if !o.Values {
		return component, nil
	}
	out, err := valuesYAML(component.GetValues())
	if err != nil {
		return nil, err
	}
	_, err = o.Out.Write(out)
	return nil, err
}

// ComponentList lists what is on --cluster: the components the platform
// installed, the tenant's, and the ones the tenant could enable. Without
// --cluster: the tenant's components on every cluster, a page at a time.
func ComponentList(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	res, err := call(ctx, &cloudv1.ListComponentsRequest{TenantId: o.Tenant, ClusterId: o.Cluster,
		PageSize: o.PageSize, PageToken: o.PageToken}, c.Component.ListComponents)
	if err != nil || o.Output != OutputTable {
		return res, err
	}
	list := res.(*cloudv1.ListComponentsResponse)
	tw := tabwriter.NewWriter(o.Out, 0, 4, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ID\tNAME\tMANAGED BY\tVERSION\tPHASE\tSINCE\tMESSAGE"); err != nil {
		return nil, err
	}
	for _, comp := range list.GetComponents() {
		st := comp.GetStatus()
		since, phase := "-", "-"
		if st.GetSince() != nil {
			since = st.GetSince().AsTime().UTC().Format(time.RFC3339)
		}
		if st.GetPhase() != cloudv1.ComponentPhase_COMPONENT_PHASE_UNSPECIFIED {
			phase = componentPhaseWord(st.GetPhase())
		}
		if _, err := fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", dash(comp.GetId()), comp.GetComponent(),
			managedBy(comp.GetManagement()), dash(comp.GetVersion()), phase, since, dash(oneLine(st.GetMessage()))); err != nil {
			return nil, err
		}
	}
	if err := tw.Flush(); err != nil {
		return nil, err
	}
	if o.Details {
		if err := printDetails(o.Out, list.GetComponents()); err != nil {
			return nil, err
		}
	}
	if token := list.GetNextPageToken(); token != "" {
		_, err = fmt.Fprintf(o.Out, "\nMore: --page-token %s\n", token)
	}
	return nil, err
}

// waitComponent is --wait for a component: until the cluster reconcile
// reports it READY (or FAILED, an error), or gone for a disable.
func waitComponent(ctx context.Context, c Clients, o Options, component *cloudv1.Component, gone bool) error {
	if !o.Wait {
		return nil
	}
	what := "component " + component.GetComponent()
	return waitUntil(ctx, o, what, gone, func(ctx context.Context) (observation, error) {
		res, err := call(ctx, &cloudv1.GetComponentRequest{TenantId: o.Tenant, ComponentId: component.GetId()}, c.Component.GetComponent)
		if err != nil {
			return observation{}, err
		}
		st := res.(*cloudv1.GetComponentResponse).GetComponent().GetStatus()
		return observation{state: componentPhaseWord(st.GetPhase()), message: st.GetMessage(),
			done:   !gone && st.GetPhase() == cloudv1.ComponentPhase_COMPONENT_PHASE_READY,
			failed: st.GetPhase() == cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED}, nil
	})
}

// ComponentEdit opens the values of the component o.ID in the caller's
// editor and sends them back when they changed: the values the tenant
// stored, or, when it stored none, the component's values file with the
// platform's comments. A refused change is shown and, on a terminal, the
// editor opens again on what was typed.
func ComponentEdit(ctx context.Context, c Clients, o Options) (proto.Message, error) {
	if err := o.requireTenant(); err != nil {
		return nil, err
	}
	res, err := call(ctx, &cloudv1.GetComponentRequest{TenantId: o.Tenant, ComponentId: o.ID}, c.Component.GetComponent)
	if err != nil {
		return nil, err
	}
	component := res.(*cloudv1.GetComponentResponse).GetComponent()
	start, err := editStart(ctx, c, component)
	if err != nil {
		return nil, err
	}
	file, err := os.CreateTemp("", "cluster-vending-"+component.GetComponent()+"-*.yaml") // 0600
	if err != nil {
		return nil, err
	}
	path := file.Name()
	defer func() { _ = os.Remove(path) }()
	if _, err := file.Write(start); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := file.Close(); err != nil {
		return nil, err
	}
	edit := o.Edit
	if edit == nil {
		edit = runEditor
	}
	o.ETag = component.GetEtag()
	last := start
	for {
		if err := edit(ctx, path); err != nil {
			return nil, fmt.Errorf("editor: %w", err)
		}
		edited, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if bytes.Equal(edited, last) {
			_, err := fmt.Fprintln(o.Out, "no changes")
			return nil, err
		}
		last = edited
		updated, err := sendEdit(ctx, c, o, edited)
		if err == nil {
			return updated, nil
		}
		if !o.Terminal || !again(o, err) {
			return nil, err
		}
	}
}

// editStart is what the editor opens with.
func editStart(ctx context.Context, c Clients, component *cloudv1.Component) ([]byte, error) {
	if len(component.GetValues().GetFields()) > 0 {
		return valuesYAML(component.GetValues())
	}
	res, err := call(ctx, &cloudv1.GetComponentValuesRequest{Name: component.GetComponent()}, c.Component.GetComponentValues)
	if connect.CodeOf(err) == connect.CodeFailedPrecondition {
		return nil, fmt.Errorf("component %s takes no values: there is nothing to edit", component.GetComponent())
	}
	if err != nil {
		return nil, err
	}
	return []byte(res.(*cloudv1.GetComponentValuesResponse).GetUserValues()), nil
}

// sendEdit checks the edited values and, when the server accepts them,
// stores them.
func sendEdit(ctx context.Context, c Clients, o Options, edited []byte) (*cloudv1.Component, error) {
	values, err := parseValues(edited, "the edited values")
	if err != nil {
		return nil, err
	}
	if _, err := updateComponent(ctx, c, o, values, true); err != nil {
		return nil, err
	}
	return updateComponent(ctx, c, o, values, false)
}

// again shows why the edit was refused and asks whether to open the
// editor again; anything but "n" is yes.
func again(o Options, refused error) bool {
	if o.Progress != nil {
		_, _ = fmt.Fprintf(o.Progress, "%v\nEdit again? [Y/n] ", refused)
	}
	in := o.In
	if in == nil {
		in = os.Stdin
	}
	answer, _ := bufio.NewReader(in).ReadString('\n')
	return !strings.HasPrefix(strings.ToLower(strings.TrimSpace(answer)), "n")
}

// runEditor opens path in the caller's editor, attached to their
// terminal: $VISUAL, else $EDITOR, else vi. The command comes from the
// caller's own environment only, and is run without a shell: its words
// are the program and its arguments (e.g. "code --wait").
func runEditor(ctx context.Context, path string) error {
	editor := os.Getenv("VISUAL")
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	words := strings.Fields(editor)
	if len(words) == 0 {
		words = []string{"vi"}
	}
	cmd := exec.CommandContext(ctx, words[0], append(words[1:], path)...) //nolint:gosec // the caller's own $VISUAL/$EDITOR, no shell
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

// printDetails writes, under the table, what it has no room for: for each
// component with something to explain, its chart, Helm state, failures and
// recoveries, and the provider's last error.
func printDetails(w io.Writer, components []*cloudv1.Component) error {
	for _, comp := range components {
		st := comp.GetStatus()
		if st.GetChart() == "" && st.GetLastError() == "" {
			continue
		}
		if _, err := fmt.Fprintf(w, "\n%s: chart %s, Helm state %s, %d failed attempts, recovered %d times\n", comp.GetComponent(),
			dash(st.GetChart()), dash(st.GetHelmState()), st.GetFailures(), st.GetRecoveries()); err != nil {
			return err
		}
		if st.GetLastError() != "" {
			if _, err := fmt.Fprintf(w, "  last error: %s\n", st.GetLastError()); err != nil {
				return err
			}
		}
	}
	return nil
}
