package naas

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	apiextensionsinternal "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	structuralschema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	"sigs.k8s.io/yaml"
)

// Local artifact checks never download dependencies or contact a cluster. CI
// may provide CAPSULE_CHART_DIR; without a local chart these checks skip while
// the self-contained Go rendering and content tests still run.
func localChartDir() string {
	if dir := os.Getenv("CAPSULE_CHART_DIR"); dir != "" {
		return dir
	}
	return "/tmp/capsule"
}

func pinnedChart(t *testing.T) string {
	t.Helper()
	dir := localChartDir()
	raw, err := os.ReadFile(filepath.Join(dir, "Chart.yaml"))
	if os.IsNotExist(err) && os.Getenv("CAPSULE_CHART_DIR") == "" {
		t.Skip("Capsule 0.15.2 chart not available locally; no download attempted")
	}
	if err != nil {
		t.Fatal(err)
	}
	var chart struct {
		Version    string `json:"version"`
		AppVersion string `json:"appVersion"`
	}
	if err := yaml.Unmarshal(raw, &chart); err != nil {
		t.Fatal(err)
	}
	if chart.Version != "0.15.2" || chart.AppVersion != "0.15.2" {
		t.Fatalf("local chart is %s/%s, want chart/app 0.15.2", chart.Version, chart.AppVersion)
	}
	return dir
}

func TestLocalPinnedHelmChartRoundTrip(t *testing.T) {
	chart := pinnedChart(t)
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("Helm binary not available; no installation attempted")
	}
	b := mustLoad(t)
	dir := t.TempDir()
	values, err := yaml.Marshal(map[string]any{"extraManifests": b.HelmExtraManifests()})
	if err != nil {
		t.Fatal(err)
	}
	valuesFile := filepath.Join(dir, "values.yaml")
	if err := os.WriteFile(valuesFile, values, 0600); err != nil {
		t.Fatal(err)
	}
	// Local chart and dependencies only. No --dependency-update, --validate,
	// --dry-run=server, remote chart URL, or cluster-facing Helm action.
	cmd := exec.CommandContext(t.Context(), helm, "template", "naas-offline", chart,
		"--namespace", "capsule-system", "--show-only", "templates/extra-manifests.yaml", "--values", valuesFile)
	cmd.Env = append(os.Environ(), "HELM_CACHE_HOME="+filepath.Join(dir, "cache"),
		"HELM_CONFIG_HOME="+filepath.Join(dir, "config"), "HELM_DATA_HOME="+filepath.Join(dir, "data"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("offline Helm render: %v\n%s", err, out)
	}
	rendered, err := load(out)
	if err != nil {
		t.Fatalf("Helm YAML: %v\n%s", err, out)
	}
	if !reflect.DeepEqual(rendered.Objects(), b.Objects()) || rendered.Revision() != b.Revision() {
		t.Fatal("actual pinned Helm tpl changed the bootstrap or Capsule template content")
	}
	checkGenerators(t, rendered)
}

func TestLocalPinnedCapsuleSchema(t *testing.T) {
	chart := pinnedChart(t)
	raw, err := os.ReadFile(filepath.Join(chart, "crds", "capsule.clastix.io_globaltenantresources.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	if crd.Spec.Group != "capsule.clastix.io" || crd.Spec.Names.Kind != "GlobalTenantResource" {
		t.Fatal("unexpected local CRD")
	}
	var schema *apiextensionsv1.JSONSchemaProps
	for _, version := range crd.Spec.Versions {
		if version.Name == "v1beta2" && version.Served && version.Storage {
			schema = version.Schema.OpenAPIV3Schema
		}
	}
	if schema == nil {
		t.Fatal("local CRD does not serve/store v1beta2")
	}
	internal := &apiextensionsinternal.JSONSchemaProps{}
	if err := apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(schema, internal, nil); err != nil {
		t.Fatal(err)
	}
	validator, _, err := validation.NewSchemaValidator(internal)
	if err != nil {
		t.Fatal(err)
	}
	structural, err := structuralschema.NewStructural(internal)
	if err != nil {
		t.Fatal(err)
	}
	celValidator := cel.NewValidator(structural, true, celconfig.PerCallLimit)
	for _, object := range mustLoad(t).Objects() {
		if object.GetKind() != "GlobalTenantResource" {
			continue
		}
		t.Run(object.GetName(), func(t *testing.T) {
			unknown := pruning.PruneWithOptions(object.Object, structural, true, structuralschema.UnknownFieldPathOptions{TrackUnknownFieldPaths: true})
			if len(unknown) != 0 {
				t.Fatalf("unknown fields in pinned schema: %v", unknown)
			}
			if errs := validation.ValidateCustomResource(nil, object.Object, validator); len(errs) != 0 {
				t.Fatal(errs.ToAggregate())
			}
			if errs, _ := celValidator.Validate(t.Context(), nil, structural, object.Object, nil, celconfig.RuntimeCELCostBudget); len(errs) != 0 {
				t.Fatal(errs.ToAggregate())
			}
		})
	}
}
