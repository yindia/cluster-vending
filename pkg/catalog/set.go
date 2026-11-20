package catalog

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	apiyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	cloudv1alpha1 "github.com/yindia/cluster-vending/api/v1alpha1"
)

// The platform catalog as manifests (docs/proposals/PROP-01): the
// ComponentDefinitions, Releases and ReleaseChannels of a platform. A Set
// is read from rendered manifests (Decode, `controller --check -f`) or from
// the management cluster (Export, `controller --export`), and judged by
// Check.

// SchemaAnnotation is carried by every catalog object: the version of the
// catalog's schema, bumped when a field's meaning changes or a required one
// is added. The export writes it.
const SchemaAnnotation = "cloud.cluster-vending.io/catalog-schema"

// SchemaVersion is the schema this build reads.
const SchemaVersion = "1"

// Set is a platform's catalog objects.
type Set struct {
	Definitions []cloudv1alpha1.ComponentDefinition
	Releases    []cloudv1alpha1.Release
	Channels    []cloudv1alpha1.ReleaseChannel
}

// Problem is one thing wrong with a Set: the object (Kind/name, or "set"
// for the whole) with the path of the field, and what is wrong.
type Problem struct {
	Object  string
	Message string
}

func (p Problem) String() string { return p.Object + ": " + p.Message }

// problemf is a Problem of the object kind/name.
func problemf(kind, name, format string, args ...any) Problem {
	return Problem{Object: kind + "/" + name, Message: fmt.Sprintf(format, args...)}
}

// Decode reads a multi-document YAML stream (the output of `kustomize
// build`). Documents of the three kinds are strictly decoded (a field the
// kind does not have is a problem); documents of any other kind, and empty
// ones, are left out, so a rendered overlay with more in it can be piped
// in. Every problem is returned, the objects that decoded stay in the set.
func Decode(r io.Reader) (Set, []Problem) {
	var set Set
	var problems []Problem
	reader := apiyaml.NewYAMLReader(bufio.NewReader(r))
	for i := 1; ; i++ {
		doc, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return set, append(problems, Problem{Object: "input", Message: err.Error()})
		}
		var head struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
			Metadata   struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(doc, &head); err != nil {
			problems = append(problems, Problem{Object: fmt.Sprintf("document %d", i), Message: err.Error()})
			continue
		}
		if head.APIVersion != cloudv1alpha1.GroupVersion.String() {
			continue
		}
		where := fmt.Sprintf("%s/%s", head.Kind, head.Metadata.Name)
		var decodeErr error
		switch head.Kind {
		case "ComponentDefinition":
			d := cloudv1alpha1.ComponentDefinition{}
			if decodeErr = yaml.UnmarshalStrict(doc, &d); decodeErr == nil {
				set.Definitions = append(set.Definitions, d)
			}
		case "Release":
			rel := cloudv1alpha1.Release{}
			if decodeErr = yaml.UnmarshalStrict(doc, &rel); decodeErr == nil {
				set.Releases = append(set.Releases, rel)
			}
		case "ReleaseChannel":
			ch := cloudv1alpha1.ReleaseChannel{}
			if decodeErr = yaml.UnmarshalStrict(doc, &ch); decodeErr == nil {
				set.Channels = append(set.Channels, ch)
			}
		}
		if decodeErr != nil {
			problems = append(problems, Problem{Object: where, Message: "cannot be read: " + decodeErr.Error()})
		}
	}
	return set, problems
}

// Sort puts the set in its stable order: definitions by name, releases by
// serial (a release without one by creation time, below), channels alpha,
// beta, stable (others by name).
func (s Set) Sort() Set {
	out := Set{Definitions: slices.Clone(s.Definitions), Releases: slices.Clone(s.Releases), Channels: slices.Clone(s.Channels)}
	slices.SortFunc(out.Definitions, func(a, b cloudv1alpha1.ComponentDefinition) int { return strings.Compare(a.Name, b.Name) })
	cloudv1alpha1.SortReleasesNewestFirst(out.Releases)
	slices.Reverse(out.Releases) // oldest first, as a catalog is read
	order := []string{cloudv1alpha1.ChannelAlpha, cloudv1alpha1.ChannelBeta, cloudv1alpha1.ChannelStable}
	rank := func(name string) int {
		if i := slices.Index(order, name); i >= 0 {
			return i
		}
		return len(order)
	}
	slices.SortFunc(out.Channels, func(a, b cloudv1alpha1.ReleaseChannel) int {
		if c := rank(a.Name) - rank(b.Name); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}
