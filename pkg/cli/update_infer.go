package cli

import (
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/yindia/cluster-vending/pkg/cluster"
	cloudv1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/maintenance"
	"github.com/yindia/cluster-vending/pkg/network"
	"github.com/yindia/cluster-vending/pkg/updatemask"
)

// maxValue bounds a value shown in a change line.
const maxValue = 60

// inferredMask is the mask of an update: the fields the manifest
// (desired) states that differ from the object now (current),
// printed one per line to Progress. Empty: nothing to change, said so.
func inferredMask(o Options, what string, current, desired proto.Message, accepted []string) ([]string, error) {
	changes, err := updatemask.Diff(current, desired, accepted)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", what, err)
	}
	if len(changes) == 0 {
		o.progressf("%s: nothing to change\n", what)
		return nil, nil
	}
	o.progressf("Changing %s:\n", what)
	for _, c := range changes {
		o.progressf("  %s: %s → %s\n", c.Path, shownValue(c.Field, c.From, c.WasSet), shownValue(c.Field, c.To, true))
	}
	return updatemask.Paths(changes), nil
}

// clusterAsStored is the manifest's cluster with what the server merges in
// itself (the default add-ons), so it compares with the resolved spec: for
// a server that cannot answer the cluster as requested.
func clusterAsStored(cl *cloudv1.Cluster) *cloudv1.Cluster {
	out := proto.CloneOf(cl)
	if aws := out.GetSpec().GetAws(); len(aws.GetAddons()) > 0 {
		aws.Addons = cluster.MergeAddons(aws.GetAddons())
	}
	return out
}

// networkAsStored is the manifest's network with the defaults the server
// stores flow logs with.
func networkAsStored(n *cloudv1.Network) *cloudv1.Network {
	out := proto.CloneOf(n)
	if out.GetSpec().GetFlowLogs() != nil {
		out.Spec.FlowLogs = network.ResolveFlowLogs(out.GetSpec().GetFlowLogs())
	}
	return out
}

// shownValue is a field's value in a change line: <none> when unset,
// enums as words, a maintenance window as it reads, other messages and
// lists as compact JSON (cut at maxValue).
func shownValue(fd protoreflect.FieldDescriptor, v protoreflect.Value, set bool) string {
	switch {
	case !set:
		return none
	case fd.IsMap():
		return cut(mapText(v.Map()))
	case fd.IsList():
		return cut(listText(fd, v.List()))
	case fd.Enum() != nil:
		return enumText(fd.Enum(), v.Enum())
	case fd.Message() != nil:
		if w, ok := v.Message().Interface().(*cloudv1.MaintenanceWindow); ok {
			return maintenance.Describe(w)
		}
		return cut(jsonText(v.Message().Interface()))
	default:
		return cut(fmt.Sprint(v.Interface()))
	}
}

func enumText(e protoreflect.EnumDescriptor, n protoreflect.EnumNumber) string {
	vals := e.Values()
	v := vals.ByNumber(n)
	if v == nil {
		return fmt.Sprint(n)
	}
	names := make([]string, 0, vals.Len())
	for i := range vals.Len() {
		names = append(names, string(vals.Get(i).Name()))
	}
	return word(string(v.Name()), commonPrefix(names))
}

// commonPrefix is the prefix the names share, up to an underscore.
func commonPrefix(names []string) string {
	if len(names) == 0 {
		return ""
	}
	p := names[0]
	for _, n := range names[1:] {
		for !strings.HasPrefix(n, p) {
			p = p[:len(p)-1]
		}
	}
	return p[:strings.LastIndex(p, "_")+1]
}

func mapText(m protoreflect.Map) string {
	var kv []string
	m.Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
		kv = append(kv, fmt.Sprintf("%s=%v", k.String(), v.Interface()))
		return true
	})
	slices.Sort(kv)
	return strings.Join(kv, ",")
}

func listText(fd protoreflect.FieldDescriptor, l protoreflect.List) string {
	items := make([]string, 0, l.Len())
	for i := range l.Len() {
		v := l.Get(i)
		switch {
		case fd.Enum() != nil:
			items = append(items, enumText(fd.Enum(), v.Enum()))
		case fd.Message() != nil:
			items = append(items, jsonText(v.Message().Interface()))
		default:
			items = append(items, fmt.Sprint(v.Interface()))
		}
	}
	return "[" + strings.Join(items, ", ") + "]"
}

func jsonText(m proto.Message) string {
	b, err := protojson.MarshalOptions{}.Marshal(m)
	if err != nil {
		return "…"
	}
	return strings.ReplaceAll(string(b), " ", "")
}

func cut(s string) string {
	if r := []rune(s); len(r) > maxValue {
		return string(r[:maxValue-1]) + "…"
	}
	return s
}

// withoutAutomaticVersion is desired with the cluster's own Kubernetes
// version when its upgrades are automatic after the update (the manifest's
// mode when it states one, else the cluster's): the channel sets the
// version, so a manifest's is not a change. Said once when they differ.
func withoutAutomaticVersion(o Options, current, desired *cloudv1.Cluster) *cloudv1.Cluster {
	mode := current.GetSpec().GetUpgradeMode()
	if m := desired.GetSpec().GetUpgradeMode(); m != cloudv1.UpgradeMode_UPGRADE_MODE_UNSPECIFIED {
		mode = m
	}
	want, have := desired.GetSpec().GetControlPlane().GetKubernetesVersion(), current.GetSpec().GetControlPlane().GetKubernetesVersion()
	if mode != cloudv1.UpgradeMode_UPGRADE_MODE_AUTOMATIC || want == "" || want == have {
		return desired
	}
	o.progressf("cluster %s: kubernetesVersion %s ignored: upgrades are automatic, the release channel sets the version (now %s)\n",
		current.GetName(), want, have)
	out := proto.CloneOf(desired)
	out.Spec.ControlPlane.KubernetesVersion = have
	return out
}
