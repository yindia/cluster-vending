package planning

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"google.golang.org/protobuf/proto"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/fault"
	v1 "github.com/yindia/cluster-vending/pkg/gen/go/cloud/v1"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// ObjectKey includes the complete Kubernetes identity, including API group
// and namespace. Two providers can safely use the same kind and object name.
func ObjectKey(apiVersion, kind, namespace, name string) string {
	return strings.Join([]string{apiVersion, kind, namespace, name}, "/")
}

type Entry struct {
	managed.Ref
	Retained bool
}

type Snapshot struct {
	Client client.Client
	Reader client.Reader
	// Changes is the cloud-specific field comparison. Nil uses Policy.
	Changes      func(*unstructured.Unstructured, *unstructured.Unstructured) []*v1.PlannedChange
	Policy       Policy
	Prepare      func(*unstructured.Unstructured)
	CreateDetail func(*unstructured.Unstructured) string
	Replacing    func(*unstructured.Unstructured) bool
}

// Render reads live resources and uses Kubernetes dry-run apply for existing
// resources. It never writes desired state or a managed resource.
func (s Snapshot) Render(ctx context.Context, objects []client.Object, plan *v1.Plan) ([]string, map[string]bool, error) {
	reader := s.Reader
	if reader == nil {
		reader = s.Client
	}
	versions := []string{}
	rendered := map[string]bool{}
	for _, object := range objects {
		body, err := kube.ApplyObject(object, s.Client.Scheme())
		if err != nil {
			return nil, nil, err
		}
		key := ObjectKey(body.GetAPIVersion(), body.GetKind(), body.GetNamespace(), body.GetName())
		rendered[key] = true
		if body.GetKind() == "Usage" {
			continue
		}
		if s.Prepare != nil {
			s.Prepare(body)
		}
		// Bind the plan to the rendered desired state as well as live versions.
		spec, _, err := unstructured.NestedMap(body.Object, "spec")
		if err != nil {
			return nil, nil, err
		}
		encoded, err := json.Marshal(spec)
		if err != nil {
			return nil, nil, err
		}
		digest := sha256.Sum256(encoded)
		versions = append(versions, key+" desired="+hex.EncodeToString(digest[:]))
		live := &unstructured.Unstructured{}
		live.SetGroupVersionKind(body.GroupVersionKind())
		err = reader.Get(ctx, client.ObjectKeyFromObject(body), live)
		if apierrors.IsNotFound(err) {
			detail := "does not exist yet"
			if s.CreateDetail != nil {
				detail = s.CreateDetail(body)
			}
			replacement := s.Replacing != nil && s.Replacing(body)
			plan.Changes = append(plan.Changes, &v1.PlannedChange{ApiVersion: body.GetAPIVersion(), Kind: body.GetKind(), Namespace: body.GetNamespace(), Name: body.GetName(), Action: v1.PlannedAction_PLANNED_ACTION_CREATE, Detail: detail, RequiresReplacement: replacement})
			versions = append(versions, key+"=absent")
			continue
		}
		if err != nil {
			return nil, nil, fault.New(fault.Unavailable, fmt.Sprintf("cannot read %s: %v", key, err))
		}
		versions = append(versions, key+"="+ObjectVersion(live))
		dry, err := kube.DryRunApply(ctx, s.Client, body)
		if err != nil {
			return nil, nil, fault.New(fault.FailedPrecondition, "the management cluster refuses the planned resource: "+err.Error())
		}
		changes := FieldChanges(live, dry, s.Policy)
		if s.Changes != nil {
			changes = s.Changes(live, dry)
		}
		for _, change := range changes {
			change.Namespace = body.GetNamespace()
		}
		plan.Changes = append(plan.Changes, changes...)
	}
	return versions, rendered, nil
}

// Removals compares the persisted inventory with the desired set. Carried-over
// resources are retained. Every deletion records its live version so applying
// a previously reviewed plan fails if a removal target changes.
func (s Snapshot) Removals(ctx context.Context, entries []Entry, rendered map[string]bool, plan *v1.Plan) ([]string, error) {
	reader := s.Reader
	if reader == nil {
		reader = s.Client
	}
	var versions []string
	for _, entry := range entries {
		key := ObjectKey(entry.APIVersion, entry.Kind, entry.Namespace, entry.Name)
		if rendered[key] || entry.Retained || entry.Kind == "Usage" {
			continue
		}
		change := &v1.PlannedChange{ApiVersion: entry.APIVersion, Kind: entry.Kind, Namespace: entry.Namespace, Name: entry.Name, ExternalName: entry.ExternalName, Action: v1.PlannedAction_PLANNED_ACTION_DELETE, Detail: "no longer in the desired configuration; managed resource and cloud resource will be deleted"}
		live := &unstructured.Unstructured{}
		live.SetAPIVersion(entry.APIVersion)
		live.SetKind(entry.Kind)
		err := reader.Get(ctx, client.ObjectKey{Namespace: entry.Namespace, Name: entry.Name}, live)
		switch {
		case apierrors.IsNotFound(err):
			versions = append(versions, key+"=absent")
			change.Detail = "managed resource is already absent; its inventory reference will be removed (cloud state is unknown)"
		case err != nil:
			return nil, fault.New(fault.Unavailable, fmt.Sprintf("cannot read removal target %s: %v", key, err))
		default:
			versions = append(versions, key+"="+ObjectVersion(live))
			if external := live.GetAnnotations()[managed.ExternalNameAnnotation]; external != "" {
				change.ExternalName = external
			}
			policies, found, _ := unstructured.NestedStringSlice(live.Object, "spec", "managementPolicies")
			change.RetainsCloudResource = live.GetAnnotations()[managed.RetainAnnotation] == "true" ||
				found && !slices.Contains(policies, "*") && !slices.Contains(policies, "Delete") && !slices.Contains(policies, "Create")
			if change.RetainsCloudResource {
				change.Detail = "managed resource will be removed; the cloud resource is retained by its management policy"
			}
		}
		plan.Changes = append(plan.Changes, change)
	}
	return versions, nil
}

// ObjectVersion is what a plan binds to of a live object: its identity, its
// generation and a digest of its spec and annotations (retention and the
// external name live there). Not its resourceVersion, which every status
// write of a controller changes: a plan made while the object is being
// updated would be stale before it is applied.
func ObjectVersion(live *unstructured.Unstructured) string {
	spec, _, _ := unstructured.NestedMap(live.Object, "spec")
	encoded, _ := json.Marshal([]any{spec, live.GetAnnotations()}) // maps marshal with sorted keys
	digest := sha256.Sum256(encoded)
	return fmt.Sprintf("%s/gen%d/%s", live.GetUID(), live.GetGeneration(), hex.EncodeToString(digest[:8]))
}

func ID(request proto.Message, versions []string) (string, error) {
	raw, err := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if err != nil {
		return "", err
	}
	versions = slices.Clone(versions)
	slices.Sort(versions)
	digest := sha256.New()
	digest.Write(raw)
	digest.Write([]byte(strings.Join(versions, "\n")))
	return hex.EncodeToString(digest.Sum(nil)), nil
}
