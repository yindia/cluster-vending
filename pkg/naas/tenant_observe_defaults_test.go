package naas

import (
	"encoding/json"
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func networkPolicyPayload(spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "networking.k8s.io/v1", "kind": "NetworkPolicy",
		"metadata": map[string]any{"name": "baseline", "namespace": "tenant-app"}, "spec": spec,
	}}
}

func TestPayloadMatchesNetworkPolicyAPIDefaults(t *testing.T) {
	expected := networkPolicyPayload(map[string]any{
		"podSelector": map[string]any{}, "policyTypes": []any{"Ingress", "Egress"},
		"ingress": []any{}, "egress": []any{},
	})
	current := expected.DeepCopy()
	delete(current.Object["spec"].(map[string]any), "ingress")
	delete(current.Object["spec"].(map[string]any), "egress")
	beforeExpected, beforeCurrent := expected.DeepCopy(), current.DeepCopy()
	if !payloadMatches(current, expected) || !payloadMatches(expected, current) {
		t.Fatal("API omission of empty rule lists differs from deny-by-default template")
	}
	if !reflect.DeepEqual(expected, beforeExpected) || !reflect.DeepEqual(current, beforeCurrent) {
		t.Fatal("comparison mutated baseline or observed input")
	}
	current.Object["spec"].(map[string]any)["ingress"] = nil
	if !payloadMatches(current, expected) {
		t.Fatal("null/omitted optional ingress list differs from empty list")
	}
}

func TestPayloadMatchesNetworkPolicyTypedPorts(t *testing.T) {
	tcp := corev1.ProtocolTCP
	endPort := int32(15010)
	typed := &networkingv1.NetworkPolicy{
		TypeMeta: metav1.TypeMeta{APIVersion: "networking.k8s.io/v1", Kind: "NetworkPolicy"},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
			Ingress:     []networkingv1.NetworkPolicyIngressRule{{Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: ptrPort(15008), EndPort: &endPort}}}},
			Egress:      []networkingv1.NetworkPolicyEgressRule{{Ports: []networkingv1.NetworkPolicyPort{{Protocol: &tcp, Port: ptrPort(53)}}}},
		},
	}
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(typed)
	if err != nil {
		t.Fatal(err)
	}
	current := &unstructured.Unstructured{Object: object}
	// Reproduce the generic JSON template representation versus the typed
	// client's int64 values, and let the API supply TCP on both directions.
	raw, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	expected := &unstructured.Unstructured{Object: generic}
	for _, direction := range []string{"ingress", "egress"} {
		port := expected.Object["spec"].(map[string]any)[direction].([]any)[0].(map[string]any)["ports"].([]any)[0].(map[string]any)
		delete(port, "protocol")
		if _, ok := port["port"].(float64); !ok {
			t.Fatal("fixture did not exercise generic numeric decoding")
		}
	}
	beforeExpected, beforeCurrent := expected.DeepCopy(), current.DeepCopy()
	if !payloadMatches(current, expected) || !payloadMatches(expected, current) {
		t.Fatal("default TCP and integral port representations did not normalize")
	}
	if !reflect.DeepEqual(expected, beforeExpected) || !reflect.DeepEqual(current, beforeCurrent) {
		t.Fatal("normalization mutated inputs")
	}
}

func ptrPort(value int) *intstr.IntOrString {
	port := intstr.FromInt(value)
	return &port
}

func TestPayloadMatchesNetworkPolicyRejectsSecurityDrift(t *testing.T) {
	baseline := networkPolicyPayload(map[string]any{
		"podSelector": map[string]any{}, "policyTypes": []any{"Ingress", "Egress"},
		"ingress": []any{}, "egress": []any{},
	})
	for _, direction := range []string{"ingress", "egress"} {
		current := baseline.DeepCopy()
		current.Object["spec"].(map[string]any)[direction] = []any{map[string]any{}}
		if payloadMatches(current, baseline) {
			t.Fatalf("accepted allow-all %s rule", direction)
		}
	}
	for _, test := range []struct {
		name string
		edit func(map[string]any)
	}{
		{"pod selector", func(spec map[string]any) {
			spec["podSelector"] = map[string]any{"matchLabels": map[string]any{"app": "other"}}
		}},
		{"policy types", func(spec map[string]any) { spec["policyTypes"] = []any{"Ingress"} }},
		{"omitted policy types", func(spec map[string]any) { delete(spec, "policyTypes") }},
		{"unknown numeric field", func(spec map[string]any) { spec["unexpected"] = float64(53) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := baseline.DeepCopy()
			test.edit(current.Object["spec"].(map[string]any))
			if payloadMatches(current, baseline) {
				t.Fatal("accepted policy drift")
			}
		})
	}
	for _, direction := range []string{"ingress", "egress"} {
		peerField := "from"
		if direction == "egress" {
			peerField = "to"
		}
		expected := networkPolicyPayload(map[string]any{
			"podSelector": map[string]any{}, "policyTypes": []any{"Ingress", "Egress"},
			direction: []any{map[string]any{
				peerField: []any{map[string]any{"namespaceSelector": map[string]any{"matchLabels": map[string]any{"capsule.clastix.io/tenant": "owned"}}}},
				"ports":   []any{map[string]any{"port": int64(15008), "endPort": int64(15010), "protocol": "TCP"}},
			}},
		})
		for _, test := range []struct {
			name string
			edit func(map[string]any, map[string]any)
		}{
			{"UDP", func(_ map[string]any, port map[string]any) { port["protocol"] = "UDP" }},
			{"SCTP", func(_ map[string]any, port map[string]any) { port["protocol"] = "SCTP" }},
			{"named port", func(_ map[string]any, port map[string]any) { port["port"] = "15008" }},
			{"changed port", func(_ map[string]any, port map[string]any) { port["port"] = float64(15009) }},
			{"fractional port", func(_ map[string]any, port map[string]any) { port["port"] = 15008.5 }},
			{"changed range", func(_ map[string]any, port map[string]any) { port["endPort"] = float64(15011) }},
			{"namespace selector", func(rule map[string]any, _ map[string]any) {
				rule[peerField] = []any{map[string]any{"namespaceSelector": map[string]any{}}}
			}},
			{"omitted peer", func(rule map[string]any, _ map[string]any) { delete(rule, peerField) }},
			{"omitted ports", func(rule map[string]any, _ map[string]any) { delete(rule, "ports") }},
		} {
			t.Run(direction+"/"+test.name, func(t *testing.T) {
				current := expected.DeepCopy()
				rule := current.Object["spec"].(map[string]any)[direction].([]any)[0].(map[string]any)
				port := rule["ports"].([]any)[0].(map[string]any)
				test.edit(rule, port)
				if payloadMatches(current, expected) {
					t.Fatal("accepted security policy drift")
				}
			})
		}
	}
}

func TestPayloadMatchesDoesNotNormalizeOtherResources(t *testing.T) {
	expected := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "LimitRange", "spec": map[string]any{"limits": []any{map[string]any{"type": "Container", "default": map[string]any{"memory": "256Mi"}}}}}}
	current := expected.DeepCopy()
	if !payloadMatches(current, expected) {
		t.Fatal("unchanged live LimitRange quantity should match")
	}
	current.Object["spec"].(map[string]any)["limits"].([]any)[0].(map[string]any)["default"].(map[string]any)["memory"] = "257Mi"
	if payloadMatches(current, expected) {
		t.Fatal("accepted LimitRange resource drift")
	}
}
