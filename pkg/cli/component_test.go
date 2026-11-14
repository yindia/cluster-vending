package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1/cloudv1connect"
)

// componentSince is since when the platform's component is Ready.
var componentSince = time.Date(2026, 11, 13, 4, 0, 0, 0, time.UTC)

const (
	componentCluster = "01a1d0a6-69d5-7e15-965e-fc6a53fa3d33"
	componentID      = "01a1d0a7-0000-7000-8000-000000000001"
	policiesFile     = "# baseline or restricted.\nlevel: baseline\n"
)

// componentAPI is a ComponentService that holds one component and records
// what it was asked.
type componentAPI struct {
	cloudv1connect.ComponentServiceClient
	component *cloudv1.Component
	// refuse makes the next real (not validate-only) update fail once.
	refuseValidation error
	enables          []*cloudv1.EnableComponentRequest
	updates          []*cloudv1.UpdateComponentRequest
	disabled         []string
	listed           []*cloudv1.ListComponentsRequest
	// details is listed after the other components when set.
	details *cloudv1.Component
}

func (a *componentAPI) ListComponentDefinitions(_ context.Context, req *cloudv1.ListComponentDefinitionsRequest) (*cloudv1.ListComponentDefinitionsResponse, error) {
	policies := &cloudv1.ComponentDefinition{Name: "policies", Description: "Pod security\npolicies.", Requires: []string{"kyverno"},
		Configurable: true, Management: cloudv1.ComponentManagement_COMPONENT_MANAGEMENT_TENANT}
	karpenter := &cloudv1.ComponentDefinition{Name: "karpenter", Description: "Node autoscaling.",
		Management: cloudv1.ComponentManagement_COMPONENT_MANAGEMENT_PLATFORM}
	if req.GetClusterId() != "" {
		policies.Versions, karpenter.Versions = map[string]string{"1.37": "3.10.0"}, map[string]string{"1.37": "1.15.1"}
	}
	return &cloudv1.ListComponentDefinitionsResponse{Definitions: []*cloudv1.ComponentDefinition{karpenter, policies}}, nil
}

func (a *componentAPI) GetComponentValues(_ context.Context, req *cloudv1.GetComponentValuesRequest) (*cloudv1.GetComponentValuesResponse, error) {
	if req.GetName() != "policies" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, "component "+req.GetName()+" takes no values")
	}
	return &cloudv1.GetComponentValuesResponse{UserValues: policiesFile}, nil
}

func (a *componentAPI) EnableComponent(_ context.Context, req *cloudv1.EnableComponentRequest) (*cloudv1.EnableComponentResponse, error) {
	a.enables = append(a.enables, req)
	out := proto.CloneOf(req.GetComponent())
	out.Status = &cloudv1.ComponentStatus{Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING,
		Message: "stored; not installed yet: this build installs platform components only"}
	if !req.GetValidateOnly() {
		out.Id = componentID
	}
	return &cloudv1.EnableComponentResponse{Component: out}, nil
}

func (a *componentAPI) UpdateComponent(_ context.Context, req *cloudv1.UpdateComponentRequest) (*cloudv1.UpdateComponentResponse, error) {
	a.updates = append(a.updates, req)
	if err := a.refuseValidation; err != nil && req.GetValidateOnly() {
		a.refuseValidation = nil
		return nil, err
	}
	out := proto.CloneOf(a.component)
	out.Values = req.GetComponent().GetValues()
	if !req.GetValidateOnly() {
		a.component = out
	}
	return &cloudv1.UpdateComponentResponse{Component: out}, nil
}

func (a *componentAPI) DisableComponent(_ context.Context, req *cloudv1.DisableComponentRequest) (*cloudv1.DisableComponentResponse, error) {
	a.disabled = append(a.disabled, req.GetComponentId())
	out := proto.CloneOf(a.component)
	a.component = nil
	return &cloudv1.DisableComponentResponse{Component: out}, nil
}

func (a *componentAPI) GetComponent(_ context.Context, req *cloudv1.GetComponentRequest) (*cloudv1.GetComponentResponse, error) {
	if a.component == nil || a.component.GetId() != req.GetComponentId() {
		return nil, connect.NewError(connect.CodeNotFound, "no component "+req.GetComponentId())
	}
	return &cloudv1.GetComponentResponse{Component: a.component}, nil
}

