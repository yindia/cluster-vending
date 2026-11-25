package aws

import (
	_ "embed"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

// addonsFile is the EKS add-ons the platform knows: which every cluster
// gets, where their controllers run and what they need in IAM. Its header
// describes each field.
//
//go:embed addons.yaml
var addonsFile []byte

// AddonCatalogFile is addons.yaml as shipped.
func AddonCatalogFile() []byte { return slices.Clone(addonsFile) }

// addonCatalog is addons.yaml, read when the package loads: a file that
// does not pass ParseAddonCatalog stops the binary (and every test) at
// start.
var addonCatalog = mustParseAddonCatalog(addonsFile)

// DefaultAddons are the EKS add-ons every cluster gets, in install order
// (default: true in addons.yaml).
var DefaultAddons = addonCatalog.Defaults()

// KnownAddons are the EKS add-ons the platform knows (addons.yaml), in
// its order: the names the API accepts.
func KnownAddons() []string { return addonCatalog.Names() }

// ImportableAddons are the known add-ons an imported cluster's spec can
// hold: those whose setup takes nothing from the spec that an import
// cannot read back (the Mountpoint driver's buckets).
func ImportableAddons() []string {
	return slices.DeleteFunc(addonCatalog.Names(), func(name string) bool {
		s, ok := addonCatalog.setups[name]
		return ok && s.hook != "" && !addonHooks[s.hook].importable
	})
}

// AddonEntry is one add-on of addons.yaml.
type AddonEntry struct {
	Name      string             `json:"name"`
	Default   bool               `json:"default,omitempty"`
	Placement *AddonPlacementDoc `json:"placement,omitempty"`
	Schema    *AddonSchemaDoc    `json:"schema,omitempty"`
	Setup     *AddonSetupDoc     `json:"setup,omitempty"`
}

// AddonPlacementDoc is where an add-on's configuration places its
// controller pods: the objects (dotted keys, "." the top level) that take
// nodeSelector and tolerations, and the add-on's own default tolerations.
type AddonPlacementDoc struct {
	Paths              []string         `json:"paths"`
	DefaultTolerations []map[string]any `json:"defaultTolerations,omitempty"`
}

// AddonSchemaDoc is where a placement's keys were confirmed.
type AddonSchemaDoc struct {
	Version string `json:"version"`
	Source  string `json:"source"`
}

// AddonSetupDoc is the IAM role (EKS Pod Identity) the platform creates
// for an add-on that calls AWS.
type AddonSetupDoc struct {
	Short           string   `json:"short"`
	Namespace       string   `json:"namespace"`
	ServiceAccount  string   `json:"serviceAccount"`
	ManagedPolicies []string `json:"managedPolicies,omitempty"`
	Hook            string   `json:"hook,omitempty"`
	ProtectIAM      bool     `json:"protectIAM,omitempty"`
}

// AddonCatalog is a read addons.yaml.
type AddonCatalog struct {
	entries    []AddonEntry
	placements map[string]addonPlacement
	setups     map[string]addonSetup
}

// Names are the catalog's add-ons, in its order.
func (c AddonCatalog) Names() []string {
	out := make([]string, 0, len(c.entries))
	for _, e := range c.entries {
		out = append(out, e.Name)
	}
	return out
}

// Defaults are the add-ons every cluster gets, in the catalog's order.
func (c AddonCatalog) Defaults() []string {
	var out []string
	for _, e := range c.entries {
		if e.Default {
			out = append(out, e.Name)
		}
	}
	return out
}

// addonHook is platform code an add-on's setup names (setup.hook): what a
// data entry cannot say.
type addonHook struct {
	// objects are the hook's objects of the add-on's setup, after its
	// managed policy attachments.
	objects func(b clusterBuilder, a AddonPlan, s addonSetup, role string) []client.Object
	// importable: an import can read back what the hook takes from the
	// spec.
	importable bool
}

// hookS3BucketPolicy writes a policy for the add-on's S3 buckets
// (AddonPlan.S3BucketNames) and attaches it.
const hookS3BucketPolicy = "s3-bucket-policy"

// addonHookNames are the hooks setup.hook may name; addonHooks holds their
// code (kept apart so parsing the catalog does not depend on the renderer).
var addonHookNames = []string{hookS3BucketPolicy}

var addonHooks = map[string]addonHook{
	hookS3BucketPolicy: {objects: clusterBuilder.s3BucketPolicyObjects},
}

func mustParseAddonCatalog(raw []byte) AddonCatalog {
	c, err := ParseAddonCatalog(raw)
	if err != nil {
		panic(err)
	}
	return c
}

var addonNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// maxShort keeps "<prefix>-<short>-role" within IAM's 64 characters.
const maxShort = 8

// ParseAddonCatalog reads an addons.yaml. It refuses unknown fields,
// duplicate names and short names, placements without paths or schema,
// empty path keys, setups without a policy and unknown hooks.
func ParseAddonCatalog(raw []byte) (AddonCatalog, error) {
	var doc struct {
		Addons []AddonEntry `json:"addons"`
	}
	if err := yaml.UnmarshalStrict(raw, &doc); err != nil {
		return AddonCatalog{}, fmt.Errorf("addons.yaml: %w", err)
	}
	c := AddonCatalog{entries: doc.Addons, placements: map[string]addonPlacement{}, setups: map[string]addonSetup{}}
	if len(doc.Addons) == 0 {
		return AddonCatalog{}, errors.New("addons.yaml: no add-ons")
	}
	seen, shorts := map[string]bool{}, map[string]string{}
	for _, e := range doc.Addons {
		if !addonNamePattern.MatchString(e.Name) {
			return AddonCatalog{}, fmt.Errorf("addons.yaml: name %q: want lower-case letters, digits, - and _", e.Name)
		}
		if seen[e.Name] {
			return AddonCatalog{}, fmt.Errorf("addons.yaml: %q twice", e.Name)
		}
		seen[e.Name] = true
		if err := c.addPlacement(e); err != nil {
			return AddonCatalog{}, fmt.Errorf("addons.yaml: %s: %w", e.Name, err)
		}
		if err := c.addSetup(e, shorts); err != nil {
			return AddonCatalog{}, fmt.Errorf("addons.yaml: %s: %w", e.Name, err)
		}
	}
	if len(c.Defaults()) == 0 {
		return AddonCatalog{}, errors.New("addons.yaml: no default add-on")
	}
	return c, nil
}

func (c AddonCatalog) addPlacement(e AddonEntry) error {
	if e.Placement == nil {
		return nil
	}
	if len(e.Placement.Paths) == 0 {
		return errors.New("placement has no paths")
	}
	if e.Schema == nil || e.Schema.Version == "" || e.Schema.Source == "" {
		return errors.New("placement needs schema.version and schema.source: where its keys were confirmed")
	}
	placement := addonPlacement{defaultTolerations: e.Placement.DefaultTolerations}
	for i, path := range e.Placement.Paths {
		if slices.Index(e.Placement.Paths, path) != i {
			return fmt.Errorf("path %q twice", path)
		}
		if path == "." {
			placement.paths = append(placement.paths, nil)
			continue
		}
		keys := strings.Split(path, ".")
		if slices.Contains(keys, "") {
			return fmt.Errorf("path %q: want dotted keys, or \".\" for the top level", path)
		}
		placement.paths = append(placement.paths, keys)
	}
	c.placements[e.Name] = placement
	return nil
}

func (c AddonCatalog) addSetup(e AddonEntry, shorts map[string]string) error {
	s := e.Setup
	if s == nil {
		return nil
	}
	switch {
	case s.Short == "" || len(s.Short) > maxShort:
		return fmt.Errorf("setup.short %q: want 1 to %d characters", s.Short, maxShort)
	case shorts[s.Short] != "":
		return fmt.Errorf("setup.short %q is %s's too", s.Short, shorts[s.Short])
	case s.Namespace == "" || s.ServiceAccount == "":
		return errors.New("setup needs namespace and serviceAccount")
	case s.Hook != "" && !slices.Contains(addonHookNames, s.Hook):
		return fmt.Errorf("setup: unknown hook %q (known: %s)", s.Hook, strings.Join(addonHookNames, ", "))
	case len(s.ManagedPolicies) == 0 && s.Hook == "":
		return errors.New("setup needs managedPolicies or a hook")
	}
	shorts[s.Short] = e.Name
	c.setups[e.Name] = addonSetup{short: s.Short, namespace: s.Namespace, serviceAccount: s.ServiceAccount,
		managedPolicies: s.ManagedPolicies, hook: s.Hook, protectIAM: s.ProtectIAM}
	return nil
}
