package aws_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strconv"
	"strings"
	"testing"

	"buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/descriptorpb"

	crossplaneaws "github.com/yindia/cluster-vending/pkg/crossplane/aws"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
)

// fieldMarkers are the kubebuilder markers ("+kubebuilder:...") on the
// doc comment of field of type typ in the Go file at path.
func fieldMarkers(t *testing.T, path, typ, field string) []string {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ParseComments)
	if err != nil {
		t.Fatal(err)
	}
	var markers []string
	ast.Inspect(f, func(n ast.Node) bool {
		spec, ok := n.(*ast.TypeSpec)
		if !ok || spec.Name.Name != typ {
			return true
		}
		for _, fl := range spec.Type.(*ast.StructType).Fields.List {
			if len(fl.Names) == 1 && fl.Names[0].Name == field && fl.Doc != nil {
				for _, c := range fl.Doc.List {
					if m, ok := strings.CutPrefix(c.Text, "// +"); ok {
						markers = append(markers, m)
					}
				}
			}
		}
		return false
	})
	return markers
}

// enumOf is the value list of the marker with that prefix
// ("kubebuilder:validation:Enum=a;b").
func enumOf(t *testing.T, markers []string, prefix string) []string {
	t.Helper()
	for _, m := range markers {
		if list, ok := strings.CutPrefix(m, prefix); ok {
			return strings.Split(list, ";")
		}
	}
	t.Fatalf("no %s marker in %v", prefix, markers)
	return nil
}

func intMarker(t *testing.T, markers []string, prefix string) int {
	t.Helper()
	for _, m := range markers {
		if v, ok := strings.CutPrefix(m, prefix); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				t.Fatal(err)
			}
			return n
		}
	}
	t.Fatalf("no %s marker in %v", prefix, markers)
	return 0
}

// protoRules are the buf.validate rules of a field of message m.
func protoRules(t *testing.T, m proto.Message, field string) *validate.FieldRules {
	t.Helper()
	fd := m.ProtoReflect().Descriptor().Fields().ByName(protoreflect.Name(field))
	if fd == nil {
		t.Fatalf("no field %s", field)
	}
	opts, ok := fd.Options().(*descriptorpb.FieldOptions)
	if !ok {
		t.Fatalf("%s: no options", field)
	}
	rules, ok := proto.GetExtension(opts, validate.E_Field).(*validate.FieldRules)
	if !ok || rules == nil {
		t.Fatalf("%s: no buf.validate rules", field)
	}
	return rules
}

// The API's lists of add-on names are hand-written (kubebuilder markers,
// the proto's buf.validate rules): they must name the add-ons of
// addons.yaml, in its order, and leave room for all of them.
func TestAddonCatalogMatchesTheAPI(t *testing.T) {
	known := crossplaneaws.KnownAddons()
	const api = "../../../api/v1alpha1/"

	lists := map[string][]string{
		"AWSAddon.name (cluster_types.go)": enumOf(t, fieldMarkers(t, api+"cluster_types.go", "AWSAddon", "Name"),
			"kubebuilder:validation:Enum="),
		"conflictsWithAddons (component_types.go)": enumOf(t,
			fieldMarkers(t, api+"component_types.go", "ComponentDefinitionSpec", "ConflictsWithAddons"),
			"kubebuilder:validation:items:Enum="),
		"AwsAddon.name (aws.proto)": protoRules(t, &cloudv1.AwsAddon{}, "name").GetString().GetIn(),
	}
	for where, names := range lists {
		if !slices.Equal(names, known) {
			t.Errorf("%s names %v; addons.yaml has %v", where, names, known)
		}
	}

	if n := intMarker(t, fieldMarkers(t, api+"cluster_types.go", "AWSClusterSpec", "Addons"), "kubebuilder:validation:MaxItems="); n < len(known) {
		t.Errorf("AWSClusterSpec.addons takes at most %d add-ons; addons.yaml has %d", n, len(known))
	}
	if n := protoRules(t, &cloudv1.AwsClusterSpec{}, "addons").GetRepeated().GetMaxItems(); n < uint64(len(known)) {
		t.Errorf("AwsClusterSpec.addons takes at most %d add-ons; addons.yaml has %d", n, len(known))
	}
}
