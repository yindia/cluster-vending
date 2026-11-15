package sequence

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"

	"github.com/yindia/cluster-vending/pkg/crossplane/managed"
	"github.com/yindia/cluster-vending/pkg/kube"
)

// fingerprintLen is how many hex digits of an object's hash a fingerprint
// keeps: enough to tell a change, short enough to store per object.
const fingerprintLen = 16

// built is the fingerprint of an item rendered from live values (Build):
// only its identity counts, the body is not known before it is applied.
const built = "built"

// Fingerprints are the set's objects as it would apply them, one short
// hash per object (by apiVersion/kind/namespace/name) of its apply body
// (kube.ApplyObject, with Adjust): two renders of an owner with equal
// fingerprints apply the same objects. A Build item counts by its
// identity only, so a change inside a Build function is not seen here.
func Fingerprints(set Set, scheme *runtime.Scheme) (map[string]string, error) {
	out := make(map[string]string, len(set))
	for _, item := range set {
		ref, err := managed.RefOf(item.Object, scheme)
		if err != nil {
			return nil, err
		}
		if item.Build != nil {
			out[key(ref)] = built
			continue
		}
		body, err := kube.ApplyObject(item.Object, scheme)
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", key(ref), err)
		}
		if item.Adjust != nil {
			item.Adjust(body)
		}
		raw, err := json.Marshal(body.Object)
		if err != nil {
			return nil, fmt.Errorf("render %s: %w", key(ref), err)
		}
		sum := sha256.Sum256(raw)
		out[key(ref)] = hex.EncodeToString(sum[:])[:fingerprintLen]
	}
	return out, nil
}

// Digest is one short hash of a set's fingerprints: two renders with the
// same digest apply the same objects.
func Digest(fingerprints map[string]string) string {
	keys := slices.Sorted(maps.Keys(fingerprints))
	h := sha256.New()
	for _, k := range keys {
		_, _ = fmt.Fprintf(h, "%s=%s\n", k, fingerprints[k])
	}
	return hex.EncodeToString(h.Sum(nil))[:fingerprintLen]
}

// Changes counts how the objects of render now differ from those applied:
// changed bodies, objects added and objects removed.
type Changes struct {
	Changed, Added, Removed int
}

// Total is how many objects change.
func (c Changes) Total() int { return c.Changed + c.Added + c.Removed }

// Diff is how now differs from applied (Fingerprints of both).
func Diff(applied, now map[string]string) Changes {
	var c Changes
	for k, v := range now {
		was, ok := applied[k]
		switch {
		case !ok:
			c.Added++
		case was != v:
			c.Changed++
		}
	}
	for k := range applied {
		if _, ok := now[k]; !ok {
			c.Removed++
		}
	}
	return c
}

// String says it for people: "3 objects change (1 added)".
func (c Changes) String() string {
	n := c.Total()
	out := fmt.Sprintf("%d objects change", n)
	if n == 1 {
		out = "1 object changes"
	}
	var parts []string
	if c.Added > 0 {
		parts = append(parts, fmt.Sprintf("%d added", c.Added))
	}
	if c.Removed > 0 {
		parts = append(parts, fmt.Sprintf("%d removed", c.Removed))
	}
	if len(parts) > 0 {
		out += " (" + strings.Join(parts, ", ") + ")"
	}
	return out
}