func (a *componentAPI) ListComponents(_ context.Context, req *cloudv1.ListComponentsRequest) (*cloudv1.ListComponentsResponse, error) {
	a.listed = append(a.listed, req)
	components := []*cloudv1.Component{
		{Component: "karpenter", Version: "1.15.1", Management: cloudv1.ComponentManagement_COMPONENT_MANAGEMENT_PLATFORM,
			Status: &cloudv1.ComponentStatus{Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_READY, Since: timestamppb.New(componentSince)}},
		{Id: componentID, Component: "policies", Version: "3.10.0", Management: cloudv1.ComponentManagement_COMPONENT_MANAGEMENT_TENANT,
			Status: &cloudv1.ComponentStatus{Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING, Message: "stored;\nnot installed yet"}},
		{Component: "gpu-operator", Management: cloudv1.ComponentManagement_COMPONENT_MANAGEMENT_TENANT,
			Status: &cloudv1.ComponentStatus{Message: "not enabled"}},
	}
	if a.details != nil {
		components = append(components, a.details)
	}
	return &cloudv1.ListComponentsResponse{NextPageToken: "next", Components: components}, nil
}

func stored(t *testing.T, values map[string]any) *cloudv1.Component {
	t.Helper()
	c := &cloudv1.Component{TenantId: "acme", Id: componentID, ClusterId: componentCluster, Component: "policies", Etag: "e1",
		Status: &cloudv1.ComponentStatus{Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_PENDING}}
	if values != nil {
		s, err := structpb.NewStruct(values)
		if err != nil {
			t.Fatal(err)
		}
		c.Values = s
	}
	return c
}

func valuesFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "values.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestComponentCatalogTable(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want string
	}{
		{"the catalog", Options{Tenant: "acme"},
			"NAME       MANAGED BY  CONFIGURABLE  REQUIRES  DESCRIPTION\n" +
				"karpenter  platform    no            -         Node autoscaling.\n" +
				"policies   tenant      yes           kyverno   Pod security policies.\n"},
		{"with a cluster's versions", Options{Tenant: "acme", Cluster: componentCluster},
			"NAME       MANAGED BY  CONFIGURABLE  VERSION  REQUIRES  DESCRIPTION\n" +
				"karpenter  platform    no            1.15.1   -         Node autoscaling.\n" +
				"policies   tenant      yes           3.10.0   kyverno   Pod security policies.\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer

			err := Run(t.Context(), ComponentCatalog, Clients{Component: &componentAPI{}}, tt.opts, &out, OutputTable)

			if err != nil || out.String() != tt.want {
				t.Fatalf("got %v\n%s\nwant\n%s", err, out.String(), tt.want)
			}
		})
	}
}

func TestComponentValuesPrintsTheFileAsWritten(t *testing.T) {
	var out bytes.Buffer

	err := Run(t.Context(), ComponentValues, Clients{Component: &componentAPI{}}, Options{Component: "policies"}, &out, OutputYAML)

	if err != nil || out.String() != policiesFile {
		t.Fatalf("got %q, %v", out.String(), err)
	}
}

func TestComponentEnable(t *testing.T) {
	file := valuesFile(t, "# my edits\nlevel: restricted\n")
	tests := []struct {
		name    string
		opts    Options
		format  string
		wantOut []string
		wantErr string
		// check looks at the request that was sent.
		check func(*testing.T, *cloudv1.EnableComponentRequest)
	}{
		{name: "with a values file", opts: Options{Tenant: "acme", Cluster: componentCluster, Component: "policies", Input: file},
			wantOut: []string{"id: " + componentID, "component: policies"},
			check: func(t *testing.T, req *cloudv1.EnableComponentRequest) {
				if req.GetRequestId() == "" || req.GetComponent().GetValues().AsMap()["level"] != "restricted" || req.GetValidateOnly() {
					t.Errorf("request = %v", req)
				}
			}},
		{name: "without values", opts: Options{Tenant: "acme", Cluster: componentCluster, Component: "policies"},
			check: func(t *testing.T, req *cloudv1.EnableComponentRequest) {
				if req.GetComponent().GetValues() != nil {
					t.Errorf("values = %v, want none", req.GetComponent().GetValues())
				}
			}},
		{name: "validate only says what would be stored",
			opts:    Options{Tenant: "acme", Cluster: componentCluster, Component: "policies", Input: file, ValidateOnly: true},
			wantOut: []string{"valid. 1 value differs from the defaults and would be stored:\n  level: restricted\n"},
			check: func(t *testing.T, req *cloudv1.EnableComponentRequest) {
				if !req.GetValidateOnly() || req.GetRequestId() != "" {
					t.Errorf("request = %v", req)
				}
			}},
		{name: "validate only with nothing to store",
			opts:    Options{Tenant: "acme", Cluster: componentCluster, Component: "policies", ValidateOnly: true},
			wantOut: []string{"valid. No value differs from the defaults: nothing would be stored.\n"}},
		{name: "no cluster", opts: Options{Tenant: "acme", Component: "policies"}, wantErr: "--cluster is required"},
		{name: "no tenant", opts: Options{Cluster: componentCluster, Component: "policies"}, wantErr: "--tenant"},
		{name: "values that are not an object", wantErr: "values must be an object",
			opts: Options{Tenant: "acme", Cluster: componentCluster, Component: "policies", Input: valuesFile(t, "- a\n- b\n")}},
		{name: "a missing values file", wantErr: "-f:",
			opts: Options{Tenant: "acme", Cluster: componentCluster, Component: "policies", Input: "/does/not/exist.yaml"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &componentAPI{}
			var out bytes.Buffer

			err := Run(t.Context(), ComponentEnable, Clients{Component: api}, tt.opts, &out, OutputYAML)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) || len(api.enables) != 0 {
					t.Fatalf("Run = %v after %d requests, want an error naming %q", err, len(api.enables), tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range tt.wantOut {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output lacks %q:\n%s", want, out.String())
				}
			}
			if tt.check != nil {
				tt.check(t, api.enables[0])
			}
		})
	}
}

