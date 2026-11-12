package sequence_test

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
)

// userOfKey is an item built from a value of another object: the "arn"
// of ConfigMap "key". It waits while there is none.
func userOfKey() sequence.Item {
	return sequence.Item{
		Object: &corev1.ConfigMap{ObjectMeta: meta("user")},
		Build: func(ctx context.Context, seen sequence.Observed) (client.Object, string, error) {
			key := &corev1.ConfigMap{ObjectMeta: meta("key")}
			found, err := seen.Get(ctx, key)
			if err != nil || !found || key.Data["arn"] == "" {
				return nil, "the key has an ARN", err
			}
			return &corev1.ConfigMap{ObjectMeta: meta("user"), Data: map[string]string{"keyArn": key.Data["arn"]}}, "", nil
		},
	}
}

func data(t *testing.T, k client.Client, name string) map[string]string {
	t.Helper()
	cm := &corev1.ConfigMap{ObjectMeta: meta(name)}
	if err := k.Get(t.Context(), client.ObjectKeyFromObject(cm), cm); err != nil {
		t.Fatal(err)
	}
	return cm.Data
}

// An item built from a value that does not exist yet waits, also when the
// owner already has the object, and nothing is removed meanwhile. Once
// the value is there the object is applied with it.
func TestBuildWaitsForALateValue(t *testing.T) {
	e, inv, k := world(t)
	ctx := t.Context()
	before := sequence.Of(
		&corev1.ConfigMap{ObjectMeta: meta("user"), Data: map[string]string{"keyArn": "old"}},
		&corev1.ConfigMap{ObjectMeta: meta("dropped")})
	if _, err := e.Apply(ctx, before, inv); err != nil {
		t.Fatal(err)
	}
	key := &corev1.ConfigMap{ObjectMeta: meta("key")}
	if err := k.Create(ctx, key); err != nil {
		t.Fatal(err)
	}
	set := sequence.Set{userOfKey()}

	res, err := e.Apply(ctx, set, inv)
	if err != nil {
		t.Fatal(err)
	}
	if res.Complete() || res.Applied != 0 || len(res.Waiting) != 1 || res.Waiting[0].Ref.Name != "user" ||
		res.Waiting[0].For[0] != "the key has an ARN" {
		t.Fatalf("apply without the value = %+v, want the item waiting for it", res)
	}
	if got := data(t, k, "user")["keyArn"]; got != "old" {
		t.Errorf("the object the owner has was applied without its value: keyArn = %q", got)
	}
	if !exists(t, k, &corev1.ConfigMap{ObjectMeta: meta("dropped")}) {
		t.Error("an object was removed while an item waited for a value")
	}

	key.Data = map[string]string{"arn": "arn:aws:kms:key/1"}
	if err := k.Update(ctx, key); err != nil {
		t.Fatal(err)
	}
	res, err = e.Apply(ctx, set, inv)
	if err != nil || !res.Complete() || res.Applied != 1 || len(res.Removed) != 1 {
		t.Fatalf("apply with the value = %+v (%v), want the item applied and the dropped object removed", res, err)
	}
	if got := data(t, k, "user")["keyArn"]; got != "arn:aws:kms:key/1" {
		t.Errorf("keyArn = %q, want the key's ARN", got)
	}
}

// A Build makes the object its item names, nothing else.
func TestBuildMustMakeItsOwnObject(t *testing.T) {
	e, inv, k := world(t)
	set := sequence.Set{{
		Object: &corev1.ConfigMap{ObjectMeta: meta("user")},
		Build: func(context.Context, sequence.Observed) (client.Object, string, error) {
			return &corev1.ConfigMap{ObjectMeta: meta("another")}, "", nil
		},
	}}
	_, err := e.Apply(t.Context(), set, inv)
	if err == nil || !strings.Contains(err.Error(), "it made ConfigMap another") {
		t.Fatalf("apply = %v, want the build refused", err)
	}
	if exists(t, k, &corev1.ConfigMap{ObjectMeta: meta("another")}) || len(inv.entries) != 0 {
		t.Error("an object its item does not name was applied or remembered")
	}
}

// Observe gives a caller what a Build sees, without applying.
func TestObserve(t *testing.T) {
	e, inv, k := world(t)
	ctx := t.Context()
	owned := &corev1.ConfigMap{ObjectMeta: meta("owned")}
	if _, err := e.Apply(ctx, sequence.Of(owned), inv); err != nil {
		t.Fatal(err)
	}
	if err := k.Create(ctx, &corev1.ConfigMap{ObjectMeta: meta("foreign"), Data: map[string]string{"a": "b"}}); err != nil {
		t.Fatal(err)
	}
	seen, err := e.Observe(ctx, inv)
	if err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct {
		name                string
		found, ready, owned bool
	}{
		{"owned", true, true, true},
		{"foreign", true, true, false},
		{"missing", false, false, false},
	} {
		cm := &corev1.ConfigMap{ObjectMeta: meta(tt.name)}
		found, err := seen.Get(ctx, cm)
		if err != nil {
			t.Fatal(err)
		}
		ready, err := seen.Ready(ctx, &corev1.ConfigMap{ObjectMeta: meta(tt.name)})
		if err != nil {
			t.Fatal(err)
		}
		if found != tt.found || ready != tt.ready || seen.Owned(cm) != tt.owned {
			t.Errorf("%s: found %t, ready %t, owned %t; want %t, %t, %t", tt.name, found, ready, seen.Owned(cm),
				tt.found, tt.ready, tt.owned)
		}
	}
	foreign := &corev1.ConfigMap{ObjectMeta: meta("foreign")}
	if _, err := seen.Get(ctx, foreign); err != nil || foreign.Data["a"] != "b" {
		t.Errorf("Get did not read the object: %v (%v)", foreign.Data, err)
	}
}
