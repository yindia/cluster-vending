package sequence_test

import (
	"context"
	"testing"

	awsec2 "github.com/upbound/provider-aws/v2/apis/namespaced/ec2/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/yindia/cluster-vending/pkg/crossplane/sequence"
)

func gateBy(decisions map[string]sequence.Decision) func(context.Context, client.Object) sequence.Decision {
	return func(_ context.Context, obj client.Object) sequence.Decision { return decisions[obj.GetName()] }
}

// A gate that says Skip leaves the live object alone: it is remembered,
// not applied, and the set is still complete.
func TestGateSkipWritesNothing(t *testing.T) {
	e, inv, k := world(t)
	ctx := t.Context()
	a := &awsec2.InternetGateway{ObjectMeta: meta("a")}
	b := &awsec2.InternetGateway{ObjectMeta: meta("b")}
	set := sequence.Set{{Object: a}, {Object: b, Gate: gateBy(map[string]sequence.Decision{"b": {Skip: true}})}}
	if _, err := e.Apply(ctx, sequence.Of(b), inv); err != nil { // b exists already
		t.Fatal(err)
	}
	before := &awsec2.InternetGateway{}
	_ = k.Get(ctx, client.ObjectKeyFromObject(b), before)

	res, err := e.Apply(ctx, set, inv)

	if err != nil || !res.Complete() || res.Applied != 1 || len(res.Removed) != 0 {
		t.Fatalf("%+v (%v), want only a applied", res, err)
	}
	after := &awsec2.InternetGateway{}
	_ = k.Get(ctx, client.ObjectKeyFromObject(b), after)
	if after.ResourceVersion != before.ResourceVersion {
		t.Errorf("a skipped object was written: %s -> %s", before.ResourceVersion, after.ResourceVersion)
	}
	if !exists(t, k, a) {
		t.Error("a was not applied")
	}
	if len(inv.names()) != 2 {
		t.Errorf("a skipped object stays remembered: %v", inv.names())
	}
}

// A Hold keeps a new object from being created and an existing one from
// being changed; either is reported as waiting, and nothing is removed
// meanwhile.
func TestGateHoldWaits(t *testing.T) {
	e, inv, k := world(t)
	ctx := t.Context()
	live := &awsec2.InternetGateway{ObjectMeta: meta("live")}
	dropped := &awsec2.EIP{ObjectMeta: meta("dropped")}
	if _, err := e.Apply(ctx, sequence.Of(live, dropped), inv); err != nil {
		t.Fatal(err)
	}
	fresh := &awsec2.InternetGateway{ObjectMeta: meta("fresh")}
	hold := gateBy(map[string]sequence.Decision{"live": {Hold: "Release x to settle"}, "fresh": {Hold: "Release x to settle"}})
	set := sequence.Set{{Object: live, Gate: hold}, {Object: fresh, Gate: hold}}

	res, err := e.Apply(ctx, set, inv)

	if err != nil || res.Complete() || res.Applied != 0 || len(res.Waiting) != 2 {
		t.Fatalf("%+v (%v)", res, err)
	}
	for _, w := range res.Waiting {
		if len(w.For) != 1 || w.For[0] != "Release x to settle" {
			t.Errorf("%s waits for %v", w.Ref.Name, w.For)
		}
	}
	if exists(t, k, fresh) {
		t.Error("a held object was created")
	}
	if !exists(t, k, dropped) {
		t.Error("something was removed while objects wait")
	}
}

// The gate is asked in set order, once per object, and only about an
// object that otherwise would be applied.
func TestGateAskedInOrderAfterConditions(t *testing.T) {
	e, inv, _ := world(t)
	var asked []string
	gate := func(_ context.Context, obj client.Object) sequence.Decision {
		asked = append(asked, obj.GetName())
		return sequence.Decision{}
	}
	first := &awsec2.InternetGateway{ObjectMeta: meta("first")}
	second := &awsec2.InternetGateway{ObjectMeta: meta("second")}
	blocked := &awsec2.NATGateway{ObjectMeta: meta("blocked")}
	set := sequence.Set{{Object: first, Gate: gate}, {Object: second, Gate: gate},
		{Object: blocked, Gate: gate, After: []sequence.Condition{sequence.Ready(first)}}}

	if _, err := e.Apply(t.Context(), set, inv); err != nil {
		t.Fatal(err)
	}

	if len(asked) != 2 || asked[0] != "first" || asked[1] != "second" {
		t.Errorf("asked %v: an item that waits for a condition is not asked", asked)
	}
}