// --wait follows the component until the cluster reconcile reports it
// READY, and fails when it reports FAILED.
func TestComponentEnableWaits(t *testing.T) {
	tests := []struct {
		name    string
		phase   cloudv1.ComponentPhase
		wantErr string
	}{
		{"ready", cloudv1.ComponentPhase_COMPONENT_PHASE_READY, ""},
		{"failed", cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, "is Failed: chart policies: boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &componentAPI{component: &cloudv1.Component{Id: componentID, Component: "policies",
				Status: &cloudv1.ComponentStatus{Phase: tt.phase, Message: "chart policies: boom"}}}
			var out, progress bytes.Buffer
			opts := Options{Tenant: "acme", Cluster: componentCluster, Component: "policies", Wait: true, Progress: &progress}

			err := Run(t.Context(), ComponentEnable, Clients{Component: api}, opts, &out, OutputYAML)

			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("Run = %v, want %q; progress %q", err, tt.wantErr, progress.String())
			}
		})
	}
}

func TestComponentUpdateAndDisable(t *testing.T) {
	api := &componentAPI{component: stored(t, map[string]any{"level": "restricted"})}
	clients := Clients{Component: api}
	var out bytes.Buffer

	// -f is required: an update without it would clear the values.
	err := Run(t.Context(), ComponentUpdate, clients, Options{Tenant: "acme", ID: componentID}, &out, OutputYAML)
	if err == nil || !strings.Contains(err.Error(), "-f is required") {
		t.Fatalf("update without -f = %v", err)
	}

	err = Run(t.Context(), ComponentUpdate, clients,
		Options{Tenant: "acme", ID: componentID, ETag: "e1", Input: valuesFile(t, "level: baseline\n")}, &out, OutputYAML)
	if err != nil || len(api.updates) != 1 {
		t.Fatalf("update = %v, %d requests", err, len(api.updates))
	}
	req := api.updates[0]
	if req.GetComponent().GetId() != componentID || req.GetComponent().GetEtag() != "e1" ||
		req.GetUpdateMask().GetPaths()[0] != "values" || req.GetComponent().GetValues().AsMap()["level"] != "baseline" {
		t.Errorf("update request = %v", req)
	}

	var progress bytes.Buffer
	err = Run(t.Context(), ComponentDisable, clients, Options{Tenant: "acme", ID: componentID, Wait: true, Progress: &progress}, &out, OutputYAML)
	if err != nil || len(api.disabled) != 1 || !strings.Contains(progress.String(), "deleted") {
		t.Fatalf("disable = %v, %v, progress %q", err, api.disabled, progress.String())
	}
}

func TestComponentGet(t *testing.T) {
	tests := []struct {
		name   string
		values map[string]any
		opts   Options
		want   string
	}{
		{"the component", map[string]any{"level": "restricted"}, Options{Tenant: "acme", ID: componentID}, "id: " + componentID},
		{"only its values", map[string]any{"level": "restricted"}, Options{Tenant: "acme", ID: componentID, Values: true}, "level: restricted\n"},
		{"no stored values", nil, Options{Tenant: "acme", ID: componentID, Values: true}, "{}\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out bytes.Buffer

			err := Run(t.Context(), ComponentGet, Clients{Component: &componentAPI{component: stored(t, tt.values)}}, tt.opts, &out, OutputYAML)

			if err != nil || !strings.Contains(out.String(), tt.want) || (tt.opts.Values && out.String() != tt.want) {
				t.Fatalf("got %v:\n%s\nwant %q", err, out.String(), tt.want)
			}
		})
	}
}

