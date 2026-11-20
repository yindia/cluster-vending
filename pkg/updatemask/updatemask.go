// Package updatemask holds the update mask paths the API accepts and works
// out the mask of an update from a manifest: the fields the manifest
// states that differ from the object now (Diff), the way kubectl apply
// does. The servers name these paths in their errors; a server test keeps
// each list equal to the fields the server can copy.
package updatemask

import (
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Cluster are the paths UpdateCluster accepts.
var Cluster = []string{
	"display_name",
	"labels",
	"spec.access.access_entries",
	"spec.access.admin_principals",
	"spec.access.oidc",
	"spec.aws.addons",
	"spec.aws.control_plane_log_group",
	"spec.aws.default_node_pool.ami_type",
	"spec.aws.default_node_pool.launch_template",
	"spec.aws.enabled_cluster_log_types",
	"spec.aws.irsa",
	"spec.aws.upgrade_policy.support_type",
	"spec.control_plane.endpoint_access.endpoint_private_access",
	"spec.control_plane.endpoint_access.endpoint_public_access",
	"spec.control_plane.endpoint_access.public_access_cidrs",
	"spec.control_plane.kubernetes_version",
	"spec.control_plane.secrets_encryption",
	"spec.default_node_pool.capacity_type",
	"spec.default_node_pool.disk_size_gb",
	"spec.default_node_pool.instance_types",
	"spec.maintenance_window",
	"spec.naas",
	"spec.node_groups",
	"spec.release_channel",
	"spec.upgrade_mode",
}

// Network are the paths UpdateNetwork accepts.
var Network = []string{"display_name", "labels", "spec.flow_logs", "spec.tier"}

// managedDefaultGroup is why the default node group's taints, labels and
// scaling cannot be changed.
const managedDefaultGroup = "the default node group is managed by the platform"

// fixedWhy says why a field that is set at creation cannot change.
var fixedWhy = map[string]string{
	"name":              "it names the object's resources; create another",
	"spec.region":       "it is set when the object is created",
	"spec.network":      "a cluster stays in the network it was created in",
	"spec.bootstrap_id": "a network stays in its account",
	"spec.layout":       "a network's layout is set when it is created",

	"spec.default_node_pool.taints":         managedDefaultGroup,
	"spec.default_node_pool.labels":         managedDefaultGroup,
	"spec.default_node_pool.scaling_config": managedDefaultGroup,
}

// skipped are the fields of the object itself that are not the client's
// to change: identity and output only. A manifest may state them.
var skipped = map[string]bool{
	"tenant_id": true, "id": true, "status": true, "generation": true,
	"etag": true, "create_time": true, "update_time": true,
}

// Change is one field an update changes: its mask path, its value now
// (WasSet false: unset) and the value it gets.
type Change struct {
	Path     string
	Field    protoreflect.FieldDescriptor
	From, To protoreflect.Value
	WasSet   bool
}

// Paths are the mask paths of changes.
func Paths(changes []Change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		out = append(out, c.Path)
	}
	return out
}

// FixedError names the fields a manifest changes that no update may.
type FixedError struct{ Paths []string }

func (e *FixedError) Error() string {
	parts := make([]string, 0, len(e.Paths))
	for _, p := range e.Paths {
		parts = append(parts, fmt.Sprintf("%s cannot be changed (%s)", p, why(p)))
	}
	return strings.Join(parts, "; ")
}

func why(path string) string {
	for p := path; p != ""; p = parent(p) {
		if w, ok := fixedWhy[p]; ok {
			return w
		}
	}
	return "no update changes it"
}

// Diff is what an update of current to desired changes, as mask paths of
// accepted, in path order. Only what desired states counts: a field it
// leaves unset is not mentioned and stays as it is (to clear one, name it
// in an explicit mask). A changed field that accepted does not cover is a
// *FixedError. Repeated and map fields are compared whole.
func Diff(current, desired proto.Message, accepted []string) ([]Change, error) {
	d := differ{accepted: accepted}
	d.walk("", current.ProtoReflect(), desired.ProtoReflect())
	if len(d.fixed) > 0 {
		slices.Sort(d.fixed)
		return nil, &FixedError{Paths: d.fixed}
	}
	slices.SortFunc(d.changes, func(a, b Change) int { return strings.Compare(a.Path, b.Path) })
	return d.changes, nil
}

type differ struct {
	accepted []string
	changes  []Change
	fixed    []string
}

func (d *differ) walk(prefix string, cur, des protoreflect.Message) {
	des.Range(func(fd protoreflect.FieldDescriptor, to protoreflect.Value) bool {
		path := string(fd.Name())
		if prefix != "" {
			path = prefix + "." + path
		} else if skipped[path] {
			return true
		}
		from, wasSet := cur.Get(fd), cur.Has(fd)
		changed := !wasSet || !from.Equal(to)
		switch {
		case slices.Contains(d.accepted, path):
			if changed {
				d.changes = append(d.changes, Change{Path: path, Field: fd, From: from, To: to, WasSet: wasSet})
			}
		case fd.Message() != nil && !fd.IsList() && !fd.IsMap() && d.covers(path):
			d.walk(path, from.Message(), to.Message())
		case changed:
			d.fixed = append(d.fixed, path)
		}
		return true
	})
}

// covers reports whether an accepted path is inside path.
func (d *differ) covers(path string) bool {
	return slices.ContainsFunc(d.accepted, func(p string) bool { return strings.HasPrefix(p, path+".") })
}

func parent(path string) string {
	if i := strings.LastIndex(path, "."); i >= 0 {
		return path[:i]
	}
	return ""
}
