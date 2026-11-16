package subtenant

import "testing"

func TestQuotaUsesKubernetesQuantities(t *testing.T) {
	for _, test := range []struct {
		name    string
		count   int
		hard    map[string]string
		keys    []string
		invalid bool
	}{
		{"millicores and binary memory", 2, map[string]string{"requests.cpu": "500m", "limits.memory": "2Gi", "pods": "12"}, []string{"team-a", "team-b"}, false},
		{"negative", 1, map[string]string{"requests.cpu": "-1"}, []string{"a"}, true},
		{"malformed", 1, map[string]string{"requests.memory": "lots"}, []string{"a"}, true},
		{"fractional count", 1, map[string]string{"pods": "1.5"}, []string{"a"}, true},
		{"unsupported", 1, map[string]string{"privileged": "1"}, []string{"a"}, true},
		{"zero disables", 1, map[string]string{"services": "0"}, []string{"a"}, false},
		{"namespace limit", 1, map[string]string{"pods": "5"}, []string{"a", "b"}, true},
		{"too many namespaces", 65, map[string]string{"pods": "5"}, []string{"a"}, true},
		{"unbounded quota", 1, nil, []string{"a"}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateQuota(test.count, test.hard, test.keys)
			if (err != nil) != test.invalid {
				t.Fatalf("invalid=%v, error=%v", test.invalid, err)
			}
		})
	}
}

func TestNamespaceKeysAndAppendOnly(t *testing.T) {
	for _, keys := range [][]string{nil, {"a", "a"}, {"Uppercase"}, {"a/b"}, {"this-namespace-key-is-far-too-long"}} {
		if err := validateKeys(keys); err == nil {
			t.Fatalf("accepted invalid keys %v", keys)
		}
	}
	if err := validateKeys([]string{"team-a", "team-b"}); err != nil {
		t.Fatal(err)
	}
	if err := appendOnlyKeys([]string{"team-a"}, []string{"team-a", "team-b"}); err != nil {
		t.Fatal(err)
	}
	if err := appendOnlyKeys([]string{"team-a"}, []string{"renamed"}); err == nil {
		t.Fatal("removed an existing namespace key")
	}
}