func TestComponentListTable(t *testing.T) {
	api := &componentAPI{}
	var out bytes.Buffer

	err := Run(t.Context(), ComponentList, Clients{Component: api}, Options{Tenant: "acme", Cluster: componentCluster}, &out, OutputTable)

	want := "ID                                    NAME          MANAGED BY  VERSION  PHASE    SINCE                 MESSAGE\n" +
		"-                                     karpenter     platform    1.15.1   Ready    2026-11-13T04:00:00Z  -\n" +
		componentID + "  policies      tenant      3.10.0   Pending  -                     stored; not installed yet\n" +
		"-                                     gpu-operator  tenant      -        -        -                     not enabled\n" +
		"\nMore: --page-token next\n"
	if err != nil || out.String() != want {
		t.Fatalf("got %v\n%s\nwant\n%s", err, out.String(), want)
	}
	if api.listed[0].GetClusterId() != componentCluster {
		t.Errorf("request = %v", api.listed[0])
	}
}

// --details adds, under the table, why each component is not ready.
func TestComponentListDetails(t *testing.T) {
	api := &componentAPI{}
	api.details = &cloudv1.Component{Component: "kyverno", Version: "3.10.0", Management: cloudv1.ComponentManagement_COMPONENT_MANAGEMENT_PLATFORM,
		Status: &cloudv1.ComponentStatus{Phase: cloudv1.ComponentPhase_COMPONENT_PHASE_FAILED, Message: "chart kyverno failed",
			Chart: "kyverno", HelmState: "uninstalling", LastError: "has no deployed releases", Failures: 3, Recoveries: 1}}
	var out bytes.Buffer

	err := Run(t.Context(), ComponentList, Clients{Component: api}, Options{Tenant: "acme", Cluster: componentCluster, Details: true}, &out, OutputTable)

	want := "\nkyverno: chart kyverno, Helm state uninstalling, 3 failed attempts, recovered 1 times\n  last error: has no deployed releases\n"
	if err != nil || !strings.Contains(out.String(), want) {
		t.Fatalf("got %v\n%s\nwant it to contain\n%s", err, out.String(), want)
	}
	if strings.Contains(out.String(), "karpenter: chart") {
		t.Errorf("a component with nothing to explain has details:\n%s", out.String())
	}
}

// editor is an editor that writes content to the file it is given, once
// for each content.
func editor(contents ...string) (func(context.Context, string) error, *[]string) {
	var opened []string
	return func(_ context.Context, path string) error {
		before, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		opened = append(opened, string(before))
		if len(contents) == 0 {
			return nil // closed without a change
		}
		next := contents[0]
		contents = contents[1:]
		return os.WriteFile(path, []byte(next), 0o600)
	}, &opened
}

func TestComponentEdit(t *testing.T) {
	refusal := connect.NewError(connect.CodeInvalidArgument, "only the platform sets failurePolicy")
	tests := []struct {
		name     string
		values   map[string]any // stored before the edit
		typed    []string       // what the editor writes, one per opening
		refuse   error
		terminal bool
		answer   string
		// wantOpened is what the editor showed at each opening.
		wantOpened []string
		wantStored string // the level stored at the end
		wantOut    string
		wantErr    string
	}{
		{name: "stored values are edited and sent", values: map[string]any{"level": "restricted"}, typed: []string{"level: baseline\n"},
			wantOpened: []string{"level: restricted\n"}, wantStored: "baseline", wantOut: "id: " + componentID},
		{name: "without stored values the commented file is opened", typed: []string{"level: restricted\n"},
			wantOpened: []string{policiesFile}, wantStored: "restricted"},
		{name: "closed without a change", values: map[string]any{"level": "restricted"},
			wantOpened: []string{"level: restricted\n"}, wantStored: "restricted", wantOut: "no changes\n"},
		{name: "a refusal off a terminal ends the edit", values: map[string]any{"level": "restricted"}, typed: []string{"failurePolicy: Ignore\n"},
			refuse: refusal, wantOpened: []string{"level: restricted\n"}, wantStored: "restricted", wantErr: "only the platform sets failurePolicy"},
		{name: "a refusal on a terminal opens the editor again", values: map[string]any{"level": "restricted"},
			typed: []string{"failurePolicy: Ignore\n", "level: baseline\n"}, refuse: refusal, terminal: true, answer: "\n",
			wantOpened: []string{"level: restricted\n", "failurePolicy: Ignore\n"}, wantStored: "baseline"},
		{name: "answering no ends the edit", values: map[string]any{"level": "restricted"}, typed: []string{"failurePolicy: Ignore\n"},
			refuse: refusal, terminal: true, answer: "n\n", wantOpened: []string{"level: restricted\n"}, wantStored: "restricted",
			wantErr: "only the platform sets failurePolicy"},
		{name: "values that are not an object", values: map[string]any{"level": "restricted"}, typed: []string{"- a\n"},
			wantOpened: []string{"level: restricted\n"}, wantStored: "restricted", wantErr: "values must be an object"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := &componentAPI{component: stored(t, tt.values), refuseValidation: tt.refuse}
			edit, opened := editor(tt.typed...)
			var out, progress bytes.Buffer
			opts := Options{Tenant: "acme", ID: componentID, Edit: edit, Terminal: tt.terminal,
				In: strings.NewReader(tt.answer), Progress: &progress}

			err := Run(t.Context(), ComponentEdit, Clients{Component: api}, opts, &out, OutputYAML)

			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Run = %v, want an error naming %q", err, tt.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if strings.Join(*opened, "|") != strings.Join(tt.wantOpened, "|") {
				t.Errorf("the editor showed %q, want %q", *opened, tt.wantOpened)
			}
			if got, _ := api.component.GetValues().AsMap()["level"].(string); got != tt.wantStored && (tt.wantStored != "restricted" || tt.values != nil) {
				t.Errorf("stored level = %q, want %q", got, tt.wantStored)
			}
			if !strings.Contains(out.String(), tt.wantOut) {
				t.Errorf("output %q lacks %q", out.String(), tt.wantOut)
			}
			for _, req := range api.updates {
				if req.GetComponent().GetEtag() != "e1" {
					t.Errorf("update without the etag the edit started from: %v", req)
				}
			}
		})
	}
}

// The temp file is private to the caller and gone afterwards.
func TestComponentEditLeavesNoFile(t *testing.T) {
	api := &componentAPI{component: stored(t, map[string]any{"level": "restricted"})}
	var path string
	edit := func(_ context.Context, p string) error {
		path = p
		info, err := os.Stat(p)
		if err != nil {
			return err
		}
		if info.Mode().Perm() != 0o600 {
			return errors.New("the temp file is readable by others: " + info.Mode().String())
		}
		return nil
	}

	err := Run(t.Context(), ComponentEdit, Clients{Component: api}, Options{Tenant: "acme", ID: componentID, Edit: edit}, &bytes.Buffer{}, OutputYAML)

	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s is still there: %v", path, err)
	}
}

// A component without a values file has nothing to edit.
func TestComponentEditOfAComponentWithoutValues(t *testing.T) {
	c := stored(t, nil)
	c.Component = "scanner"
	edit, opened := editor("a: 1\n")

	err := Run(t.Context(), ComponentEdit, Clients{Component: &componentAPI{component: c}},
		Options{Tenant: "acme", ID: componentID, Edit: edit}, &bytes.Buffer{}, OutputYAML)

	if err == nil || !strings.Contains(err.Error(), "nothing to edit") || len(*opened) != 0 {
		t.Fatalf("Run = %v, the editor opened %d times", err, len(*opened))
	}
}

// A component that is off says so, and why, in front of its description.
func TestCatalogDescriptionOfADisabledComponent(t *testing.T) {
	tests := []struct {
		name string
		def  *cloudv1.ComponentDefinition
		want string
	}{
		{"on", &cloudv1.ComponentDefinition{Description: "Node autoscaling."}, "Node autoscaling."},
		{"disabled", &cloudv1.ComponentDefinition{Description: "Node autoscaling.", Disabled: true, DisabledReason: "disabled"},
			"[off: disabled] Node autoscaling."},
		{"off with what it needs", &cloudv1.ComponentDefinition{Disabled: true, DisabledReason: "it requires kyverno, which is off"},
			"[off: it requires kyverno, which is off]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := catalogDescription(tt.def); got != tt.want {
				t.Errorf("catalogDescription = %q, want %q", got, tt.want)
			}
		})
	}
}
